package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/scottjab/folio/internal/index"
)

// hitJSON is one search result as the browser sees it.
type hitJSON struct {
	Vault      string    `json:"vault"`
	OwnerLogin string    `json:"ownerLogin"`
	Path       string    `json:"path"`
	Title      string    `json:"title"`
	Snippet    string    `json:"snippet"`
	Tags       []string  `json:"tags"`
	Score      float64   `json:"score"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// handleSearch runs a query across every vault the caller can read.
//
// Two filters are applied, and both matter. The index is scoped to readable
// vault ids, and then results are filtered per path, because a share may cover
// only part of someone's vault. Skipping the second would leak the titles and
// snippets of notes the caller was never granted.
func (a *API) handleSearch(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	q := r.URL.Query()

	found, hasMore, err := a.Notes.Search(r.Context(), u,
		q.Get("q"), intParam(q.Get("limit"), 50), intParam(q.Get("offset"), 0))
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}

	hits := make([]hitJSON, 0, len(found))
	for _, h := range found {
		hits = append(hits, hitJSON{
			Vault: h.Vault, OwnerLogin: h.OwnerLogin, Path: h.Path, Title: h.Title,
			Snippet: renderSnippet(h.Snippet), Tags: orEmpty(h.Tags),
			Score: h.Score, UpdatedAt: h.UpdatedAt,
		})
	}

	a.JSON(w, http.StatusOK, struct {
		Query   string    `json:"query"`
		Hits    []hitJSON `json:"hits"`
		HasMore bool      `json:"hasMore"`
	}{q.Get("q"), hits, hasMore})
}

// renderSnippet converts the index's private highlight markers into the
// <mark> tags the client renders.
//
// The markers are control characters rather than markup on purpose: FTS5 injects
// them around the match, and if they were literal "<mark>" then a note
// containing that text could forge a highlight. Escaping the note's own angle
// brackets first, and only then substituting the markers, makes that impossible.
func renderSnippet(s string) string {
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
	return strings.NewReplacer(
		index.HighlightOpen, "<mark>",
		index.HighlightClose, "</mark>",
	).Replace(escaped)
}

func (a *API) handleTags(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	ids, err := a.Shares.ReadableVaults(r.Context(), u)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	tags, err := a.Index.Tags(r.Context(), ids)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	type tagJSON struct {
		Tag   string `json:"tag"`
		Count int    `json:"count"`
	}
	out := make([]tagJSON, 0, len(tags))
	for _, t := range tags {
		out = append(out, tagJSON{t.Tag, t.Count})
	}
	a.JSON(w, http.StatusOK, struct {
		Tags []tagJSON `json:"tags"`
	}{out})
}

func (a *API) handleFolders(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	ids, err := a.Shares.ReadableVaults(r.Context(), u)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	folders, err := a.Index.Folders(r.Context(), ids)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	a.JSON(w, http.StatusOK, struct {
		Folders []string `json:"folders"`
	}{orEmpty(folders)})
}

// handleUsers lists known tailnet users so the share dialog can offer
// autocomplete. It exposes logins and display names only, which are already
// visible to anyone on the tailnet.
func (a *API) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.Identity.Users(r.Context())
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	me := userFrom(r.Context())
	type userJSON struct {
		Login       string `json:"login"`
		DisplayName string `json:"displayName"`
	}
	out := make([]userJSON, 0, len(users))
	for _, u := range users {
		if u.ID == me.ID {
			continue
		}
		out = append(out, userJSON{u.Login, u.DisplayName})
	}
	a.JSON(w, http.StatusOK, struct {
		Users []userJSON `json:"users"`
	}{out})
}
