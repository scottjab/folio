package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/vault"
	"github.com/scottjab/folio/internal/watch"
)

// vaultWatchers keeps one filesystem watcher per vault, so edits made outside
// folio reach the index.
//
// Vaults are not all known at startup: one is created the first time a person
// opens folio. So this both scans for existing vaults and accepts new ones as
// they are provisioned. Without the second half, a new user's vault would go
// unwatched until the next restart, and their Obsidian edits would silently not
// be indexed.
type vaultWatchers struct {
	app *app
	log *slog.Logger
	ctx context.Context

	mu       sync.Mutex
	watching map[int64]*watch.Watcher
}

func newVaultWatchers(ctx context.Context, a *app, log *slog.Logger) *vaultWatchers {
	return &vaultWatchers{app: a, log: log, ctx: ctx, watching: map[int64]*watch.Watcher{}}
}

// startAll watches every vault that already exists.
func (w *vaultWatchers) startAll() error {
	rows, err := w.app.vaultRows(w.ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		w.start(row.ID, row.Dir, true)
	}
	return nil
}

// startFor watches a vault that has just been created.
func (w *vaultWatchers) startFor(u identity.User) {
	// A brand new vault is empty, so there is nothing to reconcile first.
	w.start(u.VaultID, u.VaultDir, false)
}

// start opens a watcher for one vault, optionally reconciling the index first.
func (w *vaultWatchers) start(vaultID int64, dir string, reconcile bool) {
	w.mu.Lock()
	if _, ok := w.watching[vaultID]; ok {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	v, err := w.app.vaults.Get(dir)
	if err != nil {
		w.log.Warn("cannot open vault to watch it", "vault", dir, "err", err)
		return
	}

	if reconcile {
		// The vault may have changed while folio was not running: a git pull,
		// a restore, an afternoon in Obsidian.
		if stats, err := w.app.index.Sync(w.ctx, vaultID, v); err != nil {
			w.log.Warn("initial index sync failed", "vault", dir, "err", err)
		} else if stats.Added+stats.Updated+stats.Removed > 0 {
			w.log.Info("caught the index up with the files", "vault", dir,
				"added", stats.Added, "updated", stats.Updated, "removed", stats.Removed)
		}
	}

	watcher, err := watch.NewWatcher(v, watch.DefaultQuiet, watch.DefaultMaxDelay, w.log,
		func(paths []string) { w.onChange(vaultID, dir, v, paths) })
	if err != nil {
		w.log.Warn("cannot watch vault for external edits", "vault", dir, "err", err)
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	// Another goroutine may have won the race while we were opening.
	if _, ok := w.watching[vaultID]; ok {
		watcher.Close()
		return
	}
	w.watching[vaultID] = watcher
	w.log.Debug("watching vault for external edits", "vault", dir)
}

// onChange reindexes the paths a burst of filesystem events settled on.
func (w *vaultWatchers) onChange(vaultID int64, dir string, v *vault.Vault, paths []string) {
	stats, err := w.app.index.SyncPaths(w.ctx, vaultID, v, paths)
	if err != nil {
		w.log.Warn("reindexing external changes failed", "vault", dir, "err", err)
		return
	}
	if stats.Added+stats.Updated+stats.Removed == 0 {
		return // our own write coming back around, already indexed
	}
	w.log.Debug("reindexed external changes", "vault", dir,
		"added", stats.Added, "updated", stats.Updated, "removed", stats.Removed)

	// Tell open browser tabs and any subscribed agent. ByLogin is deliberately
	// empty, which is how the UI tells "you saved this" from "something else
	// changed this file".
	for _, p := range paths {
		w.app.bus.Emit(w.ctx, events.NoteChanged{
			ID: newEventID(), Kind: events.NoteUpdated,
			VaultID: vaultID, Vault: dir, Path: p, At: time.Now(),
		})
	}
}

// stop closes every watcher.
func (w *vaultWatchers) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, watcher := range w.watching {
		watcher.Close()
	}
	clear(w.watching)
}
