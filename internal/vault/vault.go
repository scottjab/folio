// Package vault is the only thing in tsnotes that touches note files.
//
// A Vault is one user's directory of markdown, and it is the source of truth for
// everything: the SQLite index downstream is a cache that can be rebuilt from
// these files at any time. Two properties matter more than anything else here.
//
// Confinement: every operation goes through an *os.Root, so a path can never
// escape the vault directory even through a symlink someone planted. This is
// enforced by the kernel, not by string checking, which is why vaultpath.Clean
// validating the same thing is belt and braces rather than duplication.
//
// Atomicity: a writer stages into .tsnotes/tmp and renames into place, so a
// concurrent reader (the indexer, Obsidian, your editor) sees either the whole
// old file or the whole new one, never a torn half.
package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/maphash"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/scottjab/tsnotes/internal/vaultpath"
)

// Sidecar layout inside every vault. These names are reserved; vaultpath.Clean
// refuses to produce a path under sidecarDir, so callers cannot address them.
const (
	sidecarDir = ".tsnotes"
	tmpDir     = sidecarDir + "/tmp"
	trashDir   = sidecarDir + "/trash"

	dirPerm  = 0o755
	filePerm = 0o644

	// stripes is the size of the per-path lock table. Distinct paths may share a
	// stripe, which costs a little contention and buys us a lock table that
	// never has to be garbage collected.
	stripes = 64

	// tsLayout stamps conflict copies and trash entries. Colons are illegal on
	// several filesystems, so it is RFC3339 with dashes.
	tsLayout = "2006-01-02T15-04-05"
)

var (
	// ErrNotFound means the path does not exist in the vault.
	ErrNotFound = errors.New("note not found")
	// ErrExists means the destination is already occupied.
	ErrExists = errors.New("note already exists")
	// ErrConflict means a compare-and-swap write lost the race. The rejected
	// content is never dropped; see ConflictError.ConflictPath.
	ErrConflict = errors.New("write conflict")
)

// ConflictError carries the detail behind an ErrConflict so the API can tell the
// browser exactly what happened and where its rejected draft went.
type ConflictError struct {
	Path         string
	ExpectedSHA  string
	ActualSHA    string
	ConflictPath string
}

func (e *ConflictError) Error() string {
	if e.ConflictPath != "" {
		return fmt.Sprintf("write conflict on %s: expected sha %s, found %s; your version was saved as %s",
			e.Path, short(e.ExpectedSHA), short(e.ActualSHA), e.ConflictPath)
	}
	return fmt.Sprintf("write conflict on %s: expected sha %s, but the note no longer exists",
		e.Path, short(e.ExpectedSHA))
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}

// Note is the metadata for one file, including its content hash. The hash is
// what drives ETags, compare-and-swap writes, and the indexer's "did this
// actually change" check.
type Note struct {
	Path    string
	SHA256  string
	Size    int64
	ModTime time.Time
}

// Entry is one file in a listing. It deliberately carries no hash: hashing a
// whole vault to produce a file tree would be wasteful, so callers that need a
// hash ask for it per note.
type Entry struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// Vault is one user's markdown directory.
type Vault struct {
	root *os.Root
	dir  string

	// Now is the clock used for conflict and trash timestamps. Tests replace it;
	// production leaves it as time.Now.
	Now func() time.Time

	seed maphash.Seed
	mu   [stripes]sync.Mutex
}

// Open prepares dir as a vault, creating it and its sidecar directories if
// needed. It is safe to call on an existing vault.
func Open(dir string) (*Vault, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve vault dir: %w", err)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, fmt.Errorf("create vault dir: %w", err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open vault root: %w", err)
	}
	for _, d := range []string{sidecarDir, tmpDir, trashDir} {
		if err := root.MkdirAll(d, dirPerm); err != nil {
			root.Close()
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	return &Vault{root: root, dir: abs, Now: time.Now, seed: maphash.MakeSeed()}, nil
}

// Close releases the vault's root handle.
func (v *Vault) Close() error { return v.root.Close() }

// Dir returns the vault's absolute directory. For display and for tests; do not
// build paths from it, use the Vault's methods so confinement is preserved.
func (v *Vault) Dir() string { return v.dir }

// lock returns the stripe guarding p.
func (v *Vault) lock(p string) *sync.Mutex {
	return &v.mu[maphash.String(v.seed, p)%stripes]
}

// lock2 takes both stripes in a fixed order so Move can never deadlock against
// a concurrent Move going the other way.
func (v *Vault) lock2(a, b string) func() {
	ia := maphash.String(v.seed, a) % stripes
	ib := maphash.String(v.seed, b) % stripes
	if ia == ib {
		v.mu[ia].Lock()
		return func() { v.mu[ia].Unlock() }
	}
	if ia > ib {
		ia, ib = ib, ia
	}
	v.mu[ia].Lock()
	v.mu[ib].Lock()
	return func() { v.mu[ib].Unlock(); v.mu[ia].Unlock() }
}

// Read returns a note's content along with its metadata.
func (v *Vault) Read(p string) ([]byte, Note, error) {
	clean, err := vaultpath.Clean(p)
	if err != nil {
		return nil, Note{}, err
	}
	return v.read(clean)
}

func (v *Vault) read(clean string) ([]byte, Note, error) {
	b, err := v.root.ReadFile(clean)
	if err != nil {
		return nil, Note{}, wrapNotFound(clean, err)
	}
	fi, err := v.root.Stat(clean)
	if err != nil {
		return nil, Note{}, wrapNotFound(clean, err)
	}
	return b, Note{
		Path:    clean,
		SHA256:  hashOf(b),
		Size:    int64(len(b)),
		ModTime: fi.ModTime(),
	}, nil
}

// Stat returns a note's metadata, hash included. It reads the file, because a
// content hash is the only change signal we trust; mtime alone lies whenever a
// sync tool rewrites a file with identical content.
func (v *Vault) Stat(p string) (Note, error) {
	clean, err := vaultpath.Clean(p)
	if err != nil {
		return Note{}, err
	}
	_, n, err := v.read(clean)
	return n, err
}

// Exists reports whether p is a readable file in the vault. An invalid or
// escaping path is simply absent, never an error.
func (v *Vault) Exists(p string) bool {
	clean, err := vaultpath.Clean(p)
	if err != nil {
		return false
	}
	fi, err := v.root.Stat(clean)
	return err == nil && fi.Mode().IsRegular()
}

// Write saves content to p.
//
// baseSHA selects the concurrency semantics:
//
//   - "" writes unconditionally, creating or replacing.
//   - a hash requires the note to currently hold exactly that content. If it
//     does not, the write is refused with a *ConflictError and the caller's
//     content is parked next to the note rather than thrown away.
func (v *Vault) Write(p string, content []byte, baseSHA string) (Note, error) {
	clean, err := vaultpath.Clean(p)
	if err != nil {
		return Note{}, err
	}
	unlock := v.lock(clean)
	unlock.Lock()
	defer unlock.Unlock()

	if baseSHA != "" {
		if err := v.checkCAS(clean, content, baseSHA); err != nil {
			return Note{}, err
		}
	}
	return v.writeLocked(clean, content)
}

// checkCAS verifies the on-disk content still matches baseSHA. On a mismatch it
// saves the caller's content to a sibling conflict file first: losing a race
// should cost you a second file to merge, never your writing.
func (v *Vault) checkCAS(clean string, content []byte, baseSHA string) error {
	current, err := v.root.ReadFile(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &ConflictError{Path: clean, ExpectedSHA: baseSHA}
		}
		return fmt.Errorf("read %s: %w", clean, err)
	}
	actual := hashOf(current)
	if actual == baseSHA {
		return nil
	}
	cp, saveErr := v.saveConflictCopy(clean, content)
	ce := &ConflictError{Path: clean, ExpectedSHA: baseSHA, ActualSHA: actual, ConflictPath: cp}
	if saveErr != nil {
		return fmt.Errorf("%w (and the rejected content could not be saved: %v)", ce, saveErr)
	}
	return ce
}

// saveConflictCopy parks rejected content beside the note as
// "Name (conflict 2026-08-30T12-00-00Z).md", the same shape Obsidian's own sync
// uses, so the file is obvious in any file browser.
func (v *Vault) saveConflictCopy(clean string, content []byte) (string, error) {
	dir, base := path.Split(clean)
	stem, ext, hasExt := strings.CutLast(base, ".")
	if !hasExt || stem == "" {
		stem, ext = base, ""
	} else {
		ext = "." + ext
	}
	stamp := v.Now().UTC().Format(tsLayout) + "Z"

	for n := 0; n < 100; n++ {
		suffix := ""
		if n > 0 {
			suffix = fmt.Sprintf(" %d", n+1)
		}
		candidate := fmt.Sprintf("%s%s (conflict %s)%s%s", dir, stem, stamp, suffix, ext)
		if _, err := v.root.Stat(candidate); errors.Is(err, fs.ErrNotExist) {
			if _, err := v.writeLocked(candidate, content); err != nil {
				return "", err
			}
			return candidate, nil
		}
	}
	return "", errors.New("too many conflict copies for one note")
}

// Create writes a new note, refusing to touch an existing one.
func (v *Vault) Create(p string, content []byte) (Note, error) {
	clean, err := vaultpath.Clean(p)
	if err != nil {
		return Note{}, err
	}
	mu := v.lock(clean)
	mu.Lock()
	defer mu.Unlock()

	if _, err := v.root.Stat(clean); err == nil {
		return Note{}, fmt.Errorf("%s: %w", clean, ErrExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Note{}, fmt.Errorf("stat %s: %w", clean, err)
	}
	return v.writeLocked(clean, content)
}

// writeLocked stages into .tsnotes/tmp and renames into place. The caller holds
// the stripe for clean.
func (v *Vault) writeLocked(clean string, content []byte) (Note, error) {
	if dir := path.Dir(clean); dir != "." && dir != "/" {
		if err := v.root.MkdirAll(dir, dirPerm); err != nil {
			return Note{}, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	tmp := tmpDir + "/" + uuid.NewV7().String()
	f, err := v.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return Note{}, fmt.Errorf("stage %s: %w", clean, err)
	}
	cleanupTmp := func() { v.root.Remove(tmp) }

	if _, err := f.Write(content); err != nil {
		f.Close()
		cleanupTmp()
		return Note{}, fmt.Errorf("write %s: %w", clean, err)
	}
	// Durability before the rename: a crash may lose the write, but it must
	// never leave a renamed-into-place file with garbage contents.
	if err := f.Sync(); err != nil {
		f.Close()
		cleanupTmp()
		return Note{}, fmt.Errorf("sync %s: %w", clean, err)
	}
	if err := f.Close(); err != nil {
		cleanupTmp()
		return Note{}, fmt.Errorf("close %s: %w", clean, err)
	}
	if err := v.root.Rename(tmp, clean); err != nil {
		cleanupTmp()
		return Note{}, fmt.Errorf("commit %s: %w", clean, err)
	}

	var mod time.Time
	if fi, err := v.root.Stat(clean); err == nil {
		mod = fi.ModTime()
	}
	return Note{Path: clean, SHA256: hashOf(content), Size: int64(len(content)), ModTime: mod}, nil
}

// Delete moves a note into .tsnotes/trash under a timestamped directory rather
// than unlinking it. Notes are cheap; a note you meant to keep is not.
func (v *Vault) Delete(p string) error {
	clean, err := vaultpath.Clean(p)
	if err != nil {
		return err
	}
	mu := v.lock(clean)
	mu.Lock()
	defer mu.Unlock()

	if _, err := v.root.Stat(clean); err != nil {
		return wrapNotFound(clean, err)
	}

	dest := trashDir + "/" + v.Now().UTC().Format(tsLayout) + "Z/" + clean
	if dir := path.Dir(dest); dir != "." {
		if err := v.root.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("create trash dir: %w", err)
		}
	}
	// A same-second re-delete of the same path would collide; give it a suffix.
	if _, err := v.root.Stat(dest); err == nil {
		dest += "." + uuid.NewV7().String()[:8]
	}
	if err := v.root.Rename(clean, dest); err != nil {
		return fmt.Errorf("trash %s: %w", clean, err)
	}
	return nil
}

// Move renames a note. It refuses to overwrite an existing destination; the
// caller decides what to do about that, because silently clobbering a note is
// never what anyone wanted.
func (v *Vault) Move(from, to string) error {
	src, err := vaultpath.Clean(from)
	if err != nil {
		return err
	}
	dst, err := vaultpath.Clean(to)
	if err != nil {
		return err
	}
	if src == dst {
		return nil
	}

	unlock := v.lock2(src, dst)
	defer unlock()

	if _, err := v.root.Stat(src); err != nil {
		return wrapNotFound(src, err)
	}
	if _, err := v.root.Stat(dst); err == nil {
		return fmt.Errorf("%s: %w", dst, ErrExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", dst, err)
	}

	if dir := path.Dir(dst); dir != "." && dir != "/" {
		if err := v.root.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := v.root.Rename(src, dst); err != nil {
		return fmt.Errorf("move %s to %s: %w", src, dst, err)
	}
	return nil
}

// List walks the vault and returns every regular, non-hidden file: notes and
// attachments alike.
//
// Symlinks are skipped outright. A symlink inside a vault is either an escape
// attempt or a sync artifact, and neither is content we want to serve or index.
func (v *Vault) List() ([]Entry, error) {
	var out []Entry
	err := fs.WalkDir(v.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read is not a reason to fail the whole
			// listing; skip it and carry on.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == "." {
			return nil
		}
		if vaultpath.IsHidden(p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, Entry{Path: p, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk vault: %w", err)
	}
	return out, nil
}

// ListNotes returns just the markdown paths, which is what the indexer wants.
func (v *Vault) ListNotes() ([]string, error) {
	entries, err := v.List()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if vaultpath.IsMarkdown(e.Path) {
			out = append(out, e.Path)
		}
	}
	return out, nil
}

// hashOf is the content hash used for ETags, CAS, and change detection.
func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func wrapNotFound(p string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", p, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", p, err)
}
