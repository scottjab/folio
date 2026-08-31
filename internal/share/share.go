// Package share decides who may read or write which notes.
//
// The model is deliberately small. You own your vault outright. Everyone else
// gets in only through a grant you made: a specific note, or a folder and
// everything under it, at read or write. There are no groups, no roles, and no
// inherited-from-parent-vault cleverness, because every one of those makes it
// harder to answer "who can see this note?" at a glance.
//
// Every check fails closed. A caller with no readable vaults sees nothing, not
// everything.
package share

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/scottjab/tsnotes/internal/identity"
	"github.com/scottjab/tsnotes/internal/store"
	"github.com/scottjab/tsnotes/internal/vaultpath"
)

// Perm is the level of access a grant confers.
type Perm string

const (
	// Read allows viewing and searching a note.
	Read Perm = "read"
	// Write allows editing, and implies Read.
	Write Perm = "write"
)

// Valid reports whether p is a permission we understand.
func (p Perm) Valid() bool { return p == Read || p == Write }

// covers reports whether holding p satisfies a request for want.
func (p Perm) covers(want Perm) bool {
	if p == Write {
		return true // write implies read
	}
	return p == want
}

var (
	// ErrDenied means the user may not do this. It is the only error a handler
	// should turn into a 403.
	ErrDenied = errors.New("access denied")
	// ErrNotOwner means the user tried to administer someone else's share.
	ErrNotOwner = errors.New("not the owner of this share")
	// ErrNoSuchShare means the share id does not exist.
	ErrNoSuchShare = errors.New("share not found")
)

// Share is one grant.
type Share struct {
	ID           string `db:"id"`
	VaultID      int64  `db:"vault_id"`
	OwnerUserID  int64  `db:"owner_user_id"`
	OwnerLogin   string `db:"owner_login"`
	Path         string `db:"path"`
	IsFolder     bool   `db:"is_folder"`
	GranteeLogin string `db:"grantee_login"`
	Perm         Perm   `db:"perm"`
	CreatedAt    int64  `db:"created_at"`
}

// Resolver answers access questions and administers grants.
type Resolver struct {
	db *store.DB
}

// NewResolver returns a Resolver backed by db.
func NewResolver(db *store.DB) *Resolver { return &Resolver{db: db} }

// Check reports whether u may act on path in vaultID at the given level,
// returning nil when allowed and an ErrDenied-wrapping error otherwise.
func (r *Resolver) Check(ctx context.Context, u identity.User, vaultID int64, path string, want Perm) error {
	if u.VaultID == vaultID {
		return nil // you own it
	}
	clean, err := vaultpath.Clean(path)
	if err != nil {
		// An unparseable path is not a permission question; it is a bad request.
		// Denying is still the safe answer.
		return fmt.Errorf("%w: %v", ErrDenied, err)
	}

	grants, err := r.grantsFor(ctx, u.Login, vaultID)
	if err != nil {
		return err
	}
	best, ok := bestGrant(grants, clean)
	if !ok || !best.Perm.covers(want) {
		return fmt.Errorf("%w: %s on %s", ErrDenied, want, clean)
	}
	return nil
}

// grantsFor loads every grant made to login in one vault.
func (r *Resolver) grantsFor(ctx context.Context, login string, vaultID int64) ([]Share, error) {
	grants, err := r.db.All[Share](ctx, `
		SELECT s.id, s.vault_id, s.owner_user_id, u.login AS owner_login, s.path, s.is_folder,
		       s.grantee_login, s.perm, s.created_at
		FROM shares s JOIN users u ON u.id = s.owner_user_id
		WHERE s.vault_id = ? AND s.grantee_login = ? COLLATE NOCASE`, vaultID, login)
	if err != nil {
		return nil, fmt.Errorf("load grants: %w", err)
	}
	return grants, nil
}

// bestGrant picks the most specific grant covering path.
//
// Specificity is the point: a broad read on "Projects" plus a write on
// "Projects/shared" should mean write inside the subfolder and read outside it.
// An exact note grant is the most specific thing there is, so it always wins.
func bestGrant(grants []Share, path string) (Share, bool) {
	var best Share
	bestScore := -1

	for _, g := range grants {
		score := -1
		switch {
		case !g.IsFolder && g.Path == path:
			// Exact note match. Deeper than any folder can be.
			score = 1 << 20
		case g.IsFolder && vaultpath.Within(g.Path, path):
			// Depth of the folder, so a nested grant outranks its parent.
			score = strings.Count(g.Path, "/") + 1
		}
		if score < 0 {
			continue
		}
		// At equal specificity the stronger permission wins, so adding a write
		// grant beside an existing read grant does what you meant.
		if score > bestScore || (score == bestScore && g.Perm == Write) {
			best, bestScore = g, score
		}
	}
	return best, bestScore >= 0
}

// Grant shares a path out of owner's vault with another tailnet user. Passing a
// path and permission that already have a grant updates it in place.
func (r *Resolver) Grant(ctx context.Context, owner identity.User, path string, isFolder bool, granteeLogin string, perm Perm) (Share, error) {
	if !perm.Valid() {
		return Share{}, fmt.Errorf("invalid permission %q, want read or write", perm)
	}
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return Share{}, err
	}
	if strings.EqualFold(granteeLogin, owner.Login) {
		return Share{}, errors.New("you already have access to your own notes")
	}

	// Sharing with a login tsnotes has never seen is almost always a typo, and
	// accepting it would leave you thinking the note was shared when it was not.
	if _, err := r.db.One[int64](ctx, `SELECT id FROM users WHERE login = ? COLLATE NOCASE`, granteeLogin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Share{}, fmt.Errorf("%w: %s has not used tsnotes yet, so there is nothing to share with",
				identity.ErrUnknownUser, granteeLogin)
		}
		return Share{}, fmt.Errorf("look up %q: %w", granteeLogin, err)
	}

	// The owner can only ever share out of their own vault. Nothing in the API
	// should be able to reach this with someone else's vault id, but this is the
	// place that guarantees it.
	if owner.VaultID == 0 {
		return Share{}, fmt.Errorf("%w: no vault for %s", ErrDenied, owner.Login)
	}
	if err := r.assertOwnsVault(ctx, owner); err != nil {
		return Share{}, err
	}

	s := Share{
		ID:           uuid.NewV7().String(),
		VaultID:      owner.VaultID,
		OwnerUserID:  owner.ID,
		OwnerLogin:   owner.Login,
		Path:         clean,
		IsFolder:     isFolder,
		GranteeLogin: granteeLogin,
		Perm:         perm,
		CreatedAt:    time.Now().Unix(),
	}
	if _, err := r.db.Exec(ctx, `
		INSERT INTO shares (id, vault_id, owner_user_id, path, is_folder, grantee_login, perm, created_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (vault_id, path, grantee_login) DO UPDATE SET
			perm = excluded.perm, is_folder = excluded.is_folder`,
		s.ID, s.VaultID, s.OwnerUserID, s.Path, s.IsFolder, s.GranteeLogin, string(s.Perm), s.CreatedAt); err != nil {
		return Share{}, fmt.Errorf("create share: %w", err)
	}

	// Read the id back: on an update the existing row keeps its own id.
	id, err := r.db.One[string](ctx,
		`SELECT id FROM shares WHERE vault_id = ? AND path = ? AND grantee_login = ?`,
		s.VaultID, s.Path, s.GranteeLogin)
	if err != nil {
		return Share{}, fmt.Errorf("read back share: %w", err)
	}
	s.ID = id
	return s, nil
}

// assertOwnsVault confirms the user record and vault record agree, guarding
// against a caller assembling an identity.User by hand.
func (r *Resolver) assertOwnsVault(ctx context.Context, u identity.User) error {
	ownerID, err := r.db.One[int64](ctx, `SELECT user_id FROM vaults WHERE id = ?`, u.VaultID)
	if err != nil {
		return fmt.Errorf("verify vault ownership: %w", err)
	}
	if ownerID != u.ID {
		return fmt.Errorf("%w: vault %d does not belong to %s", ErrDenied, u.VaultID, u.Login)
	}
	return nil
}

// Revoke removes a grant. Only the person who made it can.
func (r *Resolver) Revoke(ctx context.Context, owner identity.User, shareID string) error {
	ownerID, err := r.db.One[int64](ctx, `SELECT owner_user_id FROM shares WHERE id = ?`, shareID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNoSuchShare, shareID)
	}
	if err != nil {
		return fmt.Errorf("look up share: %w", err)
	}
	if ownerID != owner.ID {
		return fmt.Errorf("%w: %s", ErrNotOwner, shareID)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM shares WHERE id = ?`, shareID); err != nil {
		return fmt.Errorf("revoke share: %w", err)
	}
	return nil
}

// ReadableVaults lists every vault u may read from, always including their own.
//
// This is the authorization boundary for search and listing: hand the result
// straight to the index, and a bug that loses a vault id shows fewer notes
// rather than more.
func (r *Resolver) ReadableVaults(ctx context.Context, u identity.User) ([]int64, error) {
	ids := []int64{}
	if u.VaultID != 0 {
		ids = append(ids, u.VaultID)
	}
	shared, err := r.db.All[int64](ctx,
		`SELECT DISTINCT vault_id FROM shares WHERE grantee_login = ? COLLATE NOCASE`, u.Login)
	if err != nil {
		return nil, fmt.Errorf("list readable vaults: %w", err)
	}
	for _, id := range shared {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// SharedWithMe lists grants made to u by other people.
func (r *Resolver) SharedWithMe(ctx context.Context, u identity.User) ([]Share, error) {
	return r.db.All[Share](ctx, `
		SELECT s.id, s.vault_id, s.owner_user_id, o.login AS owner_login, s.path, s.is_folder,
		       s.grantee_login, s.perm, s.created_at
		FROM shares s JOIN users o ON o.id = s.owner_user_id
		WHERE s.grantee_login = ? COLLATE NOCASE
		ORDER BY s.created_at DESC`, u.Login)
}

// SharesIMade lists grants u has handed out.
func (r *Resolver) SharesIMade(ctx context.Context, u identity.User) ([]Share, error) {
	return r.db.All[Share](ctx, `
		SELECT s.id, s.vault_id, s.owner_user_id, o.login AS owner_login, s.path, s.is_folder,
		       s.grantee_login, s.perm, s.created_at
		FROM shares s JOIN users o ON o.id = s.owner_user_id
		WHERE s.owner_user_id = ?
		ORDER BY s.created_at DESC`, u.ID)
}

// FilterReadable trims a list of paths in one vault to those u may read.
//
// Search results go through here, so a hit on a note you cannot see never
// reaches you, not even as a title.
func (r *Resolver) FilterReadable(ctx context.Context, u identity.User, vaultID int64, paths []string) ([]string, error) {
	if u.VaultID == vaultID {
		return paths, nil
	}
	grants, err := r.grantsFor(ctx, u.Login, vaultID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		clean, err := vaultpath.Clean(p)
		if err != nil {
			continue
		}
		if g, ok := bestGrant(grants, clean); ok && g.Perm.covers(Read) {
			out = append(out, p)
		}
	}
	return out, nil
}

// PermFor reports the strongest permission u holds on path, or "" for none.
// Handlers use it to tell the browser whether to show an editor or a viewer.
func (r *Resolver) PermFor(ctx context.Context, u identity.User, vaultID int64, path string) (Perm, error) {
	if u.VaultID == vaultID {
		return Write, nil
	}
	clean, err := vaultpath.Clean(path)
	if err != nil {
		return "", err
	}
	grants, err := r.grantsFor(ctx, u.Login, vaultID)
	if err != nil {
		return "", err
	}
	if g, ok := bestGrant(grants, clean); ok {
		return g.Perm, nil
	}
	return "", nil
}
