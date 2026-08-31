// Package notes is the operations layer: what it means to read, create, edit,
// move, or search a note, independent of how the request arrived.
//
// It exists because tsnotes has two front ends. The web API and the MCP server
// must agree exactly on permissions, conflict handling, link rewriting, and
// which events get published, and the only reliable way to guarantee that is to
// have one implementation. A handler's job is to translate HTTP or MCP into a
// call here and translate the result back.
package notes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/scottjab/tsnotes/internal/events"
	"github.com/scottjab/tsnotes/internal/identity"
	"github.com/scottjab/tsnotes/internal/index"
	"github.com/scottjab/tsnotes/internal/markdown"
	"github.com/scottjab/tsnotes/internal/share"
	"github.com/scottjab/tsnotes/internal/store"
	"github.com/scottjab/tsnotes/internal/vault"
	"github.com/scottjab/tsnotes/internal/vaultpath"
)

// ErrNoMatch means an edit's search text was not found.
var ErrNoMatch = errors.New("text to replace was not found")

// ErrAmbiguous means an edit's search text appears more than once, so applying
// it would be a guess.
var ErrAmbiguous = errors.New("text to replace appears more than once")

// Deps are the collaborators the service needs.
type Deps struct {
	DB       *store.DB
	Index    *index.Index
	Vaults   *vault.Set
	Identity *identity.Resolver
	Shares   *share.Resolver
	Bus      *events.Bus
}

// Service performs note operations on behalf of a user.
type Service struct {
	Deps
}

// New returns a Service.
func New(d Deps) *Service { return &Service{Deps: d} }

// Scope is a resolved "which vault, whose, and opened" answer. Handlers build
// one per request and pass it to every call, so the vault lookup and the
// ownership question are answered once.
type Scope struct {
	User    identity.User
	Owner   identity.User
	Vault   *vault.Vault
	VaultID int64
	Dir     string
}

// IsMine reports whether the scope is the caller's own vault.
func (s Scope) IsMine() bool { return s.User.ID == s.Owner.ID }

// Note is a note and its content.
type Note struct {
	Vault      string
	OwnerLogin string
	Path       string
	Content    string
	SHA256     string
	Size       int64
	ModTime    time.Time
	Title      string
	Tags       []string
	Perm       share.Perm
}

// Summary is a note without its content, for listings.
type Summary struct {
	Vault      string
	OwnerLogin string
	Path       string
	Title      string
	Tags       []string
	SHA256     string
	UpdatedAt  time.Time
}

// Hit is a search result.
type Hit struct {
	Summary
	Snippet string
	Score   float64
}

// Link is an inbound or outbound reference.
type Link struct {
	Path   string
	Title  string
	Kind   string
	Alias  string
	Anchor string
}

// Edit is one find-and-replace within a note.
//
// It exists for agents. Asking a model to rewrite a whole note to change one
// line wastes tokens and risks it quietly dropping the rest, so MCP callers send
// targeted edits instead.
type Edit struct {
	// Old is the exact text to replace. It must appear exactly once unless
	// ReplaceAll is set, so an edit can never land somewhere unintended.
	Old string
	New string
	// ReplaceAll permits multiple matches.
	ReplaceAll bool
}

// Scope resolves a vault name for a user. An empty name or "me" means their own.
func (s *Service) Scope(ctx context.Context, u identity.User, vaultName string) (Scope, error) {
	if vaultName == "" || vaultName == "me" {
		vaultName = u.VaultDir
	}

	owner := u
	if vaultName != u.VaultDir {
		id, err := s.DB.One[int64](ctx, `SELECT id FROM vaults WHERE dir = ?`, vaultName)
		if err != nil {
			// Reported as denied rather than not-found, so probing this cannot
			// be used to enumerate who is on the tailnet.
			return Scope{}, fmt.Errorf("%w: no vault %q", share.ErrDenied, vaultName)
		}
		owner, err = s.Identity.ByVaultID(ctx, id)
		if err != nil {
			return Scope{}, fmt.Errorf("%w: no vault %q", share.ErrDenied, vaultName)
		}
	}

	v, err := s.Vaults.Get(owner.VaultDir)
	if err != nil {
		return Scope{}, fmt.Errorf("open vault %s: %w", owner.VaultDir, err)
	}
	return Scope{User: u, Owner: owner, Vault: v, VaultID: owner.VaultID, Dir: owner.VaultDir}, nil
}

// Read returns a note's content and metadata.
func (s *Service) Read(ctx context.Context, sc Scope, path string) (Note, error) {
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return Note{}, err
	}
	if err := s.Shares.Check(ctx, sc.User, sc.VaultID, clean, share.Read); err != nil {
		return Note{}, err
	}
	content, n, err := sc.Vault.Read(clean)
	if err != nil {
		return Note{}, err
	}

	note := Note{
		Vault: sc.Dir, OwnerLogin: sc.Owner.Login, Path: n.Path, Content: string(content),
		SHA256: n.SHA256, Size: n.Size, ModTime: n.ModTime, Tags: []string{},
		Title: vaultpath.TitleFor(n.Path),
	}
	if e, err := s.Index.Get(ctx, sc.VaultID, clean); err == nil {
		note.Title, note.Tags = e.Title, orEmpty(e.Tags)
	}
	if perm, err := s.Shares.PermFor(ctx, sc.User, sc.VaultID, clean); err == nil {
		note.Perm = perm
	}
	return note, nil
}

// Create writes a new note, refusing to overwrite an existing one.
//
// The note is stamped with a uuid in its frontmatter. That id is what lets an
// agent or a share keep referring to the same note after it has been renamed.
func (s *Service) Create(ctx context.Context, sc Scope, path, content string) (Note, error) {
	clean, err := vaultpath.Clean(vaultpath.EnsureMarkdown(path))
	if err != nil {
		return Note{}, err
	}
	if err := s.Shares.Check(ctx, sc.User, sc.VaultID, clean, share.Write); err != nil {
		return Note{}, err
	}

	body := markdown.EnsureFrontmatterID([]byte(content), uuid.NewV7().String())
	n, err := sc.Vault.Create(clean, body)
	if err != nil {
		return Note{}, err
	}
	if err := s.reindex(ctx, sc, n, body); err != nil {
		return Note{}, err
	}
	s.publish(ctx, sc, n, events.NoteCreated, "")
	return s.Read(ctx, sc, clean)
}

// Update replaces a note's content.
//
// A non-empty baseSHA makes the write conditional: if the note changed
// underneath the caller, the write is refused with a *vault.ConflictError and
// the caller's content is parked in a sibling file rather than discarded.
func (s *Service) Update(ctx context.Context, sc Scope, path, content, baseSHA string) (Note, error) {
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return Note{}, err
	}
	if err := s.Shares.Check(ctx, sc.User, sc.VaultID, clean, share.Write); err != nil {
		return Note{}, err
	}
	n, err := sc.Vault.Write(clean, []byte(content), baseSHA)
	if err != nil {
		return Note{}, err
	}
	if err := s.reindex(ctx, sc, n, []byte(content)); err != nil {
		return Note{}, err
	}
	s.publish(ctx, sc, n, events.NoteUpdated, "")
	return s.Read(ctx, sc, clean)
}

// Edit applies find-and-replace edits to a note.
//
// Every edit is applied against the content read at the start, and the whole set
// is written back under compare-and-swap. Either all of them land or none do; a
// half-applied set of edits is worse than a rejected one.
func (s *Service) Edit(ctx context.Context, sc Scope, path string, edits []Edit) (Note, error) {
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return Note{}, err
	}
	if err := s.Shares.Check(ctx, sc.User, sc.VaultID, clean, share.Write); err != nil {
		return Note{}, err
	}
	content, n, err := sc.Vault.Read(clean)
	if err != nil {
		return Note{}, err
	}

	updated := string(content)
	for i, e := range edits {
		if e.Old == "" {
			return Note{}, fmt.Errorf("edit %d: the text to replace must not be empty", i)
		}
		count := strings.Count(updated, e.Old)
		switch {
		case count == 0:
			return Note{}, fmt.Errorf("edit %d: %w: %q", i, ErrNoMatch, truncate(e.Old, 60))
		case count > 1 && !e.ReplaceAll:
			return Note{}, fmt.Errorf("edit %d: %w (%d times): %q; add more surrounding context or set replaceAll",
				i, ErrAmbiguous, count, truncate(e.Old, 60))
		}
		if e.ReplaceAll {
			updated = strings.ReplaceAll(updated, e.Old, e.New)
		} else {
			updated = strings.Replace(updated, e.Old, e.New, 1)
		}
	}

	return s.Update(ctx, sc, clean, updated, n.SHA256)
}

// Append adds text to a note, either at the end or under a named heading.
//
// Appending under a heading is what makes "add this to my daily note's Tasks
// section" work without an agent having to rewrite the file.
func (s *Service) Append(ctx context.Context, sc Scope, path, text, underHeading string) (Note, error) {
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return Note{}, err
	}
	if err := s.Shares.Check(ctx, sc.User, sc.VaultID, clean, share.Write); err != nil {
		return Note{}, err
	}
	content, n, err := sc.Vault.Read(clean)
	if err != nil {
		return Note{}, err
	}

	updated, err := appendToNote(string(content), text, underHeading)
	if err != nil {
		return Note{}, err
	}
	return s.Update(ctx, sc, clean, updated, n.SHA256)
}

// Delete moves a note to the vault's trash.
func (s *Service) Delete(ctx context.Context, sc Scope, path string) error {
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return err
	}
	if err := s.Shares.Check(ctx, sc.User, sc.VaultID, clean, share.Write); err != nil {
		return err
	}
	if err := sc.Vault.Delete(clean); err != nil {
		return err
	}
	if err := s.Index.Remove(ctx, sc.VaultID, clean); err != nil {
		return err
	}
	s.publish(ctx, sc, vault.Note{Path: clean}, events.NoteDeleted, "")
	return nil
}

// Move renames a note and rewrites every wikilink that named it.
func (s *Service) Move(ctx context.Context, sc Scope, from, to string) (Note, error) {
	src, err := vaultpath.Clean(from)
	if err != nil {
		return Note{}, err
	}
	dst, err := vaultpath.Clean(vaultpath.EnsureMarkdown(to))
	if err != nil {
		return Note{}, err
	}
	// A move is a delete and a create, so both ends need write access.
	for _, p := range []string{src, dst} {
		if err := s.Shares.Check(ctx, sc.User, sc.VaultID, p, share.Write); err != nil {
			return Note{}, err
		}
	}
	if err := s.Index.Rename(ctx, sc.VaultID, sc.Vault, src, dst); err != nil {
		return Note{}, err
	}
	if n, err := sc.Vault.Stat(dst); err == nil {
		s.publish(ctx, sc, n, events.NoteMoved, src)
	}
	return s.Read(ctx, sc, dst)
}

// List browses a vault.
type ListOptions struct {
	Folder string
	Tag    string
	Since  time.Time
	Limit  int
	Offset int
}

// List returns notes in a vault the caller may read.
func (s *Service) List(ctx context.Context, sc Scope, opts ListOptions) ([]Summary, error) {
	// Listing reveals a vault's shape, so it needs read access at the root,
	// which only the owner or a root-folder grantee has.
	if err := s.Shares.Check(ctx, sc.User, sc.VaultID, ".", share.Read); err != nil {
		return nil, fmt.Errorf("%w: cannot list %s", share.ErrDenied, sc.Dir)
	}
	entries, err := s.Index.List(ctx, index.ListRequest{
		VaultIDs: []int64{sc.VaultID},
		Folder:   opts.Folder,
		Tag:      opts.Tag,
		Since:    opts.Since,
		Limit:    opts.Limit,
		Offset:   opts.Offset,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(entries))
	for _, e := range entries {
		out = append(out, Summary{
			Vault: sc.Dir, OwnerLogin: sc.Owner.Login, Path: e.Path, Title: e.Title,
			Tags: orEmpty(e.Tags), SHA256: e.SHA256, UpdatedAt: e.UpdatedAt,
		})
	}
	return out, nil
}

// Search runs a query across every vault the user can read.
//
// Results are filtered twice: the index is scoped to readable vault ids, and
// then each hit is checked per path, because a share may cover only part of a
// vault. Skipping the second would leak titles and snippets.
func (s *Service) Search(ctx context.Context, u identity.User, query string, limit, offset int) ([]Hit, bool, error) {
	vaultIDs, err := s.Shares.ReadableVaults(ctx, u)
	if err != nil {
		return nil, false, err
	}
	if limit <= 0 {
		limit = 50
	}

	res, err := s.Index.Search(ctx, index.SearchRequest{
		Query:    index.ParseQuery(query),
		VaultIDs: vaultIDs,
		Limit:    limit * 2, // over-fetch; the per-path filter below may drop some
		Offset:   offset,
	})
	if err != nil {
		return nil, false, err
	}

	owners := map[int64]identity.User{}
	hits := make([]Hit, 0, min(len(res.Hits), limit))
	for _, h := range res.Hits {
		if len(hits) >= limit {
			return hits, true, nil
		}
		owner, ok := owners[h.VaultID]
		if !ok {
			owner, err = s.Identity.ByVaultID(ctx, h.VaultID)
			if err != nil {
				continue
			}
			owners[h.VaultID] = owner
		}
		if err := s.Shares.Check(ctx, u, h.VaultID, h.Path, share.Read); err != nil {
			continue
		}
		hits = append(hits, Hit{
			Summary: Summary{
				Vault: owner.VaultDir, OwnerLogin: owner.Login, Path: h.Path, Title: h.Title,
				Tags: orEmpty(h.Tags), SHA256: h.SHA256, UpdatedAt: h.UpdatedAt,
			},
			Snippet: h.Snippet,
			Score:   h.Score,
		})
	}
	return hits, res.HasMore, nil
}

// Backlinks returns inbound links the caller may see.
func (s *Service) Backlinks(ctx context.Context, sc Scope, path string) ([]Link, error) {
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return nil, err
	}
	if err := s.Shares.Check(ctx, sc.User, sc.VaultID, clean, share.Read); err != nil {
		return nil, err
	}
	links, err := s.Index.Backlinks(ctx, sc.VaultID, clean)
	if err != nil {
		return nil, err
	}

	paths := make([]string, len(links))
	for i, l := range links {
		paths[i] = l.SourcePath
	}
	// A backlink from a note the caller cannot read would leak its existence and
	// its title, so filter before returning.
	visible, err := s.Shares.FilterReadable(ctx, sc.User, sc.VaultID, paths)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(visible))
	for _, p := range visible {
		allowed[p] = true
	}

	out := make([]Link, 0, len(links))
	for _, l := range links {
		if !allowed[l.SourcePath] {
			continue
		}
		out = append(out, Link{Path: l.SourcePath, Title: l.SourceTitle, Kind: l.Kind, Alias: l.Alias, Anchor: l.Anchor})
	}
	return out, nil
}

// DailyNotePath is where a daily note for a given date lives.
func DailyNotePath(day time.Time) string {
	return "Daily/" + day.Format("2006-01-02") + ".md"
}

// DailyNote returns the note for a date, creating it from a minimal template if
// it does not exist and create is set.
func (s *Service) DailyNote(ctx context.Context, sc Scope, day time.Time, create bool) (Note, error) {
	path := DailyNotePath(day)
	note, err := s.Read(ctx, sc, path)
	if err == nil {
		return note, nil
	}
	if !create || !errors.Is(err, vault.ErrNotFound) {
		return Note{}, err
	}
	template := fmt.Sprintf("---\ntags: [daily]\n---\n# %s\n\n", day.Format("Monday, 2 January 2006"))
	return s.Create(ctx, sc, path, template)
}

// reindex updates the search index, tolerating a frontmatter complaint. A note
// with broken YAML is still a note and should still be findable.
func (s *Service) reindex(ctx context.Context, sc Scope, n vault.Note, content []byte) error {
	err := s.Index.Put(ctx, sc.VaultID, sc.Vault, n, content)
	if err == nil || strings.Contains(err.Error(), "frontmatter") {
		return nil
	}
	return err
}

// publish announces a change to the SSE stream and any MCP subscribers.
func (s *Service) publish(ctx context.Context, sc Scope, n vault.Note, kind, oldPath string) {
	if s.Bus == nil {
		return
	}
	s.Bus.Emit(ctx, events.NoteChanged{
		ID: uuid.NewV7().String(), Kind: kind,
		VaultID: sc.VaultID, Vault: sc.Dir, Path: n.Path, OldPath: oldPath,
		SHA256: n.SHA256, ByLogin: sc.User.Login, At: time.Now(),
	})
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
