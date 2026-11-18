package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/scottjab/folio/internal/config"
	"github.com/scottjab/folio/internal/httpapi"
	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/mcpsrv"
	"github.com/scottjab/folio/internal/tsserve"
	"github.com/scottjab/folio/internal/web"
)

const shutdownGrace = 15 * time.Second

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("folio serve", flag.ContinueOnError)
	var verbose, allowStubUI bool
	cfg, err := loadConfig(fs, args, func(fs *flag.FlagSet, c *config.Config) {
		fs.BoolVar(&allowStubUI, "allow-stub-ui", false, "start even though no web app bundle was built in")
		fs.StringVar(&c.Hostname, "hostname", c.Hostname, "tailnet node name")
		fs.StringVar(&c.Addr, "addr", c.Addr, "listen address inside the tailnet")
		fs.BoolVar(&c.WatchExternal, "watch", c.WatchExternal, "notice edits made outside folio, such as in Obsidian")
		fs.BoolVar(&c.Git.Enabled, "git", c.Git.Enabled, "keep each vault in a git repository and commit changes to it")
		fs.BoolVar(&verbose, "verbose-tsnet", false, "pass tsnet's own logs through at info level")
	})
	if err != nil {
		return err
	}

	// Refuse to serve a stub UI rather than warning about it. A warning scrolls
	// past and the first anyone knows is a blank-looking app in the browser.
	if !allowStubUI {
		if err := web.CheckBuilt(); err != nil {
			return err
		}
	}

	log := newLogger(cfg.LogLevel)

	a, err := openApp(cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()

	ts, err := tsserve.Start(ctx, tsserve.Options{
		Hostname:   cfg.Hostname,
		StateDir:   cfg.TSNetDir(),
		AuthKey:    cfg.AuthKey,
		ControlURL: cfg.ControlURL,
		Addr:       cfg.Addr,
		PlainHTTP:  cfg.PlainHTTP,
		Log:        log,
		Verbose:    verbose,
	})
	if err != nil {
		return err
	}
	defer ts.Close()

	// History stops after the watchers, so an external edit the watcher flushes
	// on the way out still gets committed.
	hist := newVaultHistory(a, log)
	defer hist.stop()
	watchers := newVaultWatchers(ctx, a, log)
	defer watchers.stop()
	a.withIdentity(ts.WhoIsFunc(), func(u identity.User) {
		watchers.startFor(u)
		hist.startFor(u)
	})

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
	if err := hist.startAll(ctx); err != nil {
		return err
	}

	ln, err := listen(ts, cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: api,
		// A note can be large and a connection can be slow, but a request that
		// has not finished sending headers in 30s is not a real client.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		// No WriteTimeout: it would cut off the SSE stream, which is meant to
		// stay open indefinitely.
	}

	if !cfg.PlainHTTP {
		go serveRedirect(ctx, ts, cfg, log)
	}

	url := ts.URL(ctx)
	log.Info("folio is up", "url", url, "mcp", url+"/mcp", "state", cfg.StateDir)

	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		// End the SSE streams first, or Shutdown waits out the whole grace
		// period on connections that never close by themselves.
		api.Close()
		return tsserve.Shutdown(srv, shutdownGrace, log)
	}
}

// listen opens the app's own listener: HTTPS with the tailnet's certificate,
// or plain HTTP when the control server cannot issue one.
func listen(ts *tsserve.Server, cfg config.Config) (net.Listener, error) {
	if cfg.PlainHTTP {
		ln, err := ts.ListenPlain(cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("listen on %s: %w", cfg.Addr, err)
		}
		return ln, nil
	}
	return ts.ListenTLS()
}

// serveRedirect sends plain HTTP to HTTPS, so typing the bare hostname works.
func serveRedirect(ctx context.Context, ts *tsserve.Server, cfg config.Config, log *slog.Logger) {
	ln, err := ts.ListenPlain(":80")
	if err != nil {
		log.Debug("no plain HTTP listener; only HTTPS will work", "err", err)
		return
	}
	defer ln.Close()

	host := hostOf(ts.URL(ctx))
	srv := &http.Server{Handler: tsserve.RedirectToHTTPS(host), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	srv.Serve(ln)
}

func hostOf(url string) string {
	if h, ok := strings.CutPrefix(url, "https://"); ok {
		return h
	}
	return url
}
