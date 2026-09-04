package httpapi

import (
	"net/http"

	"github.com/scottjab/folio/internal/prefs"
)

// maxPrefsBytes caps a settings body. Preferences are a handful of short
// strings; anything larger is not a settings write.
const maxPrefsBytes = 16 << 10

// prefsJSON is the settings shape both clients read and write.
//
// It is flat rather than nested by feature, because the shape is the contract
// with two front ends and a nested one invites each of them to invent a
// different half-populated version of it.
type prefsJSON struct {
	AttachmentMode   string `json:"attachmentMode"`
	AttachmentFolder string `json:"attachmentFolder"`
}

func toPrefsJSON(p prefs.Prefs) prefsJSON {
	return prefsJSON{
		AttachmentMode:   string(p.Attachments.Mode),
		AttachmentFolder: p.Attachments.Folder,
	}
}

func (a *API) handleGetPrefs(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	p, err := a.Notes.Prefs.Get(r.Context(), u.ID)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	a.JSON(w, http.StatusOK, toPrefsJSON(p))
}

// handlePutPrefs replaces the caller's settings.
//
// It is a full replace rather than a merge: a merge means a client that has
// never heard of a setting can silently preserve a stale value for it, and the
// body is small enough that sending all of it costs nothing.
func (a *API) handlePutPrefs(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	body, err := a.Decode[prefsJSON](r, maxPrefsBytes)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	p := prefs.Prefs{Attachments: prefs.Attachments{
		Mode:   prefs.AttachmentMode(body.AttachmentMode),
		Folder: body.AttachmentFolder,
	}}
	if err := a.Notes.Prefs.Set(r.Context(), u.ID, p); err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	a.JSON(w, http.StatusOK, toPrefsJSON(p))
}
