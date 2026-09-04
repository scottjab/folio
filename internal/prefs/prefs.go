// Package prefs holds the settings a folio user carries with them: the ones the
// browser and the terminal client both have to agree on.
//
// The test for whether something belongs here is whether two clients disagreeing
// about it would produce a wrong file on disk. Where a dropped image lands is
// exactly that, so it lives on the server. How wide the text runs in a browser
// window is not, so it stays in that browser's localStorage.
package prefs

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/scottjab/folio/internal/store"
	"github.com/scottjab/folio/internal/vaultpath"
)

// ErrInvalidPref is the base for every rejection out of [Prefs.Validate].
var ErrInvalidPref = errors.New("invalid preference")

// AttachmentMode says where a newly uploaded file is filed. The four modes are
// Obsidian's, by the same names it uses in its own settings, so a vault that is
// also open in Obsidian can be made to agree with it.
type AttachmentMode string

const (
	// AttachVault puts attachments at the vault root.
	AttachVault AttachmentMode = "vault"
	// AttachFolder puts them all in one folder, named by Folder.
	AttachFolder AttachmentMode = "folder"
	// AttachCurrent puts them beside the note that references them.
	AttachCurrent AttachmentMode = "current"
	// AttachSubfolder puts them in a subfolder of the note's own folder, named
	// by Folder.
	AttachSubfolder AttachmentMode = "subfolder"
)

// Attachments is the "where do new attachments go" setting.
type Attachments struct {
	Mode AttachmentMode `json:"mode"`
	// Folder is the folder name for the two modes that need one, and is ignored
	// by the other two. It is kept across a mode change rather than cleared, so
	// switching to "vault root" and back does not lose what you had typed.
	Folder string `json:"folder"`
}

// Prefs is everything a user can set.
type Prefs struct {
	Attachments Attachments `json:"attachments"`
}

// DefaultAttachmentFolder is where uploads go until someone says otherwise. It
// matches the layout the README documents, which is also the most common way an
// Obsidian vault is set up.
const DefaultAttachmentFolder = "attachments"

// Default is what a user who has never changed anything gets.
func Default() Prefs {
	return Prefs{
		Attachments: Attachments{Mode: AttachFolder, Folder: DefaultAttachmentFolder},
	}
}

// Dir returns the folder an attachment for notePath should be written to, as a
// vault-relative path. The vault root is "".
//
// notePath is the note doing the referencing. It is only consulted by the two
// modes that are relative to it, so a caller with no note open can pass "" and
// still get a sensible answer out of the other two.
func (a Attachments) Dir(notePath string) string {
	switch a.Mode {
	case AttachVault:
		return ""
	case AttachCurrent:
		return vaultpath.Dir(notePath)
	case AttachSubfolder:
		return path.Join(vaultpath.Dir(notePath), a.Folder)
	default: // AttachFolder, and anything unrecognized, which Validate rejects
		return a.Folder
	}
}

// Validate reports whether p is storable, and is the only thing standing between
// a typo'd mode and an upload that silently lands in the wrong place.
func (p Prefs) Validate() error {
	a := p.Attachments
	switch a.Mode {
	case AttachVault, AttachCurrent:
		// Neither reads Folder, so an empty or odd one is not an error here.
		// It is still validated below when it is non-empty, because it is kept
		// across a mode change and has to stay storable.
		if a.Folder == "" {
			return nil
		}
	case AttachFolder, AttachSubfolder:
		if strings.TrimSpace(a.Folder) == "" {
			return fmt.Errorf("%w: attachment mode %q needs a folder name", ErrInvalidPref, a.Mode)
		}
	default:
		return fmt.Errorf("%w: unknown attachment mode %q", ErrInvalidPref, a.Mode)
	}

	// A folder name goes into a path we then write to, so it has to survive the
	// same rules every other vault path does.
	clean, err := vaultpath.Clean(a.Folder)
	if err != nil {
		return fmt.Errorf("%w: attachment folder: %w", ErrInvalidPref, err)
	}
	if clean != a.Folder {
		return fmt.Errorf("%w: attachment folder %q is not canonical, use %q", ErrInvalidPref, a.Folder, clean)
	}
	if vaultpath.IsHidden(clean) {
		return fmt.Errorf("%w: attachment folder %q is hidden and would never be served", ErrInvalidPref, clean)
	}
	return nil
}

// attachmentsKey is the row these live under. One key per setting, so adding the
// next one is an insert rather than a rewrite of everybody's row.
const attachmentsKey = "attachments"

// Store reads and writes preferences.
type Store struct{ db *store.DB }

// New wraps a database handle.
func New(db *store.DB) *Store { return &Store{db: db} }

// Get returns userID's preferences, falling back to [Default] for anything they
// have never set. A user with no rows at all is the normal case, not an error.
func (s *Store) Get(ctx context.Context, userID int64) (Prefs, error) {
	out := Default()
	raw, err := s.db.One[string](ctx,
		`SELECT value FROM prefs WHERE user_id = ? AND key = ?`, userID, attachmentsKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, nil
	case err != nil:
		return out, fmt.Errorf("read prefs for user %d: %w", userID, err)
	}

	var a Attachments
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		// A row we cannot parse is a bug or a downgrade, and neither is worth
		// failing a page load over. The default is always safe.
		return out, nil
	}
	out.Attachments = a
	return out, nil
}

// Set replaces userID's preferences. It validates first, so an invalid setting
// is rejected at the API rather than discovered on the next upload.
func (s *Store) Set(ctx context.Context, userID int64, p Prefs) error {
	if err := p.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(p.Attachments)
	if err != nil {
		return fmt.Errorf("encode prefs: %w", err)
	}
	_, err = s.db.Exec(ctx,
		`INSERT INTO prefs (user_id, key, value) VALUES (?, ?, ?)
		 ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value`,
		userID, attachmentsKey, string(raw))
	if err != nil {
		return fmt.Errorf("write prefs for user %d: %w", userID, err)
	}
	return nil
}
