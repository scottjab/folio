package markdown_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scottjab/tsnotes/internal/markdown"
)

func TestSplitFrontmatter(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		wantFM    string
		wantBody  string
		wantFound bool
	}{
		{
			name:      "standard",
			src:       "---\ntags: [a]\n---\n# Hi\n",
			wantFM:    "tags: [a]\n",
			wantBody:  "# Hi\n",
			wantFound: true,
		},
		{
			name:      "no frontmatter",
			src:       "# Hi\n",
			wantFM:    "",
			wantBody:  "# Hi\n",
			wantFound: false,
		},
		{
			name:      "empty frontmatter block",
			src:       "---\n---\nbody\n",
			wantFM:    "",
			wantBody:  "body\n",
			wantFound: true,
		},
		{
			name:      "hr is not frontmatter when not at start",
			src:       "text\n---\nmore\n",
			wantFM:    "",
			wantBody:  "text\n---\nmore\n",
			wantFound: false,
		},
		{
			name:      "unterminated block is not frontmatter",
			src:       "---\ntags: [a]\nno closing fence\n",
			wantFM:    "",
			wantBody:  "---\ntags: [a]\nno closing fence\n",
			wantFound: false,
		},
		{
			name:      "body containing --- survives",
			src:       "---\na: 1\n---\nbefore\n---\nafter\n",
			wantFM:    "a: 1\n",
			wantBody:  "before\n---\nafter\n",
			wantFound: true,
		},
		{
			name:      "no trailing newline after fence",
			src:       "---\na: 1\n---",
			wantFM:    "a: 1\n",
			wantBody:  "",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, body, found := markdown.SplitFrontmatter([]byte(tt.src))
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if string(fm) != tt.wantFM {
				t.Errorf("frontmatter = %q, want %q", fm, tt.wantFM)
			}
			if string(body) != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestSplitFrontmatterHandlesCRLF(t *testing.T) {
	src := "---\r\ntags: [a]\r\n---\r\n# Hi\r\n"
	fm, body, found := markdown.SplitFrontmatter([]byte(src))
	if !found {
		t.Fatal("expected frontmatter to be found in a CRLF file")
	}
	if !strings.Contains(string(fm), "tags: [a]") {
		t.Errorf("frontmatter = %q, want it to contain the tags line", fm)
	}
	if !strings.Contains(string(body), "# Hi") {
		t.Errorf("body = %q, want it to contain the heading", body)
	}
}

func TestParseBodyIsByteExact(t *testing.T) {
	// The whole design rests on the file on disk being the source of truth, so
	// parsing must never rewrite the body. Not one byte.
	body := "# Hi\n\nSome *text* with [[a link]] and a trailing space   \n\n\ttab-indented\n"
	src := "---\ntags: [x]\n---\n" + body

	doc, err := markdown.Parse("n.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Body != body {
		t.Errorf("body was rewritten:\n got %q\nwant %q", doc.Body, body)
	}
}

func TestParseFrontmatter(t *testing.T) {
	src := `---
id: 019bd0f4-8c31-7a2e-9f10-3b6c7d8e9f01
title: My Title
tags: [daily, go]
aliases:
  - Thursday notes
created: 2026-08-30T09:23:00Z
custom_field: kept
---
body
`
	doc, err := markdown.Parse("Daily/x.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fm := doc.Frontmatter
	if fm.ID != "019bd0f4-8c31-7a2e-9f10-3b6c7d8e9f01" {
		t.Errorf("ID = %q", fm.ID)
	}
	if fm.Title != "My Title" {
		t.Errorf("Title = %q", fm.Title)
	}
	if !slices.Equal(fm.Tags, []string{"daily", "go"}) {
		t.Errorf("Tags = %v", fm.Tags)
	}
	if !slices.Equal(fm.Aliases, []string{"Thursday notes"}) {
		t.Errorf("Aliases = %v", fm.Aliases)
	}
	want := time.Date(2026, 8, 30, 9, 23, 0, 0, time.UTC)
	if !fm.Created.Equal(want) {
		t.Errorf("Created = %v, want %v", fm.Created, want)
	}
	if got := fm.Extra["custom_field"]; got != "kept" {
		t.Errorf("Extra[custom_field] = %v, want an untouched passthrough", got)
	}
}

func TestParseFrontmatterScalarTags(t *testing.T) {
	// Obsidian accepts both "tags: a" and "tags: [a, b]", and people write both.
	doc, err := markdown.Parse("n.md", []byte("---\ntags: solo\n---\nbody\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !slices.Equal(doc.Frontmatter.Tags, []string{"solo"}) {
		t.Errorf("Tags = %v, want [solo]", doc.Frontmatter.Tags)
	}
}

func TestParseBadFrontmatterDoesNotLoseTheNote(t *testing.T) {
	// A syntactically broken frontmatter block must not make the note
	// unreadable. We surface the error but still return a usable doc.
	src := "---\n\tthis: is: not: yaml\n---\n# Body survives\n"
	doc, err := markdown.Parse("n.md", []byte(src))
	if err == nil {
		t.Fatal("expected a frontmatter error")
	}
	if doc == nil {
		t.Fatal("doc must still be returned when frontmatter is broken")
	}
	if !strings.Contains(doc.Body, "# Body survives") {
		t.Errorf("body = %q, want the body preserved", doc.Body)
	}
}

func TestParseNoFrontmatter(t *testing.T) {
	doc, err := markdown.Parse("n.md", []byte("# Just a heading\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.HasFrontmatter {
		t.Error("HasFrontmatter = true, want false")
	}
	if doc.Title != "Just a heading" {
		t.Errorf("Title = %q, want the H1", doc.Title)
	}
}

func TestTitlePrecedence(t *testing.T) {
	tests := []struct {
		name, path, src, want string
	}{
		{"frontmatter wins", "Daily/2026-08-30.md", "---\ntitle: FM\n---\n# H1\n", "FM"},
		{"h1 when no frontmatter title", "Daily/2026-08-30.md", "---\ntags: [a]\n---\n# H1\n", "H1"},
		{"filename when neither", "Daily/2026-08-30.md", "no headings here\n", "2026-08-30"},
		{"first h1 only", "n.md", "# First\n# Second\n", "First"},
		{"h2 does not count as a title", "n.md", "## Not a title\n", "n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := markdown.Parse(tt.path, []byte(tt.src))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if doc.Title != tt.want {
				t.Errorf("Title = %q, want %q", doc.Title, tt.want)
			}
		})
	}
}

func TestHeadings(t *testing.T) {
	src := "# Top\n\n## Sub Section\n\n### Deep, with punctuation!\n\n## Sub Section\n"
	doc, err := markdown.Parse("n.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []markdown.Heading{
		{Level: 1, Text: "Top", Slug: "top"},
		{Level: 2, Text: "Sub Section", Slug: "sub-section"},
		{Level: 3, Text: "Deep, with punctuation!", Slug: "deep-with-punctuation"},
		{Level: 2, Text: "Sub Section", Slug: "sub-section-1"}, // duplicates disambiguate
	}
	if len(doc.Headings) != len(want) {
		t.Fatalf("got %d headings, want %d: %+v", len(doc.Headings), len(want), doc.Headings)
	}
	for i, w := range want {
		got := doc.Headings[i]
		if got.Level != w.Level || got.Text != w.Text || got.Slug != w.Slug {
			t.Errorf("heading %d = %+v, want %+v", i, got, w)
		}
	}
}

func TestHeadingsIgnoreCodeFences(t *testing.T) {
	src := "# Real\n\n```\n# Not a heading\n```\n\n    # Also not, indented code\n"
	doc, err := markdown.Parse("n.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Headings) != 1 || doc.Headings[0].Text != "Real" {
		t.Errorf("headings = %+v, want only the real one", doc.Headings)
	}
}

func TestWikilinks(t *testing.T) {
	src := `
Plain [[Target]] here.
Aliased [[Projects/tsnotes|the project]].
Anchored [[Notes/x#Some Heading]].
Both [[a/b#H|alias]].
Embed ![[attachments/diagram.png]].
Embedded note ![[Other Note]].
`
	doc, err := markdown.Parse("n.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := []markdown.Link{
		{Kind: markdown.LinkWiki, Target: "Target"},
		{Kind: markdown.LinkWiki, Target: "Projects/tsnotes", Alias: "the project"},
		{Kind: markdown.LinkWiki, Target: "Notes/x", Anchor: "Some Heading"},
		{Kind: markdown.LinkWiki, Target: "a/b", Anchor: "H", Alias: "alias"},
		{Kind: markdown.LinkEmbed, Target: "attachments/diagram.png"},
		{Kind: markdown.LinkEmbed, Target: "Other Note"},
	}
	assertLinks(t, doc.Links, want)
}

func TestMarkdownLinks(t *testing.T) {
	src := "See [the project](Projects/tsnotes.md) and [ext](https://example.com) and ![img](a.png).\n"
	doc, err := markdown.Parse("n.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// External links are not vault links and must not create dangling backlinks.
	want := []markdown.Link{
		{Kind: markdown.LinkMarkdown, Target: "Projects/tsnotes.md", Alias: "the project"},
		{Kind: markdown.LinkEmbed, Target: "a.png", Alias: "img"},
	}
	assertLinks(t, doc.Links, want)
}

func TestLinksIgnoreCodeRegions(t *testing.T) {
	src := "Real [[Yes]].\n\n`[[NotInCodeSpan]]`\n\n```\n[[NotInFence]]\n```\n\n    [[NotInIndentedCode]]\n"
	doc, err := markdown.Parse("n.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertLinks(t, doc.Links, []markdown.Link{{Kind: markdown.LinkWiki, Target: "Yes"}})
}

func TestTags(t *testing.T) {
	src := `---
tags: [from-frontmatter]
---
Inline #go127 and #nested/tag and #with-dash and #with_underscore.
Not a tag: a#b, #, #123, and the URL https://example.com/page#frag
`
	doc, err := markdown.Parse("n.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"from-frontmatter", "go127", "nested/tag", "with-dash", "with_underscore"}
	if !slices.Equal(doc.Tags, want) {
		t.Errorf("Tags = %v, want %v", doc.Tags, want)
	}
}

func TestTagsIgnoreCodeRegions(t *testing.T) {
	src := "Real #yes\n\n`#nospan`\n\n```\n#nofence\n```\n\nheading is not a tag:\n# Heading\n"
	doc, err := markdown.Parse("n.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !slices.Equal(doc.Tags, []string{"yes"}) {
		t.Errorf("Tags = %v, want [yes]", doc.Tags)
	}
}

func TestTagsAreDedupedAndSorted(t *testing.T) {
	doc, err := markdown.Parse("n.md", []byte("---\ntags: [b, a]\n---\n#b #a #B\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Tags are case-insensitive for matching but keep their first-seen casing.
	if !slices.Equal(doc.Tags, []string{"a", "b"}) {
		t.Errorf("Tags = %v, want [a b]", doc.Tags)
	}
}

func TestPlaintext(t *testing.T) {
	src := `---
tags: [x]
---
# Heading

Some **bold** and *italic* and ` + "`inline code`" + ` text.

- list item one
- list item two

> a quote

[[Wiki Target]] and [md link](a.md).

` + "```go\nfunc main() {}\n```" + `
`
	doc, err := markdown.Parse("n.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := doc.Plain

	for _, must := range []string{
		"Heading", "bold", "italic", "inline code", "list item one",
		"a quote", "Wiki Target", "md link",
		"func main", // code is searchable, we index it deliberately
	} {
		if !strings.Contains(p, must) {
			t.Errorf("plaintext missing %q\ngot: %q", must, p)
		}
	}
	for _, mustNot := range []string{"**", "```", "[[", "]]", "---", "tags: [x]"} {
		if strings.Contains(p, mustNot) {
			t.Errorf("plaintext still contains markup %q\ngot: %q", mustNot, p)
		}
	}
}

func TestParseEmptyAndWhitespaceOnly(t *testing.T) {
	for _, src := range []string{"", "\n", "   \n\n", "---\n---\n"} {
		doc, err := markdown.Parse("n.md", []byte(src))
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		if doc.Title != "n" {
			t.Errorf("Parse(%q).Title = %q, want the filename fallback", src, doc.Title)
		}
	}
}

func TestResolveWikilink(t *testing.T) {
	// Obsidian resolves [[X]] against the whole vault, preferring an exact path
	// match, then a unique basename match, relative to the linking note.
	vault := []string{
		"Projects/tsnotes.md",
		"Daily/2026-08-30.md",
		"Archive/tsnotes.md",
		"Notes/unique.md",
		"attachments/diagram.png",
	}
	tests := []struct {
		name, from, target, want string
	}{
		{"exact path", "Daily/x.md", "Projects/tsnotes", "Projects/tsnotes.md"},
		{"exact path with ext", "Daily/x.md", "Projects/tsnotes.md", "Projects/tsnotes.md"},
		{"unique basename", "Daily/x.md", "unique", "Notes/unique.md"},
		{"ambiguous basename prefers same folder", "Archive/y.md", "tsnotes", "Archive/tsnotes.md"},
		{"attachment", "Daily/x.md", "attachments/diagram.png", "attachments/diagram.png"},
		{"dangling", "Daily/x.md", "Nope", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := markdown.ResolveWikilink(vault, tt.from, tt.target)
			if got != tt.want {
				t.Errorf("ResolveWikilink(from=%q, target=%q) = %q, want %q", tt.from, tt.target, got, tt.want)
			}
		})
	}
}

func assertLinks(t *testing.T, got, want []markdown.Link) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d links, want %d:\n got %+v\nwant %+v", len(got), len(want), got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Kind != w.Kind || g.Target != w.Target || g.Anchor != w.Anchor || g.Alias != w.Alias {
			t.Errorf("link %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestRewriteWikilinks(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{
			name: "plain link",
			src:  "See [[Projects/tsnotes]] today.\n",
			want: "See [[Archive/tsnotes]] today.\n",
		},
		{
			name: "keeps the alias",
			src:  "See [[Projects/tsnotes|the project]].\n",
			want: "See [[Archive/tsnotes|the project]].\n",
		},
		{
			name: "keeps the anchor",
			src:  "See [[Projects/tsnotes#Design]].\n",
			want: "See [[Archive/tsnotes#Design]].\n",
		},
		{
			name: "keeps anchor and alias together",
			src:  "See [[Projects/tsnotes#Design|design notes]].\n",
			want: "See [[Archive/tsnotes#Design|design notes]].\n",
		},
		{
			name: "keeps the embed marker",
			src:  "![[Projects/tsnotes]]\n",
			want: "![[Archive/tsnotes]]\n",
		},
		{
			name: "author wrote the extension, so we keep one",
			src:  "See [[Projects/tsnotes.md]].\n",
			want: "See [[Archive/tsnotes.md]].\n",
		},
		{
			name: "several links in one file",
			src:  "[[Projects/tsnotes]] and [[Projects/tsnotes|x]] and [[Other]].\n",
			want: "[[Archive/tsnotes]] and [[Archive/tsnotes|x]] and [[Other]].\n",
		},
		{
			name: "unrelated links are untouched",
			src:  "[[Projects/other]] and [[Projects/tsnotes2]].\n",
			want: "[[Projects/other]] and [[Projects/tsnotes2]].\n",
		},
		{
			name: "basename links are left alone because they still resolve",
			src:  "See [[tsnotes]].\n",
			want: "See [[tsnotes]].\n",
		},
		{
			name: "code regions are documentation, not links",
			src:  "`[[Projects/tsnotes]]`\n\n```\n[[Projects/tsnotes]]\n```\n",
			want: "`[[Projects/tsnotes]]`\n\n```\n[[Projects/tsnotes]]\n```\n",
		},
		{
			name: "no links at all",
			src:  "Just prose.\n",
			want: "Just prose.\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := markdown.RewriteWikilinks([]byte(tt.src), "Daily/x.md", "Projects/tsnotes.md", "Archive/tsnotes.md")
			if string(got) != tt.want {
				t.Errorf("RewriteWikilinks:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestRewriteWikilinksLeavesEverythingElseByteExact(t *testing.T) {
	src := "---\ntags: [a]\n---\n# H\n\ntrailing space   \n\n\ttab\n\n[[Projects/tsnotes]]\n"
	got := markdown.RewriteWikilinks([]byte(src), "Daily/x.md", "Projects/tsnotes.md", "Archive/tsnotes.md")
	want := strings.Replace(src, "[[Projects/tsnotes]]", "[[Archive/tsnotes]]", 1)
	if string(got) != want {
		t.Errorf("rewrite touched more than the link:\n got %q\nwant %q", got, want)
	}
}

func TestEnsureFrontmatterID(t *testing.T) {
	const id = "019bd0f4-8c31-7a2e-9f10-3b6c7d8e9f01"

	tests := []struct {
		name, src, want string
	}{
		{
			name: "no frontmatter at all",
			src:  "# Hi\n\nbody\n",
			want: "---\nid: " + id + "\n---\n# Hi\n\nbody\n",
		},
		{
			name: "empty note",
			src:  "",
			want: "---\nid: " + id + "\n---\n",
		},
		{
			name: "existing frontmatter without an id",
			src:  "---\ntags: [a]\n---\n# Hi\n",
			want: "---\nid: " + id + "\ntags: [a]\n---\n# Hi\n",
		},
		{
			name: "already has an id, left alone",
			src:  "---\nid: existing-id\ntags: [a]\n---\n# Hi\n",
			want: "---\nid: existing-id\ntags: [a]\n---\n# Hi\n",
		},
		{
			name: "id later in the block still counts",
			src:  "---\ntags: [a]\nid: existing-id\n---\n# Hi\n",
			want: "---\ntags: [a]\nid: existing-id\n---\n# Hi\n",
		},
		{
			name: "a nested id key does not count",
			src:  "---\nmeta:\n  id: nested\n---\n# Hi\n",
			want: "---\nid: " + id + "\nmeta:\n  id: nested\n---\n# Hi\n",
		},
		{
			name: "empty frontmatter block",
			src:  "---\n---\nbody\n",
			want: "---\nid: " + id + "\n---\nbody\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := markdown.EnsureFrontmatterID([]byte(tt.src), id)
			if string(got) != tt.want {
				t.Errorf("EnsureFrontmatterID:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestEnsureFrontmatterIDKeepsTheBodyByteExact(t *testing.T) {
	body := "# Hi\n\ntrailing space   \n\n\ttab\n\n---\nnot frontmatter\n"
	got := markdown.EnsureFrontmatterID([]byte(body), "x")
	if !strings.HasSuffix(string(got), body) {
		t.Errorf("body was altered:\n got %q\nwant it to end with %q", got, body)
	}
}

func TestEnsureFrontmatterIDIsIdempotent(t *testing.T) {
	once := markdown.EnsureFrontmatterID([]byte("# Hi\n"), "x")
	twice := markdown.EnsureFrontmatterID(once, "y")
	if string(once) != string(twice) {
		t.Errorf("a second call changed the note:\n got %q\nwant %q", twice, once)
	}
}
