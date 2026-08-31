package web_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/scottjab/folio/internal/web"
)

func TestFSAlwaysHasAnIndex(t *testing.T) {
	// go:embed fails to compile on an empty directory, and the API's SPA
	// fallback reads index.html unconditionally. A committed placeholder keeps
	// both working without a node toolchain in the loop.
	if _, err := fs.ReadFile(web.FS(), "index.html"); err != nil {
		t.Fatalf("index.html must always be embedded: %v", err)
	}
}

func TestPlaceholderDetectionTracksTheBundle(t *testing.T) {
	// The stub and a real build both have a short index.html, so presence of the
	// bundle is what distinguishes them. Getting this backwards makes folio
	// lie about whether the UI will work.
	_, err := fs.Stat(web.FS(), "app.js")
	bundlePresent := err == nil

	if web.IsPlaceholder() == bundlePresent {
		t.Errorf("IsPlaceholder() = %v but app.js present = %v", web.IsPlaceholder(), bundlePresent)
	}
}

func TestCheckBuiltAgreesWithIsPlaceholder(t *testing.T) {
	err := web.CheckBuilt()
	if web.IsPlaceholder() != (err != nil) {
		t.Fatalf("CheckBuilt() = %v but IsPlaceholder() = %v", err, web.IsPlaceholder())
	}
	if err == nil {
		return
	}
	// The message is the whole point of this error: it is what someone sees
	// after a build that quietly did half the job.
	for _, want := range []string{"make build", "nix build", "go generate", "--allow-stub-ui"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CheckBuilt() should tell the reader about %q:\n%s", want, err)
		}
	}
}
