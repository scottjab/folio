// Package mcpsrv exposes a user's notes to AI agents over the Model Context
// Protocol.
//
// Every tool runs as the tailnet user who made the request, through the same
// [notes.Service] the web API uses. That is the whole security story: an agent
// sees exactly what the person it acts for sees, because it is going through the
// identical permission checks, not a parallel set that could drift.
//
// Two transports are offered. Streamable HTTP is mounted at /mcp on the tsnet
// listener, which is what a modern MCP client connects to directly. For clients
// that only speak stdio, `folio mcp` bridges the two; see bridge.go.
package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/index"
	"github.com/scottjab/folio/internal/notes"
	"github.com/scottjab/folio/internal/share"
)

// Version is reported to clients during initialize.
const Version = "0.1.0"

// Deps are the collaborators the MCP server needs.
type Deps struct {
	Notes    *notes.Service
	Index    *index.Index
	Identity *identity.Resolver
	Shares   *share.Resolver
	Bus      *events.Bus
	Log      *slog.Logger
}

// Server builds per-user MCP servers.
type Server struct {
	Deps
}

// New returns a Server.
func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Server{Deps: d}
}

// session is the per-request state every tool handler closes over: who is
// asking, and the services to ask on their behalf.
type session struct {
	*Server
	user identity.User
}

// ForUser builds an MCP server whose every tool acts as u.
//
// A fresh server per request sounds wasteful, but it is what makes the identity
// binding airtight: there is no code path where a handler could read a user from
// somewhere mutable and get the wrong one.
func (s *Server) ForUser(u identity.User) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "folio",
		Title:   "folio",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	sess := &session{Server: s, user: u}
	sess.addTools(srv)
	sess.addResources(srv)
	sess.addPrompts(srv)
	return srv
}

// instructions tell the model how this vault is organised, which saves it
// guessing at conventions and asking the user.
const instructions = `folio is a markdown notes vault on a Tailscale tailnet.

Notes are plain markdown files with YAML frontmatter. They link to each other
with Obsidian-style wikilinks, [[Folder/Note]], and carry #tags inline or in
frontmatter. Paths are vault-relative and use forward slashes, for example
"Daily/2026-08-30.md" or "Projects/folio.md".

Start with search_notes to find things; it does full-text search with
BM25 ranking and supports tag:, path:, and title: filters, "quoted phrases",
prefix* matching, and -exclusions. Use list_notes to browse by folder or tag.

When changing a note, prefer edit_note or append_note over update_note: they
touch only what needs to change, which is cheaper and cannot accidentally drop
the rest of the note. update_note accepts a baseSha for safe read-modify-write.

A link is not a path. [[folio]] resolves against the whole vault, preferring the
linking note's own folder, so the same bare name can mean different notes in
different places. Use resolve_link rather than guessing: it answers with the same
rule the editor and the index use, and it understands [[Note#Heading]] and
![[Note]] embeds, returning the section or the note that would render there.

To add a file, use attach_file and write the link it hands back. Do not invent a
path for it: where attachments go is a setting this user chose, get_settings will
tell you what it is, and the browser and terminal clients obey the same one.

Other people's notes appear only when they have shared them with this user, and
are read-only unless the share grants write.`

// HTTPHandler returns the Streamable HTTP handler.
//
// It expects the caller to already be authenticated: mount it behind the API's
// identity middleware, which puts the user in the request context. Doing the
// lookup here instead would duplicate the error mapping and let the two drift.
func (s *Server) HTTPHandler(userFrom func(context.Context) identity.User) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return s.ForUser(userFrom(r.Context()))
	}, nil)
}

// scope resolves a vault name for the session's user.
func (sess *session) scope(ctx context.Context, vaultName string) (notes.Scope, error) {
	return sess.Notes.Scope(ctx, sess.user, vaultName)
}

// toolErr converts an internal error into something a model can act on.
//
// The wording matters more than usual here: a model reads the error and decides
// what to do next, so "you may only read this note" produces a better next step
// than a bare "denied".
func toolErr(err error) error {
	switch {
	case errors.Is(err, share.ErrDenied):
		return fmt.Errorf("permission denied: this note is not shared with you, or is shared read-only: %w", err)
	case errors.Is(err, index.ErrNotFound):
		return fmt.Errorf("not found: %w; use search_notes or list_notes to find the right path", err)
	default:
		return err
	}
}

// now is the clock used for daily notes; tests replace it.
var now = time.Now

// The index marks search matches with these control characters rather than
// markup, so a note containing literal "<mark>" cannot forge a highlight.
const (
	indexHighlightOpen  = index.HighlightOpen
	indexHighlightClose = index.HighlightClose
)
