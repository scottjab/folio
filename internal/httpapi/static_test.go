package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/scottjab/folio/internal/httpapi"
)

// pwaFS stands in for a real build: the files an installed folio needs, with
// contents nobody looks at. The default harness serves a one-page fallback, so
// without this there is nothing to ask for.
func pwaFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte("<!doctype html><title>folio</title>")},
		"app.js":               &fstest.MapFile{Data: []byte("export {}")},
		"app.css":              &fstest.MapFile{Data: []byte(".app{}")},
		"sw.js":                &fstest.MapFile{Data: []byte("self.addEventListener('fetch', () => {})")},
		"manifest.webmanifest": &fstest.MapFile{Data: []byte(`{"name":"folio"}`)},
		"icons/icon-192.png":   &fstest.MapFile{Data: []byte("\x89PNG\r\n\x1a\n")},
	}
}

func withPWA(d *httpapi.Deps) { d.Static = pwaFS() }

func TestManifestIsServedAsAManifest(t *testing.T) {
	// Go's own extension table has no entry for .webmanifest, so without the
	// override this is whatever the host's /etc/mime.types says, or a sniffed
	// text/plain. Chrome will then refuse to treat the app as installable.
	h := newHarness(t, withPWA)
	alice := h.as("100.64.0.1", aliceWho)

	resp := alice.do("GET", "/manifest.webmanifest", nil)
	wantStatus(t, resp, 200)
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("Content-Type = %q, want application/manifest+json", ct)
	}
}

func TestServiceWorkerIsRevalidatedEveryTime(t *testing.T) {
	// sw.js decides what an installed copy of folio has cached. If it is itself
	// cached, a deploy can never reach anyone who has already installed the app:
	// the stale worker keeps serving the stale bundle, with no way in.
	h := newHarness(t, withPWA)
	alice := h.as("100.64.0.1", aliceWho)

	resp := alice.do("GET", "/sw.js", nil)
	wantStatus(t, resp, 200)
	defer resp.Body.Close()

	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a javascript type", ct)
	}
}

func TestStaticFilesBeatTheSPAFallback(t *testing.T) {
	// A registration for /sw.js that came back as index.html would fail with a
	// MIME error rather than a 404, which is a much harder thing to recognise.
	h := newHarness(t, withPWA)
	alice := h.as("100.64.0.1", aliceWho)

	for _, path := range []string{"/sw.js", "/manifest.webmanifest", "/icons/icon-192.png", "/app.css"} {
		resp := alice.do("GET", path, nil)
		wantStatus(t, resp, 200)
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s came back as HTML (%q), so the SPA fallback swallowed it", path, ct)
		}
		resp.Body.Close()
	}
}

func TestTheOfflineShellIsServedWithoutARedirect(t *testing.T) {
	// The service worker precaches "/" and answers offline navigations with what
	// it stored there. If "/" redirected, the worker would cache a response
	// flagged as redirected, and browsers refuse to answer a navigation with one
	// of those: an installed folio would fail to open offline with a network
	// error instead of falling back to the shell.
	//
	// "/index.html" does redirect, which is exactly why the worker does not ask
	// for it. That is asserted here so the reason stays written down next to the
	// behaviour it depends on.
	h := newHarness(t, withPWA)
	alice := h.as("100.64.0.1", aliceWho)

	client := h.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	get := func(path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", h.srv.URL+path, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("X-Test-Peer", alice.ip)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	resp := get("/")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / = %d, want 200 with no redirect", resp.StatusCode)
	}

	redirected := get("/index.html")
	defer redirected.Body.Close()
	if redirected.StatusCode < 300 || redirected.StatusCode >= 400 {
		t.Errorf("GET /index.html = %d; if this stopped redirecting, the comment "+
			"in web/shell.mjs about why the worker precaches \"/\" is now wrong",
			redirected.StatusCode)
	}
}

func TestCSPAllowsTheServiceWorkerAndManifest(t *testing.T) {
	// Both would fall back to default-src today. They are named so that a later
	// change to default-src cannot silently decide where folio may install a
	// worker from.
	h := newHarness(t, withPWA)
	alice := h.as("100.64.0.1", aliceWho)

	resp := alice.do("GET", "/", nil)
	wantStatus(t, resp, http.StatusOK)
	defer resp.Body.Close()

	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"worker-src 'self'", "manifest-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q:\n%s", want, csp)
		}
	}
}
