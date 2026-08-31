// Package client is a Go client for the folio JSON API.
//
// It is what the terminal UI talks to, and it is deliberately the same surface
// the browser uses: every endpoint in [httpapi] has a method here, with the same
// semantics, so the TUI cannot quietly acquire behaviour the web app does not
// have.
//
// Authentication is the tailnet, exactly as it is everywhere else in folio.
// There is no token to configure. Requests carry no Origin header, which is what
// the server's CSRF check wants from a non-browser client: it only blocks a
// request that a browser was tricked into sending, and this is not one.
package client

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultServer is where `folio dev` listens, which is the server most people
// point the TUI at first.
const DefaultServer = "http://127.0.0.1:8080"

// maxErrorBody caps how much of a failed response we read looking for the JSON
// error body. An HTML error page from a proxy in front of folio would otherwise
// be pulled into memory in full just to be thrown away.
const maxErrorBody = 64 << 10

// Client talks to one folio server.
type Client struct {
	base *url.URL
	http *http.Client
	ua   string
}

// Option configures a [Client].
type Option func(*Client)

// WithHTTPClient replaces the underlying HTTP client. The event stream sets its
// own timeout regardless, since an SSE response is meant to stay open.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option { return func(c *Client) { c.ua = ua } }

// New returns a client for the folio server at the given base URL.
//
// A bare host is taken as https, since the only way to reach folio without TLS
// is loopback, and that is always spelled out.
func New(server string, opts ...Option) (*Client, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		server = DefaultServer
	}
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	u, err := url.Parse(strings.TrimSuffix(server, "/"))
	if err != nil {
		return nil, fmt.Errorf("bad server URL %q: %w", server, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("bad server URL %q: no host", server)
	}
	c := &Client{
		base: u,
		http: &http.Client{Timeout: 30 * time.Second},
		ua:   "folio-tui",
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Server is the base URL this client was built with.
func (c *Client) Server() string { return c.base.String() }

// Error is a failed API call.
type Error struct {
	Status  int
	Method  string
	Path    string
	Message string
	// ConflictPath is set on a 409 write conflict and names the file the server
	// wrote the caller's rejected content to.
	ConflictPath string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s %s: %s", e.Method, e.Path, http.StatusText(e.Status))
	}
	return fmt.Sprintf("%s %s: %s", e.Method, e.Path, e.Message)
}

// Sentinels for the statuses a caller acts on differently. Match them with
// errors.Is rather than comparing Status, so a wrapped error still works.
var (
	ErrBadRequest   = errors.New("bad request")
	ErrForbidden    = errors.New("forbidden")
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrUnavailable  = errors.New("service unavailable")
	ErrServerFailed = errors.New("server error")
)

// Is maps the HTTP status onto the sentinels above.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrBadRequest:
		return e.Status == http.StatusBadRequest
	case ErrForbidden:
		return e.Status == http.StatusForbidden
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict
	case ErrUnavailable:
		return e.Status == http.StatusServiceUnavailable
	case ErrServerFailed:
		return e.Status >= 500
	}
	return false
}

// ConflictPath returns where the server stashed a rejected write, if err is a
// save conflict. The editor uses it to tell you nothing was lost.
func ConflictPath(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.ConflictPath
	}
	return ""
}

// request is one call's inputs. Zero values are fine for everything but Method
// and Path.
type request struct {
	Method string
	// Path is a decoded API path, such as "/api/vaults/me/notes/Daily/a b.md".
	// Escaping is net/url's job: assigning it to URL.Path and letting String()
	// encode is the only way to get a path segment containing a space, a '#', or
	// a '%' onto the wire correctly.
	Path   string
	Query  url.Values
	Body   any
	Header http.Header
	// Raw, when set, receives the response body instead of it being decoded as
	// JSON. Used for attachments.
	Raw func(resp *http.Response) error
}

// do performs a request and decodes the response into a T.
//
// This is a Go 1.27 generic method, for the same reason [httpapi.API.JSON] is
// one on the other side of the wire: the response shape is fixed at the call
// site at compile time, rather than being an `any` the caller has to assert.
func (c *Client) do[T any](ctx context.Context, req request) (T, http.Header, error) {
	var zero T

	u := *c.base
	u.Path = c.base.Path + req.Path
	if len(req.Query) > 0 {
		u.RawQuery = req.Query.Encode()
	}

	var body io.Reader
	if req.Body != nil {
		if b, ok := req.Body.([]byte); ok {
			body = bytes.NewReader(b)
		} else {
			buf := &bytes.Buffer{}
			if err := json.MarshalWrite(buf, req.Body); err != nil {
				return zero, nil, fmt.Errorf("encoding request: %w", err)
			}
			body = buf
		}
	}

	r, err := http.NewRequestWithContext(ctx, req.Method, u.String(), body)
	if err != nil {
		return zero, nil, err
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	if req.Body != nil && r.Header.Get("Content-Type") == "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("User-Agent", c.ua)
	r.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(r)
	if err != nil {
		return zero, nil, fmt.Errorf("%s %s: %w", req.Method, req.Path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return zero, resp.Header, apiError(req, resp)
	}
	if req.Raw != nil {
		return zero, resp.Header, req.Raw(resp)
	}
	if resp.StatusCode == http.StatusNoContent || resp.ContentLength == 0 {
		return zero, resp.Header, nil
	}

	var out T
	if err := json.UnmarshalRead(resp.Body, &out); err != nil {
		return zero, resp.Header, fmt.Errorf("%s %s: decoding response: %w", req.Method, req.Path, err)
	}
	return out, resp.Header, nil
}

// apiError turns a failed response into an [Error], using the server's JSON
// error body when there is one.
func apiError(req request, resp *http.Response) error {
	e := &Error{Status: resp.StatusCode, Method: req.Method, Path: req.Path}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var body struct {
		Error        string `json:"error"`
		ConflictPath string `json:"conflictPath"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Error != "" {
		e.Message = body.Error
		e.ConflictPath = body.ConflictPath
	} else if s := strings.TrimSpace(string(raw)); s != "" && !strings.HasPrefix(s, "<") {
		// Not JSON and not an HTML error page: some proxy's plain-text message,
		// which is still more use than the bare status.
		e.Message = firstLine(s)
	}
	return e
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// etag strips the quoting from an ETag header.
func etag(h http.Header) string {
	return strings.Trim(h.Get("ETag"), `"`)
}
