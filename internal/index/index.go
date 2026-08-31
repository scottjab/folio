// Package index maintains the searchable view of a vault: what notes exist,
// what they are called, what they link to, what they are tagged with, and their
// full text.
//
// Everything here is derived. The markdown files are the source of truth, and
// [Index.Rebuild] can reconstruct this entire package's tables from them. The
// authoritative tables (users, vaults, shares) live in the same database but are
// never touched by this package.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/scottjab/tsnotes/internal/markdown"
	"github.com/scottjab/tsnotes/internal/store"
	"github.com/scottjab/tsnotes/internal/vault"
	"github.com/scottjab/tsnotes/internal/vaultpath"
)

// ErrNotFound means the path is not in the index.
var ErrNotFound = errors.New("note not indexed")

// Highlight markers wrapped around search matches in a snippet. They are not
// HTML: the API hands these to the browser, which decides how to render them,
// so a note containing literal "<mark>" cannot forge a highlight.
const (
	HighlightOpen   = "\x02"
	HighlightClose  = "\x03"
	snippetEllipsis = "…"
)

// Column weights for bm25. Order matches the FTS5 table: title, body, tags,
// path. A title match should beat a body match comfortably; a path match is a
// weak signal but better than nothing.
const bm25Weights = "10.0, 1.0, 5.0, 2.0"

// Index is the read and write interface to the derived tables.
type Index struct {
	db *store.DB
}

// New wraps a database handle.
func New(db *store.DB) *Index { return &Index{db: db} }

// Entry is one indexed note.
type Entry struct {
	ID          int64
	VaultID     int64
	Path        string
	UUID        string
	Title       string
	SHA256      string
	Size        int64
	ModTime     time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Frontmatter string
	Tags        []string
}

// Hit is one search result.
type Hit struct {
	Entry
	Snippet string
	Score   float64
}

// SearchRequest asks for notes matching Query within VaultIDs.
//
// VaultIDs is the authorization boundary and is never optional: an empty slice
// means the caller can read nothing, and returns no results. Callers get this
// list from the share resolver, so a bug here fails closed.
type SearchRequest struct {
	Query    Query
	VaultIDs []int64
	Limit    int
	Offset   int
}

// SearchResponse carries the hits plus enough state for a "load more" button.
type SearchResponse struct {
	Hits    []Hit
	HasMore bool
}

// ListRequest browses notes without a full-text query.
type ListRequest struct {
	VaultIDs []int64
	Folder   string
	Tag      string
	Since    time.Time
	Limit    int
	Offset   int
}

// Backlink is an inbound reference to a note.
type Backlink struct {
	SourceID    int64  `db:"source_id"`
	SourcePath  string `db:"source_path"`
	SourceTitle string `db:"source_title"`
	Kind        string `db:"kind"`
	Alias       string `db:"alias"`
	Anchor      string `db:"anchor"`
}

// DanglingLink is a wikilink whose target does not exist yet.
type DanglingLink struct {
	SourceID   int64  `db:"source_id"`
	SourcePath string `db:"source_path"`
	Target     string `db:"target"`
}

// TagCount is one tag and how many notes carry it.
type TagCount struct {
	Tag   string `db:"tag"`
	Count int    `db:"count"`
}

// Stats summarizes a Sync or Rebuild.
type Stats struct {
	Added     int
	Updated   int
	Removed   int
	Unchanged int
	// Errors holds per-note problems that did not stop the run. A note with
	// broken frontmatter is still indexed; we just say so.
	Errors []string
}

// VaultStat is a quick summary for the UI and for `tsnotes doctor`.
type VaultStat struct {
	Notes int `db:"notes"`
	Tags  int `db:"tags"`
	Links int `db:"links"`
}

// Put indexes a note that already exists in the vault.
//
// v is needed to resolve wikilinks, which depend on what else is in the vault.
func (ix *Index) Put(ctx context.Context, vaultID int64, v *vault.Vault, n vault.Note, content []byte) error {
	doc, parseErr := markdown.Parse(n.Path, content)
	if doc == nil {
		return fmt.Errorf("parse %s: %w", n.Path, parseErr)
	}
	if err := ix.put(ctx, vaultID, v, n, doc); err != nil {
		return err
	}
	// A bad frontmatter block is reported but never blocks indexing: the body is
	// still worth searching.
	return parseErr
}

func (ix *Index) put(ctx context.Context, vaultID int64, v *vault.Vault, n vault.Note, doc *markdown.Doc) error {
	return ix.db.Tx(ctx, func(tx *store.Tx) error {
		id, err := upsertNote(ctx, tx, vaultID, n, doc)
		if err != nil {
			return err
		}
		if err := replaceDerived(ctx, tx, id, doc); err != nil {
			return err
		}
		return resolveLinks(ctx, tx, vaultID, v)
	})
}

// upsertNote writes the notes row and its FTS companion, returning the note id.
func upsertNote(ctx context.Context, tx *store.Tx, vaultID int64, n vault.Note, doc *markdown.Doc) (int64, error) {
	now := time.Now().Unix()
	created := now
	if !doc.Frontmatter.Created.IsZero() {
		created = doc.Frontmatter.Created.Unix()
	}

	// Preserve the original created_at across re-indexing; only a first sighting
	// gets to set it.
	res, err := tx.Exec(ctx, `
		INSERT INTO notes (vault_id, path, note_uuid, title, sha256, size, mtime, created_at, updated_at, frontmatter)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (vault_id, path) DO UPDATE SET
			note_uuid   = excluded.note_uuid,
			title       = excluded.title,
			sha256      = excluded.sha256,
			size        = excluded.size,
			mtime       = excluded.mtime,
			updated_at  = excluded.updated_at,
			frontmatter = excluded.frontmatter`,
		vaultID, n.Path, doc.Frontmatter.ID, doc.Title, n.SHA256, n.Size,
		n.ModTime.Unix(), created, now, doc.FrontmatterRaw)
	if err != nil {
		return 0, fmt.Errorf("upsert note %s: %w", n.Path, err)
	}
	_ = res

	id, err := tx.One[int64](ctx, `SELECT id FROM notes WHERE vault_id = ? AND path = ?`, vaultID, n.Path)
	if err != nil {
		return 0, fmt.Errorf("read back note id for %s: %w", n.Path, err)
	}
	return id, nil
}

// replaceDerived rewrites the FTS row, tags, and links for one note. Delete then
// insert, because an in-place update of an FTS5 row is not simpler and this is
// unambiguously correct.
func replaceDerived(ctx context.Context, tx *store.Tx, noteID int64, doc *markdown.Doc) error {
	for _, q := range []string{
		`DELETE FROM notes_fts WHERE rowid = ?`,
		`DELETE FROM tags WHERE note_id = ?`,
		`DELETE FROM links WHERE src_note_id = ?`,
	} {
		if _, err := tx.Exec(ctx, q, noteID); err != nil {
			return fmt.Errorf("clear derived rows: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO notes_fts (rowid, title, body, tags, path) VALUES (?,?,?,?,?)`,
		noteID, doc.Title, doc.Plain, strings.Join(doc.Tags, " "), searchablePath(doc.Path)); err != nil {
		return fmt.Errorf("index text: %w", err)
	}

	for _, tag := range doc.Tags {
		if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO tags (note_id, tag) VALUES (?,?)`, noteID, tag); err != nil {
			return fmt.Errorf("index tag %q: %w", tag, err)
		}
	}
	for i, l := range doc.Links {
		if _, err := tx.Exec(ctx,
			`INSERT INTO links (src_note_id, kind, target, target_path, anchor, alias, ord) VALUES (?,?,?,?,?,?,?)`,
			noteID, string(l.Kind), l.Target, "", l.Anchor, l.Alias, i); err != nil {
			return fmt.Errorf("index link %q: %w", l.Target, err)
		}
	}
	return nil
}

// searchablePath makes a path match on its own words, so "path:Daily" and a
// bare search for a folder name both work.
func searchablePath(p string) string {
	return strings.NewReplacer("/", " ", "-", " ", "_", " ").Replace(p) + " " + p
}

// resolveLinks recomputes target_path for every link in the vault that does not
// have one yet, and is the mechanism by which a link you wrote before the note
// existed quietly starts working once you create it.
func resolveLinks(ctx context.Context, tx *store.Tx, vaultID int64, v *vault.Vault) error {
	type row struct {
		Rowid  int64  `db:"rowid"`
		Src    string `db:"src"`
		Target string `db:"target"`
	}
	unresolved, err := tx.All[row](ctx, `
		SELECT l.rowid AS rowid, n.path AS src, l.target AS target
		FROM links l JOIN notes n ON n.id = l.src_note_id
		WHERE n.vault_id = ? AND l.target_path = ''`, vaultID)
	if err != nil {
		return fmt.Errorf("load unresolved links: %w", err)
	}
	if len(unresolved) == 0 {
		return nil
	}

	// Resolution needs every path in the vault, notes and attachments alike,
	// since ![[diagram.png]] is a link too.
	entries, err := v.List()
	if err != nil {
		return fmt.Errorf("list vault: %w", err)
	}
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}

	for _, r := range unresolved {
		resolved := markdown.ResolveWikilink(paths, r.Src, r.Target)
		if resolved == "" {
			continue // still dangling; it will be retried next time
		}
		if _, err := tx.Exec(ctx, `UPDATE links SET target_path = ? WHERE rowid = ?`, resolved, r.Rowid); err != nil {
			return fmt.Errorf("resolve link %q: %w", r.Target, err)
		}
	}
	return nil
}

// Remove drops a note and everything derived from it.
func (ix *Index) Remove(ctx context.Context, vaultID int64, path string) error {
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return err
	}
	// The notes_fts row goes with it via the AFTER DELETE trigger; tags and
	// links go via ON DELETE CASCADE.
	if _, err := ix.db.Exec(ctx, `DELETE FROM notes WHERE vault_id = ? AND path = ?`, vaultID, clean); err != nil {
		return fmt.Errorf("remove %s: %w", clean, err)
	}
	return nil
}

// Get returns one indexed note.
func (ix *Index) Get(ctx context.Context, vaultID int64, path string) (Entry, error) {
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return Entry{}, err
	}
	rows, err := ix.entries(ctx, `WHERE n.vault_id = ? AND n.path = ?`, []any{vaultID, clean}, "", 1, 0)
	if err != nil {
		return Entry{}, err
	}
	if len(rows) == 0 {
		return Entry{}, fmt.Errorf("%s: %w", clean, ErrNotFound)
	}
	return rows[0], nil
}

// noteRow mirrors the notes table for scanning.
type noteRow struct {
	ID          int64  `db:"id"`
	VaultID     int64  `db:"vault_id"`
	Path        string `db:"path"`
	NoteUUID    string `db:"note_uuid"`
	Title       string `db:"title"`
	SHA256      string `db:"sha256"`
	Size        int64  `db:"size"`
	MTime       int64  `db:"mtime"`
	CreatedAt   int64  `db:"created_at"`
	UpdatedAt   int64  `db:"updated_at"`
	Frontmatter string `db:"frontmatter"`
}

func (r noteRow) entry() Entry {
	return Entry{
		ID: r.ID, VaultID: r.VaultID, Path: r.Path, UUID: r.NoteUUID, Title: r.Title,
		SHA256: r.SHA256, Size: r.Size,
		ModTime:     time.Unix(r.MTime, 0),
		CreatedAt:   time.Unix(r.CreatedAt, 0),
		UpdatedAt:   time.Unix(r.UpdatedAt, 0),
		Frontmatter: r.Frontmatter,
	}
}

const noteColumns = `n.id, n.vault_id, n.path, n.note_uuid, n.title, n.sha256, n.size, n.mtime, n.created_at, n.updated_at, n.frontmatter`

// entries runs a notes query and attaches each note's tags.
func (ix *Index) entries(ctx context.Context, where string, args []any, orderBy string, limit, offset int) ([]Entry, error) {
	q := `SELECT ` + noteColumns + ` FROM notes n ` + where
	if orderBy != "" {
		q += ` ORDER BY ` + orderBy
	}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
	}
	rows, err := ix.db.All[noteRow](ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query notes: %w", err)
	}
	out := make([]Entry, len(rows))
	ids := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.entry()
		ids[i] = r.ID
	}
	tags, err := ix.tagsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Tags = tags[out[i].ID]
	}
	return out, nil
}

// tagsFor loads tags for a batch of notes in one query rather than N.
func (ix *Index) tagsFor(ctx context.Context, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	type row struct {
		NoteID int64  `db:"note_id"`
		Tag    string `db:"tag"`
	}
	ph, args := placeholders(ids)
	rows, err := ix.db.All[row](ctx,
		`SELECT note_id, tag FROM tags WHERE note_id IN (`+ph+`) ORDER BY tag`, args...)
	if err != nil {
		return nil, fmt.Errorf("load tags: %w", err)
	}
	for _, r := range rows {
		out[r.NoteID] = append(out[r.NoteID], r.Tag)
	}
	return out, nil
}

// placeholders builds "?,?,?" and the matching args for an IN clause.
func placeholders[T any](vals []T) (string, []any) {
	args := make([]any, len(vals))
	for i, v := range vals {
		args[i] = v
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(vals)), ","), args
}

// List browses notes by folder, tag, or recency, newest first.
func (ix *Index) List(ctx context.Context, req ListRequest) ([]Entry, error) {
	if len(req.VaultIDs) == 0 {
		return nil, nil
	}
	ph, args := placeholders(req.VaultIDs)
	where := `WHERE n.vault_id IN (` + ph + `)`

	if req.Folder != "" {
		// Match the folder itself and its descendants, never a sibling that
		// merely shares a prefix ("Projects" must not match "Projects2").
		where += ` AND (n.path LIKE ? ESCAPE '\')`
		args = append(args, likePrefix(strings.TrimSuffix(req.Folder, "/")+"/"))
	}
	if req.Tag != "" {
		where += ` AND EXISTS (SELECT 1 FROM tags t WHERE t.note_id = n.id AND t.tag = ? COLLATE NOCASE)`
		args = append(args, req.Tag)
	}
	if !req.Since.IsZero() {
		where += ` AND n.updated_at >= ?`
		args = append(args, req.Since.Unix())
	}
	return ix.entries(ctx, where, args, `n.updated_at DESC, n.path ASC`, limitOr(req.Limit, 100), req.Offset)
}

// likePrefix escapes LIKE metacharacters so a folder called "100%" behaves.
func likePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s) + "%"
}

func limitOr(limit, def int) int {
	if limit <= 0 {
		return def
	}
	return min(limit, 500)
}

// Search runs a full-text query scoped to the caller's readable vaults.
//
// An empty query is not an error: it lists recent notes, which is what a search
// box should show before you type anything.
func (ix *Index) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	if len(req.VaultIDs) == 0 {
		return SearchResponse{}, nil
	}
	limit := limitOr(req.Limit, 50)

	if req.Query.Match == "" {
		return ix.searchWithoutMatch(ctx, req, limit)
	}
	return ix.searchWithMatch(ctx, req, limit)
}

// searchWithMatch is the real FTS5 path: rank by bm25 and pull a snippet.
func (ix *Index) searchWithMatch(ctx context.Context, req SearchRequest, limit int) (SearchResponse, error) {
	vaultPh, args := placeholders(req.VaultIDs)
	args = append([]any{req.Query.Match}, args...)

	exclude := ""
	if req.Query.Exclude != "" {
		exclude = ` AND n.id NOT IN (SELECT rowid FROM notes_fts WHERE notes_fts MATCH ?)`
		args = append(args, req.Query.Exclude)
	}

	type hitRow struct {
		noteRow
		Snippet string  `db:"snippet"`
		Score   float64 `db:"score"`
	}
	q := `
		SELECT ` + noteColumns + `,
		       snippet(notes_fts, 1, ?, ?, ?, 24) AS snippet,
		       bm25(notes_fts, ` + bm25Weights + `) AS score
		FROM notes_fts
		JOIN notes n ON n.id = notes_fts.rowid
		WHERE notes_fts MATCH ? AND n.vault_id IN (` + vaultPh + `)` + exclude + `
		ORDER BY score ASC, n.updated_at DESC
		LIMIT ? OFFSET ?`

	// snippet()'s markers come first in the argument order.
	full := append([]any{HighlightOpen, HighlightClose, snippetEllipsis}, args...)
	full = append(full, limit+1, req.Offset)

	rows, err := ix.db.All[hitRow](ctx, q, full...)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("search: %w", err)
	}
	return ix.assemble(ctx, rows, limit, func(r hitRow) (Entry, string, float64) {
		// bm25 returns a negative number, more negative meaning a better match.
		// Flip it so a bigger Score is a better hit, which is what any caller
		// will assume.
		return r.noteRow.entry(), r.Snippet, -r.Score
	})
}

// searchWithoutMatch handles the empty query and the exclusion-only query, where
// there is nothing for FTS5 to rank.
func (ix *Index) searchWithoutMatch(ctx context.Context, req SearchRequest, limit int) (SearchResponse, error) {
	vaultPh, args := placeholders(req.VaultIDs)
	exclude := ""
	if req.Query.Exclude != "" {
		exclude = ` AND n.id NOT IN (SELECT rowid FROM notes_fts WHERE notes_fts MATCH ?)`
		args = append(args, req.Query.Exclude)
	}

	type hitRow struct {
		noteRow
		Snippet string `db:"snippet"`
	}
	q := `
		SELECT ` + noteColumns + `, substr(f.body, 1, 200) AS snippet
		FROM notes n JOIN notes_fts f ON f.rowid = n.id
		WHERE n.vault_id IN (` + vaultPh + `)` + exclude + `
		ORDER BY n.updated_at DESC, n.path ASC
		LIMIT ? OFFSET ?`
	args = append(args, limit+1, req.Offset)

	rows, err := ix.db.All[hitRow](ctx, q, args...)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("list recent: %w", err)
	}
	return ix.assemble(ctx, rows, limit, func(r hitRow) (Entry, string, float64) {
		return r.noteRow.entry(), r.Snippet, 0
	})
}

// assemble trims the extra row used for HasMore detection and attaches tags.
func (ix *Index) assemble[R any](ctx context.Context, rows []R, limit int, split func(R) (Entry, string, float64)) (SearchResponse, error) {
	resp := SearchResponse{}
	if len(rows) > limit {
		resp.HasMore = true
		rows = rows[:limit]
	}

	ids := make([]int64, len(rows))
	resp.Hits = make([]Hit, len(rows))
	for i, r := range rows {
		e, snip, score := split(r)
		resp.Hits[i] = Hit{Entry: e, Snippet: snip, Score: score}
		ids[i] = e.ID
	}
	tags, err := ix.tagsFor(ctx, ids)
	if err != nil {
		return SearchResponse{}, err
	}
	for i := range resp.Hits {
		resp.Hits[i].Tags = tags[resp.Hits[i].ID]
	}
	return resp, nil
}

// Backlinks returns the notes pointing at path.
func (ix *Index) Backlinks(ctx context.Context, vaultID int64, path string) ([]Backlink, error) {
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return nil, err
	}
	return ix.db.All[Backlink](ctx, `
		SELECT src.id AS source_id, src.path AS source_path, src.title AS source_title,
		       l.kind AS kind, l.alias AS alias, l.anchor AS anchor
		FROM links l
		JOIN notes src ON src.id = l.src_note_id
		WHERE src.vault_id = ? AND l.target_path = ?
		ORDER BY src.path, l.ord`, vaultID, clean)
}

// DanglingLinks lists wikilinks whose target does not exist. Useful in the UI
// and worth surfacing: a dangling link is usually a note you meant to write.
func (ix *Index) DanglingLinks(ctx context.Context, vaultID int64) ([]DanglingLink, error) {
	return ix.db.All[DanglingLink](ctx, `
		SELECT src.id AS source_id, src.path AS source_path, l.target AS target
		FROM links l
		JOIN notes src ON src.id = l.src_note_id
		WHERE src.vault_id = ? AND l.target_path = ''
		ORDER BY src.path, l.ord`, vaultID)
}

// Tags returns every tag across the given vaults, most used first.
func (ix *Index) Tags(ctx context.Context, vaultIDs []int64) ([]TagCount, error) {
	if len(vaultIDs) == 0 {
		return nil, nil
	}
	ph, args := placeholders(vaultIDs)
	return ix.db.All[TagCount](ctx, `
		SELECT t.tag AS tag, count(*) AS count
		FROM tags t JOIN notes n ON n.id = t.note_id
		WHERE n.vault_id IN (`+ph+`)
		GROUP BY t.tag
		ORDER BY count DESC, t.tag ASC`, args...)
}

// Folders lists every folder that contains at least one note.
func (ix *Index) Folders(ctx context.Context, vaultIDs []int64) ([]string, error) {
	if len(vaultIDs) == 0 {
		return nil, nil
	}
	ph, args := placeholders(vaultIDs)
	paths, err := ix.db.All[string](ctx,
		`SELECT DISTINCT path FROM notes WHERE vault_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		for d := vaultpath.Dir(p); d != ""; d = vaultpath.Dir(d) {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// VaultStats summarizes one vault.
func (ix *Index) VaultStats(ctx context.Context, vaultID int64) (VaultStat, error) {
	return ix.db.One[VaultStat](ctx, `
		SELECT (SELECT count(*) FROM notes WHERE vault_id = ?1)                                     AS notes,
		       (SELECT count(DISTINCT t.tag) FROM tags t
		          JOIN notes n ON n.id = t.note_id WHERE n.vault_id = ?1)                           AS tags,
		       (SELECT count(*) FROM links l
		          JOIN notes n ON n.id = l.src_note_id WHERE n.vault_id = ?1)                       AS links`, vaultID)
}

// Rename moves a note on disk, updates the index, and rewrites every inbound
// wikilink so the rest of the vault keeps pointing at it.
//
// Rewriting other people's files is a big hammer, but the alternative is worse:
// a rename that silently breaks every link into the note.
func (ix *Index) Rename(ctx context.Context, vaultID int64, v *vault.Vault, from, to string) error {
	src, err := vaultpath.Clean(from)
	if err != nil {
		return err
	}
	dst, err := vaultpath.Clean(to)
	if err != nil {
		return err
	}
	if src == dst {
		return nil
	}

	inbound, err := ix.Backlinks(ctx, vaultID, src)
	if err != nil {
		return err
	}
	if err := v.Move(src, dst); err != nil {
		return err
	}

	if _, err := ix.db.Exec(ctx,
		`UPDATE notes SET path = ?, updated_at = ? WHERE vault_id = ? AND path = ?`,
		dst, time.Now().Unix(), vaultID, src); err != nil {
		return fmt.Errorf("reindex moved note: %w", err)
	}
	if _, err := ix.db.Exec(ctx, `
		UPDATE links SET target_path = ?
		WHERE target_path = ? AND src_note_id IN (SELECT id FROM notes WHERE vault_id = ?)`,
		dst, src, vaultID); err != nil {
		return fmt.Errorf("repoint links: %w", err)
	}

	// Re-read the moved note so its own FTS path column is right.
	if content, n, err := v.Read(dst); err == nil {
		if err := ix.Put(ctx, vaultID, v, n, content); err != nil && !isParseError(err) {
			return err
		}
	}

	for _, bl := range dedupeBySource(inbound) {
		if err := ix.rewriteLinksIn(ctx, vaultID, v, bl.SourcePath, src, dst); err != nil {
			return err
		}
	}
	return nil
}

// dedupeBySource collapses multiple backlinks from the same note, since we
// rewrite that file once and handle all of its links together.
func dedupeBySource(in []Backlink) []Backlink {
	seen := map[string]bool{}
	var out []Backlink
	for _, bl := range in {
		if !seen[bl.SourcePath] {
			seen[bl.SourcePath] = true
			out = append(out, bl)
		}
	}
	return out
}

// rewriteLinksIn updates one file's wikilinks from oldPath to newPath, keeping
// whatever alias and anchor the author wrote.
func (ix *Index) rewriteLinksIn(ctx context.Context, vaultID int64, v *vault.Vault, notePath, oldPath, newPath string) error {
	content, n, err := v.Read(notePath)
	if err != nil {
		return fmt.Errorf("read %s to rewrite links: %w", notePath, err)
	}
	updated := markdown.RewriteWikilinks(content, notePath, oldPath, newPath)
	if string(updated) == string(content) {
		return nil
	}
	written, err := v.Write(notePath, updated, n.SHA256)
	if err != nil {
		return fmt.Errorf("rewrite links in %s: %w", notePath, err)
	}
	if err := ix.Put(ctx, vaultID, v, written, updated); err != nil && !isParseError(err) {
		return err
	}
	return nil
}

// isParseError reports whether err is only a frontmatter complaint, which never
// means the write or index failed.
func isParseError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "frontmatter")
}

// Sync brings the index in line with what is actually on disk, adding new files,
// re-indexing changed ones, and dropping notes whose files are gone.
//
// Change detection is by content hash, not mtime: a sync tool that rewrites a
// file with identical bytes must not cost us a reindex, and one that preserves
// mtime while changing content must not be missed.
func (ix *Index) Sync(ctx context.Context, vaultID int64, v *vault.Vault) (Stats, error) {
	var stats Stats

	onDisk, err := v.ListNotes()
	if err != nil {
		return stats, fmt.Errorf("list vault: %w", err)
	}
	type known struct {
		Path   string `db:"path"`
		SHA256 string `db:"sha256"`
	}
	indexed, err := ix.db.All[known](ctx, `SELECT path, sha256 FROM notes WHERE vault_id = ?`, vaultID)
	if err != nil {
		return stats, fmt.Errorf("load index: %w", err)
	}
	have := make(map[string]string, len(indexed))
	for _, k := range indexed {
		have[k.Path] = k.SHA256
	}

	for _, p := range onDisk {
		content, n, err := v.Read(p)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		prev, existed := have[p]
		delete(have, p)

		if existed && prev == n.SHA256 {
			stats.Unchanged++
			continue
		}
		if err := ix.Put(ctx, vaultID, v, n, content); err != nil {
			if !isParseError(err) {
				stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", p, err))
				continue
			}
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", p, err))
		}
		if existed {
			stats.Updated++
		} else {
			stats.Added++
		}
	}

	// Whatever is left in have no longer exists on disk.
	for p := range have {
		if err := ix.Remove(ctx, vaultID, p); err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		stats.Removed++
	}
	return stats, nil
}

// SyncPaths reconciles just the named paths, which is what the file watcher
// feeds it after a burst of filesystem events settles.
//
// It compares content hashes before doing any work. That is what stops the
// obvious feedback loop: tsnotes writes a note, fsnotify reports the write,
// the watcher asks for a sync, and without the hash check we would reindex a
// note whose index entry is already correct.
func (ix *Index) SyncPaths(ctx context.Context, vaultID int64, v *vault.Vault, paths []string) (Stats, error) {
	var stats Stats
	for _, p := range paths {
		clean, err := vaultpath.Clean(p)
		if err != nil || !vaultpath.IsMarkdown(clean) || vaultpath.IsHidden(clean) {
			continue
		}

		prev, err := ix.db.One[string](ctx, `SELECT sha256 FROM notes WHERE vault_id = ? AND path = ?`, vaultID, clean)
		indexed := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", clean, err))
			continue
		}

		content, n, readErr := v.Read(clean)
		if readErr != nil {
			if errors.Is(readErr, vault.ErrNotFound) {
				if indexed {
					if err := ix.Remove(ctx, vaultID, clean); err != nil {
						stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", clean, err))
						continue
					}
					stats.Removed++
				}
				continue
			}
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", clean, readErr))
			continue
		}

		if indexed && prev == n.SHA256 {
			stats.Unchanged++
			continue
		}
		if err := ix.Put(ctx, vaultID, v, n, content); err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", clean, err))
			if !isParseError(err) {
				continue
			}
		}
		if indexed {
			stats.Updated++
		} else {
			stats.Added++
		}
	}
	return stats, nil
}

// Rebuild discards the derived rows for a vault and indexes every file from
// scratch. It never touches users, vaults, or shares, which is what makes it the
// supported recovery path instead of deleting the database.
func (ix *Index) Rebuild(ctx context.Context, vaultID int64, v *vault.Vault) (Stats, error) {
	if _, err := ix.db.Exec(ctx, `DELETE FROM notes WHERE vault_id = ?`, vaultID); err != nil {
		return Stats{}, fmt.Errorf("clear derived index: %w", err)
	}
	return ix.Sync(ctx, vaultID, v)
}
