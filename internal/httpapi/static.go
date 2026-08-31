package httpapi

import (
	"io/fs"
	"net/http"
	"strings"
	"testing/fstest"
)

// staticHandler serves the built web app with a single-page-app fallback.
//
// Anything that is not a real file falls back to index.html so a client-side
// route survives a refresh or a pasted link. /api/ is excluded from that
// fallback by the router, so a mistyped endpoint returns a JSON 404 rather than
// a page of HTML that a fetch() then fails to parse.
func (a *API) staticHandler() http.Handler {
	files := http.FileServerFS(a.Static)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if f, err := a.Static.Open(p); err == nil {
			f.Close()
			// The bundle is content-hashed at build time, so it can be cached
			// hard. index.html must not be, or a deploy never reaches anyone.
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}
		a.serveIndex(w, r)
	})
}

func (a *API) serveIndex(w http.ResponseWriter, r *http.Request) {
	body, err := fs.ReadFile(a.Static, "index.html")
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(body)
}

// fallbackApp is served when no built frontend was supplied. It keeps the API
// runnable and testable without a node toolchain, and tells anyone who lands on
// it what happened rather than showing a blank page.
func fallbackApp() fs.FS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>tsnotes</title></head>
<body>
<h1>tsnotes</h1>
<p>The API is running, but no web bundle was built into this binary.</p>
<p>Build it with <code>nix build .#</code>, or run <code>npm run build</code> in <code>web/</code>.</p>
</body>
</html>
`)},
	}
}
