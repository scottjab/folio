package httpapi

import (
	"net/http"
)

// handleEmbed resolves one ![[target]] and returns what it points at.
//
// The editors could resolve embeds themselves, since both already have a vault
// listing. They must not: resolution and section extraction are the rules that
// decide what text a reader sees, and a browser and a terminal that each
// implement them separately will eventually show different halves of the same
// note. One endpoint, one answer.
func (a *API) handleEmbed(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	q := r.URL.Query()
	target := q.Get("target")
	if target == "" {
		a.fail(w, r, http.StatusBadRequest, errNoEmbedTarget)
		return
	}

	em, err := a.Notes.Embed(r.Context(), sc, q.Get("from"), target)
	if err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}

	a.JSON(w, http.StatusOK, struct {
		Kind      string `json:"kind"`
		Vault     string `json:"vault"`
		Path      string `json:"path,omitzero"`
		Title     string `json:"title,omitzero"`
		Anchor    string `json:"anchor,omitzero"`
		Content   string `json:"content,omitzero"`
		Truncated bool   `json:"truncated,omitzero"`
	}{string(em.Kind), em.Vault, em.Path, em.Title, em.Anchor, em.Content, em.Truncated})
}
