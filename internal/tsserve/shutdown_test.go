package tsserve_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/scottjab/folio/internal/tsserve"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// serveOn starts an http.Server on a loopback listener and returns both.
func serveOn(t *testing.T, h http.Handler) (*http.Server, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	return srv, ln
}

func TestShutdownReturnsImmediatelyWhenIdle(t *testing.T) {
	// The bug this guards: Shutdown used to wait on a context it was handed,
	// and the caller handed it context.Background(). Ctrl-C hung forever.
	srv, _ := serveOn(t, http.NotFoundHandler())

	done := make(chan error, 1)
	go func() { done <- tsserve.Shutdown(srv, 5*time.Second, quietLogger()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown blocked on an idle server; it must not wait for anything")
	}
}

func TestShutdownStopsAcceptingRequests(t *testing.T) {
	srv, ln := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := "http://" + ln.Addr().String() + "/"

	if resp, err := http.Get(url); err != nil {
		t.Fatalf("request before shutdown: %v", err)
	} else {
		resp.Body.Close()
	}

	if err := tsserve.Shutdown(srv, 5*time.Second, quietLogger()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if resp, err := http.Get(url); err == nil {
		resp.Body.Close()
		t.Error("the server still served a request after shutdown")
	}
}

func TestShutdownGivesInFlightRequestsTimeToFinish(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})

	srv, ln := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		close(finished)
	}))

	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started

	done := make(chan error, 1)
	go func() { done <- tsserve.Shutdown(srv, 5*time.Second, quietLogger()) }()

	// Shutdown must still be waiting: the request has not finished.
	select {
	case <-done:
		t.Fatal("Shutdown abandoned a request that was still running")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	<-finished

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not return after the request finished")
	}
}

func TestShutdownGivesUpAfterTheGracePeriod(t *testing.T) {
	// A handler that never returns must not wedge the process forever.
	block := make(chan struct{})
	defer close(block)

	srv, ln := serveOn(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block
	}))

	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- tsserve.Shutdown(srv, 300*time.Millisecond, quietLogger()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("Shutdown took %v; it should give up shortly after the grace period", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never gave up on a stuck handler")
	}
}

func TestRedirectToHTTPS(t *testing.T) {
	h := tsserve.RedirectToHTTPS("notes.example.ts.net")
	req := httptestRequest("GET", "http://notes/some/path?q=1")
	rec := &recorder{header: http.Header{}}
	h.ServeHTTP(rec, req)

	if rec.code != http.StatusMovedPermanently {
		t.Errorf("status = %d, want 301", rec.code)
	}
	if got := rec.header.Get("Location"); got != "https://notes.example.ts.net/some/path?q=1" {
		t.Errorf("Location = %q", got)
	}
}

func httptestRequest(method, url string) *http.Request {
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		panic(err)
	}
	return req
}

type recorder struct {
	header http.Header
	code   int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(code int)        { r.code = code }
