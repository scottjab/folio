package httpapi

import (
	"net/http"
	"time"

	"github.com/scottjab/tsnotes/internal/share"
	"github.com/scottjab/tsnotes/internal/vaultpath"
)

type shareJSON struct {
	ID           string     `json:"id"`
	Vault        string     `json:"vault"`
	OwnerLogin   string     `json:"ownerLogin"`
	Path         string     `json:"path"`
	IsFolder     bool       `json:"isFolder"`
	GranteeLogin string     `json:"granteeLogin"`
	Perm         share.Perm `json:"perm"`
	CreatedAt    time.Time  `json:"createdAt"`
}

func (a *API) shareJSON(r *http.Request, s share.Share) shareJSON {
	dir := ""
	if owner, err := a.Identity.ByVaultID(r.Context(), s.VaultID); err == nil {
		dir = owner.VaultDir
	}
	return shareJSON{
		ID: s.ID, Vault: dir, OwnerLogin: s.OwnerLogin, Path: s.Path,
		IsFolder: s.IsFolder, GranteeLogin: s.GranteeLogin, Perm: s.Perm,
		CreatedAt: time.Unix(s.CreatedAt, 0),
	}
}

type createShareRequest struct {
	Path     string     `json:"path"`
	Grantee  string     `json:"grantee"`
	Perm     share.Perm `json:"perm"`
	IsFolder bool       `json:"isFolder"`
}

// handleCreateShare grants access to part of the caller's own vault.
//
// There is deliberately no vault parameter: Grant always works on the caller's
// vault, which makes sharing someone else's note impossible by construction
// rather than by check.
func (a *API) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	req, err := a.Decode[createShareRequest](r, 1<<16)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	path := req.Path
	if !req.IsFolder {
		path = vaultpath.EnsureMarkdown(path)
	}
	if req.Perm == "" {
		req.Perm = share.Read
	}

	s, err := a.Shares.Grant(r.Context(), u, path, req.IsFolder, req.Grantee, req.Perm)
	if err != nil {
		status := statusFor(err)
		if status == http.StatusInternalServerError {
			// Grant's own validation errors (bad permission, sharing with
			// yourself, unknown login) are the caller's mistake, not ours.
			status = http.StatusBadRequest
		}
		a.fail(w, r, status, err)
		return
	}
	a.JSON(w, http.StatusCreated, a.shareJSON(r, s))
}

func (a *API) handleDeleteShare(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if err := a.Shares.Revoke(r.Context(), u, r.PathValue("id")); err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListShares reports the grants the caller has handed out.
func (a *API) handleListShares(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	shares, err := a.Shares.SharesIMade(r.Context(), u)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	out := make([]shareJSON, 0, len(shares))
	for _, s := range shares {
		out = append(out, a.shareJSON(r, s))
	}
	a.JSON(w, http.StatusOK, struct {
		Shares []shareJSON `json:"shares"`
	}{out})
}

// handleSharedWithMe reports the grants other people have made to the caller.
func (a *API) handleSharedWithMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	shares, err := a.Shares.SharedWithMe(r.Context(), u)
	if err != nil {
		a.fail(w, r, statusFor(err), err)
		return
	}
	out := make([]shareJSON, 0, len(shares))
	for _, s := range shares {
		out = append(out, a.shareJSON(r, s))
	}
	a.JSON(w, http.StatusOK, struct {
		Shares []shareJSON `json:"shares"`
	}{out})
}
