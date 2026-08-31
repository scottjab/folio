package httpapi

import (
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"

	"github.com/scottjab/tsnotes/internal/share"
	"github.com/scottjab/tsnotes/internal/vaultpath"
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
