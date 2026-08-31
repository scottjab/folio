package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The renderer's job is to put markdown on a fixed-width screen. Two things can
// go wrong: the text comes out wrong, and the width comes out wrong. The width
// is the one that breaks the layout, so it is checked on everything.

func testRenderer(width int) renderer {
	return renderer{st: newStyles(), width: width}
}

// plainLines renders and strips the styling, which is what these assertions are
// about.
func plainLines(t *testing.T, md string, width int) []string {
	t.Helper()
	lines := testRenderer(width).render(md)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ansi.Strip(l)
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("line %d is %d columns wide in a %d column pane: %q", i, w, width, out[i])
		}
	}
	return out
}

func TestRenderHeadings(t *testing.T) {
	got := plainLines(t, "# One\n## Two\n### Three\n#NotAHeading\n", 40)
	want := []string{"One", "Two", "Three", "#NotAHeading"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("line %d = %q, want %q", i, got[i], w)
		}
	}
}

func TestRenderInlineMarkersAreConsumed(t *testing.T) {
	tests := []struct {
		md   string
		want string
	}{
		{"**bold** text", "bold text"},
		{"*italic* text", "italic text"},
		{"__also bold__", "also bold"},
		{"~~gone~~", "gone"},
		{"a `code span` here", "a code span here"},
		{"see [the docs](https://example.com)", "see the docs"},
		{"link to [[Projects/folio]]", "link to [[Projects/folio]]"},
		{"link to [[Projects/folio|the indexer]]", "link to [[the indexer]]"},
		{"anchor [[Note#Heading]]", "anchor [[Note]]"},
		{"embed ![[diagram.png]]", "embed ![[diagram.png]]"},
		{"tagged #go127 here", "tagged #go127 here"},
		// The things that must not be treated as markers.
		{"snake_case_name stays", "snake_case_name stays"},
		{"issue #1 is a number, not a tag", "issue #1 is a number, not a tag"},
		{"2 * 3 * 4", "2 * 3 * 4"},
		{`escaped \*not italics\*`, "escaped *not italics*"},
		{"`**not bold in code**`", "**not bold in code**"},
	}
	for _, tc := range tests {
		got := plainLines(t, tc.md, 60)
		if len(got) == 0 || got[0] != tc.want {
			t.Errorf("render(%q) = %q, want %q", tc.md, got, tc.want)
		}
	}
}

func TestRenderStylesTheRightRuns(t *testing.T) {
	st := newStyles()
	r := renderer{st: st, width: 60}

	segs := r.segments("plain **bold** and #tag and [[link]]")
	var styled []string
	for _, s := range segs {
		if s.style != nil {
			styled = append(styled, s.text)
		}
	}
	want := []string{"bold", "#tag", "[[link]]"}
	if len(styled) != len(want) {
		t.Fatalf("styled runs = %v, want %v", styled, want)
	}
	for i := range want {
		if styled[i] != want[i] {
			t.Errorf("styled run %d = %q, want %q", i, styled[i], want[i])
		}
	}
}

func TestRenderLists(t *testing.T) {
	got := plainLines(t, "- one\n- [ ] todo\n- [x] done\n1. first\n  - nested\n", 40)
	want := []string{"• one", "☐ todo", "☑ done", "1. first", "  • nested"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("line %d = %q, want %q", i, got[i], w)
		}
	}
}

// A wrapped list item must line up under its own text, not under the bullet.
func TestRenderListHangingIndent(t *testing.T) {
	got := plainLines(t, "- a bullet whose text is long enough to wrap twice over\n", 24)
	if len(got) < 2 {
		t.Fatalf("expected the item to wrap: %q", got)
	}
	if !strings.HasPrefix(got[0], "• ") {
		t.Errorf("first line = %q, want it to start with the bullet", got[0])
	}
	if !strings.HasPrefix(got[1], "  ") || strings.HasPrefix(strings.TrimSpace(got[1]), "•") {
		t.Errorf("continuation = %q, want it indented under the text", got[1])
	}
}

func TestRenderCodeBlockIsNotReflowed(t *testing.T) {
	md := "```go\nfunc main() { println(\"hi\") }\n```\n"
	got := plainLines(t, md, 60)
	if got[0] != "┌ go" {
		t.Errorf("fence header = %q", got[0])
	}
	if !strings.Contains(got[1], `func main() { println("hi") }`) {
		t.Errorf("code line = %q, want it verbatim", got[1])
	}
	if got[2] != "└" {
		t.Errorf("fence footer = %q, want the closing gutter", got[2])
	}

	// Markers inside a fence are code, not markup.
	got = plainLines(t, "```\n**not bold** and #nottag\n```\n", 60)
	if !strings.Contains(got[1], "**not bold** and #nottag") {
		t.Errorf("code line = %q, want the markers left alone", got[1])
	}
}

func TestRenderTableAligns(t *testing.T) {
	md := "| Feature | Used for |\n|---|---:|\n| generics | `All[T]` |\n| os.Root | confinement |\n"
	got := plainLines(t, md, 60)
	if len(got) < 4 {
		t.Fatalf("expected four rows, got %q", got)
	}
	// Every row of a table has to be the same width, or the borders zigzag.
	width := ansi.StringWidth(got[0])
	for i, line := range got[:4] {
		if w := ansi.StringWidth(line); w != width {
			t.Errorf("row %d is %d wide, row 0 is %d: %q", i, w, width, line)
		}
	}
	if !strings.Contains(got[1], "┼") {
		t.Errorf("row 1 = %q, want the header rule", got[1])
	}
	// The right-aligned column puts its padding on the left.
	if !strings.Contains(got[2], "  All[T] ") {
		t.Errorf("row 2 = %q, want All[T] right-aligned", got[2])
	}
}

// A table too wide for the pane must be narrowed, not allowed to overflow.
func TestRenderTableFitsTheWidth(t *testing.T) {
	md := "| A very long first column indeed | And a second one just as long |\n|---|---|\n| x | y |\n"
	plainLines(t, md, 40) // plainLines fails the test if any line is too wide
}

func TestRenderFrontmatterIsShown(t *testing.T) {
	got := plainLines(t, "---\ntags: [daily, go]\n---\n\n# Thursday\n", 40)
	if !strings.Contains(strings.Join(got, "\n"), "tags: [daily, go]") {
		t.Errorf("frontmatter is missing:\n%s", strings.Join(got, "\n"))
	}
	if !strings.Contains(strings.Join(got, "\n"), "Thursday") {
		t.Error("the body after the frontmatter is missing")
	}
}

func TestRenderBlockquoteAndRule(t *testing.T) {
	got := plainLines(t, "> quoted\n\n---\n", 30)
	if got[0] != "│ quoted" {
		t.Errorf("quote = %q", got[0])
	}
	if strings.TrimRight(got[2], "─") != "" || len(got[2]) == 0 {
		t.Errorf("rule = %q, want a line of dashes", got[2])
	}
}

// Long prose has to wrap to the pane, and an unbreakable URL has to be cut
// rather than allowed to run over the edge.
func TestRenderWrapsProseAndURLs(t *testing.T) {
	long := strings.Repeat("word ", 40)
	lines := plainLines(t, long, 30)
	if len(lines) < 5 {
		t.Errorf("expected the paragraph to wrap, got %d lines", len(lines))
	}
	plainLines(t, "https://example.com/"+strings.Repeat("segment/", 20), 30)
}

func TestRenderNarrowPaneDoesNotPanic(t *testing.T) {
	for _, w := range []int{0, 1, 4, 8} {
		testRenderer(w).render("# Heading\n\n- item\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n> quote\n")
	}
}

func TestOutgoingLinks(t *testing.T) {
	md := "See [[Projects/folio]] and [[Daily/2026-08-30|today]] and [[Projects/folio]] again, plus [[Note#Section]].\n"
	got := outgoingLinks(md)
	want := []string{"Projects/folio", "Daily/2026-08-30", "Note"}
	if len(got) != len(want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSafeFilename(t *testing.T) {
	tests := map[string]string{
		"Daily/2026-08-30.md": "2026-08-30.md",
		"Projects/a b c.md":   "a b c.md",
		"weird/../../etc.md":  "etc.md",
		"":                    "note.md",
		"Notes/naughty;rm.md": "naughty-rm.md",
	}
	for in, want := range tests {
		if got := safeFilename(in); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoteURLMatchesTheWebRouter(t *testing.T) {
	got := noteURL("https://folio.example.ts.net/", "alice-github", "Daily/a b.md")
	want := "https://folio.example.ts.net/n/alice-github/Daily/a%20b.md"
	if got != want {
		t.Errorf("noteURL = %q, want %q", got, want)
	}
}
