package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"uuid"

	"github.com/scottjab/folio/internal/config"
	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/index"
	"github.com/scottjab/folio/internal/notes"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/store"
	"github.com/scottjab/folio/internal/vault"
)

// app is everything the commands share: the database, the vaults, the index,
// and the services built on them.
//
// It is assembled without a tailnet so that the offline commands (index,
// doctor) work on a state directory without touching the network.
type app struct {
	cfg    config.Config
	log    *slog.Logger
	db     *store.DB
	index  *index.Index
	vaults *vault.Set
	shares *share.Resolver
	bus    *events.Bus
	notes  *notes.Service
	ident  *identity.Resolver
}

// openApp prepares the state directory and opens everything in it.
func openApp(cfg config.Config, log *slog.Logger) (*app, error) {
	for _, dir := range []string{cfg.StateDir, cfg.VaultsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	db, err := store.Open(cfg.DatabasePath())
	if err != nil {
		return nil, err
	}

	a := &app{
		cfg:    cfg,
		log:    log,
		db:     db,
		index:  index.New(db),
		vaults: vault.NewSet(cfg.VaultsDir()),
		shares: share.NewResolver(db),
		bus:    events.NewBus(),
	}
	a.bus.Log = log
	return a, nil
}

// withIdentity attaches an identity resolver. Commands that serve requests need
// one; the offline commands do not.
func (a *app) withIdentity(whois identity.WhoIsFunc, onNewUser func(identity.User)) {
	a.ident = identity.NewResolver(a.db, whois, identity.Options{
		CacheTTL: a.cfg.CacheTTL.Duration(),
		Agents:   a.cfg.AgentMap(),
		OnNewUser: func(_ context.Context, u identity.User) {
			if onNewUser != nil {
				onNewUser(u)
			}
		},
	})
	a.notes = notes.New(notes.Deps{
		DB: a.db, Index: a.index, Vaults: a.vaults,
		Identity: a.ident, Shares: a.shares, Bus: a.bus,
	})
}

// Close releases everything, in the reverse order it was opened.
func (a *app) Close() error {
	a.vaults.Close()
	return a.db.Close()
}

// vaultRow is one user's vault, as the offline commands see it.
type vaultRow struct {
	ID    int64  `db:"id"`
	Dir   string `db:"dir"`
	Login string `db:"login"`
}

// vaultRows lists every vault in the state directory.
func (a *app) vaultRows(ctx context.Context) ([]vaultRow, error) {
	return a.db.All[vaultRow](ctx, `
		SELECT v.id AS id, v.dir AS dir, u.login AS login
		FROM vaults v JOIN users u ON u.id = v.user_id
		ORDER BY u.login`)
}

// dirExists reports whether a vault's directory is actually on disk. A vault row
// with no directory means someone moved or restored the state dir by hand.
func (a *app) dirExists(dir string) bool {
	fi, err := os.Stat(filepath.Join(a.cfg.VaultsDir(), dir))
	return err == nil && fi.IsDir()
}

// newEventID returns a time-ordered id for an event, matching what the API uses
// so a browser can compare them.
func newEventID() string { return uuid.NewV7().String() }
