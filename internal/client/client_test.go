package client_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scottjab/folio/internal/client"
)

// recorder captures what the client put on the wire, which is the part of this
// package that can silently be wrong: a path built by hand, a header left off, a
// body shaped the way the server does not expect.
type recorder struct {
	mu      sync.Mutex
	method  string
	path    string
	rawPath string
	query   string
	header  http.Header
	body    string
}

func (r *recorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.method, r.path, r.rawPath = req.Method, req.URL.Path, req.URL.EscapedPath()
	r.query, r.header, r.body = req.URL.RawQuery, req.Header.Clone(), string(body)
}

func (r *recorder) get() recorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return recorder{method: r.method, path: r.path, rawPath: r.rawPath, query: r.query, header: r.header, body: r.body}
}

// newTest starts a server running h and returns a client pointed at it.
func newTest(t *testing.T, h http.HandlerFunc) *client.Client {
	t.Helper()
	// An in-memory network, so the suite opens no real sockets.
	srv := httptest.NewTestServer(t, h)
	srv.Start()
	t.Cleanup(srv.Close)

	c, err := client.New(srv.URL, client.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

// writeJSON is the server side of a happy-path response.
func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.MarshalWrite(w, v); err != nil {
		t.Errorf("encoding test response: %v", err)
	}
}

func TestNewNormalizesServer(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", client.DefaultServer},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080"},
		// A bare host means the tailnet, which is always HTTPS. Guessing http
		// there would send notes over the wire in the clear.
		{"folio.example.ts.net", "https://folio.example.ts.net"},
		{"  folio.example.ts.net  ", "https://folio.example.ts.net"},
	}
	for _, tc := range tests {
		c, err := client.New(tc.in)
		if err != nil {
			t.Fatalf("New(%q): %v", tc.in, err)
		}
		if got := c.Server(); got != tc.want {
			t.Errorf("New(%q).Server() = %q, want %q", tc.in, got, tc.want)
		}
	}

	if _, err := client.New("://nope"); err == nil {
		t.Error("New with an unparseable URL should fail")
	}
}

func TestNoteRoundTrip(t *testing.T) {
	var rec recorder
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		writeJSON(t, w, http.StatusOK, map[string]any{
			"vault": "me", "path": "Daily/2026-08-30.md", "content": "# Thursday\n",
			"sha256": "abc", "title": "Thursday", "tags": []string{"daily"},
			"perm": "write", "backlinks": []any{},
		})
	})

	n, err := c.Note(t.Context(), "me", "Daily/2026-08-30.md")
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	if n.Title != "Thursday" || n.Content != "# Thursday\n" || !n.Perm.CanWrite() {
		t.Errorf("decoded note is wrong: %+v", n)
	}
	got := rec.get()
	if want := "/api/vaults/me/notes/Daily/2026-08-30.md"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
}

// A path with characters that must be escaped is the case most likely to be got
// wrong, and it fails as a confusing 404 rather than anything obvious.
func TestNotePathIsEscapedButKeepsSlashes(t *testing.T) {
	var rec recorder
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		writeJSON(t, w, http.StatusOK, map[string]any{"path": r.URL.Path})
	})

	if _, err := c.Note(t.Context(), "me", "Projects/notes & ideas/a#b.md"); err != nil {
		t.Fatalf("Note: %v", err)
	}
	got := rec.get()
	// The server's {path...} wildcard wants real slashes and escaped everything
	// else, and must see the decoded path.
	if want := "/api/vaults/me/notes/Projects/notes & ideas/a#b.md"; got.path != want {
		t.Errorf("decoded path = %q, want %q", got.path, want)
	}
	if strings.Contains(got.rawPath, "#") || strings.Contains(got.rawPath, " ") {
		t.Errorf("raw path %q should have escaped the space and the fragment marker", got.rawPath)
	}
}

func TestUpdateNoteSendsIfMatch(t *testing.T) {
	var rec recorder
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		writeJSON(t, w, http.StatusOK, map[string]any{"path": "a.md", "sha256": "new"})
	})

	if _, err := c.UpdateNote(t.Context(), "me", "a.md", "body", "old"); err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}
	got := rec.get()
	if want := `"old"`; got.header.Get("If-Match") != want {
		t.Errorf("If-Match = %q, want %q", got.header.Get("If-Match"), want)
	}
	if !strings.Contains(got.body, `"content":"body"`) {
		t.Errorf("body = %q, want it to carry the content", got.body)
	}

	// An empty base is a deliberate unconditional overwrite, and must not send
	// the header at all: If-Match: "" would be rejected as malformed.
	if _, err := c.UpdateNote(t.Context(), "me", "a.md", "body", ""); err != nil {
		t.Fatalf("UpdateNote unconditional: %v", err)
	}
	if h := rec.get().header.Get("If-Match"); h != "" {
		t.Errorf("If-Match = %q on an unconditional save, want none", h)
	}
}

func TestSaveConflictCarriesTheConflictPath(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"note changed underneath","conflictPath":"a (conflict 2026-08-30T12-00-00Z).md"}`)
	})

	_, err := c.UpdateNote(t.Context(), "me", "a.md", "body", "old")
	if err == nil {
		t.Fatal("UpdateNote should fail on a 409")
	}
	if !errors.Is(err, client.ErrConflict) {
		t.Errorf("errors.Is(err, ErrConflict) = false for %v", err)
	}
	if got, want := client.ConflictPath(err), "a (conflict 2026-08-30T12-00-00Z).md"; got != want {
		t.Errorf("ConflictPath = %q, want %q", got, want)
	}
	if !strings.Contains(err.Error(), "note changed underneath") {
		t.Errorf("error %q should quote the server's message", err)
	}
}

func TestErrorStatusesMapToSentinels(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusBadRequest, client.ErrBadRequest},
		{http.StatusForbidden, client.ErrForbidden},
		{http.StatusNotFound, client.ErrNotFound},
		{http.StatusConflict, client.ErrConflict},
		{http.StatusServiceUnavailable, client.ErrUnavailable},
		{http.StatusInternalServerError, client.ErrServerFailed},
	}
	for _, tc := range tests {
		c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			fmt.Fprintf(w, `{"error":"nope"}`)
		})
		_, err := c.Note(t.Context(), "me", "a.md")
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: errors.Is(%v, %v) = false", tc.status, err, tc.want)
		}
	}
}

// tailscaled being down is a 503, and the client must not let it look like a
// permission problem: that sends someone off debugging ACLs for an hour.
func TestUnavailableIsNotForbidden(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"identity service unavailable"}`)
	})
	_, err := c.Me(t.Context())
	if !errors.Is(err, client.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if errors.Is(err, client.ErrForbidden) {
		t.Error("a 503 must not read as forbidden")
	}
}

func TestNonJSONErrorBodyStillReports(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "upstream connect error\nsecond line")
	})
	_, err := c.Me(t.Context())
	if err == nil || !strings.Contains(err.Error(), "upstream connect error") {
		t.Errorf("err = %v, want the proxy's message", err)
	}
	if err != nil && strings.Contains(err.Error(), "second line") {
		t.Errorf("err = %v, want only the first line", err)
	}
}

func TestListNotesQuery(t *testing.T) {
	var rec recorder
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		writeJSON(t, w, http.StatusOK, map[string]any{"notes": []any{}})
	})

	if _, err := c.ListNotes(t.Context(), "me", client.ListOptions{Folder: "Daily", Tag: "go", Limit: 10, Offset: 20}); err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	got := rec.get()
	for _, want := range []string{"folder=Daily", "tag=go", "limit=10", "offset=20"} {
		if !strings.Contains(got.query, want) {
			t.Errorf("query %q missing %q", got.query, want)
		}
	}

	// Zero values must be left out rather than sent as empty, or the server
	// filters on a folder named "".
	if _, err := c.ListNotes(t.Context(), "me", client.ListOptions{}); err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	if q := rec.get().query; q != "" {
		t.Errorf("query = %q for zero options, want empty", q)
	}
}

func TestDailyNoteQuery(t *testing.T) {
	var rec recorder
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		writeJSON(t, w, http.StatusOK, map[string]any{"path": "Daily/2026-08-30.md"})
	})

	day := time.Date(2026, 8, 30, 15, 4, 5, 0, time.UTC)
	if _, err := c.DailyNote(t.Context(), "me", day, false); err != nil {
		t.Fatalf("DailyNote: %v", err)
	}
	got := rec.get()
	if !strings.Contains(got.query, "date=2026-08-30") || !strings.Contains(got.query, "create=false") {
		t.Errorf("query = %q", got.query)
	}

	// A zero day means today, which the server decides, so no date is sent.
	if _, err := c.DailyNote(t.Context(), "me", time.Time{}, true); err != nil {
		t.Fatalf("DailyNote today: %v", err)
	}
	if q := rec.get().query; q != "" {
		t.Errorf("query = %q for today-and-create, want empty", q)
	}
}

func TestDeleteAcceptsNoContent(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteNote(t.Context(), "me", "a.md"); err != nil {
		t.Errorf("DeleteNote: %v", err)
	}
	if err := c.Revoke(t.Context(), "share-id"); err != nil {
		t.Errorf("Revoke: %v", err)
	}
}

func TestGrantBody(t *testing.T) {
	var rec recorder
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		writeJSON(t, w, http.StatusCreated, map[string]any{"id": "s1", "perm": "write"})
	})

	s, err := c.Grant(t.Context(), "Projects", true, "bob@github", client.PermWrite)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if s.ID != "s1" || !s.Perm.CanWrite() {
		t.Errorf("share = %+v", s)
	}
	body := rec.get().body
	for _, want := range []string{`"path":"Projects"`, `"grantee":"bob@github"`, `"perm":"write"`, `"isFolder":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q missing %q", body, want)
		}
	}
}

func TestAttachmentRoundTrip(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("ETag", `"deadbeef"`)
			w.Write([]byte("\x89PNG..."))
			return
		}
		body, _ := io.ReadAll(r.Body)
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"vault": "me", "path": "attachments/x.png", "size": len(body), "sha256": "cafe",
		})
	})

	a, err := c.Attachment(t.Context(), "me", "attachments/x.png")
	if err != nil {
		t.Fatalf("Attachment: %v", err)
	}
	if string(a.Data) != "\x89PNG..." || a.ContentType != "image/png" || a.SHA256 != "deadbeef" {
		t.Errorf("attachment = %+v", a)
	}

	up, err := c.PutAttachment(t.Context(), "me", "attachments/x.png", []byte("bytes"), "image/png")
	if err != nil {
		t.Fatalf("PutAttachment: %v", err)
	}
	if up.SHA256 != "cafe" {
		t.Errorf("uploaded sha = %q, want cafe", up.SHA256)
	}
}

// The client must send no Origin header. folio's CSRF check compares Origin
// against Host when the request has no Sec-Fetch-Site, so a client that invented
// one would have its own writes rejected.
func TestWritesCarryNoOrigin(t *testing.T) {
	var rec recorder
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		writeJSON(t, w, http.StatusCreated, map[string]any{"path": "a.md"})
	})
	if _, err := c.CreateNote(t.Context(), "me", "a.md", "hi"); err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if o := rec.get().header.Get("Origin"); o != "" {
		t.Errorf("Origin = %q, want none", o)
	}
}

func TestParseSnippet(t *testing.T) {
	tests := []struct {
		in   string
		want []client.SnippetSegment
	}{
		{"", nil},
		{"plain text", []client.SnippetSegment{{Text: "plain text"}}},
		{
			"a <mark>hit</mark> here",
			[]client.SnippetSegment{{Text: "a "}, {Text: "hit", Match: true}, {Text: " here"}},
		},
		{
			// The server escapes the note's own angle brackets before inserting
			// the marks, so unescaping must happen per segment.
			"&lt;script&gt; <mark>tag</mark> &amp; more",
			[]client.SnippetSegment{{Text: "<script> "}, {Text: "tag", Match: true}, {Text: " & more"}},
		},
		{
			"<mark>one</mark><mark>two</mark>",
			[]client.SnippetSegment{{Text: "onetwo", Match: true}},
		},
		{
			"unterminated <mark>rest",
			[]client.SnippetSegment{{Text: "unterminated "}, {Text: "rest", Match: true}},
		},
		{
			"line\nbreak",
			[]client.SnippetSegment{{Text: "line break"}},
		},
	}
	for _, tc := range tests {
		got := client.ParseSnippet(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("ParseSnippet(%q) = %+v, want %+v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ParseSnippet(%q)[%d] = %+v, want %+v", tc.in, i, got[i], tc.want[i])
			}
		}
	}

	if got, want := client.PlainSnippet("a <mark>hit</mark> &amp; more"), "a hit & more"; got != want {
		t.Errorf("PlainSnippet = %q, want %q", got, want)
	}
}

func TestWatchDeliversEvents(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()

		fmt.Fprint(w, ": keepalive\n\n")
		fmt.Fprint(w, "id: 01\nevent: note.updated\ndata: {\"id\":\"01\",\"kind\":\"note.updated\",\"vault\":\"me\",\"path\":\"a.md\"}\n\n")
		fmt.Fprint(w, "id: 02\nevent: note.moved\ndata: {\"id\":\"02\",\"kind\":\"note.moved\",\"vault\":\"me\",\"path\":\"b.md\",\"oldPath\":\"a.md\"}\n\n")
		f.Flush()
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var got []string
	connected := false
	for e := range c.Watch(ctx) {
		if e.Change == nil {
			if e.Connected {
				connected = true
			}
			continue
		}
		got = append(got, e.Change.Kind+" "+e.Change.Path)
		if len(got) == 2 {
			break
		}
	}
	if !connected {
		t.Error("never reported connected")
	}
	if len(got) != 2 || got[0] != "note.updated a.md" || got[1] != "note.moved b.md" {
		t.Errorf("events = %v", got)
	}
}

// A dropped stream must reconnect rather than ending the UI's event feed.
func TestWatchReconnects(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		fmt.Fprintf(w, "id: %d\nevent: note.updated\ndata: {\"id\":\"%d\",\"kind\":\"note.updated\",\"path\":\"a.md\"}\n\n", n, n)
		f.Flush()
		if n == 1 {
			return // hang up, forcing a reconnect
		}
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var ids []string
	dropped := false
	for e := range c.Watch(ctx) {
		if e.Change == nil {
			if !e.Connected {
				dropped = true
			}
			continue
		}
		ids = append(ids, e.Change.ID)
		if len(ids) == 2 {
			break
		}
	}
	if !dropped {
		t.Error("the drop was never reported")
	}
	if len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Errorf("event ids = %v, want [1 2] across two connections", ids)
	}
}

func TestWatchStopsOnContextCancel(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(t.Context())
	ch := c.Watch(ctx)
	// Drain until closed, which must happen once the context is cancelled.
	cancel()
	for range ch {
	}
}
