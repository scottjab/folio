package e2e

// folio.go assembles the real folio server on a tsnet node.
//
// It mirrors cmd/folio's serve path minus three things: TLS (headscale cannot
// provision ts.net certificates, so the tests speak plain HTTP inside the
// WireGuard tunnel), the vault watchers and git history (they belong to the
// long-running process, not to what the identity tests exercise), and the
// embedded web app (the API tests cover static serving; the fallback shell
// keeps the build independent of web/dist).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/tsnet"

	"github.com/scottjab/folio/internal/client"
	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/httpapi"
	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/index"
	"github.com/scottjab/folio/internal/mcpsrv"
	"github.com/scottjab/folio/internal/notes"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/store"
	"github.com/scottjab/folio/internal/tsserve"
	"github.com/scottjab/folio/internal/vault"
)

// verbose turns tsnet's own chatty logs through to the test log. Set by the
// test binary's -verbose-tsnet flag.
var verbose bool

// folioServer is folio running on its own tsnet node.
type folioServer struct {
	t   *testing.T
	ts  *tsserve.Server
	api *httpapi.API
	srv *http.Server

	// url is the plain-HTTP base URL inside the tailnet, by MagicDNS name,
	// e.g. http://folio.tailnet.test:80.
	url string
	// ip is the node's tailnet address, for clients that dial by IP.
	ip netip.Addr
	// vaultsDir is the directory holding every user's vault, for assertions
	// on what actually reached disk.
	vaultsDir string
}

// folioOptions configures the folio node under test.
type folioOptions struct {
	// Hostname is the tsnet node name. Defaults to "folio", the production
	// default, so MagicDNS name resolution is exercised the way the TUI
	// exercises it.
	Hostname string
	// Agents maps node tags to the logins they may act as, straight from the
	// production config's agents table.
	Agents map[string]string
}

// startFolio boots folio on the tailnet the control server runs, registered to
// the user that authKey belongs to. It returns once the node is up and serving.
func startFolio(ctx context.Context, t *testing.T, ctrl *control, authKey string, opts folioOptions) *folioServer {
	t.Helper()

	hostname := opts.Hostname
	if hostname == "" {
		hostname = "folio"
	}

	stateDir := t.TempDir()
	vaultsDir := stateDir + "/vaults"

	ts, err := tsserve.Start(ctx, tsserve.Options{
		Hostname:   hostname,
		StateDir:   stateDir + "/tsnet",
		AuthKey:    authKey,
		ControlURL: ctrl.url,
		Addr:       fmt.Sprintf(":%d", folioPort),
		Log:        discardLogger(),
	})
	if err != nil {
		t.Fatalf("start folio tsnet node: %v", err)
	}

	db, err := store.Open(stateDir + "/folio.db")
	if err != nil {
		ts.Close()
		t.Fatalf("open folio database: %v", err)
	}
	idx := index.New(db)
	vaults := vault.NewSet(vaultsDir)
	shares := share.NewResolver(db)
	bus := events.NewBus()

	ident := identity.NewResolver(db, ts.WhoIsFunc(), identity.Options{
		// No cache: every request must resolve fresh, or a test that changes
		// what the tailnet says about a peer would read yesterday's answer.
		CacheTTL: 0,
		Agents:   opts.Agents,
	})
	notesSvc := notes.New(notes.Deps{
		DB: db, Index: idx, Vaults: vaults,
		Identity: ident, Shares: shares, Bus: bus,
	})

	mcp := mcpsrv.New(mcpsrv.Deps{
		Notes: notesSvc, Index: idx, Identity: ident,
		Shares: shares, Bus: bus, Log: discardLogger(),
	})
	api := httpapi.New(httpapi.Deps{
		DB: db, Index: idx, Notes: notesSvc, Vaults: vaults,
		Identity: ident, Shares: shares, Bus: bus, Log: discardLogger(),
		MCP: mcp.HTTPHandler(httpapi.UserFrom),
	})

	ln, err := ts.ListenPlain(fmt.Sprintf(":%d", folioPort))
	if err != nil {
		ts.Close()
		t.Fatalf("listen inside the tailnet: %v", err)
	}
	srv := &http.Server{Handler: api, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln) //nolint:errcheck // closed by cleanup

	ips := ts.IPs(ctx)
	if len(ips) == 0 {
		ts.Close()
		t.Fatal("folio node came up with no tailnet address")
	}

	f := &folioServer{
		t:         t,
		ts:        ts,
		api:       api,
		srv:       srv,
		url:       folioURL(hostname, folioPort),
		ip:        ips[0],
		vaultsDir: vaultsDir,
	}
	t.Cleanup(f.close)
	return f
}

func (f *folioServer) close() {
	f.api.Close()
	f.srv.Close()
	f.ts.Close()
}

// node is a tsnet node on the test tailnet standing in for a person's device:
// a browser, a terminal running folio tui, an MCP client.
type node struct {
	t  *testing.T
	ts *tsnet.Server
}

// joinNode registers a new tsnet node with the control server under authKey's
// user and returns once it is Running.
func joinNode(t *testing.T, ctrl *control, hostname, authKey string) *node {
	t.Helper()
	ts := &tsnet.Server{
		Hostname:   hostname,
		Dir:        t.TempDir(),
		ControlURL: ctrl.url,
		AuthKey:    authKey,
		Ephemeral:  true,
		Logf:       nodeLogf(t),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	st, err := ts.Up(ctx)
	if err != nil {
		ts.Close()
		t.Fatalf("join tailnet as %s: %v", hostname, err)
	}
	if len(st.TailscaleIPs) == 0 {
		ts.Close()
		t.Fatalf("%s joined with no tailnet address", hostname)
	}
	t.Cleanup(func() { ts.Close() })
	return &node{t: t, ts: ts}
}

// client builds a folio API client that dials the folio server over the
// tailnet, exactly as folio tui does.
func (n *node) client(f *folioServer) *client.Client {
	n.t.Helper()
	cl, err := client.New(f.url, client.WithHTTPClient(n.ts.HTTPClient()))
	if err != nil {
		n.t.Fatalf("build client for %s: %v", f.url, err)
	}
	return cl
}

// nodeLogf discards a node's logs unless verbose mode is on. Verbose output
// goes through the same guard as headscale's: tsnet goroutines can still be
// finishing a shutdown message after the test has ended, and a late t.Logf
// panics.
func nodeLogf(t *testing.T) func(format string, args ...any) {
	if !verbose {
		return func(string, ...any) {}
	}
	w := guardedLogWriter(t, t.Name())
	return func(format string, args ...any) {
		fmt.Fprintf(w, format, args...)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
