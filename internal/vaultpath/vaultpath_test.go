package vaultpath_test

import (
	"strings"
	"testing"

	"github.com/scottjab/tsnotes/internal/vaultpath"
)

func TestClean(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{"simple", "note.md", "note.md", false},
		{"nested", "Daily/2026-08-30.md", "Daily/2026-08-30.md", false},
		{"leading slash stripped", "/Daily/x.md", "Daily/x.md", false},
		{"dot segments collapsed", "Daily/./x.md", "Daily/x.md", false},
		{"redundant slashes", "Daily//x.md", "Daily/x.md", false},
		{"trailing slash", "Daily/", "Daily", false},
		{"backslash converted", `Daily\x.md`, "Daily/x.md", false},

		{"empty", "", "", true},
		{"dot", ".", "", true},
		{"parent escape", "../etc/passwd", "", true},
		{"nested parent escape", "Daily/../../etc/passwd", "", true},
		{"parent in middle resolving out", "a/../../b", "", true},
		{"NUL byte", "no\x00pe.md", "", true},
		{"only slashes", "///", "", true},
		{"reserved dir tsnotes", ".tsnotes/tmp/x", "", true},
		{"newline", "a\nb.md", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := vaultpath.Clean(tt.in)
			if tt.err {
				if err == nil {
					t.Fatalf("Clean(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Clean(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("Clean(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCleanIsIdempotent(t *testing.T) {
	for _, in := range []string{"Daily/x.md", "/a//b/./c.md", `A\B\c.md`} {
		once, err := vaultpath.Clean(in)
		if err != nil {
			t.Fatalf("Clean(%q): %v", in, err)
		}
		twice, err := vaultpath.Clean(once)
		if err != nil {
			t.Fatalf("Clean(%q): %v", once, err)
		}
		if once != twice {
			t.Errorf("Clean not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}

func TestCleanNormalizesUnicode(t *testing.T) {
	// "é" as e + combining acute (NFD) must normalize to the NFC single rune,
	// so macOS-created files and Linux-created files land on the same key.
	nfd := "Café/note.md"
	nfc := "Café/note.md"

	got, err := vaultpath.Clean(nfd)
	if err != nil {
		t.Fatalf("Clean(NFD): %v", err)
	}
	if got != nfc {
		t.Errorf("Clean(NFD) = %q, want NFC %q", got, nfc)
	}
}

func TestIsMarkdown(t *testing.T) {
	tests := map[string]bool{
		"a.md":       true,
		"a.markdown": true,
		"A.MD":       true,
		"Daily/x.md": true,
		"a.txt":      false,
		"a":          false,
		"a.md.bak":   false,
	}
	for in, want := range tests {
		if got := vaultpath.IsMarkdown(in); got != want {
			t.Errorf("IsMarkdown(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestEnsureMarkdown(t *testing.T) {
	tests := map[string]string{
		"a":          "a.md",
		"a.md":       "a.md",
		"a.markdown": "a.markdown",
		"Daily/x":    "Daily/x.md",
		"a.txt":      "a.txt.md",
	}
	for in, want := range tests {
		if got := vaultpath.EnsureMarkdown(in); got != want {
			t.Errorf("EnsureMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTitleFor(t *testing.T) {
	tests := map[string]string{
		"Daily/2026-08-30.md":  "2026-08-30",
		"note.md":              "note",
		"a/b/My Note.markdown": "My Note",
		"noext":                "noext",
	}
	for in, want := range tests {
		if got := vaultpath.TitleFor(in); got != want {
			t.Errorf("TitleFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitAnchor(t *testing.T) {
	tests := []struct {
		in         string
		path, anch string
	}{
		{"Projects/x.md", "Projects/x.md", ""},
		{"Projects/x.md#Heading", "Projects/x.md", "Heading"},
		{"Projects/x#Some Heading", "Projects/x", "Some Heading"},
		{"a#b#c", "a", "b#c"}, // first # wins, rest is the anchor
		{"#OnlyAnchor", "", "OnlyAnchor"},
	}
	for _, tt := range tests {
		p, a := vaultpath.SplitAnchor(tt.in)
		if p != tt.path || a != tt.anch {
			t.Errorf("SplitAnchor(%q) = (%q, %q), want (%q, %q)", tt.in, p, a, tt.path, tt.anch)
		}
	}
}

func TestSlug(t *testing.T) {
	tests := map[string]string{
		"scottjab@github":      "scottjab-github",
		"alice@example.com":    "alice-example.com",
		"Bob Smith@passkey":    "bob-smith-passkey",
		"user@tailnet.ts.net":  "user-tailnet.ts.net",
		"UPPER@CASE":           "upper-case",
		"weird/../slashes":     "weird-slashes",
		"trailing---dashes---": "trailing-dashes",
	}
	for in, want := range tests {
		if got := vaultpath.Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlugIsStableAndSafe(t *testing.T) {
	for _, in := range []string{"scottjab@github", "a/b/c", "..", "", "!!!"} {
		s := vaultpath.Slug(in)
		if s == "" {
			t.Errorf("Slug(%q) is empty, must always produce something usable", in)
		}
		if strings.ContainsAny(s, `/\.`+"\x00") && !strings.Contains(in, ".") {
			t.Errorf("Slug(%q) = %q leaked a path separator", in, s)
		}
		if got := vaultpath.Slug(s); got != s {
			t.Errorf("Slug not idempotent: %q -> %q -> %q", in, s, got)
		}
	}
}

func TestIsHidden(t *testing.T) {
	tests := map[string]bool{
		".obsidian/app.json": true,
		".git/config":        true,
		".tsnotes/tmp/x":     true,
		"a/.hidden/b.md":     true,
		"Daily/x.md":         false,
		"a.md":               false,
		".dotfile.md":        true,
	}
	for in, want := range tests {
		if got := vaultpath.IsHidden(in); got != want {
			t.Errorf("IsHidden(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDir(t *testing.T) {
	tests := map[string]string{
		"Daily/2026-08-30.md": "Daily",
		"a/b/c.md":            "a/b",
		"top.md":              "",
	}
	for in, want := range tests {
		if got := vaultpath.Dir(in); got != want {
			t.Errorf("Dir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithin(t *testing.T) {
	tests := []struct {
		folder, path string
		want         bool
	}{
		{"Projects", "Projects/x.md", true},
		{"Projects", "Projects/sub/x.md", true},
		{"Projects", "Projects2/x.md", false}, // the classic prefix bug
		{"Projects", "Projects", true},
		{"", "anything.md", true}, // empty folder means the vault root
		{"a/b", "a/b/c/d.md", true},
		{"a/b", "a/bc/d.md", false},
	}
	for _, tt := range tests {
		if got := vaultpath.Within(tt.folder, tt.path); got != tt.want {
			t.Errorf("Within(%q, %q) = %v, want %v", tt.folder, tt.path, got, tt.want)
		}
	}
}
