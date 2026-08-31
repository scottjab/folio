// Package httpapi serves the JSON API and the embedded web app.
//
// Authentication is the tailnet. Every request's peer address goes to
// [identity.Resolver], which asks tailscaled who is on the other end. There are
// no cookies, tokens, or sessions anywhere, which removes a whole class of bugs
// and creates exactly one: because the browser authenticates by virtue of being
// on the tailnet, any page on the public internet could aim a form at
// https://folio.your-tailnet.ts.net and have it arrive authenticated. That is
// what the CSRF check in middleware.go is for, and why it is not optional.
package httpapi

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/index"
	"github.com/scottjab/folio/internal/notes"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/store"
	"github.com/scottjab/folio/internal/vault"
)

// maxNoteBytes caps a single note. Notes are prose; anything past this is a
// paste accident or an attachment in the wrong place.
const maxNoteBytes = 8 << 20 // 8 MiB

// maxAttachmentBytes caps an upload.
const maxAttachmentBytes = 64 << 20 // 64 MiB

// Deps is everything the API needs.
type Deps struct {
	DB       *store.DB
	Index    *index.Index
	Notes    *notes.Service
	Vaults   *vault.Set
	Identity *identity.Resolver
	Shares   *share.Resolver
	Bus      *events.Bus
	Log      *slog.Logger

	// Static serves the built web app. Nil falls back to a minimal built-in
	// shell, so the API is usable and testable without a frontend build.
	Static fs.FS

	// PeerAddr extracts the tailnet peer address from a request. Nil means
	// r.RemoteAddr, which is right for the tsnet listener. The dev listener and
	// the tests override it.
	PeerAddr func(*http.Request) string

	// MCP, if set, is mounted at /mcp.
	MCP http.Handler
}

// API is the HTTP surface of folio.
type API struct {
	Deps
	mux *http.ServeMux

	// closed is cancelled by Close and watched by every open-ended response, so
	// shutdown does not have to wait out its grace period on streams that were
	// designed never to end.
	closed   context.Context
	closeNow context.CancelFunc
}

// New builds the API and its routes.
func New(d Deps) *API {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.PeerAddr == nil {
		d.PeerAddr = func(r *http.Request) string { return r.RemoteAddr }
	}
	if d.Static == nil {
		d.Static = fallbackApp()
	}
	if d.Notes == nil {
		d.Notes = notes.New(notes.Deps{
			DB: d.DB, Index: d.Index, Vaults: d.Vaults,
			Identity: d.Identity, Shares: d.Shares, Bus: d.Bus,
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	a := &API{Deps: d, mux: http.NewServeMux(), closed: ctx, closeNow: cancel}
	a.routes()
	return a
}

// Close ends every streaming response.
//
// Call it before http.Server.Shutdown. An SSE stream stays open by design, and
// Shutdown waits for active handlers, so without this a single open browser tab
// makes Ctrl-C take the full grace period.
func (a *API) Close() { a.closeNow() }

// ServeHTTP applies the middleware chain and dispatches.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.withSecurityHeaders(a.withCSRF(a.mux)).ServeHTTP(w, r)
}

func (a *API) routes() {
	// Identity is required for everything under /api. Attaching it per-route
	// rather than globally keeps the static app servable before we know who is
	// asking, which matters for the browser's first paint.
	api := func(h authedHandler) http.Handler { return a.withIdentity(h) }

	a.mux.Handle("GET /api/me", api(a.handleMe))
	a.mux.Handle("GET /api/vaults", api(a.handleListVaults))

	a.mux.Handle("GET /api/vaults/{vault}/notes", api(a.handleListNotes))
	a.mux.Handle("POST /api/vaults/{vault}/notes", api(a.handleCreateNote))
	a.mux.Handle("GET /api/vaults/{vault}/notes/{path...}", api(a.handleGetNote))
	a.mux.Handle("PUT /api/vaults/{vault}/notes/{path...}", api(a.handlePutNote))
	a.mux.Handle("DELETE /api/vaults/{vault}/notes/{path...}", api(a.handleDeleteNote))
	a.mux.Handle("POST /api/vaults/{vault}/move", api(a.handleMoveNote))
	a.mux.Handle("POST /api/vaults/{vault}/append", api(a.handleAppendNote))
	a.mux.Handle("POST /api/vaults/{vault}/edit", api(a.handleEditNote))
	a.mux.Handle("GET /api/vaults/{vault}/daily", api(a.handleDailyNote))
	a.mux.Handle("GET /api/vaults/{vault}/backlinks/{path...}", api(a.handleBacklinks))

	a.mux.Handle("GET /api/vaults/{vault}/attachments/{path...}", api(a.handleGetAttachment))
	a.mux.Handle("POST /api/vaults/{vault}/attachments/{path...}", api(a.handlePutAttachment))

	a.mux.Handle("GET /api/search", api(a.handleSearch))
	a.mux.Handle("GET /api/tags", api(a.handleTags))
	a.mux.Handle("GET /api/folders", api(a.handleFolders))
	a.mux.Handle("GET /api/users", api(a.handleUsers))

	a.mux.Handle("GET /api/shares", api(a.handleListShares))
	a.mux.Handle("POST /api/shares", api(a.handleCreateShare))
	a.mux.Handle("DELETE /api/shares/{id}", api(a.handleDeleteShare))
	a.mux.Handle("GET /api/shared", api(a.handleSharedWithMe))

	a.mux.Handle("GET /api/events", api(a.handleEvents))

	if a.MCP != nil {
		// Behind withIdentity, so an MCP tool call is authenticated exactly like
		// an API call and reads the same user out of the context.
		mcpHandler := a.withIdentity(a.MCP.ServeHTTP)
		a.mux.Handle("/mcp", mcpHandler)
		a.mux.Handle("/mcp/", mcpHandler)
	}

	// An unknown /api path must 404 as JSON rather than falling through to the
	// single-page app, or a typo in a fetch() comes back as HTML and the client
	// reports a confusing parse error instead of a 404.
	a.mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.fail(w, r, http.StatusNotFound, fmt.Errorf("no such endpoint: %s %s", r.Method, r.URL.Path))
	}))

	a.mux.Handle("/", a.staticHandler())
}

// JSON writes v as the response body.
//
// This is a Go 1.27 generic method. The type parameter buys real safety at the
// call sites: the handler's response shape is fixed at compile time rather than
// being an `any` that silently marshals whatever it was handed.
func (a *API) JSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// MarshalWrite streams straight to the socket, with no intermediate buffer
	// per response.
	if err := json.MarshalWrite(w, v); err != nil {
		// The status line is already gone, so all we can do is record it.
		a.Log.Error("encoding response failed mid-write", "err", err)
	}
}

// Decode reads a JSON request body into a T.
//
// RejectUnknownMembers is the important part. A client that misspells a field
// gets a 400 naming the field, instead of a request that appears to succeed
// while silently dropping what the user typed.
func (a *API) Decode[T any](r *http.Request, limit int64) (T, error) {
	var v T
	body := http.MaxBytesReader(nil, r.Body, limit)
	if err := json.UnmarshalRead(body, &v, json.RejectUnknownMembers(true)); err != nil {
		return v, fmt.Errorf("invalid request body: %w", err)
	}
	return v, nil
}

// errorBody is the shape of every error response.
type errorBody struct {
	Error string `json:"error"`
	// ConflictPath is set on a 409 write conflict and names the file holding the
	// caller's rejected content.
	ConflictPath string `json:"conflictPath,omitzero"`
}

// fail writes an error response, logging server-side problems.
func (a *API) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	if status >= 500 {
		a.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "status", status, "err", err)
	} else {
		a.Log.Debug("request rejected", "method", r.Method, "path", r.URL.Path, "status", status, "err", err)
	}

	body := errorBody{Error: err.Error()}
	var ce *vault.ConflictError
	if errors.As(err, &ce) {
		body.ConflictPath = ce.ConflictPath
	}
	a.JSON(w, status, body)
}

// statusFor maps a domain error to an HTTP status.
//
// The distinction that matters most: an identity service we could not reach is
// 503, never 403. Telling someone they are forbidden when tailscaled is
// restarting sends them off debugging their ACLs for an hour.
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, identity.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, identity.ErrNoIdentity),
		errors.Is(err, share.ErrDenied),
		errors.Is(err, share.ErrNotOwner):
		return http.StatusForbidden
	case errors.Is(err, vault.ErrNotFound),
		errors.Is(err, index.ErrNotFound),
		errors.Is(err, share.ErrNoSuchShare):
		return http.StatusNotFound
	case errors.Is(err, vault.ErrExists):
		return http.StatusConflict
	case errors.Is(err, vault.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, identity.ErrUnknownUser),
		errors.Is(err, notes.ErrNoMatch),
		errors.Is(err, notes.ErrAmbiguous):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// writeSSE emits one server-sent event, using jsontext to encode the payload
// straight into the response.
func writeSSE(w http.ResponseWriter, enc *jsontext.Encoder, id, event string, payload any) error {
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: ", event); err != nil {
		return err
	}
	if err := json.MarshalEncode(enc, payload); err != nil {
		return err
	}
	_, err := fmt.Fprint(w, "\n\n")
	return err
}

// ctxKey namespaces the values the middleware attaches to a request.
type ctxKey int

const userKey ctxKey = iota

// userFrom returns the authenticated user attached by withIdentity.
func userFrom(ctx context.Context) identity.User {
	u, _ := ctx.Value(userKey).(identity.User)
	return u
}

// UserFrom returns the authenticated user attached to a request context by the
// identity middleware. It is exported so the MCP handler, which is mounted
// behind that middleware, can read the same user rather than resolving the peer
// a second time and risking a different answer.
func UserFrom(ctx context.Context) identity.User { return userFrom(ctx) }
