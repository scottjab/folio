package watch

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/scottjab/folio/internal/vault"
	"github.com/scottjab/folio/internal/vaultpath"
)

// Default timings. 300ms is long enough to absorb an editor's write-chmod-rename
// dance and short enough that a change you made in Obsidian is searchable before
// you have switched windows.
const (
	DefaultQuiet    = 300 * time.Millisecond
	DefaultMaxDelay = 3 * time.Second
)

// Watcher reports changes made to a vault from outside folio.
//
// fsnotify on Linux watches a single directory, not a tree, so the watcher keeps
// its own set of directory watches and adds new ones as directories appear.
// Hidden directories are never watched: .obsidian churns constantly while
// Obsidian is open and none of it is content.
type Watcher struct {
	vault *vault.Vault
	fsw   *fsnotify.Watcher
	deb   *Debouncer
	log   *slog.Logger

	mu      sync.Mutex
	watched map[string]bool

	closeOnce sync.Once
	done      chan struct{}
}

// NewWatcher starts watching v. onChange is called with vault-relative paths
// once a burst of events has settled.
func NewWatcher(v *vault.Vault, quiet, maxDelay time.Duration, log *slog.Logger, onChange func(paths []string)) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create filesystem watcher: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}

	w := &Watcher{
		vault:   v,
		fsw:     fsw,
		log:     log,
		watched: map[string]bool{},
		done:    make(chan struct{}),
	}
	w.deb = NewDebouncer(quiet, maxDelay, onChange)

	if err := w.addTree(v.Dir()); err != nil {
		fsw.Close()
		return nil, err
	}
	go w.run()
	return w, nil
}

// addTree registers a watch on dir and every non-hidden directory beneath it.
func (w *Watcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished mid-walk is normal during a git
			// checkout; it is not a reason to fail startup.
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if rel := w.rel(p); rel != "" && vaultpath.IsHidden(rel) {
			return filepath.SkipDir
		}
		w.add(p)
		return nil
	})
}

func (w *Watcher) add(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.watched[dir] {
		return
	}
	if err := w.fsw.Add(dir); err != nil {
		w.log.Debug("could not watch directory", "dir", dir, "err", err)
		return
	}
	w.watched[dir] = true
}

// rel converts an absolute path from fsnotify into a vault-relative one.
func (w *Watcher) rel(abs string) string {
	r, err := filepath.Rel(w.vault.Dir(), abs)
	if err != nil || r == "." || strings.HasPrefix(r, "..") {
		return ""
	}
	return filepath.ToSlash(r)
}

// run pumps fsnotify events into the debouncer until Close.
func (w *Watcher) run() {
	defer close(w.done)
	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.log.Warn("filesystem watch error", "vault", w.vault.Dir(), "err", err)
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	rel := w.rel(ev.Name)
	if rel == "" || vaultpath.IsHidden(rel) {
		return
	}

	// A newly created directory needs its own watch, and anything already
	// inside it needs reporting: files can land before we get the watch on.
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			w.addTree(ev.Name)
			w.reportExisting(ev.Name)
			return
		}
	}
	if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		w.mu.Lock()
		delete(w.watched, ev.Name)
		w.mu.Unlock()
	}
	if !vaultpath.IsMarkdown(rel) {
		return
	}
	w.deb.Add(rel)
}

// reportExisting walks a directory that just appeared and reports its notes, so
// a `git checkout` that creates a folder full of files does not go unnoticed.
func (w *Watcher) reportExisting(dir string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel := w.rel(p); rel != "" && !vaultpath.IsHidden(rel) && vaultpath.IsMarkdown(rel) {
			w.deb.Add(rel)
		}
		return nil
	})
}

// Close stops watching and flushes any pending batch.
func (w *Watcher) Close() error {
	var err error
	w.closeOnce.Do(func() {
		err = w.fsw.Close()
		<-w.done
		w.deb.Close()
	})
	return err
}

// Wait blocks until the watcher has stopped, which happens on Close.
func (w *Watcher) Wait(ctx context.Context) error {
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
