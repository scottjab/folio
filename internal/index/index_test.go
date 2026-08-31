package index_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/scottjab/folio/internal/index"
	"github.com/scottjab/folio/internal/store"
	"github.com/scottjab/folio/internal/vault"
)

type fixture struct {
	ix      *index.Index
	db      *store.DB
	v       *vault.Vault
	vaultID int64
	ctx     context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()

	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	v, err := vault.Open(filepath.Join(dir, "vault"))
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	t.Cleanup(func() { v.Close() })

	ctx := context.Background()
	if _, err := db.Exec(ctx,
		`INSERT INTO users(id, tailscale_user_id, login, display_name, vault_dir, created_at) VALUES (1,1,'alice','Alice','alice',0)`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO vaults(id, user_id, dir, created_at) VALUES (1,1,'alice',0)`); err != nil {
		t.Fatalf("seed vault: %v", err)
	}

	return &fixture{ix: index.New(db), db: db, v: v, vaultID: 1, ctx: ctx}
}

// write puts a note in the vault and indexes it, the way the API layer will.
func (f *fixture) write(t *testing.T, path, content string) {
	t.Helper()
	n, err := f.v.Write(path, []byte(content), "")
	if err != nil {
		t.Fatalf("vault.Write(%q): %v", path, err)
	}
	if err := f.ix.Put(f.ctx, f.vaultID, f.v, n, []byte(content)); err != nil {
		t.Fatalf("index.Put(%q): %v", path, err)
	}
}

func (f *fixture) searchPaths(t *testing.T, q string) []string {
	t.Helper()
	res, err := f.ix.Search(f.ctx, index.SearchRequest{
		Query:    index.ParseQuery(q),
		VaultIDs: []int64{f.vaultID},
		Limit:    50,
	})
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	paths := make([]string, len(res.Hits))
	for i, h := range res.Hits {
		paths[i] = h.Path
	}
	return paths
}

func TestPutAndGet(t *testing.T) {
	f := newFixture(t)
	f.write(t, "Daily/2026-08-30.md", "---\ntags: [daily, go]\n---\n# Thursday\n\nShipped the indexer.\n")

	e, err := f.ix.Get(f.ctx, f.vaultID, "Daily/2026-08-30.md")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Title != "Thursday" {
		t.Errorf("Title = %q, want the H1", e.Title)
	}
	if !slices.Equal(e.Tags, []string{"daily", "go"}) {
		t.Errorf("Tags = %v", e.Tags)
	}
	if e.SHA256 == "" {
		t.Error("SHA256 is empty")
	}
}

func TestGetMissing(t *testing.T) {
	f := newFixture(t)
	if _, err := f.ix.Get(f.ctx, f.vaultID, "nope.md"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestSearchFindsTitleBodyAndTags(t *testing.T) {
	f := newFixture(t)
	f.write(t, "Projects/folio.md", "---\ntags: [go, sqlite]\n---\n# folio\n\nA notes app on the tailnet.\n")
	f.write(t, "Daily/2026-08-30.md", "---\ntags: [daily]\n---\n# Thursday\n\nUnrelated content here.\n")

	for _, tc := range []struct{ query, want string }{
		{"folio", "Projects/folio.md"},      // title
		{"tailnet", "Projects/folio.md"},    // body
		{"tag:sqlite", "Projects/folio.md"}, // tag
		{"Thursday", "Daily/2026-08-30.md"},
		{"path:Daily", "Daily/2026-08-30.md"},
	} {
		got := f.searchPaths(t, tc.query)
		if !slices.Contains(got, tc.want) {
			t.Errorf("search %q = %v, want it to contain %q", tc.query, got, tc.want)
		}
	}
}

func TestSearchRanksTitleAboveBody(t *testing.T) {
	f := newFixture(t)
	f.write(t, "body-mention.md", "# Something Else\n\nI mention widgets a lot. widgets widgets widgets.\n")
	f.write(t, "Widgets.md", "# Widgets\n\nUnrelated body text.\n")

	got := f.searchPaths(t, "widgets")
	if len(got) < 2 {
		t.Fatalf("search returned %v, want both notes", got)
	}
	if got[0] != "Widgets.md" {
		t.Errorf("top hit = %q, want the title match to outrank the body match (%v)", got[0], got)
	}
}

func TestSearchReturnsHighlightedSnippet(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "# A\n\nThe quick brown fox jumps over the lazy dog and keeps going for a while.\n")

	res, err := f.ix.Search(f.ctx, index.SearchRequest{
		Query: index.ParseQuery("fox"), VaultIDs: []int64{f.vaultID}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	if !strings.Contains(res.Hits[0].Snippet, "fox") {
		t.Errorf("snippet = %q, want it to contain the match", res.Hits[0].Snippet)
	}
	if !strings.Contains(res.Hits[0].Snippet, index.HighlightOpen) {
		t.Errorf("snippet = %q, want the match marked up", res.Hits[0].Snippet)
	}
}

func TestEmptyQueryReturnsRecentNotes(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "# A\n")
	f.write(t, "b.md", "# B\n")

	got := f.searchPaths(t, "")
	if len(got) != 2 {
		t.Errorf("empty query returned %v, want both notes rather than an error", got)
	}
}

func TestSearchExcludes(t *testing.T) {
	f := newFixture(t)
	f.write(t, "keep.md", "---\ntags: [keep]\n---\n# Keep\n\nshared word here\n")
	f.write(t, "drop.md", "---\ntags: [draft]\n---\n# Drop\n\nshared word here\n")

	got := f.searchPaths(t, "shared -tag:draft")
	if !slices.Equal(got, []string{"keep.md"}) {
		t.Errorf("search = %v, want only keep.md", got)
	}

	// Exclusion with no positive term still works.
	got = f.searchPaths(t, "-tag:draft")
	if !slices.Equal(got, []string{"keep.md"}) {
		t.Errorf("negative-only search = %v, want only keep.md", got)
	}
}

func TestSearchIsScopedToVaults(t *testing.T) {
	f := newFixture(t)
	f.write(t, "mine.md", "# Mine\n\nsecret sauce\n")

	res, err := f.ix.Search(f.ctx, index.SearchRequest{
		Query: index.ParseQuery("secret"), VaultIDs: []int64{999}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("search in another vault returned %d hits; scoping is broken", len(res.Hits))
	}

	// No readable vaults at all must return nothing, not everything.
	res, err = f.ix.Search(f.ctx, index.SearchRequest{
		Query: index.ParseQuery("secret"), VaultIDs: nil, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("search with no vault scope returned %d hits; it must return none", len(res.Hits))
	}
}

func TestParseQueryProducesValidFTS5(t *testing.T) {
	// The real contract: whatever a user types, the translated query must
	// execute. This runs each one through the actual engine.
	f := newFixture(t)
	f.write(t, "a.md", "# A\n\nsome content\n")

	inputs := []string{
		"", "   ", "-", ":", "*", `"`, `""`, `"""`, `a"b`, `"unclosed`,
		"AND", "OR", "NOT", "NEAR", "a OR", "OR a", "a NOT b",
		"(", ")", "()", "^", "{", "}", "{a}", "^a", "a:b:c",
		"tag:", "tag:*", "-tag:", "path:../etc",
		`'; DROP TABLE notes;--`, `%%`, `\`, "a\x00b",
		"café", "日本語", "emoji 🎉", strings.Repeat("x", 500),
		"a- -b", "--a", "a**", "***",
	}
	for _, in := range inputs {
		t.Run(strings.ToValidUTF8(in, "?"), func(t *testing.T) {
			q := index.ParseQuery(in)
			if _, err := f.ix.Search(f.ctx, index.SearchRequest{
				Query: q, VaultIDs: []int64{f.vaultID}, Limit: 10,
			}); err != nil {
				t.Errorf("input %q compiled to match=%q exclude=%q and failed: %v", in, q.Match, q.Exclude, err)
			}
		})
	}
}

func TestReindexReplacesRatherThanDuplicates(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "# First\n\napples\n")
	f.write(t, "a.md", "# Second\n\noranges\n")

	n, err := f.db.One[int](f.ctx, `SELECT count(*) FROM notes WHERE vault_id = ?`, f.vaultID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("notes rows = %d, want 1", n)
	}
	fts, _ := f.db.One[int](f.ctx, `SELECT count(*) FROM notes_fts`)
	if fts != 1 {
		t.Errorf("notes_fts rows = %d, want 1", fts)
	}

	if got := f.searchPaths(t, "apples"); len(got) != 0 {
		t.Errorf("stale content is still searchable: %v", got)
	}
	if got := f.searchPaths(t, "oranges"); !slices.Equal(got, []string{"a.md"}) {
		t.Errorf("new content not searchable: %v", got)
	}
}

func TestRemoveClearsEverything(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "---\ntags: [x]\n---\n# A\n\n[[b]] apples\n")

	if err := f.ix.Remove(f.ctx, f.vaultID, "a.md"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM notes`,
		`SELECT count(*) FROM notes_fts`,
		`SELECT count(*) FROM tags`,
		`SELECT count(*) FROM links`,
	} {
		n, err := f.db.One[int](f.ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
}

func TestBacklinks(t *testing.T) {
	f := newFixture(t)
	f.write(t, "Projects/folio.md", "# folio\n\nThe project.\n")
	f.write(t, "Daily/2026-08-30.md", "# Thursday\n\nWorked on [[Projects/folio]] today.\n")
	f.write(t, "Daily/2026-08-29.md", "# Wednesday\n\nNothing related.\n")

	links, err := f.ix.Backlinks(f.ctx, f.vaultID, "Projects/folio.md")
	if err != nil {
		t.Fatalf("Backlinks: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("got %d backlinks, want 1: %+v", len(links), links)
	}
	if links[0].SourcePath != "Daily/2026-08-30.md" {
		t.Errorf("SourcePath = %q", links[0].SourcePath)
	}
	if links[0].SourceTitle != "Thursday" {
		t.Errorf("SourceTitle = %q", links[0].SourceTitle)
	}
}

func TestDanglingLinkResolvesWhenTargetAppears(t *testing.T) {
	f := newFixture(t)
	// Link first, create the target later. This is the normal way people write:
	// you type [[Some Idea]] and make the note afterwards.
	f.write(t, "Daily/x.md", "# X\n\nSee [[Some Idea]].\n")

	dangling, err := f.ix.DanglingLinks(f.ctx, f.vaultID)
	if err != nil {
		t.Fatalf("DanglingLinks: %v", err)
	}
	if len(dangling) != 1 || dangling[0].Target != "Some Idea" {
		t.Fatalf("dangling = %+v, want the one unresolved link", dangling)
	}

	f.write(t, "Some Idea.md", "# Some Idea\n")

	links, err := f.ix.Backlinks(f.ctx, f.vaultID, "Some Idea.md")
	if err != nil {
		t.Fatalf("Backlinks: %v", err)
	}
	if len(links) != 1 || links[0].SourcePath != "Daily/x.md" {
		t.Errorf("backlinks = %+v, want the previously dangling link to have resolved", links)
	}
}

func TestRenameRewritesInboundWikilinks(t *testing.T) {
	f := newFixture(t)
	f.write(t, "Projects/folio.md", "# folio\n")
	f.write(t, "Daily/x.md", "# X\n\nSee [[Projects/folio]] and [[Projects/folio|the project]].\n")

	if err := f.ix.Rename(f.ctx, f.vaultID, f.v, "Projects/folio.md", "Archive/folio.md"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	body, _, err := f.v.Read("Daily/x.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := string(body)
	if strings.Contains(got, "[[Projects/folio]]") {
		t.Errorf("plain wikilink was not rewritten: %q", got)
	}
	if !strings.Contains(got, "[[Archive/folio]]") {
		t.Errorf("expected the rewritten target: %q", got)
	}
	// The alias the author chose must survive the rewrite.
	if !strings.Contains(got, "[[Archive/folio|the project]]") {
		t.Errorf("alias was lost: %q", got)
	}

	links, _ := f.ix.Backlinks(f.ctx, f.vaultID, "Archive/folio.md")
	if len(links) != 2 {
		t.Errorf("got %d backlinks after rename, want 2", len(links))
	}
}

func TestRenameMovesTheFileAndTheIndex(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "# A\n")
	if err := f.ix.Rename(f.ctx, f.vaultID, f.v, "a.md", "b.md"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if f.v.Exists("a.md") {
		t.Error("source file still exists")
	}
	if !f.v.Exists("b.md") {
		t.Error("destination file missing")
	}
	if _, err := f.ix.Get(f.ctx, f.vaultID, "a.md"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("old path still indexed: %v", err)
	}
	if _, err := f.ix.Get(f.ctx, f.vaultID, "b.md"); err != nil {
		t.Errorf("new path not indexed: %v", err)
	}
}

func TestTags(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "---\ntags: [go, sqlite]\n---\n# A\n")
	f.write(t, "b.md", "---\ntags: [go]\n---\n# B\n\n#extra\n")

	tags, err := f.ix.Tags(f.ctx, []int64{f.vaultID})
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	counts := map[string]int{}
	for _, tc := range tags {
		counts[tc.Tag] = tc.Count
	}
	if counts["go"] != 2 || counts["sqlite"] != 1 || counts["extra"] != 1 {
		t.Errorf("tag counts = %v", counts)
	}
	// Most used first is what a tag sidebar wants.
	if tags[0].Tag != "go" {
		t.Errorf("tags[0] = %+v, want the most-used tag first", tags[0])
	}
}

func TestList(t *testing.T) {
	f := newFixture(t)
	f.write(t, "Daily/a.md", "---\ntags: [daily]\n---\n# A\n")
	f.write(t, "Daily/b.md", "# B\n")
	f.write(t, "Projects/c.md", "---\ntags: [work]\n---\n# C\n")

	all, err := f.ix.List(f.ctx, index.ListRequest{VaultIDs: []int64{f.vaultID}, Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List returned %d, want 3", len(all))
	}

	daily, err := f.ix.List(f.ctx, index.ListRequest{VaultIDs: []int64{f.vaultID}, Folder: "Daily", Limit: 100})
	if err != nil {
		t.Fatalf("List(folder): %v", err)
	}
	if len(daily) != 2 {
		t.Errorf("folder filter returned %d, want 2", len(daily))
	}

	tagged, err := f.ix.List(f.ctx, index.ListRequest{VaultIDs: []int64{f.vaultID}, Tag: "work", Limit: 100})
	if err != nil {
		t.Fatalf("List(tag): %v", err)
	}
	if len(tagged) != 1 || tagged[0].Path != "Projects/c.md" {
		t.Errorf("tag filter = %+v", tagged)
	}
}

func TestListFolderPrefixIsNotSubstring(t *testing.T) {
	f := newFixture(t)
	f.write(t, "Projects/a.md", "# A\n")
	f.write(t, "Projects2/b.md", "# B\n")

	got, err := f.ix.List(f.ctx, index.ListRequest{VaultIDs: []int64{f.vaultID}, Folder: "Projects", Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Path != "Projects/a.md" {
		t.Errorf("folder filter leaked into a sibling folder: %+v", got)
	}
}

func TestSyncPicksUpExternalChanges(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "# A\n\noriginal\n")

	// Obsidian, or a git pull, edits and adds files behind our back.
	if _, err := f.v.Write("a.md", []byte("# A\n\nreplaced\n"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := f.v.Write("new.md", []byte("# New\n\nappeared\n"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	stats, err := f.ix.Sync(f.ctx, f.vaultID, f.v)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.Added != 1 || stats.Updated != 1 {
		t.Errorf("stats = %+v, want 1 added and 1 updated", stats)
	}
	if got := f.searchPaths(t, "replaced"); !slices.Equal(got, []string{"a.md"}) {
		t.Errorf("updated content not searchable: %v", got)
	}
	if got := f.searchPaths(t, "original"); len(got) != 0 {
		t.Errorf("stale content still searchable: %v", got)
	}
	if got := f.searchPaths(t, "appeared"); !slices.Equal(got, []string{"new.md"}) {
		t.Errorf("new file not indexed: %v", got)
	}
}

func TestSyncRemovesDeletedNotes(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "# A\n\ngone soon\n")
	if err := f.v.Delete("a.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	stats, err := f.ix.Sync(f.ctx, f.vaultID, f.v)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.Removed != 1 {
		t.Errorf("stats = %+v, want 1 removed", stats)
	}
	if got := f.searchPaths(t, "gone"); len(got) != 0 {
		t.Errorf("deleted note is still searchable: %v", got)
	}
}

func TestSyncIsIdempotentAndSkipsUnchanged(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "# A\n")
	f.write(t, "b.md", "# B\n")

	stats, err := f.ix.Sync(f.ctx, f.vaultID, f.v)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.Added+stats.Updated+stats.Removed != 0 {
		t.Errorf("stats = %+v, want no work on an already-current index", stats)
	}
	if stats.Unchanged != 2 {
		t.Errorf("Unchanged = %d, want 2", stats.Unchanged)
	}
}

func TestRebuildRecreatesTheIndexFromFilesAlone(t *testing.T) {
	f := newFixture(t)
	f.write(t, "Projects/folio.md", "---\ntags: [go]\n---\n# folio\n\ntailnet notes\n")
	f.write(t, "Daily/x.md", "# X\n\nSee [[Projects/folio]].\n")

	// A share is authoritative data with no markdown home. Rebuild must not
	// touch it; that is the whole reason `index rebuild` exists rather than
	// telling people to delete the database.
	if _, err := f.db.Exec(f.ctx,
		`INSERT INTO shares(id, vault_id, owner_user_id, path, is_folder, grantee_login, perm, created_at)
		 VALUES ('s1', 1, 1, 'Projects/folio.md', 0, 'bob@github', 'read', 0)`); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	stats, err := f.ix.Rebuild(f.ctx, f.vaultID, f.v)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if stats.Added != 2 {
		t.Errorf("stats = %+v, want 2 notes indexed", stats)
	}

	if got := f.searchPaths(t, "tailnet"); !slices.Equal(got, []string{"Projects/folio.md"}) {
		t.Errorf("search after rebuild = %v", got)
	}
	links, _ := f.ix.Backlinks(f.ctx, f.vaultID, "Projects/folio.md")
	if len(links) != 1 {
		t.Errorf("backlinks after rebuild = %+v, want 1", links)
	}
	shares, _ := f.db.One[int](f.ctx, `SELECT count(*) FROM shares`)
	if shares != 1 {
		t.Errorf("rebuild destroyed %d share(s); it must only touch derived tables", 1-shares)
	}
	notes, _ := f.db.One[int](f.ctx, `SELECT count(*) FROM notes`)
	if notes != 2 {
		t.Errorf("notes = %d, want exactly 2 with no duplicates", notes)
	}
}

func TestRebuildSurvivesABadNote(t *testing.T) {
	f := newFixture(t)
	if _, err := f.v.Write("good.md", []byte("# Good\n\nfindable\n"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := f.v.Write("bad.md", []byte("---\n\tbroken: yaml: here\n---\n# Bad\n\nalso findable\n"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	stats, err := f.ix.Rebuild(f.ctx, f.vaultID, f.v)
	if err != nil {
		t.Fatalf("Rebuild should not abort on one bad note: %v", err)
	}
	if stats.Added != 2 {
		t.Errorf("stats = %+v, want both notes indexed despite the bad frontmatter", stats)
	}
	if len(stats.Errors) != 1 {
		t.Errorf("Errors = %v, want the bad note reported", stats.Errors)
	}
	if got := f.searchPaths(t, "findable"); len(got) != 2 {
		t.Errorf("search = %v, want both notes to remain searchable", got)
	}
}

func TestStats(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "---\ntags: [x]\n---\n# A\n\n[[b]]\n")
	f.write(t, "b.md", "# B\n")

	vs, err := f.ix.VaultStats(f.ctx, f.vaultID)
	if err != nil {
		t.Fatalf("VaultStats: %v", err)
	}
	if vs.Notes != 2 {
		t.Errorf("Notes = %d, want 2", vs.Notes)
	}
	if vs.Tags != 1 {
		t.Errorf("Tags = %d, want 1", vs.Tags)
	}
	if vs.Links != 1 {
		t.Errorf("Links = %d, want 1", vs.Links)
	}
}

func TestSyncPathsSkipsUnchangedFiles(t *testing.T) {
	// This is the loop-breaker: folio writes a note, fsnotify reports it, and
	// the resulting sync must be a no-op rather than a pointless reindex.
	f := newFixture(t)
	f.write(t, "a.md", "# A\n")

	stats, err := f.ix.SyncPaths(f.ctx, f.vaultID, f.v, []string{"a.md"})
	if err != nil {
		t.Fatalf("SyncPaths: %v", err)
	}
	if stats.Unchanged != 1 || stats.Updated != 0 || stats.Added != 0 {
		t.Errorf("stats = %+v, want the write recognised as already indexed", stats)
	}
}

func TestSyncPathsHandlesAddEditDelete(t *testing.T) {
	f := newFixture(t)
	f.write(t, "keep.md", "# Keep\n")

	// Add, edit, and delete happen behind our back.
	if _, err := f.v.Write("added.md", []byte("# Added\n\nbrandnew\n"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := f.v.Write("keep.md", []byte("# Keep\n\nedited\n"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	stats, err := f.ix.SyncPaths(f.ctx, f.vaultID, f.v, []string{"added.md", "keep.md"})
	if err != nil {
		t.Fatalf("SyncPaths: %v", err)
	}
	if stats.Added != 1 || stats.Updated != 1 {
		t.Errorf("stats = %+v, want 1 added and 1 updated", stats)
	}

	if err := f.v.Delete("added.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	stats, err = f.ix.SyncPaths(f.ctx, f.vaultID, f.v, []string{"added.md"})
	if err != nil {
		t.Fatalf("SyncPaths: %v", err)
	}
	if stats.Removed != 1 {
		t.Errorf("stats = %+v, want 1 removed", stats)
	}
	if got := f.searchPaths(t, "brandnew"); len(got) != 0 {
		t.Errorf("deleted note still searchable: %v", got)
	}
}

func TestSyncPathsIgnoresNonNotes(t *testing.T) {
	f := newFixture(t)
	stats, err := f.ix.SyncPaths(f.ctx, f.vaultID, f.v, []string{
		"attachments/img.png", ".obsidian/app.json", "../escape.md", "notes.txt",
	})
	if err != nil {
		t.Fatalf("SyncPaths: %v", err)
	}
	if stats.Added+stats.Updated+stats.Removed+stats.Unchanged != 0 {
		t.Errorf("stats = %+v, want attachments, hidden files, and bad paths all skipped", stats)
	}
}
