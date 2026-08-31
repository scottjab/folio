// Package web embeds the built browser app so tsnotes ships as one binary.
//
// The bundle is a build artifact, not source. dist/ therefore holds only a
// .gitkeep in a clean checkout, which is enough for go:embed to compile, and a
// real build fills it in:
//
//	make build          # bundles the app, then compiles
//	nix build .#        # the same, hermetically
//	go generate ./...   # just the bundle, if you then want to use `go build`
//
// The stub page lives in stub.html, in the package source rather than in dist/,
// so a build can never clobber it and a checkout can never be missing it. That
// arrangement is deliberate: an earlier version kept the placeholder inside
// dist/, where `make build` overwrote it and left the repository holding a
// generated file that looked committed.
//
// Because a stub binary still compiles, [CheckBuilt] lets callers refuse to
// start rather than serve an app that is not there.
package web

//go:generate bash -c "cd ../../web && npm ci --no-audit --no-fund && npm run build && cp -r dist/. ../internal/web/dist/"

import (
	"embed"
	"errors"
	"io/fs"
	"testing/fstest"
)

//go:embed all:dist
var dist embed.FS

//go:embed stub.html
var stub []byte

// ErrNotBuilt means this binary carries the stub rather than the app.
var ErrNotBuilt = errors.New("the web app bundle was not built into this binary")

// FS returns the app's files.
//
// When no bundle was built in, it returns a one-page filesystem serving the stub
// instead, so the HTTP layer needs no special case: it always has an index.html
// to fall back to.
func FS() fs.FS {
	if IsPlaceholder() {
		return fstest.MapFS{"index.html": &fstest.MapFile{Data: stub}}
	}
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Only reachable if the embed directive and this path disagree, which
		// is a build-time mistake, not a runtime condition.
		panic("web: embedded dist is missing: " + err.Error())
	}
	return sub
}

// IsPlaceholder reports whether this binary was built without the app bundle.
//
// The test is whether the bundle itself is present. Judging by the size of
// index.html would not work: the real one is a handful of lines too, since all
// it does is load app.js.
func IsPlaceholder() bool {
	_, err := fs.Stat(dist, "dist/app.js")
	return err != nil
}

// CheckBuilt returns an error explaining how to fix a stub binary, or nil.
func CheckBuilt() error {
	if !IsPlaceholder() {
		return nil
	}
	return errors.New(`the web app bundle was not built into this binary, so the browser UI would be a stub page

Build it one of these ways:

    make build          bundle the app and compile, in one step
    nix build .#        the same, hermetically, into ./result/bin/tsnotes
    go generate ./...   just the bundle, if you then want to use 'go build'

The API and the MCP server work without it, so if that is genuinely what you
want, pass --allow-stub-ui to start anyway`)
}
