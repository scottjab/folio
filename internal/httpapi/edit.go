package httpapi

import (
	"net/http"
	"time"

	"github.com/scottjab/tsnotes/internal/notes"
)

type appendNoteRequest struct {
	Path string `json:"path"`
	Text string `json:"text"`
	// UnderHeading appends at the end of that section rather than the file, so
	// a task meant for "## Tasks" does not land under "## Notes".
	UnderHeading string `json:"underHeading,omitzero"`
}

func (a *API) handleAppendNote(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	req, err := a.Decode[appendNoteRequest](r, maxNoteBytes)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	n, err := a.Notes.Append(r.Context(), sc, req.Path, req.Text, req.UnderHeading)
	if err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}
	a.JSON(w, http.StatusOK, summaryJSON(notes.Summary{
		Vault: n.Vault, OwnerLogin: n.OwnerLogin, Path: n.Path, Title: n.Title,
		Tags: n.Tags, SHA256: n.SHA256, UpdatedAt: n.ModTime,
	}))
}

type editJSON struct {
	Old        string `json:"old"`
	New        string `json:"new"`
	ReplaceAll bool   `json:"replaceAll,omitzero"`
}

type editNoteRequest struct {
	Path  string     `json:"path"`
	Edits []editJSON `json:"edits"`
}

func (a *API) handleEditNote(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	req, err := a.Decode[editNoteRequest](r, maxNoteBytes)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	edits := make([]notes.Edit, len(req.Edits))
	for i, e := range req.Edits {
		edits[i] = notes.Edit{Old: e.Old, New: e.New, ReplaceAll: e.ReplaceAll}
	}
	n, err := a.Notes.Edit(r.Context(), sc, req.Path, edits)
	if err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}
	a.JSON(w, http.StatusOK, summaryJSON(notes.Summary{
		Vault: n.Vault, OwnerLogin: n.OwnerLogin, Path: n.Path, Title: n.Title,
		Tags: n.Tags, SHA256: n.SHA256, UpdatedAt: n.ModTime,
	}))
}

// handleDailyNote returns today's daily note, or the one for ?date=YYYY-MM-DD,
// creating it from a template unless ?create=false.
func (a *API) handleDailyNote(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	day := time.Now()
	if s := r.URL.Query().Get("date"); s != "" {
		day, err = time.Parse("2006-01-02", s)
		if err != nil {
			a.fail(w, r, http.StatusBadRequest, err)
			return
		}
	}
	create := r.URL.Query().Get("create") != "false"

	n, err := a.Notes.DailyNote(r.Context(), sc, day, create)
	if err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}
	a.JSON(w, http.StatusOK, noteResponse{
		Vault: n.Vault, OwnerLogin: n.OwnerLogin, Path: n.Path, Content: n.Content,
		SHA256: n.SHA256, Size: n.Size, ModTime: n.ModTime, Title: n.Title,
		Tags: orEmpty(n.Tags), Perm: n.Perm, Backlinks: []linkJSON{},
	})
}
