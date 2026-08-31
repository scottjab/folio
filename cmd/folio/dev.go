package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/scottjab/folio/internal/config"
	"github.com/scottjab/folio/internal/httpapi"
	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/mcpsrv"
	"github.com/scottjab/folio/internal/tsserve"
	"github.com/scottjab/folio/internal/web"
)

// runDev starts folio without a tailnet.
//
// Everything except identity behaves exactly as it does in production: the same
// API, the same MCP server, the same editor, the same files on disk. Only the
// "who is asking" step is replaced, because that is the one part that needs a
// tailnet to answer.
func runDev(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("folio dev", flag.ContinueOnError)
	var allowStubUI bool
	cfg, err := loadConfig(fs, args, func(fs *flag.FlagSet, c *config.Config) {
		fs.StringVar(&c.DevAddr, "addr", "127.0.0.1:8080", "loopback address to listen on")
		fs.StringVar(&c.DevLogin, "login", defaultDevLogin(), "the login every request is treated as")
		fs.BoolVar(&allowStubUI, "allow-stub-ui", false, "start even though no web app bundle was built in")
	})
	if err != nil {
		return err
	}
	// The state directory defaults next to the source rather than in the real
	// one, so experimenting here cannot touch notes you care about.
	if !flagWasSet(fs, "state") && os.Getenv("FOLIO_STATE_DIR") == "" {
		cfg.StateDir = "./dev-state"
	}
	if !allowStubUI {
		if err := web.CheckBuilt(); err != nil {
			return err
		}
	}
	return serveDev(ctx, cfg, newLogger(cfg.LogLevel))
}

// flagWasSet reports whether the user passed a flag, as opposed to it holding
// its default.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// defaultDevLogin makes the fake identity look like a real tailnet login, so
// vault directory names and share behaviour match what you would see for real.
func defaultDevLogin() string {
	name := "dev"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return name + "@localhost"
}

// serveDev runs folio on a local address with a fixed identity, bypassing the
// tailnet entirely.
//
// This exists so the app can be worked on without a tailnet auth key, and so
// there is a way to smoke-test the whole stack. It is emphatically not a
// deployment mode: anything that can reach the port is the configured user, so
// it binds to loopback and says so loudly on startup.
func serveDev(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	a, err := openApp(cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()

	watchers := newVaultWatchers(ctx, a, log)
	defer watchers.stop()

	// A fixed peer: whoever connects is the configured dev login.
	a.withIdentity(func(context.Context, string) (identity.WhoIs, error) {
		return identity.WhoIs{
			UserID:      1,
			Login:       cfg.DevLogin,
			DisplayName: cfg.DevLogin,
		}, nil
	}, watchers.startFor)

	mcp := mcpsrv.New(mcpsrv.Deps{
		Notes: a.notes, Index: a.index, Identity: a.ident,
		Shares: a.shares, Bus: a.bus, Log: log,
	})
	api := httpapi.New(httpapi.Deps{
		DB: a.db, Index: a.index, Notes: a.notes, Vaults: a.vaults,
		Identity: a.ident, Shares: a.shares, Bus: a.bus, Log: log,
		Static: web.FS(),
		MCP:    mcp.HTTPHandler(httpapi.UserFrom),
	})

	if cfg.WatchExternal {
		if err := watchers.startAll(); err != nil {
			return err
		}
	}

	ln, err := net.Listen("tcp", cfg.DevAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.DevAddr, err)
	}
	if !isLoopback(ln.Addr()) {
		ln.Close()
		return fmt.Errorf("dev mode refuses to listen on %s: it authenticates every request as %q, "+
			"so it must stay on loopback", cfg.DevAddr, cfg.DevLogin)
	}

	srv := &http.Server{Handler: api, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 5 * time.Minute}

	fmt.Fprintf(os.Stderr, strings.Join([]string{
		"",
		"  folio dev  ->  http://%s",
		"",
		"  Every request is treated as %s. There is no tailnet check, which is why",
		"  this refuses to listen anywhere but loopback. Do not run it as a server.",
		"",
		"  State: %s",
		"",
		"",
	}, "\n"), ln.Addr().String(), cfg.DevLogin, cfg.StateDir)

	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		api.Close()
		return tsserve.Shutdown(srv, shutdownGrace, log)
	}
}

// isLoopback reports whether an address is safely local.
func isLoopback(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
