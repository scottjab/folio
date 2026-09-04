package httpapi

import (
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"

	"github.com/scottjab/folio/internal/notes"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/vaultpath"
)

// handleGetAttachment serves a binary file from a vault: images embedded with
// ![[diagram.png]], PDFs, whatever else people drop in.
func (a *API) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	p, err := vaultpath.Clean(r.PathValue("path"))
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if err := a.Shares.Check(r.Context(), sc.User, sc.VaultID, p, share.Read); err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}

	content, n, err := sc.Vault.Read(p)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}

	ct := mime.TypeByExtension(path.Ext(p))
	if ct == "" {
		// Refusing to guess is the safe default: an unknown type served as
		// octet-stream downloads, it does not execute.
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.FormatInt(n.Size, 10))
	h.Set("ETag", strconv.Quote(n.SHA256))
	// An attachment is user-supplied bytes. Even with a correct Content-Type,
	// forbid it from being treated as a document with the app's own privileges.
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("X-Content-Type-Options", "nosniff")

	if match := r.Header.Get("If-None-Match"); match != "" && match == strconv.Quote(n.SHA256) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(content)
}

// handlePutAttachment stores an uploaded file. The body is the file itself
// rather than a multipart form, which keeps both the client and this handler
// simple; the path comes from the URL.
func (a *API) handlePutAttachment(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	p, err := vaultpath.Clean(r.PathValue("path"))
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if vaultpath.IsMarkdown(p) {
		a.fail(w, r, http.StatusBadRequest, errMarkdownViaAttachments)
		return
	}
	if err := a.Shares.Check(r.Context(), sc.User, sc.VaultID, p, share.Write); err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}

	content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAttachmentBytes))
	if err != nil {
		a.fail(w, r, http.StatusRequestEntityTooLarge, err)
		return
	}

	n, err := sc.Vault.Write(p, content, "")
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(n.SHA256))
	a.JSON(w, http.StatusCreated, struct {
		Vault  string `json:"vault"`
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}{sc.Dir, n.Path, n.Size, n.SHA256})
}

// handleListAttachments reports the non-markdown files in a vault.
//
// The editor needs this to tell ![[diagram.png]] pointing at a real image from
// one pointing at nothing. It cannot answer that from the note listing, and
// guessing wrong means either a broken image icon on a good link or silence on
// a bad one.
func (a *API) handleListAttachments(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	list, err := a.Notes.ListAttachments(r.Context(), sc)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}

	type attachmentJSON struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	out := make([]attachmentJSON, 0, len(list))
	for _, at := range list {
		out = append(out, attachmentJSON{at.Path, at.Size})
	}
	a.JSON(w, http.StatusOK, struct {
		Vault       string           `json:"vault"`
		Attachments []attachmentJSON `json:"attachments"`
	}{sc.Dir, out})
}

// handleUpload stores a dropped or pasted file and says where it went.
//
// This is the endpoint the editors use, and it is deliberately not addressed by
// path: the caller says which note it is inserting into and what the file was
// called, and the server applies the user's attachment preference to decide the
// rest. Letting each client compute the destination is how the browser and the
// terminal would end up filing the same drop in two different folders.
func (a *API) handleUpload(w http.ResponseWriter, r *http.Request) {
	sc, err := a.scope(r)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}

	content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAttachmentBytes))
	if err != nil {
		a.fail(w, r, http.StatusRequestEntityTooLarge, err)
		return
	}

	at, err := a.Notes.Attach(r.Context(), sc, notes.Upload{
		Note:        r.URL.Query().Get("note"),
		Name:        r.URL.Query().Get("name"),
		ContentType: r.Header.Get("Content-Type"),
		Content:     content,
	})
	if err != nil {
		a.fail(w, r, statusForPath(err), err)
		return
	}

	w.Header().Set("ETag", strconv.Quote(at.SHA256))
	a.JSON(w, http.StatusCreated, struct {
		Vault  string `json:"vault"`
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		Link   string `json:"link"`
	}{at.Vault, at.Path, at.Size, at.SHA256, at.Link})
}
