// Package markdown turns a note's bytes into the facts the index needs: its
// frontmatter, title, headings, outgoing links, tags, and a plaintext rendering
// for full-text search.
//
// It never rewrites the note. Parse hands back the body byte for byte, because
// the file on disk is the source of truth and CodeMirror renders from that same
// source in the browser. Nothing here renders HTML; the Go side only extracts.
package markdown

import (
	"bytes"
	"cmp"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"

	"github.com/scottjab/folio/internal/vaultpath"
)

// LinkKind distinguishes the three ways a note can point at something.
type LinkKind string

const (
	// LinkWiki is Obsidian's [[Target]], resolved against the whole vault.
	LinkWiki LinkKind = "wiki"
	// LinkEmbed is ![[Target]] or a markdown image; it renders inline.
	LinkEmbed LinkKind = "embed"
	// LinkMarkdown is a plain [text](path) pointing inside the vault.
	LinkMarkdown LinkKind = "markdown"
)

// Link is one outgoing reference. Target is exactly what the author typed;
// resolving it to a real note is [ResolveWikilink]'s job, and happens in the
// indexer where the vault listing is available.
type Link struct {
	Kind   LinkKind
	Target string
	Anchor string
	Alias  string
	Offset int // byte offset into Doc.Body, for document ordering
}

// Heading is one ATX or setext heading.
type Heading struct {
	Level int
	Text  string
	Slug  string
}

// Frontmatter is the YAML block at the top of a note. Keys we understand get
// their own fields; everything else lands in Extra and is preserved untouched,
// because plenty of Obsidian plugins keep state up there.
type Frontmatter struct {
	ID      string
	Title   string
	Tags    []string
	Aliases []string
	Created time.Time
	Updated time.Time
	Extra   map[string]any
}

// Doc is everything we learned about one note.
type Doc struct {
	Path           string
	Frontmatter    Frontmatter
	FrontmatterRaw string
	HasFrontmatter bool
	Body           string // byte-identical to the source after the frontmatter block
	Title          string
	Headings       []Heading
	Links          []Link
	Tags           []string
	Plain          string
}

var (
	// wikilinkRe matches [[Target#Anchor|Alias]] and the ![[...]] embed form.
	wikilinkRe = regexp.MustCompile(`(!?)\[\[([^\[\]|#]*?)(?:#([^\[\]|]*?))?(?:\|([^\[\]]*?))?\]\]`)

	// tagRe matches #tag. Group 1 is the tag body. A tag must contain at least
	// one non-digit so issue references like #123 stay out of the tag index.
	tagRe = regexp.MustCompile(`(?:^|[\s(\[>])#([\p{L}\p{N}_/-]*[\p{L}_/][\p{L}\p{N}_/-]*)`)

	slugStripRe = regexp.MustCompile(`[^\p{L}\p{N}\s-]+`)
	slugSpaceRe = regexp.MustCompile(`[\s-]+`)
)

var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// SplitFrontmatter separates a leading YAML frontmatter block from the body.
//
// The block must open on the very first line with "---" and close with the next
// line that is exactly "---". If it never closes, the whole input is body: a
// note starting with a horizontal rule is worth more than a parse error.
func SplitFrontmatter(src []byte) (fm, body []byte, found bool) {
	first, _, ok := bytes.Cut(src, []byte("\n"))
	if !ok || !isFence(first) {
		return nil, src, false
	}

	fmStart := len(first) + 1 // first byte after the opening fence line
	for pos := fmStart; pos <= len(src); {
		nl := bytes.IndexByte(src[pos:], '\n')
		line, next := src[pos:], len(src)
		if nl >= 0 {
			line, next = src[pos:pos+nl], pos+nl+1
		}
		if isFence(line) {
			return src[fmStart:pos], src[min(next, len(src)):], true
		}
		if nl < 0 {
			return nil, src, false // ran out of file before the block closed
		}
		pos = next
	}
	return nil, src, false
}

// isFence reports whether a line is exactly "---", tolerating a trailing \r so
// CRLF vaults (Windows, or anything synced through one) parse the same way.
func isFence(line []byte) bool {
	return bytes.Equal(bytes.TrimRight(line, "\r"), []byte("---"))
}

// Parse extracts everything we index about a note.
//
// A malformed frontmatter block returns both a non-nil Doc and a non-nil error:
// the note still has a body worth showing and indexing, and refusing to return
// it would make one bad YAML line hide a note entirely.
func Parse(notePath string, src []byte) (*Doc, error) {
	fmRaw, body, hasFM := SplitFrontmatter(src)

	doc := &Doc{
		Path:           notePath,
		FrontmatterRaw: string(fmRaw),
		HasFrontmatter: hasFM,
		Body:           string(body),
	}

	var fmErr error
	if hasFM && len(bytes.TrimSpace(fmRaw)) > 0 {
		doc.Frontmatter, fmErr = parseFrontmatter(fmRaw)
	}

	root := md.Parser().Parse(text.NewReader(body))
	doc.Headings = collectHeadings(root, body)
	doc.Plain = plaintext(root, body)

	masked := maskCodeRegions(root, body)
	doc.Links = collectLinks(root, body, masked)
	doc.Tags = collectTags(doc.Frontmatter.Tags, masked)
	doc.Title = deriveTitle(notePath, doc.Frontmatter.Title, doc.Headings)

	return doc, fmErr
}

// parseFrontmatter decodes the YAML block, pulling out the keys we act on and
// keeping every other key in Extra so a round-trip through folio never eats
// a plugin's state.
func parseFrontmatter(raw []byte) (Frontmatter, error) {
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return Frontmatter{}, fmt.Errorf("frontmatter: %w", err)
	}

	fm := Frontmatter{Extra: map[string]any{}}
	for k, v := range m {
		switch strings.ToLower(k) {
		case "id":
			fm.ID = asString(v)
		case "title":
			fm.Title = asString(v)
		case "tags", "tag":
			fm.Tags = append(fm.Tags, asStringSlice(v)...)
		case "aliases", "alias":
			fm.Aliases = append(fm.Aliases, asStringSlice(v)...)
		case "created":
			fm.Created = asTime(v)
		case "updated", "modified":
			fm.Updated = asTime(v)
		default:
			fm.Extra[k] = v
		}
	}
	return fm, nil
}

// asString coerces a YAML scalar to a string. Numbers and dates show up here
// when someone writes `id: 12345` or an unquoted date.
func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case time.Time:
		return t.Format(time.RFC3339)
	default:
		return fmt.Sprint(t)
	}
}

// asStringSlice accepts both `tags: solo` and `tags: [a, b]`, because people
// write both and Obsidian accepts both.
func asStringSlice(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []string{s}
		}
		return nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s := strings.TrimSpace(asString(e)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	default:
		return []string{asString(v)}
	}
}

// asTime accepts a real YAML timestamp or any of the usual string spellings.
func asTime(v any) time.Time {
	if t, ok := v.(time.Time); ok {
		return t.UTC()
	}
	s := asString(v)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// deriveTitle applies the precedence a reader expects: an explicit frontmatter
// title, else the first H1, else the filename.
func deriveTitle(notePath, fmTitle string, headings []Heading) string {
	if t := strings.TrimSpace(fmTitle); t != "" {
		return t
	}
	for _, h := range headings {
		if h.Level == 1 {
			return h.Text
		}
	}
	return vaultpath.TitleFor(notePath)
}

func collectHeadings(root ast.Node, src []byte) []Heading {
	var out []Heading
	seen := map[string]int{}

	ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := n.(*ast.Heading)
		if !ok {
			return ast.WalkContinue, nil
		}
		txt := strings.TrimSpace(string(h.Text(src)))
		slug := Slugify(txt)
		// GitHub-style disambiguation, so two "Sub Section" headings still get
		// distinct anchors and a link can address either one.
		if n := seen[slug]; n > 0 {
			seen[slug] = n + 1
			slug = fmt.Sprintf("%s-%d", slug, n)
		} else {
			seen[slug] = 1
		}
		out = append(out, Heading{Level: h.Level, Text: txt, Slug: slug})
		return ast.WalkSkipChildren, nil
	})
	return out
}

// Slugify converts heading text to a URL-safe anchor, GitHub style.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugStripRe.ReplaceAllString(s, "")
	s = slugSpaceRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// maskCodeRegions returns a copy of src with every byte that lives inside code,
// raw HTML, or an autolink replaced by a space.
//
// Length is preserved so offsets into the mask are still valid offsets into the
// original. This is what lets the wikilink and tag regexes run over raw source
// without matching `#nope` inside a fenced block.
func maskCodeRegions(root ast.Node, src []byte) []byte {
	masked := slices.Clone(src)
	blank := func(start, stop int) {
		for i := max(start, 0); i < min(stop, len(masked)); i++ {
			if masked[i] != '\n' {
				masked[i] = ' '
			}
		}
	}
	blankSegments := func(segs *text.Segments) {
		for i := range segs.Len() {
			s := segs.At(i)
			blank(s.Start, s.Stop)
		}
	}

	ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := n.(type) {
		case *ast.FencedCodeBlock:
			blankSegments(t.Lines())
			return ast.WalkSkipChildren, nil
		case *ast.CodeBlock:
			blankSegments(t.Lines())
			return ast.WalkSkipChildren, nil
		case *ast.HTMLBlock:
			blankSegments(t.Lines())
			blank(t.ClosureLine.Start, t.ClosureLine.Stop)
			return ast.WalkSkipChildren, nil
		case *ast.CodeSpan:
			for c := t.FirstChild(); c != nil; c = c.NextSibling() {
				if txt, ok := c.(*ast.Text); ok {
					blank(txt.Segment.Start, txt.Segment.Stop)
				}
			}
			return ast.WalkSkipChildren, nil
		case *ast.RawHTML:
			blankSegments(t.Segments)
			return ast.WalkSkipChildren, nil
		case *ast.AutoLink:
			// goldmark keeps an autolink's segment unexported, so we cannot blank
			// it here. That is fine: tagRe already refuses a '#' that follows a
			// non-space character, which is exactly the "…/page#frag" case.
			_ = t
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return masked
}

// collectLinks gathers wikilinks and embeds from the masked source, and real
// markdown links and images from the AST, then returns them in document order.
//
// The split is deliberate: goldmark knows nothing about [[wikilinks]], and
// hand-rolling markdown link parsing would get escaping wrong.
func collectLinks(root ast.Node, src, masked []byte) []Link {
	var out []Link

	for _, m := range wikilinkRe.FindAllSubmatchIndex(masked, -1) {
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return strings.TrimSpace(string(src[m[2*i]:m[2*i+1]]))
		}
		target := group(2)
		if target == "" && group(3) == "" {
			continue // "[[]]" or "[[#anchor]]" points nowhere
		}
		kind := LinkWiki
		if group(1) == "!" {
			kind = LinkEmbed
		}
		out = append(out, Link{
			Kind:   kind,
			Target: target,
			Anchor: group(3),
			Alias:  group(4),
			Offset: m[0],
		})
	}

	ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var dest []byte
		kind := LinkMarkdown
		switch t := n.(type) {
		case *ast.Link:
			dest = t.Destination
		case *ast.Image:
			dest, kind = t.Destination, LinkEmbed
		default:
			return ast.WalkContinue, nil
		}
		target := string(dest)
		if isExternal(target) {
			return ast.WalkContinue, nil // not a vault link, not our backlink
		}
		target, anchor := vaultpath.SplitAnchor(target)
		if target == "" {
			return ast.WalkContinue, nil // pure in-page anchor
		}
		out = append(out, Link{
			Kind:   kind,
			Target: target,
			Anchor: anchor,
			Alias:  strings.TrimSpace(string(n.Text(src))),
			Offset: nodeOffset(n),
		})
		return ast.WalkContinue, nil
	})

	slices.SortStableFunc(out, func(a, b Link) int { return cmp.Compare(a.Offset, b.Offset) })
	return out
}

// isExternal reports whether a link destination leaves the vault.
func isExternal(dest string) bool {
	if strings.HasPrefix(dest, "//") {
		return true
	}
	scheme, _, ok := strings.Cut(dest, ":")
	return ok && !strings.ContainsAny(scheme, "/. ")
}

// nodeOffset finds a byte offset for an inline node so links sort in document
// order. Inline nodes carry no range of their own, so we use their first text
// descendant.
func nodeOffset(n ast.Node) int {
	var off = -1
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || off >= 0 {
			return ast.WalkContinue, nil
		}
		if t, ok := c.(*ast.Text); ok {
			off = t.Segment.Start
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return off
}

// collectTags merges frontmatter tags with inline #tags. Matching is
// case-insensitive but the first spelling seen wins, so "#Go" and "#go" are one
// tag displayed the way you first wrote it.
func collectTags(fmTags []string, masked []byte) []string {
	seen := map[string]string{}
	add := func(tag string) {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "#")
		tag = strings.Trim(tag, "/")
		if tag == "" {
			return
		}
		if _, ok := seen[strings.ToLower(tag)]; !ok {
			seen[strings.ToLower(tag)] = tag
		}
	}

	for _, t := range fmTags {
		add(t)
	}
	for _, m := range tagRe.FindAllSubmatchIndex(masked, -1) {
		add(string(masked[m[2]:m[3]]))
	}

	out := make([]string, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b string) int {
		if c := cmp.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	return out
}

// plaintext renders the note as searchable prose: markup removed, code contents
// kept. Code is kept on purpose, since "where did I write that shell one-liner"
// is a question this app has to answer.
func plaintext(root ast.Node, src []byte) string {
	var b strings.Builder

	ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch t := n.(type) {
		case *ast.Text:
			if !entering {
				return ast.WalkContinue, nil
			}
			b.Write(t.Segment.Value(src))
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte('\n')
			}
		case *ast.String:
			if entering {
				b.Write(t.Value)
			}
		case *ast.AutoLink:
			if entering {
				b.Write(t.URL(src))
			}
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			if !entering {
				return ast.WalkContinue, nil
			}
			lines := n.Lines()
			for i := range lines.Len() {
				seg := lines.At(i)
				b.Write(seg.Value(src))
			}
			b.WriteByte('\n')
			return ast.WalkSkipChildren, nil
		default:
			if !entering && n.Type() == ast.TypeBlock {
				b.WriteByte('\n')
			}
		}
		return ast.WalkContinue, nil
	})

	// Wikilinks reach here as literal "[[Target|alias]]" text, since goldmark
	// has no idea what they are. Reduce them to the words a search should hit.
	out := wikilinkRe.ReplaceAllStringFunc(b.String(), func(m string) string {
		g := wikilinkRe.FindStringSubmatch(m)
		return strings.TrimSpace(strings.Join([]string{g[2], g[3], g[4]}, " "))
	})
	return strings.TrimSpace(collapseSpace(out))
}

// collapseSpace squeezes runs of whitespace, keeping at most one blank line so
// snippets read sensibly.
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	newlines, space := 0, false
	for _, r := range s {
		switch {
		case r == '\n':
			newlines++
			space = false
		case r == ' ' || r == '\t' || r == '\r':
			space = true
		default:
			if newlines > 0 {
				b.WriteString(strings.Repeat("\n", min(newlines, 2)))
			} else if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			newlines, space = 0, false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ResolveWikilink maps a link target onto a real vault path, following the rules
// an Obsidian user already has in their head:
//
//  1. an exact path match wins;
//  2. then the same path with ".md" appended;
//  3. then a basename match, preferring one in the linking note's own folder,
//     then the shallowest candidate, then alphabetical order.
//
// It returns "" for a dangling link, which the indexer records so the link
// resolves on its own the day you create the missing note.
func ResolveWikilink(vaultPaths []string, from, target string) string {
	target, _ = vaultpath.SplitAnchor(target)
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if c, err := vaultpath.Clean(target); err == nil {
		target = c
	}

	exact := slices.Contains(vaultPaths, target)
	if exact {
		return target
	}
	if withExt := target + ".md"; slices.Contains(vaultPaths, withExt) {
		return withExt
	}

	// Basename match. Obsidian lets you write [[unique]] from anywhere.
	base := strings.ToLower(path.Base(target))
	var candidates []string
	for _, p := range vaultPaths {
		pb := strings.ToLower(path.Base(p))
		if pb == base || pb == base+".md" {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return ""
	}

	fromDir := vaultpath.Dir(from)
	slices.SortFunc(candidates, func(a, b string) int {
		// Same folder as the linking note beats everything else.
		if c := cmp.Compare(boolRank(vaultpath.Dir(a) == fromDir), boolRank(vaultpath.Dir(b) == fromDir)); c != 0 {
			return c
		}
		if c := cmp.Compare(strings.Count(a, "/"), strings.Count(b, "/")); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	return candidates[0]
}

// boolRank sorts true before false.
func boolRank(b bool) int {
	if b {
		return 0
	}
	return 1
}

// RewriteWikilinks repoints every wikilink in src that explicitly names oldPath
// so it names newPath instead, preserving the "!" embed marker, the #anchor, and
// the |alias exactly as the author wrote them.
//
// It deliberately leaves basename-only links such as [[folio]] alone. Those
// resolve by searching the vault, so they keep working after a move on their
// own, and rewriting them would churn files for no reason.
//
// Links inside code spans and fenced blocks are never touched: a fenced example
// showing [[Some/Path]] is documentation, not a link.
func RewriteWikilinks(src []byte, notePath, oldPath, newPath string) []byte {
	root := md.Parser().Parse(text.NewReader(src))
	masked := maskCodeRegions(root, src)

	oldTarget, _ := vaultpath.SplitAnchor(oldPath)
	oldBare := strings.TrimSuffix(oldTarget, "."+vaultpath.Ext(oldTarget))
	newBare := strings.TrimSuffix(newPath, "."+vaultpath.Ext(newPath))

	var out []byte
	last := 0
	for _, m := range wikilinkRe.FindAllSubmatchIndex(masked, -1) {
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return string(src[m[2*i]:m[2*i+1]])
		}
		target := strings.TrimSpace(group(2))
		clean, err := vaultpath.Clean(target)
		if err != nil {
			continue
		}
		// Match whether the author wrote the extension or not.
		if clean != oldTarget && clean != oldBare {
			continue
		}
		// Keep the author's style: an extension-less link stays extension-less.
		replacement := newBare
		if clean == oldTarget && vaultpath.Ext(target) != "" {
			replacement = newPath
		}

		var b strings.Builder
		b.WriteString(group(1)) // "!" for an embed, else empty
		b.WriteString("[[")
		b.WriteString(replacement)
		if a := group(3); a != "" || m[6] >= 0 {
			b.WriteByte('#')
			b.WriteString(a)
		}
		if al := group(4); al != "" || m[8] >= 0 {
			b.WriteByte('|')
			b.WriteString(al)
		}
		b.WriteString("]]")

		out = append(out, src[last:m[0]]...)
		out = append(out, b.String()...)
		last = m[1]
	}
	if out == nil {
		return src
	}
	return append(out, src[last:]...)
}

// EnsureFrontmatterID returns src with an "id" key in its frontmatter, adding a
// frontmatter block if there was none. If an id is already present, src is
// returned untouched.
//
// The id is what makes a note survive being renamed: shares and backlinks can
// point at something stable rather than at a path that is about to change. It is
// written once, at creation, and never rewritten.
func EnsureFrontmatterID(src []byte, id string) []byte {
	fm, body, hasFM := SplitFrontmatter(src)
	if hasFM && frontmatterHasID(fm) {
		return src
	}

	var b bytes.Buffer
	b.WriteString("---\nid: ")
	b.WriteString(id)
	b.WriteByte('\n')
	if hasFM {
		b.Write(fm)
	}
	b.WriteString("---\n")
	b.Write(body)
	return b.Bytes()
}

// frontmatterHasID looks for a top-level "id" key without a full YAML parse, so
// a block we cannot parse is still left alone rather than being mangled.
func frontmatterHasID(fm []byte) bool {
	for line := range bytes.SplitSeq(fm, []byte("\n")) {
		trimmed := bytes.TrimRight(line, "\r")
		if len(trimmed) == 0 || trimmed[0] == ' ' || trimmed[0] == '\t' || trimmed[0] == '#' {
			continue // nested key, blank line, or comment
		}
		key, _, found := bytes.Cut(trimmed, []byte(":"))
		if found && strings.EqualFold(strings.TrimSpace(string(key)), "id") {
			return true
		}
	}
	return false
}
