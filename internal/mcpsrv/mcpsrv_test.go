package mcpsrv_test

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/index"
	"github.com/scottjab/folio/internal/mcpsrv"
	"github.com/scottjab/folio/internal/notes"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/store"
	"github.com/scottjab/folio/internal/vault"
)

type env struct {
	t      *testing.T
	svc    *notes.Service
	srv    *mcpsrv.Server
	shares *share.Resolver
	ident  *identity.Resolver
	alice  identity.User
	bob    identity.User
	ctx    context.Context
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()

	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	vaults := vault.NewSet(filepath.Join(dir, "vaults"))
	t.Cleanup(func() { vaults.Close() })

	ix := index.New(db)
	shares := share.NewResolver(db)
	bus := events.NewBus()

	var who identity.WhoIs
	ident := identity.NewResolver(db, func(context.Context, string) (identity.WhoIs, error) {
		return who, nil
	}, identity.Options{})

	ctx := context.Background()
	provision := func(w identity.WhoIs, ip string) identity.User {
		t.Helper()
		who = w
		ident.Flush()
		u, err := ident.Identify(ctx, ip)
		if err != nil {
			t.Fatalf("provision %s: %v", w.Login, err)
		}
		return u
	}

	e := &env{t: t, shares: shares, ident: ident, ctx: ctx}
	e.alice = provision(identity.WhoIs{UserID: 1, Login: "alice@github", DisplayName: "Alice"}, "100.64.0.1:1")
	e.bob = provision(identity.WhoIs{UserID: 2, Login: "bob@github", DisplayName: "Bob"}, "100.64.0.2:1")

	e.svc = notes.New(notes.Deps{
		DB: db, Index: ix, Vaults: vaults, Identity: ident, Shares: shares, Bus: bus,
	})
	e.srv = mcpsrv.New(mcpsrv.Deps{
		Notes: e.svc, Index: ix, Identity: ident, Shares: shares, Bus: bus,
	})
	return e
}

// connect drives a real MCP client against the server for one user, which is the
// only way to be sure the schemas and the wire format are right.
func (e *env) connect(u identity.User) *mcp.ClientSession {
	e.t.Helper()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	ct, st := mcp.NewInMemoryTransports()

	server := e.srv.ForUser(u)
	go func() {
		if err := server.Run(e.ctx, st); err != nil && !strings.Contains(err.Error(), "closed") {
			e.t.Errorf("server.Run: %v", err)
		}
	}()

	cs, err := client.Connect(e.ctx, ct, nil)
	if err != nil {
		e.t.Fatalf("connect: %v", err)
	}
	e.t.Cleanup(func() { cs.Close() })
	return cs
}

// call invokes a tool and decodes its structured result into out.
func call[T any](t *testing.T, cs *mcp.ClientSession, name string, args any, out *T) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if res.IsError {
		t.Fatalf("CallTool(%s) returned an error: %s", name, textOf(res))
	}
	if out != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal result: %v", err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("decode %s result: %v (raw: %s)", name, err, b)
		}
	}
	return res
}

// mustCall invokes a tool whose result we do not need to inspect.
func mustCall(t *testing.T, cs *mcp.ClientSession, name string, args any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if res.IsError {
		t.Fatalf("CallTool(%s) returned an error: %s", name, textOf(res))
	}
}

// callExpectingError invokes a tool that should fail and returns its message.
func callExpectingError(t *testing.T, cs *mcp.ClientSession, name string, args any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("CallTool(%s) unexpectedly succeeded: %s", name, textOf(res))
	}
	return textOf(res)
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestToolsAreAdvertised(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	res, err := cs.ListTools(e.ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
		if tool.Description == "" {
			t.Errorf("tool %q has no description; the model needs one to choose it", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
	}

	want := []string{
		"search_notes", "list_notes", "read_note", "create_note", "update_note",
		"edit_note", "append_note", "delete_note", "move_note", "get_backlinks",
		"list_tags", "list_folders", "get_daily_note", "list_vaults", "vault_stats",
		"list_shares", "share_note", "unshare_note", "read_attachment",
		"list_attachments", "attach_file", "resolve_link", "get_settings", "set_settings",
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing tool %q; have %v", w, got)
		}
	}
}

func TestCreateReadRoundTrip(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	var created struct {
		Path string `json:"path"`
		Sha  string `json:"sha"`
	}
	call(t, cs, "create_note", map[string]any{
		"path": "Projects/folio.md", "content": "---\ntags: [go]\n---\n# folio\n\nA notes app.\n",
	}, &created)
	if created.Path != "Projects/folio.md" || created.Sha == "" {
		t.Fatalf("create = %+v", created)
	}

	var read struct {
		Path    string   `json:"path"`
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
		Perm    string   `json:"perm"`
		Sha     string   `json:"sha"`
	}
	call(t, cs, "read_note", map[string]any{"path": "Projects/folio.md"}, &read)
	if read.Title != "folio" {
		t.Errorf("Title = %q", read.Title)
	}
	if !strings.Contains(read.Content, "A notes app.") {
		t.Errorf("Content = %q", read.Content)
	}
	if !slices.Contains(read.Tags, "go") {
		t.Errorf("Tags = %v", read.Tags)
	}
	if read.Perm != "write" {
		t.Errorf("Perm = %q, want write for the owner", read.Perm)
	}
}

func TestCreateRefusesToOverwrite(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	mustCall(t, cs, "create_note", map[string]any{"path": "n.md", "content": "original\n"})
	msg := callExpectingError(t, cs, "create_note", map[string]any{"path": "n.md", "content": "replacement\n"})
	if !strings.Contains(strings.ToLower(msg), "exists") {
		t.Errorf("error = %q, want it to say the note already exists", msg)
	}

	var read struct {
		Content string `json:"content"`
	}
	call(t, cs, "read_note", map[string]any{"path": "n.md"}, &read)
	if !strings.Contains(read.Content, "original") {
		t.Errorf("the original was clobbered: %q", read.Content)
	}
}

func TestSearchTool(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	mustCall(t, cs, "create_note", map[string]any{
		"path": "a.md", "content": "---\ntags: [go]\n---\n# Widgets\n\nThe quick brown fox.\n",
	})
	mustCall(t, cs, "create_note", map[string]any{"path": "b.md", "content": "# Other\n\nNothing here.\n"})

	var out struct {
		Hits []struct {
			Path    string `json:"path"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"hits"`
	}
	call(t, cs, "search_notes", map[string]any{"query": "fox"}, &out)
	if len(out.Hits) != 1 || out.Hits[0].Path != "a.md" {
		t.Fatalf("hits = %+v", out.Hits)
	}
	if !strings.Contains(out.Hits[0].Snippet, "fox") {
		t.Errorf("snippet = %q", out.Hits[0].Snippet)
	}
	// The index's highlight markers are control characters; a model should never
	// see them.
	if strings.ContainsAny(out.Hits[0].Snippet, "\x02\x03") {
		t.Errorf("snippet leaked highlight control characters: %q", out.Hits[0].Snippet)
	}

	call(t, cs, "search_notes", map[string]any{"query": "tag:go"}, &out)
	if len(out.Hits) != 1 {
		t.Errorf("tag search = %+v", out.Hits)
	}
}

func TestEditToolMakesTargetedChanges(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	mustCall(t, cs, "create_note", map[string]any{
		"path": "n.md", "content": "# Title\n\nfirst line\nsecond line\nthird line\n",
	})

	mustCall(t, cs, "edit_note", map[string]any{
		"path": "n.md",
		"edits": []map[string]any{
			{"old": "second line", "new": "SECOND LINE"},
		},
	})

	var read struct {
		Content string `json:"content"`
	}
	call(t, cs, "read_note", map[string]any{"path": "n.md"}, &read)
	if !strings.Contains(read.Content, "SECOND LINE") {
		t.Errorf("edit did not apply: %q", read.Content)
	}
	for _, keep := range []string{"first line", "third line", "# Title"} {
		if !strings.Contains(read.Content, keep) {
			t.Errorf("edit lost %q: %q", keep, read.Content)
		}
	}
}

func TestEditToolRefusesAmbiguity(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{"path": "n.md", "content": "# T\n\nsame\nsame\n"})

	msg := callExpectingError(t, cs, "edit_note", map[string]any{
		"path":  "n.md",
		"edits": []map[string]any{{"old": "same", "new": "different"}},
	})
	if !strings.Contains(msg, "more than once") {
		t.Errorf("error = %q, want it to explain the ambiguity so the model can retry with more context", msg)
	}
}

func TestEditToolIsAllOrNothing(t *testing.T) {
	// A half-applied set of edits leaves a note in a state nobody intended.
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{"path": "n.md", "content": "# T\n\nalpha\nbeta\n"})

	callExpectingError(t, cs, "edit_note", map[string]any{
		"path": "n.md",
		"edits": []map[string]any{
			{"old": "alpha", "new": "ALPHA"},
			{"old": "missing", "new": "x"},
		},
	})

	var read struct {
		Content string `json:"content"`
	}
	call(t, cs, "read_note", map[string]any{"path": "n.md"}, &read)
	if strings.Contains(read.Content, "ALPHA") {
		t.Errorf("the first edit was applied even though the second failed: %q", read.Content)
	}
}

func TestAppendTool(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{
		"path": "n.md", "content": "# T\n\n## Tasks\n\n- one\n\n## Notes\n\nx\n",
	})

	mustCall(t, cs, "append_note", map[string]any{
		"path": "n.md", "text": "- two", "underHeading": "Tasks",
	})

	var read struct {
		Content string `json:"content"`
	}
	call(t, cs, "read_note", map[string]any{"path": "n.md"}, &read)
	if !strings.Contains(read.Content, "- one\n- two\n\n## Notes") {
		t.Errorf("append landed in the wrong place: %q", read.Content)
	}
}

func TestUpdateWithStaleShaIsRefused(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	var created struct {
		Sha string `json:"sha"`
	}
	call(t, cs, "create_note", map[string]any{"path": "n.md", "content": "one\n"}, &created)
	mustCall(t, cs, "update_note", map[string]any{"path": "n.md", "content": "two\n"})

	msg := callExpectingError(t, cs, "update_note", map[string]any{
		"path": "n.md", "content": "three\n", "baseSha": created.Sha,
	})
	if !strings.Contains(strings.ToLower(msg), "conflict") {
		t.Errorf("error = %q, want a conflict", msg)
	}

	var read struct {
		Content string `json:"content"`
	}
	call(t, cs, "read_note", map[string]any{"path": "n.md"}, &read)
	if !strings.Contains(read.Content, "two") {
		t.Errorf("the concurrent write was clobbered: %q", read.Content)
	}
}

func TestMoveToolRewritesLinks(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	mustCall(t, cs, "create_note", map[string]any{"path": "Projects/folio.md", "content": "# folio\n"})
	mustCall(t, cs, "create_note", map[string]any{"path": "Daily/x.md", "content": "See [[Projects/folio]].\n"})

	mustCall(t, cs, "move_note", map[string]any{"from": "Projects/folio.md", "to": "Archive/folio.md"})

	var read struct {
		Content string `json:"content"`
	}
	call(t, cs, "read_note", map[string]any{"path": "Daily/x.md"}, &read)
	if !strings.Contains(read.Content, "[[Archive/folio]]") {
		t.Errorf("inbound link not rewritten: %q", read.Content)
	}
}

func TestBacklinksTool(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{"path": "Target.md", "content": "# Target\n"})
	mustCall(t, cs, "create_note", map[string]any{"path": "Source.md", "content": "See [[Target]].\n"})

	var out struct {
		Backlinks []struct {
			Path string `json:"path"`
		} `json:"backlinks"`
	}
	call(t, cs, "get_backlinks", map[string]any{"path": "Target.md"}, &out)
	if len(out.Backlinks) != 1 || out.Backlinks[0].Path != "Source.md" {
		t.Errorf("backlinks = %+v", out.Backlinks)
	}
}

func TestDailyNoteTool(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	var out struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	call(t, cs, "get_daily_note", map[string]any{"date": "2026-08-30"}, &out)
	if out.Path != "Daily/2026-08-30.md" {
		t.Errorf("Path = %q", out.Path)
	}
	if !strings.Contains(out.Content, "2026") {
		t.Errorf("Content = %q, want the date template", out.Content)
	}

	// noCreate on a missing day is an error rather than a silent create.
	msg := callExpectingError(t, cs, "get_daily_note", map[string]any{"date": "2020-01-01", "noCreate": true})
	if msg == "" {
		t.Error("expected an error for a missing daily note with noCreate")
	}
}

// --- isolation, the part that matters most ---

func TestAgentSeesOnlyItsOwnUsersNotes(t *testing.T) {
	e := newEnv(t)
	aliceCS := e.connect(e.alice)
	bobCS := e.connect(e.bob)

	mustCall(t, aliceCS, "create_note", map[string]any{
		"path": "secret.md", "content": "# Secret\n\nclassified material\n",
	})

	var out struct {
		Hits []struct {
			Path string `json:"path"`
		} `json:"hits"`
	}
	call(t, bobCS, "search_notes", map[string]any{"query": "classified"}, &out)
	if len(out.Hits) != 0 {
		t.Errorf("bob's agent found alice's notes: %+v", out.Hits)
	}

	msg := callExpectingError(t, bobCS, "read_note", map[string]any{
		"path": "secret.md", "vault": "alice-github",
	})
	if !strings.Contains(strings.ToLower(msg), "denied") {
		t.Errorf("error = %q, want a permission error", msg)
	}
}

func TestSharedNoteIsReadableButNotWritable(t *testing.T) {
	e := newEnv(t)
	aliceCS := e.connect(e.alice)
	bobCS := e.connect(e.bob)

	mustCall(t, aliceCS, "create_note", map[string]any{
		"path": "Shared.md", "content": "# Shared\n\nvisible\n",
	})
	mustCall(t, aliceCS, "share_note", map[string]any{
		"path": "Shared.md", "grantee": "bob@github", "perm": "read",
	})

	var read struct {
		Content string `json:"content"`
		Perm    string `json:"perm"`
	}
	call(t, bobCS, "read_note", map[string]any{"path": "Shared.md", "vault": "alice-github"}, &read)
	if !strings.Contains(read.Content, "visible") {
		t.Errorf("Content = %q", read.Content)
	}
	if read.Perm != "read" {
		t.Errorf("Perm = %q, want read", read.Perm)
	}

	msg := callExpectingError(t, bobCS, "update_note", map[string]any{
		"path": "Shared.md", "vault": "alice-github", "content": "hijacked\n",
	})
	if !strings.Contains(strings.ToLower(msg), "denied") {
		t.Errorf("error = %q, want a permission error on a read-only share", msg)
	}
}

func TestWriteShareLetsAnAgentEdit(t *testing.T) {
	e := newEnv(t)
	aliceCS := e.connect(e.alice)
	bobCS := e.connect(e.bob)

	mustCall(t, aliceCS, "create_note", map[string]any{"path": "Joint.md", "content": "# Joint\n\nalpha\n"})
	mustCall(t, aliceCS, "share_note", map[string]any{
		"path": "Joint.md", "grantee": "bob@github", "perm": "write",
	})

	mustCall(t, bobCS, "edit_note", map[string]any{
		"path": "Joint.md", "vault": "alice-github",
		"edits": []map[string]any{{"old": "alpha", "new": "beta"}},
	})

	var read struct {
		Content string `json:"content"`
	}
	call(t, aliceCS, "read_note", map[string]any{"path": "Joint.md"}, &read)
	if !strings.Contains(read.Content, "beta") {
		t.Errorf("bob's edit did not land: %q", read.Content)
	}
}

func TestShareAndUnshareTools(t *testing.T) {
	e := newEnv(t)
	aliceCS := e.connect(e.alice)
	bobCS := e.connect(e.bob)

	mustCall(t, aliceCS, "create_note", map[string]any{"path": "S.md", "content": "# S\n"})

	var granted struct {
		ID   string `json:"id"`
		Perm string `json:"perm"`
	}
	call(t, aliceCS, "share_note", map[string]any{"path": "S.md", "grantee": "bob@github"}, &granted)
	if granted.ID == "" || granted.Perm != "read" {
		t.Fatalf("share = %+v, want a read grant with an id", granted)
	}

	var shares struct {
		Granted []struct {
			ID string `json:"id"`
		} `json:"granted"`
		SharedWithMe []struct {
			Path string `json:"path"`
		} `json:"sharedWithMe"`
	}
	call(t, aliceCS, "list_shares", map[string]any{}, &shares)
	if len(shares.Granted) != 1 {
		t.Errorf("alice's granted shares = %+v", shares.Granted)
	}
	call(t, bobCS, "list_shares", map[string]any{}, &shares)
	if len(shares.SharedWithMe) != 1 || shares.SharedWithMe[0].Path != "S.md" {
		t.Errorf("bob's sharedWithMe = %+v", shares.SharedWithMe)
	}

	mustCall(t, aliceCS, "unshare_note", map[string]any{"id": granted.ID})
	msg := callExpectingError(t, bobCS, "read_note", map[string]any{"path": "S.md", "vault": "alice-github"})
	if !strings.Contains(strings.ToLower(msg), "denied") {
		t.Errorf("access survived revocation: %q", msg)
	}
}

func TestAnAgentCannotShareSomeoneElsesNote(t *testing.T) {
	e := newEnv(t)
	aliceCS := e.connect(e.alice)
	bobCS := e.connect(e.bob)

	mustCall(t, aliceCS, "create_note", map[string]any{"path": "Private.md", "content": "# Private\n"})

	// Bob's agent tries to grant itself access to a path it does not own.
	// share_note takes no vault argument and always acts on the caller's own
	// vault, so this creates a grant on *bob's* Private.md and cannot reach
	// alice's. The structural guarantee is what is being tested here.
	var granted struct {
		Vault      string `json:"vault"`
		OwnerLogin string `json:"ownerLogin"`
	}
	call(t, bobCS, "share_note", map[string]any{
		"path": "Private.md", "grantee": "alice@github",
	}, &granted)
	if granted.Vault != "bob-github" || granted.OwnerLogin != "bob@github" {
		t.Errorf("share = %+v, want it confined to bob's own vault", granted)
	}

	// Alice's note is untouched and still unreachable.
	msg := callExpectingError(t, bobCS, "read_note", map[string]any{
		"path": "Private.md", "vault": "alice-github",
	})
	if !strings.Contains(strings.ToLower(msg), "denied") {
		t.Errorf("bob gained access to alice's note: %q", msg)
	}
}

func TestListVaultsShowsSharedVaults(t *testing.T) {
	e := newEnv(t)
	aliceCS := e.connect(e.alice)
	bobCS := e.connect(e.bob)

	var out struct {
		Vaults []struct {
			Vault  string `json:"vault"`
			IsMine bool   `json:"isMine"`
		} `json:"vaults"`
	}
	call(t, bobCS, "list_vaults", map[string]any{}, &out)
	if len(out.Vaults) != 1 || !out.Vaults[0].IsMine {
		t.Fatalf("bob's vaults = %+v, want just his own", out.Vaults)
	}

	mustCall(t, aliceCS, "create_note", map[string]any{"path": "S.md", "content": "# S\n"})
	mustCall(t, aliceCS, "share_note", map[string]any{"path": "S.md", "grantee": "bob@github"})

	call(t, bobCS, "list_vaults", map[string]any{}, &out)
	if len(out.Vaults) != 2 {
		t.Errorf("bob's vaults = %+v, want his own plus alice's", out.Vaults)
	}
}

// --- resources and prompts ---

func TestResources(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{"path": "Notes/a.md", "content": "# A\n\nhello\n"})

	templates, err := cs.ListResourceTemplates(e.ctx, nil)
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	if len(templates.ResourceTemplates) == 0 {
		t.Fatal("no resource templates advertised")
	}

	res, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "folio://me/Notes/a.md"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, "hello") {
		t.Errorf("resource contents = %+v", res.Contents)
	}
	if res.Contents[0].MIMEType != "text/markdown" {
		t.Errorf("MIMEType = %q", res.Contents[0].MIMEType)
	}
}

func TestVaultIndexResource(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{"path": "a.md", "content": "---\ntags: [x]\n---\n# Alpha\n"})

	res, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "folio://me/"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	text := res.Contents[0].Text
	if !strings.Contains(text, "Alpha") || !strings.Contains(text, "#x") {
		t.Errorf("index resource = %q, want the note and its tag", text)
	}
}

func TestResourceRespectsPermissions(t *testing.T) {
	e := newEnv(t)
	aliceCS := e.connect(e.alice)
	bobCS := e.connect(e.bob)
	mustCall(t, aliceCS, "create_note", map[string]any{"path": "secret.md", "content": "# Secret\n"})

	if _, err := bobCS.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "folio://alice-github/secret.md"}); err == nil {
		t.Error("bob read alice's note through the resource interface")
	}
}

func TestPrompts(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	list, err := cs.ListPrompts(e.ctx, nil)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	got := make([]string, 0, len(list.Prompts))
	for _, p := range list.Prompts {
		got = append(got, p.Name)
	}
	for _, want := range []string{"daily_review", "summarize_note", "find_related"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing prompt %q; have %v", want, got)
		}
	}
}

func TestSummarizePromptIncludesTheNote(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{
		"path": "Target.md", "content": "# Target\n\ndistinctive content here\n",
	})
	mustCall(t, cs, "create_note", map[string]any{"path": "Source.md", "content": "See [[Target]].\n"})

	res, err := cs.GetPrompt(e.ctx, &mcp.GetPromptParams{
		Name:      "summarize_note",
		Arguments: map[string]string{"path": "Target.md"},
	})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("prompt produced no messages")
	}
	text := res.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "distinctive content here") {
		t.Errorf("prompt = %q, want the note's content inlined", text)
	}
	if !strings.Contains(text, "Source.md") {
		t.Errorf("prompt = %q, want the backlink included", text)
	}
}

func TestDailyReviewPromptDoesNotCreateNotes(t *testing.T) {
	// Asking for a review should never have the side effect of creating the
	// notes it was asked to review.
	e := newEnv(t)
	cs := e.connect(e.alice)

	if _, err := cs.GetPrompt(e.ctx, &mcp.GetPromptParams{
		Name:      "daily_review",
		Arguments: map[string]string{"date": "2026-08-30"},
	}); err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}

	var out struct {
		Notes []struct {
			Path string `json:"path"`
		} `json:"notes"`
	}
	call(t, cs, "list_notes", map[string]any{}, &out)
	if len(out.Notes) != 0 {
		t.Errorf("daily_review created %+v; it must only read", out.Notes)
	}
}

func TestVaultStats(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{"path": "a.md", "content": "---\ntags: [x]\n---\n# A\n\n[[b]]\n"})
	mustCall(t, cs, "create_note", map[string]any{"path": "b.md", "content": "# B\n"})

	var out struct {
		Notes int `json:"notes"`
		Tags  int `json:"tags"`
		Links int `json:"links"`
	}
	call(t, cs, "vault_stats", map[string]any{}, &out)
	if out.Notes != 2 || out.Tags != 1 || out.Links != 1 {
		t.Errorf("stats = %+v, want 2 notes, 1 tag, 1 link", out)
	}
}

func TestDeleteAndTagTools(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{"path": "a.md", "content": "---\ntags: [keep]\n---\n# A\n"})

	var tags struct {
		Tags []struct {
			Tag   string `json:"tag"`
			Count int    `json:"count"`
		} `json:"tags"`
	}
	call(t, cs, "list_tags", map[string]any{}, &tags)
	if len(tags.Tags) != 1 || tags.Tags[0].Tag != "keep" {
		t.Errorf("tags = %+v", tags.Tags)
	}

	mustCall(t, cs, "delete_note", map[string]any{"path": "a.md"})
	callExpectingError(t, cs, "read_note", map[string]any{"path": "a.md"})

	call(t, cs, "list_tags", map[string]any{}, &tags)
	if len(tags.Tags) != 0 {
		t.Errorf("tags after delete = %+v, want none", tags.Tags)
	}
}

func TestUnknownPathErrorGuidesTheModel(t *testing.T) {
	// The model reads this message and decides what to do next, so it should
	// point at the tool that would have found the right path.
	e := newEnv(t)
	cs := e.connect(e.alice)

	msg := callExpectingError(t, cs, "read_note", map[string]any{"path": "does/not/exist.md"})
	if !strings.Contains(msg, "not found") {
		t.Errorf("error = %q", msg)
	}
}

func TestServerInstructionsAreProvided(t *testing.T) {
	e := newEnv(t)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	ct, st := mcp.NewInMemoryTransports()

	server := e.srv.ForUser(e.alice)
	go server.Run(e.ctx, st)

	cs, err := client.Connect(e.ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	res := cs.InitializeResult()
	if res == nil || res.Instructions == "" {
		t.Fatal("the server sent no instructions; the model has to guess the vault's conventions without them")
	}
	if !strings.Contains(res.Instructions, "wikilink") {
		t.Errorf("instructions = %q, want them to explain the linking convention", res.Instructions)
	}
}

// --- attachments, links, and settings ---
//
// An agent is a first-class folio client, so everything a person can do in the
// browser or the terminal has to be reachable here and has to produce the same
// result on disk.

const testPNG = "\x89PNG\r\n\x1a\n0123456789"

func TestAttachFileObeysTheUsersSetting(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{"path": "Daily/Mon.md", "content": "# Mon\n"})

	var got struct {
		Path string `json:"path"`
		Link string `json:"link"`
		Size int64  `json:"size"`
	}
	call(t, cs, "attach_file", map[string]any{
		"base64": base64.StdEncoding.EncodeToString([]byte(testPNG)),
		"name":   "diagram.png",
		"note":   "Daily/Mon.md",
	}, &got)

	// The default setting files everything in attachments/, and the link is the
	// short form because the basename is unambiguous.
	if got.Path != "attachments/diagram.png" {
		t.Errorf("path = %q, want attachments/diagram.png", got.Path)
	}
	if got.Link != "diagram.png" {
		t.Errorf("link = %q, want diagram.png", got.Link)
	}
	if got.Size != int64(len(testPNG)) {
		t.Errorf("size = %d, want %d", got.Size, len(testPNG))
	}

	// The bytes round trip through the tool a model would read them with.
	var back struct {
		Base64   string `json:"base64"`
		MimeType string `json:"mimeType"`
	}
	call(t, cs, "read_attachment", map[string]any{"path": got.Path}, &back)
	raw, err := base64.StdEncoding.DecodeString(back.Base64)
	if err != nil {
		t.Fatalf("decoding what read_attachment returned: %v", err)
	}
	if string(raw) != testPNG {
		t.Error("the bytes changed on the way through")
	}
	if back.MimeType != "image/png" {
		t.Errorf("mimeType = %q, want image/png", back.MimeType)
	}
}

func TestAttachFileWillNotOverwrite(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	body := base64.StdEncoding.EncodeToString([]byte(testPNG))

	var first, second struct {
		Path string `json:"path"`
	}
	call(t, cs, "attach_file", map[string]any{"base64": body, "name": "shot.png"}, &first)
	call(t, cs, "attach_file", map[string]any{"base64": body, "name": "shot.png"}, &second)

	if first.Path != "attachments/shot.png" || second.Path != "attachments/shot 1.png" {
		t.Errorf("paths = %q and %q, want attachments/shot.png and attachments/shot 1.png",
			first.Path, second.Path)
	}

	var listed struct {
		Attachments []struct {
			Path string `json:"path"`
		} `json:"attachments"`
	}
	call(t, cs, "list_attachments", map[string]any{}, &listed)
	if len(listed.Attachments) != 2 {
		t.Errorf("attachments = %+v, want both files", listed.Attachments)
	}
}

func TestAttachFileRejectsWhatIsNotBase64(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	// A model that sends prose instead of an encoded file should be told so,
	// rather than having the prose written to disk as a PNG.
	msg := callExpectingError(t, cs, "attach_file", map[string]any{
		"base64": "this is not base64!!", "name": "x.png",
	})
	if !strings.Contains(msg, "base64") {
		t.Errorf("error = %q, want it to mention base64", msg)
	}
}

func TestAttachFileRefusesMarkdown(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	msg := callExpectingError(t, cs, "attach_file", map[string]any{
		"base64": base64.StdEncoding.EncodeToString([]byte("# hi\n")), "name": "notes.md",
	})
	if !strings.Contains(msg, "note") {
		t.Errorf("error = %q, want it to point at the note tools", msg)
	}
}

func TestResolveLinkAnswersLikeTheEditorDoes(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{
		"path": "Meeting.md", "content": "# Meeting\n\n## Actions\n\n- ship it\n\n## Other\n\nNope.\n",
	})
	mustCall(t, cs, "create_note", map[string]any{"path": "Daily/Mon.md", "content": "# Mon\n"})

	type resolved struct {
		Kind    string `json:"kind"`
		Path    string `json:"path"`
		Anchor  string `json:"anchor"`
		Content string `json:"content"`
	}

	// A bare name resolves against the whole vault, which is the thing an agent
	// cannot work out from the path alone.
	var whole resolved
	call(t, cs, "resolve_link", map[string]any{"target": "Meeting", "from": "Daily/Mon.md"}, &whole)
	if whole.Kind != "note" || whole.Path != "Meeting.md" {
		t.Fatalf("resolve = %+v", whole)
	}

	// An anchor narrows to that section, by the same rule the two editors use.
	var section resolved
	call(t, cs, "resolve_link", map[string]any{"target": "Meeting#Actions"}, &section)
	if section.Anchor != "Actions" {
		t.Errorf("anchor = %q", section.Anchor)
	}
	if want := "## Actions\n\n- ship it\n\n"; section.Content != want {
		t.Errorf("content = %q, want %q", section.Content, want)
	}

	// A model that pasted the whole link rather than its interior still gets an
	// answer instead of a retry.
	var pasted resolved
	call(t, cs, "resolve_link", map[string]any{"target": "![[Meeting#Actions]]"}, &pasted)
	if pasted.Content != section.Content {
		t.Errorf("the bracketed form resolved differently: %+v", pasted)
	}

	// A link to a note nobody has written is normal, not an error.
	var missing resolved
	call(t, cs, "resolve_link", map[string]any{"target": "Nope"}, &missing)
	if missing.Kind != "missing" {
		t.Errorf("resolve of a dangling link = %+v", missing)
	}
}

func TestResolveLinkFindsAnAttachment(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "attach_file", map[string]any{
		"base64": base64.StdEncoding.EncodeToString([]byte(testPNG)), "name": "shot.png",
	})

	var got struct {
		Kind    string `json:"kind"`
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	call(t, cs, "resolve_link", map[string]any{"target": "shot.png"}, &got)
	if got.Kind != "attachment" || got.Path != "attachments/shot.png" {
		t.Errorf("resolve = %+v", got)
	}
	// Bytes come from read_attachment, not inlined into a text field.
	if got.Content != "" {
		t.Errorf("content = %q, want it empty for an attachment", got.Content)
	}
}

func TestResolveLinkDoesNotLeakUnsharedNotes(t *testing.T) {
	e := newEnv(t)
	alice := e.connect(e.alice)
	bob := e.connect(e.bob)

	mustCall(t, alice, "create_note", map[string]any{"path": "Shared.md", "content": "# Shared\nvisible\n"})
	mustCall(t, alice, "create_note", map[string]any{"path": "Private.md", "content": "# Private\nsecret\n"})
	mustCall(t, alice, "share_note", map[string]any{
		"path": "Shared.md", "grantee": "bob@github", "perm": "read",
	})

	var got struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
	}
	call(t, bob, "resolve_link", map[string]any{
		"target": "Shared", "vault": "alice-github", "from": "Shared.md",
	}, &got)
	if got.Kind != "note" {
		t.Fatalf("bob cannot resolve the note shared with him: %+v", got)
	}

	// A share covers one path. Anything outside it reports missing rather than
	// forbidden, because "forbidden" confirms the note exists.
	call(t, bob, "resolve_link", map[string]any{
		"target": "Private", "vault": "alice-github", "from": "Shared.md",
	}, &got)
	if got.Kind != "missing" || strings.Contains(got.Content, "secret") {
		t.Errorf("an unshared note leaked through resolve_link: %+v", got)
	}
}

func TestSettingsRoundTripAndChangeWhereFilesLand(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)
	mustCall(t, cs, "create_note", map[string]any{"path": "Daily/Mon.md", "content": "# Mon\n"})

	type settings struct {
		AttachmentMode   string `json:"attachmentMode"`
		AttachmentFolder string `json:"attachmentFolder"`
	}
	var got settings
	call(t, cs, "get_settings", map[string]any{}, &got)
	if got.AttachmentMode != "folder" || got.AttachmentFolder != "attachments" {
		t.Errorf("defaults = %+v", got)
	}

	// Switching to a mode with no folder must not lose the folder name, so
	// switching back does not silently file things somewhere else.
	call(t, cs, "set_settings", map[string]any{"attachmentMode": "current"}, &got)
	if got.AttachmentMode != "current" || got.AttachmentFolder != "attachments" {
		t.Errorf("after switching mode = %+v", got)
	}

	var up struct {
		Path string `json:"path"`
	}
	call(t, cs, "attach_file", map[string]any{
		"base64": base64.StdEncoding.EncodeToString([]byte(testPNG)),
		"name":   "shot.png",
		"note":   "Daily/Mon.md",
	}, &up)
	if up.Path != "Daily/shot.png" {
		t.Errorf("path = %q, want Daily/shot.png", up.Path)
	}
}

func TestSetSettingsRejectsAnUnusableFolder(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.alice)

	for _, args := range []map[string]any{
		{"attachmentMode": "elsewhere"},
		{"attachmentMode": "folder", "attachmentFolder": "../escape"},
		{"attachmentMode": "folder", "attachmentFolder": ".hidden"},
	} {
		if msg := callExpectingError(t, cs, "set_settings", args); msg == "" {
			t.Errorf("set_settings%v was accepted", args)
		}
	}
}
