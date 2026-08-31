package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/scottjab/tsnotes/internal/notes"
	"github.com/scottjab/tsnotes/internal/share"
	"github.com/scottjab/tsnotes/internal/vaultpath"
)

// scope resolves the {vault} path segment for the authenticated caller.
func (a *API) scope(r *http.Request) (notes.Scope, error) {
	return a.Notes.Scope(r.Context(), userFrom(r.Context()), r.PathValue("vault"))
}

// noteResponse is one note as the editor receives it.
type noteResponse struct {
	Vault      string     `json:"vault"`
	OwnerLogin string     `json:"ownerLogin"`
	Path       string     `json:"path"`
	Content    string     `json:"content"`
	SHA256     string     `json:"sha256"`
	Size       int64      `json:"size"`
	ModTime    time.Time  `json:"modTime"`
	Title      string     `json:"title"`
	Tags       []string   `json:"tags"`
	Perm       share.Perm `json:"perm"`
	Backlinks  []linkJSON `json:"backlinks"`
}

type linkJSON struct {
	Path   string `json:"path"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Alias  string `json:"alias,omitzero"`
	Anchor string `json:"anchor,omitzero"`
}

type noteSummary struct {
	Vault      string    `json:"vault"`
	OwnerLogin string    `json:"ownerLogin"`
	Path       string    `json:"path"`
	Title      string    `json:"title"`
	Tags       []string  `json:"tags"`
	SHA256     string    `json:"sha256"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func summaryJSON(s notes.Summary) noteSummary {
	return noteSummary{
		Vault: s.Vault, OwnerLogin: s.OwnerLogin, Path: s.Path, Title: s.Title,
		Tags: orEmpty(s.Tags), SHA256: s.SHA256, UpdatedAt: s.UpdatedAt,
	}
}

func linksJSON(in []notes.Link) []linkJSON {
	out := make([]linkJSON, 0, len(in))
	for _, l := range in {
		out = append(out, linkJSON{Path: l.Path, Title: l.Title, Kind: l.Kind, Alias: l.Alias, Anchor: l.Anchor})
	}
	return out
}

func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	a.JSON(w, http.StatusOK, struct {
		Login       string `json:"login"`
		DisplayName string `json:"displayName"`
		ProfilePic  string `json:"profilePic,omitzero"`
		Vault       string `json:"vault"`
		IsAgent     bool   `json:"isAgent"`
	}{u.Login, u.DisplayName, u.ProfilePic, u.VaultDir, u.IsAgent})
}

// handleListVaults reports every vault the caller can read, their own included.
func (a *API) handleListVaults(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	ids, err := a.Shares.ReadableVaults(r.Context(), u)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}

	type vaultJSON struct {
		Vault      string `json:"vault"`
		OwnerLogin string `json:"ownerLogin"`
		IsMine     bool   `json:"isMine"`
	}
	out := make([]vaultJSON, 0, len(ids))
	for _, id := range ids {
		owner, err := a.Identity.ByVaultID(r.Context(), id)
		if err != nil {
			continue
		}
		out = append(out, vaultJSON{owner.VaultDir, owner.Login, owner.ID == u.ID})
	}
	a.JSON(w, http.StatusOK, struct {
		Vaults []vaultJSON `json:"vaults"`
	}{out})
}

func (a *API) handleGetNote(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	n, err := a.Notes.Read(r.Context(), sc, r.PathValue("path"))
	if err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}
	backlinks, err := a.Notes.Backlinks(r.Context(), sc, n.Path)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}

	w.Header().Set("ETag", strconv.Quote(n.SHA256))
	a.JSON(w, http.StatusOK, noteResponse{
		Vault: n.Vault, OwnerLogin: n.OwnerLogin, Path: n.Path, Content: n.Content,
		SHA256: n.SHA256, Size: n.Size, ModTime: n.ModTime, Title: n.Title,
		Tags: orEmpty(n.Tags), Perm: n.Perm, Backlinks: linksJSON(backlinks),
	})
}

type createNoteRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (a *API) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	req, err := a.Decode[createNoteRequest](r, maxNoteBytes)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	n, err := a.Notes.Create(r.Context(), sc, req.Path, req.Content)
	if err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(n.SHA256))
	a.JSON(w, http.StatusCreated, summaryJSON(notes.Summary{
		Vault: n.Vault, OwnerLogin: n.OwnerLogin, Path: n.Path, Title: n.Title,
		Tags: n.Tags, SHA256: n.SHA256, UpdatedAt: n.ModTime,
	}))
}

type putNoteRequest struct {
	Content string `json:"content"`
}

func (a *API) handlePutNote(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	req, err := a.Decode[putNoteRequest](r, maxNoteBytes)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	// If-Match carries the sha the editor last saw, which turns a save into a
	// compare-and-swap. Without it the write is unconditional, which is what a
	// deliberate "overwrite anyway" action wants.
	base := strings.Trim(r.Header.Get("If-Match"), `"`)
	if base == "*" {
		base = ""
	}

	n, err := a.Notes.Update(r.Context(), sc, r.PathValue("path"), req.Content, base)
	if err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(n.SHA256))
	a.JSON(w, http.StatusOK, summaryJSON(notes.Summary{
		Vault: n.Vault, OwnerLogin: n.OwnerLogin, Path: n.Path, Title: n.Title,
		Tags: n.Tags, SHA256: n.SHA256, UpdatedAt: n.ModTime,
	}))
}

func (a *API) handleDeleteNote(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	if err := a.Notes.Delete(r.Context(), sc, r.PathValue("path")); err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// moveNoteRequest carries both paths in the body. A move is symmetric in its two
// paths, and ServeMux requires a {path...} wildcard to be last anyway, so there
// is nowhere sensible to put the source in the URL.
type moveNoteRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (a *API) handleMoveNote(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	req, err := a.Decode[moveNoteRequest](r, 1<<16)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	n, err := a.Notes.Move(r.Context(), sc, req.From, req.To)
	if err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}
	a.JSON(w, http.StatusOK, summaryJSON(notes.Summary{
		Vault: n.Vault, OwnerLogin: n.OwnerLogin, Path: n.Path, Title: n.Title,
		Tags: n.Tags, SHA256: n.SHA256, UpdatedAt: n.ModTime,
	}))
}

func (a *API) handleListNotes(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	q := r.URL.Query()
	list, err := a.Notes.List(r.Context(), sc, notes.ListOptions{
		Folder: q.Get("folder"),
		Tag:    q.Get("tag"),
		Limit:  intParam(q.Get("limit"), 200),
		Offset: intParam(q.Get("offset"), 0),
	})
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	out := make([]noteSummary, 0, len(list))
	for _, s := range list {
		out = append(out, summaryJSON(s))
	}
	a.JSON(w, http.StatusOK, struct {
		Notes []noteSummary `json:"notes"`
	}{out})
}

func (a *API) handleBacklinks(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	links, err := a.Notes.Backlinks(r.Context(), sc, r.PathValue("path"))
	if err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}
	a.JSON(w, http.StatusOK, struct {
		Backlinks []linkJSON `json:"backlinks"`
	}{linksJSON(links)})
}

// statusForPath is statusFor plus the path-validation case: a malformed path is
// the caller's mistake, so 400 rather than 500.
func statusForPath(err error) int {
	if isBadPath(err) {
		return http.StatusBadRequest
	}
	return statusFor(err)
}

func isBadPath(err error) bool {
	return err != nil && errors.Is(err, vaultpath.ErrInvalidPath)
}

func intParam(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// orEmpty keeps JSON arrays as [] rather than null, so clients need no null
// checks.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
