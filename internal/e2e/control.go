package e2e

// control.go runs headscale as a subprocess: the real control server, the one
// nixpkgs ships, driven over its unix-socket CLI.
//
// It does not link headscale as a library on purpose. folio's store and
// headscale's gorm layer use two different pure-Go SQLite drivers that both
// call sql.Register("sqlite") in their init, so the two can never live in one
// binary. A subprocess keeps folio's dependency tree clean and tests the
// released control server rather than a compiled-from-source one.
//
// The server gets a temp config per test: SQLite in a temp dir, MagicDNS on a
// reserved .test domain, and a DERP map pointing at the local DERP and STUN
// servers this file also starts, so every packet stays on the machine.

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/derp/derpserver"
	"tailscale.com/net/stun/stuntest"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"
)

// magicDNSDomain is the MagicDNS suffix the test tailnet advertises, so node
// names resolve as <hostname>.tailnet.test. .test is reserved for exactly
// this, and nothing here wants a name that could leak into a real resolver.
const magicDNSDomain = "tailnet.test"

// folioPort is the plain-HTTP port folio's test node serves on, inside the
// tailnet. tsnet listens in userspace, so 80 needs no privilege.
const folioPort = 80

// control is a running headscale server with its own DERP relay and STUN
// server, everything on loopback. One control serves one test: it has a fresh
// database, so user ids never carry between tests.
type control struct {
	t   *testing.T
	url string // where tsnet nodes find the control plane
	dir string
	bin string

	derpSrv     *derpserver.Server
	derpHTTP    *httptest.Server
	stunCleanup func()

	serveCmd   *exec.Cmd
	servedDone chan error
}

// newControl starts headscale plus the local DERP and STUN servers its clients
// should use. Skips the test in -short mode: bringing up tsnet nodes takes
// seconds, not milliseconds. Skips, too, when there is no headscale binary;
// the dev shell provides one.
func newControl(t *testing.T) *control {
	t.Helper()
	if testing.Short() {
		t.Skip("tailnet integration test skipped in -short mode")
	}
	bin, err := exec.LookPath("headscale")
	if err != nil {
		t.Skip("no headscale binary on PATH; enter the dev shell (nix develop) to run the tailnet integration tests")
	}

	dir := t.TempDir()

	// STUN and DERP first: the control server hands every node a DERP map
	// pointing at them, and without a relay two nodes that cannot form a
	// direct connection (some CI has no usable UDP) would never meet.
	stunAddr, stunCleanup := stuntest.ServeWithPacketListener(t, nettype.Std{})

	derpSrv := derpserver.New(key.NewNode(), logger.Discard)
	derpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for DERP: %v", err)
	}
	derpHTTP := httptest.NewUnstartedServer(
		derpserver.AddWebSocketSupport(derpSrv, derpserver.Handler(derpSrv)))
	derpHTTP.Listener.Close()
	derpHTTP.Listener = derpLn
	// No HTTP/2: a hijacked DERP upgrade stream and h2 do not get along, and
	// tailscale's own test DERP server disables it for the same reason.
	derpHTTP.Config.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	derpHTTP.StartTLS()
	derpPort := derpLn.Addr().(*net.TCPAddr).Port

	// The DERP map headscale hands to every joining node, in the YAML form
	// headscale's derp.paths files use. InsecureForTests is the
	// tailscale-sanctioned way for a client to accept the relay's
	// self-signed certificate.
	derpMapFile := filepath.Join(dir, "derp.yaml")
	derpYAML := fmt.Sprintf(`regions:
  1:
    regionid: 1
    regioncode: test
    regionname: Test
    nodes:
      - name: 1a
        regionid: 1
        hostname: 127.0.0.1
        ipv4: 127.0.0.1
        ipv6: none
        stunport: %d
        stunonly: false
        derpport: %d
        stuntestip: 127.0.0.1
        insecurefortests: true
`, stunAddr.Port, derpPort)
	if err := os.WriteFile(derpMapFile, []byte(derpYAML), 0o600); err != nil {
		t.Fatalf("write DERP map file: %v", err)
	}

	// A port for the control plane itself. Nothing else will take it in the
	// microseconds between closing our probe and headscale binding it.
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick control port: %v", err)
	}
	httpPort := httpLn.Addr().(*net.TCPAddr).Port
	httpLn.Close()

	sock := filepath.Join(dir, "headscale.sock")
	cfg := fmt.Sprintf(`server_url: http://127.0.0.1:%d
listen_addr: 127.0.0.1:%d
metrics_listen_addr: ""
grpc_listen_addr: 127.0.0.1:0
disable_check_updates: true
node:
  ephemeral:
    # headscale refuses anything below 1m5s; two minutes is fine, tests are
    # shorter than that.
    inactivity_timeout: 2m
noise:
  private_key_path: %s
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential
derp:
  server:
    enabled: false
  urls: []
  paths:
    - %s
  auto_update_enabled: false
database:
  type: sqlite
  sqlite:
    path: %s
    write_ahead_log: false
policy:
  mode: database
dns:
  magic_dns: true
  base_domain: %s
  override_local_dns: false
  nameservers:
    global:
      - 127.0.0.1
log:
  format: text
  level: warn
unix_socket: %s
`, httpPort, httpPort, filepath.Join(dir, "noise_private.key"),
		derpMapFile, filepath.Join(dir, "headscale.db"), magicDNSDomain, sock)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write headscale config: %v", err)
	}

	serve := exec.Command(bin, "serve", "--config", cfgPath)
	serve.Stdout = controlLogf(t)
	serve.Stderr = serve.Stdout
	if err := serve.Start(); err != nil {
		t.Fatalf("start headscale: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- serve.Wait() }()

	// Wait for both ends the clients and the CLI need.
	deadline := time.Now().Add(30 * time.Second)
	for {
		tcpOK := dialOK("tcp", "127.0.0.1:"+strconv.Itoa(httpPort))
		unixOK := dialOK("unix", sock)
		if tcpOK && unixOK {
			break
		}
		select {
		case err := <-serveDone:
			t.Fatalf("headscale exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			serve.Process.Kill()
			t.Fatal("headscale did not start listening within 30s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	c := &control{
		t:           t,
		url:         "http://127.0.0.1:" + strconv.Itoa(httpPort),
		dir:         dir,
		bin:         bin,
		derpSrv:     derpSrv,
		derpHTTP:    derpHTTP,
		stunCleanup: stunCleanup,
		serveCmd:    serve,
		servedDone:  serveDone,
	}
	t.Cleanup(c.close)
	return c
}

// dialOK reports whether something is listening.
func dialOK(network, addr string) bool {
	conn, err := net.DialTimeout(network, addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// controlLogf pipes headscale's own output into the test log only in verbose
// mode; it is chatty and rarely the interesting part of a failure.
func controlLogf(t *testing.T) io.Writer {
	if !verbose {
		return io.Discard
	}
	return guardedLogWriter(t, "headscale")
}

// guardedLogWriter returns a writer that puts lines into the test log while
// the test runs. After the test is over it goes quiet instead of panicking:
// headscale and tsnet goroutines can emit for a moment after cleanup starts,
// and testing.T panics on a Log that arrives too late.
func guardedLogWriter(t *testing.T, prefix string) logWriter {
	var done atomic.Bool
	// Cleanup order is LIFO, so this runs after every cleanup registered
	// later in the test, including node shutdowns.
	t.Cleanup(func() { done.Store(true) })
	return logWriter{t: t, prefix: prefix, done: &done}
}

type logWriter struct {
	t      *testing.T
	prefix string
	done   *atomic.Bool
}

func (w logWriter) Write(p []byte) (int, error) {
	if w.done.Load() {
		return len(p), nil
	}
	defer func() { recover() }() //nolint:errcheck // quiet, not silent
	w.t.Logf("%s: %s", w.prefix, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func (c *control) close() {
	if c.serveCmd.Process != nil {
		c.serveCmd.Process.Kill()
		<-c.servedDone //nolint:errcheck // killed on purpose
	}
	c.derpHTTP.CloseClientConnections()
	c.derpHTTP.Close()
	c.derpSrv.Close()
	c.stunCleanup()
}

// hs runs a headscale CLI command against this server and returns its stdout.
func (c *control) hs(args ...string) string {
	c.t.Helper()
	full := append([]string{"--config", filepath.Join(c.dir, "config.yaml")}, args...)
	cmd := exec.Command(c.bin, full...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		c.t.Fatalf("headscale %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return out.String()
}

// hsUser is a person on the test tailnet.
type hsUser struct {
	ctrl *control
	id   uint64
	name string
}

// user joins a person to the tailnet, ready to hand out preauth keys.
func (c *control) user(name string) *hsUser {
	c.t.Helper()
	out := c.hs("users", "create", name, "--output", "json")
	var created struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &created); err != nil {
		c.t.Fatalf("parse headscale users create output %q: %v", out, err)
	}
	id, err := created.ID.Int64()
	if err != nil {
		c.t.Fatalf("headscale user id %q is not a number: %v", created.ID, err)
	}
	return &hsUser{ctrl: c, id: uint64(id), name: name}
}

// key returns a reusable preauth key: every node registered with it belongs to
// this user.
func (h *hsUser) key() string {
	h.ctrl.t.Helper()
	return h.keyOpts(false, nil)
}

// taggedKey returns a reusable preauth key whose nodes carry ACL tags, which is
// how a headless machine joins without a person behind it.
func (h *hsUser) taggedKey(tags ...string) string {
	h.ctrl.t.Helper()
	return h.keyOpts(false, tags)
}

// ephemeralKey returns a preauth key whose nodes vanish when they disconnect.
func (h *hsUser) ephemeralKey() string {
	h.ctrl.t.Helper()
	return h.keyOpts(true, nil)
}

func (h *hsUser) keyOpts(ephemeral bool, tags []string) string {
	h.ctrl.t.Helper()
	args := []string{"preauthkeys", "create", "--user", strconv.FormatUint(h.id, 10),
		"--reusable", "--output", "json"}
	if ephemeral {
		args = append(args, "--ephemeral")
	}
	for _, tag := range tags {
		args = append(args, "--tags", tag)
	}
	out := h.ctrl.hs(args...)
	var created struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &created); err != nil {
		h.ctrl.t.Fatalf("parse preauthkey output %q: %v", out, err)
	}
	if created.Key == "" {
		h.ctrl.t.Fatalf("headscale returned an empty preauth key (output %q)", out)
	}
	return created.Key
}

// folioURL is where a node named hostname can be reached over this tailnet by
// its MagicDNS name.
func folioURL(hostname string, port int) string {
	return fmt.Sprintf("http://%s.%s:%d", hostname, magicDNSDomain, port)
}
