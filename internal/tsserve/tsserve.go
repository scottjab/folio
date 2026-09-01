// Package tsserve puts folio on the tailnet.
//
// It owns the tsnet node and the one adapter between Tailscale's world and ours:
// turning a WhoIs response into an [identity.WhoIs]. Confining the tailscale
// dependency to this package is what lets every other package, including the
// whole HTTP and MCP surface, be tested without a tailnet.
package tsserve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"github.com/scottjab/folio/internal/identity"
)

// Options configures the tailnet node.
type Options struct {
	// Hostname becomes the node name, so the app is reachable at
	// https://<hostname>.<tailnet>.ts.net.
	Hostname string
	// StateDir holds the node key and other tsnet state. It must persist, or
	// every restart registers a new node.
	StateDir string
	// AuthKey is used only on first run. Later starts reuse the stored node key.
	AuthKey string
	// Addr is the TLS listen address inside the tailnet.
	Addr string
	Log  *slog.Logger
	// Verbose passes tsnet's own chatty logs through. Off by default because
	// they are voluminous and rarely what you want.
	Verbose bool
}

// Server is a running tailnet node with an HTTP server on it.
type Server struct {
	ts   *tsnet.Server
	lc   *local.Client
	log  *slog.Logger
	opts Options
}

// Start brings up the tailnet node and blocks until it is usable.
func Start(ctx context.Context, opts Options) (*Server, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Addr == "" {
		opts.Addr = ":443"
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create tsnet state dir: %w", err)
	}

	ts := &tsnet.Server{
		Hostname: opts.Hostname,
		Dir:      opts.StateDir,
		AuthKey:  opts.AuthKey,
		// tsnet logs every DERP and netcheck detail at info. Route it to debug
		// unless asked, so folio' own logs stay readable.
		Logf: func(format string, args ...any) {
			opts.Log.Debug("tsnet: " + strings.TrimSpace(fmt.Sprintf(format, args...)))
		},
	}
	if opts.Verbose {
		ts.Logf = func(format string, args ...any) {
			opts.Log.Info("tsnet: " + strings.TrimSpace(fmt.Sprintf(format, args...)))
		}
	}

	opts.Log.Info("connecting to the tailnet", "hostname", opts.Hostname, "state", opts.StateDir)
	status, err := ts.Up(ctx)
	if err != nil {
		ts.Close()
		return nil, fmt.Errorf("bring up tailnet node: %w", authKeyHint(err, opts.AuthKey))
	}

	lc, err := ts.LocalClient()
	if err != nil {
		ts.Close()
		return nil, fmt.Errorf("open local API client: %w", err)
	}

	s := &Server{ts: ts, lc: lc, log: opts.Log, opts: opts}
	if status != nil && status.Self != nil {
		opts.Log.Info("on the tailnet",
			"name", strings.TrimSuffix(status.Self.DNSName, "."),
			"addrs", status.TailscaleIPs)
	}
	return s, nil
}

// authKeyHint turns the unhelpful failure you get on a first run with no auth
// key into something actionable.
func authKeyHint(err error, authKey string) error {
	if authKey == "" {
		return fmt.Errorf("%w\n\nThis looks like a first run with no auth key. Either set TS_AUTHKEY to a "+
			"Tailscale auth key, or watch the logs for a login URL and open it", err)
	}
	return err
}

// WhoIsFunc adapts tailscaled's WhoIs to the identity package's view of a peer.
//
// This is the only place tailcfg types appear outside this package. Everything
// downstream sees a plain struct, which is why identity, sharing, and the whole
// API can be tested with a two-line fake.
func (s *Server) WhoIsFunc() identity.WhoIsFunc {
	return func(ctx context.Context, remoteAddr string) (identity.WhoIs, error) {
		resp, err := s.lc.WhoIs(ctx, remoteAddr)
		if err != nil {
			return identity.WhoIs{}, err
		}

		var out identity.WhoIs
		if resp.Node != nil {
			out.NodeName = strings.TrimSuffix(resp.Node.Name, ".")
			out.Tags = resp.Node.Tags
		}
		if resp.UserProfile != nil {
			out.UserID = int64(resp.UserProfile.ID)
			out.Login = resp.UserProfile.LoginName
			out.DisplayName = resp.UserProfile.DisplayName
			out.ProfilePic = resp.UserProfile.ProfilePicURL
		}
		// A tagged node reports a synthetic user profile. Treating it as a
		// person would give an unattended CI box someone's notes, so the tag
		// list is what decides, not the profile.
		if len(out.Tags) > 0 {
			out.UserID, out.Login = 0, ""
		}
		return out, nil
	}
}

// ListenTLS returns a listener serving the tailnet's own HTTPS certificate.
func (s *Server) ListenTLS() (net.Listener, error) {
	ln, err := s.ts.ListenTLS("tcp", s.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w\n\nHTTPS on a tailnet needs MagicDNS and HTTPS certificates "+
			"enabled for the tailnet, under DNS in the admin console", s.opts.Addr, err)
	}
	return ln, nil
}

// ListenPlain returns a plain HTTP listener, used only to redirect to HTTPS.
func (s *Server) ListenPlain(addr string) (net.Listener, error) {
	return s.ts.Listen("tcp", addr)
}

// URL is where the app can be reached.
func (s *Server) URL(ctx context.Context) string {
	status, err := s.lc.StatusWithoutPeers(ctx)
	if err != nil || status.Self == nil {
		return "https://" + s.opts.Hostname
	}
	return "https://" + strings.TrimSuffix(status.Self.DNSName, ".")
}

// Close shuts the node down.
func (s *Server) Close() error { return s.ts.Close() }

// RedirectToHTTPS is the handler for the plain-HTTP listener. Anyone who types
// the bare hostname should land on the app rather than a connection reset.
func RedirectToHTTPS(host string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}

// Shutdown stops srv, giving requests already in flight a grace period to
// finish before the process exits.
//
// It does not wait for anything first: the caller decides when shutdown starts.
// An earlier version took a context and blocked on it, which meant passing
// context.Background() here hung forever and Ctrl-C did nothing at all.
//
// Callers serving open-ended responses (server-sent events, most obviously) must
// end those before calling this, or Shutdown waits out the whole grace period on
// connections that were never going to close on their own.
func Shutdown(srv *http.Server, grace time.Duration, log *slog.Logger) error {
	log.Info("shutting down", "grace", grace)

	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	err := srv.Shutdown(ctx)
	switch {
	case err == nil:
		log.Info("stopped cleanly")
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		// Requests were still running when the grace period ran out. Say so and
		// stop anyway rather than hanging.
		log.Warn("grace period expired with requests still in flight; closing anyway", "grace", grace)
		return srv.Close()
	default:
		return fmt.Errorf("graceful shutdown: %w", err)
	}
}

// StateDirFor keeps the tsnet state next to everything else in the state dir.
func StateDirFor(stateDir string) string { return filepath.Join(stateDir, "tsnet") }

// MagicDNSSuffix asks the tailscaled already running on this machine what its
// tailnet's MagicDNS suffix is, for example "tail1234.ts.net".
//
// This is the client half of the package: it starts no node, it asks the daemon
// the user is already running. It is how `folio tui` works out that a server
// called "folio" lives at https://folio.<suffix> without being told. An error
// means there is no tailnet to ask, which the caller should treat as "guess"
// rather than as a failure.
func MagicDNSSuffix(ctx context.Context) (string, error) {
	var lc local.Client
	// Without peers: this is a one-field question, and a large tailnet's full
	// status is a lot of JSON to marshal for it.
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return "", fmt.Errorf("asking tailscaled about this tailnet: %w", err)
	}
	if st.CurrentTailnet == nil || st.CurrentTailnet.MagicDNSSuffix == "" {
		return "", errors.New("this machine is not on a tailnet, or MagicDNS is off")
	}
	return strings.Trim(st.CurrentTailnet.MagicDNSSuffix, "."), nil
}
