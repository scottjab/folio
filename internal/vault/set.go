package vault

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// ErrClosed means the Set has been shut down.
var ErrClosed = errors.New("vault set is closed")

// Set owns every open vault under one root directory, one per user.
//
// Handles are shared rather than reopened per request, which matters for more
// than performance: a Vault carries the per-path locks that serialize writes, so
// two handles on the same directory would let two requests write the same note
// at once.
type Set struct {
	root string

	mu     sync.RWMutex
	open   map[string]*Vault
	closed bool
}

// NewSet returns a Set rooted at dir. Vaults are created on first use.
func NewSet(root string) *Set {
	return &Set{root: root, open: map[string]*Vault{}}
}

// Get returns the vault stored in the named directory, opening it if needed.
//
// dir must be a single path segment, which is what [vaultpath.Slug] produces. A
// vault directory is derived from a login and must never be able to reach out of
// the vaults root.
func (s *Set) Get(dir string) (*Vault, error) {
	if err := validDir(dir); err != nil {
		return nil, err
	}

	s.mu.RLock()
	v, ok := s.open[dir]
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	if ok {
		return v, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	// Someone may have opened it while we waited for the write lock.
	if v, ok := s.open[dir]; ok {
		return v, nil
	}

	v, err := Open(filepath.Join(s.root, dir))
	if err != nil {
		return nil, err
	}
	s.open[dir] = v
	return v, nil
}

// validDir enforces that a vault directory is one ordinary path segment.
func validDir(dir string) error {
	if dir == "" || dir == "." || dir == ".." || strings.HasPrefix(dir, ".") {
		return fmt.Errorf("invalid vault directory %q", dir)
	}
	if strings.ContainsAny(dir, `/\`) || strings.ContainsRune(dir, 0) {
		return fmt.Errorf("invalid vault directory %q: must be a single path segment", dir)
	}
	return nil
}

// Root returns the directory containing every vault.
func (s *Set) Root() string { return s.root }

// Close releases every open vault.
func (s *Set) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true

	var errs []error
	for dir, v := range s.open {
		if err := v.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close vault %s: %w", dir, err))
		}
	}
	clear(s.open)
	return errors.Join(errs...)
}
