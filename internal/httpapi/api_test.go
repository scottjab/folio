package httpapi_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/httpapi"
	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/index"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/store"
	"github.com/scottjab/folio/internal/vault"
)

// harness wires up a real API over a temporary state directory, with a fake
// WhoIs standing in for the tailnet.
type harness struct {
	t      *testing.T
	srv    *httptest.Server
	db     *store.DB
	ix     *index.Index
	vaults *vault.Set
	ident  *identity.Resolver
	shares *share.Resolver
	bus    *events.Bus
	api    *httpapi.API

	// peers maps a source IP to the tailnet user behind it.
	peers map[string]identity.WhoIs
}

var (
	aliceWho  = identity.WhoIs{UserID: 1, Login: "alice@github", DisplayName: "Alice"}
	bobWho    = identity.WhoIs{UserID: 2, Login: "bob@github", DisplayName: "Bob"}
	taggedWho = identity.WhoIs{Tags: []string{"tag:ci"}, NodeName: "builder"}
)

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := &harness{
		t:     t,
		db:    db,
		peers: map[string]identity.WhoIs{},
	}
	h.vaults = vault.NewSet(filepath.Join(dir, "vaults"))
	t.Cleanup(func() { h.vaults.Close() })

	h.ix = index.New(db)
	h.shares = share.NewResolver(db)
	h.bus = events.NewBus()
	h.ident = identity.NewResolver(db, h.whois, identity.Options{})

	api := httpapi.New(httpapi.Deps{
		DB: db, Index: h.ix, Vaults: h.vaults,
		Identity: h.ident, Shares: h.shares, Bus: h.bus,
		// PeerAddr exists because the tailnet peer address is not always
		// r.RemoteAddr: the dev listener needs to override it too. Here it lets
		// each test pick which tailnet user is calling.
		PeerAddr: func(r *http.Request) string { return r.Header.Get("X-Test-Peer") + ":40000" },
	})

	h.api = api

	// NewTestServer gives us an in-memory network, so these tests never open a
	// real socket and run happily in a sandbox.
	h.srv = httptest.NewTestServer(t, api)
	h.srv.Start()
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) whois(_ context.Context, remoteAddr string) (identity.WhoIs, error) {
	host, _, _ := strings.Cut(remoteAddr, ":")
	if w, ok := h.peers[host]; ok {
		return w, nil
	}
	return identity.WhoIs{}, fmt.Errorf("no peer at %s", remoteAddr)
}

// as registers who is behind an IP and returns a client bound to it.
func (h *harness) as(ip string, who identity.WhoIs) *client {
	h.peers[ip] = who
	h.ident.Flush()
	return &client{h: h, ip: ip}
}

type client struct {
	h  *harness
	ip string
}

// do issues a request whose source address is the client's IP, which is how the
// harness impersonates a tailnet peer.
func (c *client) do(method, path string, body any, opts ...func(*http.Request)) *http.Response {
	c.h.t.Helper()

	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.h.t.Fatalf("marshal body: %v", err)
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, c.h.srv.URL+path, r)
	if err != nil {
		c.h.t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Same-origin by default; the CSRF tests override this.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Test-Peer", c.ip)
	for _, o := range opts {
		o(req)
	}

	resp, err := c.h.srv.Client().Do(req)
	if err != nil {
		c.h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (c *client) getJSON(path string, out any) *http.Response {
	c.h.t.Helper()
	resp := c.do("GET", path, nil)
	decodeInto(c.h.t, resp, out)
	return resp
}

func decodeInto(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return
	}
	if err := json.UnmarshalRead(resp.Body, out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, want, bodyOf(t, resp))
	}
}

// --- identity ---

func TestMe(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	var me struct {
		Login       string `json:"login"`
		DisplayName string `json:"displayName"`
		Vault       string `json:"vault"`
	}
	resp := alice.getJSON("/api/me", &me)
	wantStatus(t, resp, 200)

	if me.Login != "alice@github" || me.DisplayName != "Alice" {
		t.Errorf("me = %+v", me)
	}
	if me.Vault != "alice-github" {
		t.Errorf("Vault = %q", me.Vault)
	}
}

func TestUnknownPeerIsUnavailableNotForbidden(t *testing.T) {
	h := newHarness(t)
	stranger := &client{h: h, ip: "100.64.0.99"}

	resp := stranger.do("GET", "/api/me", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when the identity service cannot answer", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTaggedNodeIsForbidden(t *testing.T) {
	h := newHarness(t)
	ci := h.as("100.64.0.50", taggedWho)

	resp := ci.do("GET", "/api/me", nil)
	wantStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}

// --- notes ---

func TestCreateReadUpdateDelete(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	// Create
	var created struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	resp := alice.do("POST", "/api/vaults/me/notes", map[string]any{
		"path":    "Daily/2026-08-30.md",
		"content": "# Thursday\n\nShipped it.\n",
	})
	wantStatus(t, resp, http.StatusCreated)
	decodeInto(t, resp, &created)
	if created.Path != "Daily/2026-08-30.md" || created.SHA256 == "" {
		t.Fatalf("created = %+v", created)
	}

	// Read
	var got struct {
		Path      string   `json:"path"`
		Content   string   `json:"content"`
		SHA256    string   `json:"sha256"`
		Title     string   `json:"title"`
		Perm      string   `json:"perm"`
		Backlinks []any    `json:"backlinks"`
		Tags      []string `json:"tags"`
	}
	resp = alice.getJSON("/api/vaults/me/notes/Daily/2026-08-30.md", &got)
	wantStatus(t, resp, 200)
	// Creation stamps a stable uuid into the frontmatter; the body the user
	// typed must come back untouched underneath it.
	if !strings.HasSuffix(got.Content, "# Thursday\n\nShipped it.\n") {
		t.Errorf("Content = %q, want the body preserved", got.Content)
	}
	if !strings.HasPrefix(got.Content, "---\nid: ") {
		t.Errorf("Content = %q, want a frontmatter id stamped on creation", got.Content)
	}
	if got.Title != "Thursday" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Perm != "write" {
		t.Errorf("Perm = %q, want write for the owner", got.Perm)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("no ETag on a note response")
	}

	// Update with the right base sha
	resp = alice.do("PUT", "/api/vaults/me/notes/Daily/2026-08-30.md", map[string]any{
		"content": "# Thursday\n\nEdited.\n",
	}, withHeader("If-Match", created.SHA256))
	wantStatus(t, resp, 200)
	resp.Body.Close()

	resp = alice.getJSON("/api/vaults/me/notes/Daily/2026-08-30.md", &got)
	wantStatus(t, resp, 200)
	if !strings.Contains(got.Content, "Edited") {
		t.Errorf("Content = %q, want the update applied", got.Content)
	}

	// Delete
	resp = alice.do("DELETE", "/api/vaults/me/notes/Daily/2026-08-30.md", nil)
	wantStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()

	resp = alice.do("GET", "/api/vaults/me/notes/Daily/2026-08-30.md", nil)
	wantStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func withHeader(k, v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func TestStaleIfMatchIsAConflict(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	var created struct {
		SHA256 string `json:"sha256"`
	}
	resp := alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "n.md", "content": "one\n"})
	wantStatus(t, resp, http.StatusCreated)
	decodeInto(t, resp, &created)

	// Someone else writes in between.
	resp = alice.do("PUT", "/api/vaults/me/notes/n.md", map[string]any{"content": "two\n"})
	wantStatus(t, resp, 200)
	resp.Body.Close()

	// Now our stale save arrives.
	resp = alice.do("PUT", "/api/vaults/me/notes/n.md", map[string]any{"content": "three\n"},
		withHeader("If-Match", created.SHA256))
	wantStatus(t, resp, http.StatusConflict)

	var conflict struct {
		Error        string `json:"error"`
		ConflictPath string `json:"conflictPath"`
	}
	decodeInto(t, resp, &conflict)
	if conflict.ConflictPath == "" {
		t.Error("a conflict response must say where the rejected content was saved")
	}

	// The rejected draft has to exist, or we just lost someone's writing.
	var side struct {
		Content string `json:"content"`
	}
	resp = alice.getJSON("/api/vaults/me/notes/"+conflict.ConflictPath, &side)
	wantStatus(t, resp, 200)
	if side.Content != "three\n" {
		t.Errorf("conflict copy = %q, want the rejected content", side.Content)
	}
}

func TestPutWithoutIfMatchOverwrites(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "n.md", "content": "one\n"}).Body.Close()
	resp := alice.do("PUT", "/api/vaults/me/notes/n.md", map[string]any{"content": "two\n"})
	wantStatus(t, resp, 200)
	resp.Body.Close()
}

func TestCreateRefusesDuplicates(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "n.md", "content": "one"}).Body.Close()
	resp := alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "n.md", "content": "two"})
	wantStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestPathTraversalIsRejected(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	for _, p := range []string{"../escape.md", "a/../../b.md", ".folio/tmp/x.md"} {
		resp := alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": p, "content": "x"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST with path %q = %d, want 400", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestUnknownJSONFieldIsRejected(t *testing.T) {
	// A typo'd field name that is silently dropped means a save that looks like
	// it worked and did not.
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	resp := alice.do("POST", "/api/vaults/me/notes", map[string]any{
		"path": "n.md", "contnet": "typo",
	})
	wantStatus(t, resp, http.StatusBadRequest)
	if !strings.Contains(bodyOf(t, resp), "contnet") {
		t.Error("the error should name the unknown field")
	}
}

func TestMoveRewritesLinks(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "Projects/folio.md", "content": "# folio\n"}).Body.Close()
	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "Daily/x.md", "content": "See [[Projects/folio]].\n"}).Body.Close()

	resp := alice.do("POST", "/api/vaults/me/move", map[string]any{
		"from": "Projects/folio.md", "to": "Archive/folio.md",
	})
	wantStatus(t, resp, 200)
	resp.Body.Close()

	var got struct {
		Content string `json:"content"`
	}
	resp = alice.getJSON("/api/vaults/me/notes/Daily/x.md", &got)
	wantStatus(t, resp, 200)
	if !strings.Contains(got.Content, "[[Archive/folio]]") {
		t.Errorf("inbound link not rewritten: %q", got.Content)
	}
}

func TestListNotes(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	for _, p := range []string{"Daily/a.md", "Daily/b.md", "Projects/c.md"} {
		alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": p, "content": "# x\n"}).Body.Close()
	}

	var list struct {
		Notes []struct {
			Path string `json:"path"`
		} `json:"notes"`
	}
	resp := alice.getJSON("/api/vaults/me/notes", &list)
	wantStatus(t, resp, 200)
	if len(list.Notes) != 3 {
		t.Errorf("got %d notes, want 3", len(list.Notes))
	}

	resp = alice.getJSON("/api/vaults/me/notes?folder=Daily", &list)
	wantStatus(t, resp, 200)
	if len(list.Notes) != 2 {
		t.Errorf("folder filter returned %d, want 2", len(list.Notes))
	}
}

// --- search ---

func TestSearch(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	alice.do("POST", "/api/vaults/me/notes", map[string]any{
		"path": "n.md", "content": "---\ntags: [go]\n---\n# Widgets\n\nThe quick brown fox.\n",
	}).Body.Close()

	var res struct {
		Hits []struct {
			Path    string `json:"path"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
			Vault   string `json:"vault"`
		} `json:"hits"`
	}
	resp := alice.getJSON("/api/search?q=fox", &res)
	wantStatus(t, resp, 200)
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	if res.Hits[0].Path != "n.md" || res.Hits[0].Title != "Widgets" {
		t.Errorf("hit = %+v", res.Hits[0])
	}
	if !strings.Contains(res.Hits[0].Snippet, "fox") {
		t.Errorf("snippet = %q", res.Hits[0].Snippet)
	}
}

func TestSearchNeverLeaksAcrossUsers(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	alice.do("POST", "/api/vaults/me/notes", map[string]any{
		"path": "secret.md", "content": "# Secret\n\nclassified material\n",
	}).Body.Close()

	bob := h.as("100.64.0.2", bobWho)
	bob.do("GET", "/api/me", nil).Body.Close() // provision bob

	var res struct {
		Hits []struct {
			Path string `json:"path"`
		} `json:"hits"`
	}
	resp := bob.getJSON("/api/search?q=classified", &res)
	wantStatus(t, resp, 200)
	if len(res.Hits) != 0 {
		t.Errorf("bob found %d of alice's notes: %+v", len(res.Hits), res.Hits)
	}
}

func TestSearchWithNoQueryListsRecent(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "a.md", "content": "# A\n"}).Body.Close()

	var res struct {
		Hits []struct {
			Path string `json:"path"`
		} `json:"hits"`
	}
	resp := alice.getJSON("/api/search?q=", &res)
	wantStatus(t, resp, 200)
	if len(res.Hits) != 1 {
		t.Errorf("an empty query should list recent notes, got %d hits", len(res.Hits))
	}
}

// --- sharing ---

func TestShareFlow(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	bob := h.as("100.64.0.2", bobWho)

	alice.do("POST", "/api/vaults/me/notes", map[string]any{
		"path": "Shared.md", "content": "# Shared\n\nvisible soon\n",
	}).Body.Close()
	bob.do("GET", "/api/me", nil).Body.Close() // provision bob

	// Before sharing, bob cannot see it.
	resp := bob.do("GET", "/api/vaults/alice-github/notes/Shared.md", nil)
	wantStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()

	// Alice shares it read-only.
	var created struct {
		ID string `json:"id"`
	}
	resp = alice.do("POST", "/api/shares", map[string]any{
		"path": "Shared.md", "grantee": "bob@github", "perm": "read",
	})
	wantStatus(t, resp, http.StatusCreated)
	decodeInto(t, resp, &created)
	if created.ID == "" {
		t.Fatal("share has no id")
	}

	// Now bob can read it, but not write it.
	var got struct {
		Content string `json:"content"`
		Perm    string `json:"perm"`
	}
	resp = bob.getJSON("/api/vaults/alice-github/notes/Shared.md", &got)
	wantStatus(t, resp, 200)
	if !strings.Contains(got.Content, "visible soon") {
		t.Errorf("Content = %q", got.Content)
	}
	if got.Perm != "read" {
		t.Errorf("Perm = %q, want read", got.Perm)
	}

	resp = bob.do("PUT", "/api/vaults/alice-github/notes/Shared.md", map[string]any{"content": "hijacked\n"})
	wantStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()

	// And it turns up in his search.
	var res struct {
		Hits []struct {
			Path  string `json:"path"`
			Vault string `json:"vault"`
		} `json:"hits"`
	}
	resp = bob.getJSON("/api/search?q=visible", &res)
	wantStatus(t, resp, 200)
	if len(res.Hits) != 1 || res.Hits[0].Vault != "alice-github" {
		t.Errorf("bob's search = %+v, want the shared note", res.Hits)
	}

	// Upgrade to write.
	resp = alice.do("POST", "/api/shares", map[string]any{
		"path": "Shared.md", "grantee": "bob@github", "perm": "write",
	})
	wantStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = bob.do("PUT", "/api/vaults/alice-github/notes/Shared.md", map[string]any{"content": "# Shared\n\nbob was here\n"})
	wantStatus(t, resp, 200)
	resp.Body.Close()

	// Revoke, and it is gone again immediately.
	resp = alice.do("DELETE", "/api/shares/"+created.ID, nil)
	wantStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()

	resp = bob.do("GET", "/api/vaults/alice-github/notes/Shared.md", nil)
	wantStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestSharedWithMeListing(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	bob := h.as("100.64.0.2", bobWho)

	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "S.md", "content": "# S\n"}).Body.Close()
	bob.do("GET", "/api/me", nil).Body.Close()
	alice.do("POST", "/api/shares", map[string]any{"path": "S.md", "grantee": "bob@github", "perm": "read"}).Body.Close()

	var shared struct {
		Shares []struct {
			Path       string `json:"path"`
			OwnerLogin string `json:"ownerLogin"`
			Vault      string `json:"vault"`
		} `json:"shares"`
	}
	resp := bob.getJSON("/api/shared", &shared)
	wantStatus(t, resp, 200)
	if len(shared.Shares) != 1 || shared.Shares[0].OwnerLogin != "alice@github" {
		t.Errorf("shared with bob = %+v", shared.Shares)
	}
}

func TestCannotRevokeAnotherUsersShare(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	bob := h.as("100.64.0.2", bobWho)

	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "S.md", "content": "# S\n"}).Body.Close()
	bob.do("GET", "/api/me", nil).Body.Close()

	var created struct {
		ID string `json:"id"`
	}
	resp := alice.do("POST", "/api/shares", map[string]any{"path": "S.md", "grantee": "bob@github", "perm": "read"})
	decodeInto(t, resp, &created)

	resp = bob.do("DELETE", "/api/shares/"+created.ID, nil)
	wantStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestOtherUsersVaultIsNotEnumerable(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "a.md", "content": "# A\n"}).Body.Close()

	bob := h.as("100.64.0.2", bobWho)
	bob.do("GET", "/api/me", nil).Body.Close()

	resp := bob.do("GET", "/api/vaults/alice-github/notes", nil)
	wantStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}

// --- CSRF ---

func TestCrossSiteWriteIsBlocked(t *testing.T) {
	// Identity comes from the network layer with no token or cookie, so without
	// this check any page on the internet could POST to the tailnet URL from
	// your browser and we would authenticate it as you.
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	resp := alice.do("POST", "/api/vaults/me/notes",
		map[string]any{"path": "evil.md", "content": "x"},
		withHeader("Sec-Fetch-Site", "cross-site"))
	wantStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()

	resp = alice.do("GET", "/api/vaults/me/notes/evil.md", nil)
	wantStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestCrossSiteWithForeignOriginIsBlocked(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	resp := alice.do("POST", "/api/vaults/me/notes",
		map[string]any{"path": "evil.md", "content": "x"},
		func(r *http.Request) {
			r.Header.Del("Sec-Fetch-Site")
			r.Header.Set("Origin", "https://evil.example.com")
		})
	wantStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestSameOriginWriteIsAllowed(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	resp := alice.do("POST", "/api/vaults/me/notes",
		map[string]any{"path": "ok.md", "content": "x"},
		func(r *http.Request) {
			r.Header.Del("Sec-Fetch-Site")
			r.Header.Set("Origin", h.srv.URL)
		})
	wantStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
}

func TestCrossSiteReadIsAllowed(t *testing.T) {
	// GET is safe and must keep working, or the browser cannot load the app.
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	resp := alice.do("GET", "/api/me", nil, withHeader("Sec-Fetch-Site", "cross-site"))
	wantStatus(t, resp, 200)
	resp.Body.Close()
}

// --- tags, folders, backlinks ---

func TestTagsAndFolders(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	alice.do("POST", "/api/vaults/me/notes", map[string]any{
		"path": "Daily/a.md", "content": "---\ntags: [go, daily]\n---\n# A\n",
	}).Body.Close()

	var tags struct {
		Tags []struct {
			Tag   string `json:"tag"`
			Count int    `json:"count"`
		} `json:"tags"`
	}
	resp := alice.getJSON("/api/tags", &tags)
	wantStatus(t, resp, 200)
	if len(tags.Tags) != 2 {
		t.Errorf("tags = %+v, want 2", tags.Tags)
	}

	var folders struct {
		Folders []string `json:"folders"`
	}
	resp = alice.getJSON("/api/folders", &folders)
	wantStatus(t, resp, 200)
	if len(folders.Folders) != 1 || folders.Folders[0] != "Daily" {
		t.Errorf("folders = %v", folders.Folders)
	}
}

func TestBacklinksEndpoint(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "Target.md", "content": "# Target\n"}).Body.Close()
	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "Source.md", "content": "See [[Target]].\n"}).Body.Close()

	var got struct {
		Backlinks []struct {
			Path  string `json:"path"`
			Title string `json:"title"`
		} `json:"backlinks"`
	}
	resp := alice.getJSON("/api/vaults/me/backlinks/Target.md", &got)
	wantStatus(t, resp, 200)
	if len(got.Backlinks) != 1 || got.Backlinks[0].Path != "Source.md" {
		t.Errorf("backlinks = %+v", got.Backlinks)
	}
}

// --- attachments ---

func TestAttachmentRoundTrip(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64)
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/vaults/me/attachments/attachments/img.png", strings.NewReader(png))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Test-Peer", alice.ip)
	req.Header.Set("Content-Type", "image/png")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	wantStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = alice.do("GET", "/api/vaults/me/attachments/attachments/img.png", nil)
	wantStatus(t, resp, 200)
	if got := bodyOf(t, resp); got != png {
		t.Errorf("attachment round trip changed %d bytes", len(png)-len(got))
	}
}

// --- SSE ---

func TestEventStreamPublishesWrites(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	alice.do("GET", "/api/me", nil).Body.Close() // provision before subscribing

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/api/events", nil)
	req.Header.Set("X-Test-Peer", alice.ip)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	wantStatus(t, resp, 200)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "live.md", "content": "# Live\n"}).Body.Close()
	}()

	frame := readSSEFrame(t, resp.Body)
	if !strings.Contains(frame, "live.md") {
		t.Errorf("frame = %q, want it to mention the note that changed", frame)
	}
	if !strings.Contains(frame, "note.created") && !strings.Contains(frame, "note.updated") {
		t.Errorf("frame = %q, want a note event type", frame)
	}
}

func TestEventStreamDoesNotLeakOtherUsersNotes(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	bob := h.as("100.64.0.2", bobWho)
	alice.do("GET", "/api/me", nil).Body.Close()
	bob.do("GET", "/api/me", nil).Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/api/events", nil)
	req.Header.Set("X-Test-Peer", bob.ip)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()

	// Alice writes; bob must not hear about it.
	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "private.md", "content": "# Private\n"}).Body.Close()

	done := make(chan string, 1)
	go func() { done <- readSSEFrameOrEmpty(resp.Body) }()

	select {
	case frame := <-done:
		if strings.Contains(frame, "private.md") {
			t.Errorf("bob received alice's note event: %q", frame)
		}
	case <-time.After(1500 * time.Millisecond):
		// Nothing arrived, which is the correct outcome.
	}
}

// readSSEFrame blocks until a data frame arrives.
func readSSEFrame(t *testing.T, r io.Reader) string {
	t.Helper()
	buf := make([]byte, 4096)
	var acc strings.Builder
	for range 50 {
		n, err := r.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
			if strings.Contains(acc.String(), "\n\n") && strings.Contains(acc.String(), "data:") {
				return acc.String()
			}
		}
		if err != nil {
			t.Fatalf("read SSE: %v (so far: %q)", err, acc.String())
		}
	}
	t.Fatalf("no complete SSE frame arrived; got %q", acc.String())
	return ""
}

func readSSEFrameOrEmpty(r io.Reader) string {
	buf := make([]byte, 4096)
	var acc strings.Builder
	for range 20 {
		n, err := r.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
			if strings.Contains(acc.String(), "data:") {
				return acc.String()
			}
		}
		if err != nil {
			return acc.String()
		}
	}
	return acc.String()
}

// --- static app ---

func TestSPAFallback(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	// A deep client-side route must serve the app, so a refresh works.
	resp := alice.do("GET", "/n/me/Daily/2026-08-30.md", nil)
	wantStatus(t, resp, 200)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want html", ct)
	}
	resp.Body.Close()

	// An unknown API path must 404 as JSON, not fall through to the app.
	resp = alice.do("GET", "/api/nope", nil)
	wantStatus(t, resp, http.StatusNotFound)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want json for an unknown API route", ct)
	}
	resp.Body.Close()
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	resp := alice.do("GET", "/", nil)
	wantStatus(t, resp, 200)
	defer resp.Body.Close()

	if csp := resp.Header.Get("Content-Security-Policy"); csp == "" {
		t.Error("no Content-Security-Policy on the app shell")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
}

func TestEditNote(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	alice.do("POST", "/api/vaults/me/notes", map[string]any{
		"path": "n.md", "content": "# Title\n\nold line\nkeep this\n",
	}).Body.Close()

	resp := alice.do("POST", "/api/vaults/me/edit", map[string]any{
		"path":  "n.md",
		"edits": []map[string]any{{"old": "old line", "new": "new line"}},
	})
	wantStatus(t, resp, 200)
	resp.Body.Close()

	var got struct {
		Content string `json:"content"`
	}
	resp = alice.getJSON("/api/vaults/me/notes/n.md", &got)
	wantStatus(t, resp, 200)
	if !strings.Contains(got.Content, "new line") || strings.Contains(got.Content, "old line") {
		t.Errorf("Content = %q", got.Content)
	}
	if !strings.Contains(got.Content, "keep this") {
		t.Errorf("the edit clobbered the rest of the note: %q", got.Content)
	}
}

func TestEditRefusesAmbiguousMatches(t *testing.T) {
	// Replacing the first of several identical lines would be a coin flip, and
	// an agent silently editing the wrong one is worse than an error.
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	alice.do("POST", "/api/vaults/me/notes", map[string]any{
		"path": "n.md", "content": "# T\n\nrepeat\nrepeat\n",
	}).Body.Close()

	resp := alice.do("POST", "/api/vaults/me/edit", map[string]any{
		"path":  "n.md",
		"edits": []map[string]any{{"old": "repeat", "new": "changed"}},
	})
	wantStatus(t, resp, http.StatusBadRequest)
	if body := bodyOf(t, resp); !strings.Contains(body, "more than once") {
		t.Errorf("error = %q, want it to explain the ambiguity", body)
	}
}

func TestEditRefusesAMissingMatch(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	alice.do("POST", "/api/vaults/me/notes", map[string]any{"path": "n.md", "content": "# T\n"}).Body.Close()

	resp := alice.do("POST", "/api/vaults/me/edit", map[string]any{
		"path":  "n.md",
		"edits": []map[string]any{{"old": "not present", "new": "x"}},
	})
	wantStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestAppendUnderHeading(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	alice.do("POST", "/api/vaults/me/notes", map[string]any{
		"path": "n.md", "content": "# T\n\n## Tasks\n\n- one\n\n## Notes\n\nx\n",
	}).Body.Close()

	resp := alice.do("POST", "/api/vaults/me/append", map[string]any{
		"path": "n.md", "text": "- two", "underHeading": "Tasks",
	})
	wantStatus(t, resp, 200)
	resp.Body.Close()

	var got struct {
		Content string `json:"content"`
	}
	alice.getJSON("/api/vaults/me/notes/n.md", &got)
	if !strings.Contains(got.Content, "- one\n- two\n\n## Notes") {
		t.Errorf("append landed in the wrong section: %q", got.Content)
	}
}

func TestDailyNoteIsCreatedOnDemand(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	var got struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	resp := alice.getJSON("/api/vaults/me/daily?date=2026-08-30", &got)
	wantStatus(t, resp, 200)
	if got.Path != "Daily/2026-08-30.md" {
		t.Errorf("Path = %q", got.Path)
	}
	if !strings.Contains(got.Content, "Sunday, 30 August 2026") {
		t.Errorf("Content = %q, want the date template", got.Content)
	}

	// Asking again returns the same note rather than a second one.
	var again struct {
		Path string `json:"path"`
	}
	alice.getJSON("/api/vaults/me/daily?date=2026-08-30", &again)
	if again.Path != got.Path {
		t.Errorf("second call returned %q", again.Path)
	}
}

func TestDailyNoteCanRefuseToCreate(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)

	resp := alice.do("GET", "/api/vaults/me/daily?date=2026-08-30&create=false", nil)
	wantStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestCloseEndsOpenEventStreams(t *testing.T) {
	// An SSE stream stays open by design, and http.Server.Shutdown waits for
	// active handlers. Without Close ending these, a single open browser tab
	// made Ctrl-C hang for the whole shutdown grace period.
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	alice.do("GET", "/api/me", nil).Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/api/events", nil)
	req.Header.Set("X-Test-Peer", alice.ip)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	wantStatus(t, resp, 200)

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		done <- err
	}()

	// The stream is live and would otherwise sit here indefinitely.
	select {
	case <-done:
		t.Fatal("the stream ended before Close was called")
	case <-time.After(150 * time.Millisecond):
	}

	h.api.Close()

	select {
	case <-done:
		// Ended promptly, which is the whole point.
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not end the event stream; shutdown would block on it")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.api.Close()
	h.api.Close() // must not panic
}

func TestRequestsStillWorkBeforeClose(t *testing.T) {
	h := newHarness(t)
	alice := h.as("100.64.0.1", aliceWho)
	resp := alice.do("GET", "/api/me", nil)
	wantStatus(t, resp, 200)
	resp.Body.Close()
}
