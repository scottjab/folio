package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// renderer turns markdown into styled terminal lines.
//
// It is deliberately a small line-based renderer rather than a full parser. The
// note being displayed is markdown the user wrote by hand, and the failure mode
// that matters in a terminal is a wrapped line landing in the wrong column, not
// an obscure CommonMark edge case being rendered a shade differently from the
// browser.
//
// Styles are never nested. A lipgloss style ends with a reset, so a bold style
// wrapped around text that already contains a colour would be cut short at the
// inner reset and the rest of the line would lose its bold. Anywhere an outer
// style applies to a whole line, the inline markers are stripped to plain text
// first; everywhere else the inline styles are leaves.
type renderer struct {
	st    *styles
	width int
}

// render returns the display lines for a note's markdown, wrapped to the
// renderer's width.
func (r renderer) render(md string) []string {
	if r.width < 8 {
		r.width = 8
	}
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")

	var out []string
	i := 0

	// Frontmatter is shown rather than hidden. It is part of the file, it holds
	// the tags, and this UI can edit the raw text, so pretending it is not there
	// would be a lie the editor immediately contradicts.
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		out = append(out, r.st.frontmat.Render("─── ─ ─"))
		for i = 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				i++
				break
			}
			out = append(out, r.st.frontmat.Render(ansi.Truncate(lines[i], r.width, "…")))
		}
		out = append(out, r.st.frontmat.Render("─── ─ ─"), "")
	}

	for i < len(lines) {
		line := lines[i]

		// Fenced code runs to its closing fence, or to the end of the note if
		// the writer never closed it.
		if fence, ok := codeFence(line); ok {
			out = append(out, r.st.muted.Render("┌ "+strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fence))))
			i++
			for ; i < len(lines); i++ {
				if _, closing := codeFence(lines[i]); closing && strings.HasPrefix(strings.TrimSpace(lines[i]), fence) {
					i++
					break
				}
				// Code is hard-wrapped, never word-wrapped: breaking a line of
				// code at a space would misrepresent it.
				for _, seg := range hardwrap(lines[i], r.width-2) {
					out = append(out, r.st.muted.Render("│ ")+r.st.codeBlock.Render(seg))
				}
			}
			out = append(out, r.st.muted.Render("└"))
			continue
		}

		if rows, next, ok := r.table(lines, i); ok {
			out = append(out, rows...)
			i = next
			continue
		}

		out = append(out, r.block(line)...)
		i++
	}
	return out
}

// block renders one non-fenced, non-table line.
func (r renderer) block(line string) []string {
	trimmed := strings.TrimSpace(line)

	switch {
	case trimmed == "":
		return []string{""}

	case isRule(trimmed):
		return []string{r.st.rule.Render(strings.Repeat("─", r.width))}

	case strings.HasPrefix(trimmed, "#"):
		if level, text, ok := heading(trimmed); ok {
			st := r.st.h3
			switch level {
			case 1:
				st = r.st.h1
			case 2:
				st = r.st.h2
			}
			// The heading style owns the whole line, so its text is flattened
			// first; see the note on nesting above.
			var out []string
			for _, seg := range r.wrap(r.plain(text), r.width) {
				out = append(out, st.Render(seg))
			}
			return out
		}

	case strings.HasPrefix(trimmed, ">"):
		text := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
		var out []string
		for _, seg := range r.wrap(r.plain(text), r.width-2) {
			out = append(out, r.st.quote.Render("│ "+seg))
		}
		return out

	default:
		if indent, marker, text, ok := parseListItem(line); ok {
			return r.renderListItem(indent, marker, text)
		}
	}

	var out []string
	for _, seg := range r.wrap(r.inline(line), r.width) {
		out = append(out, seg)
	}
	return out
}

// renderListItem renders a bullet, numbered, or task item with a hanging
// indent, so wrapped text lines up under the first word rather than the bullet.
func (r renderer) renderListItem(indent int, marker, text string) []string {
	bullet := marker
	switch {
	case marker == "-" || marker == "*" || marker == "+":
		bullet = "•"
	}

	// GitHub-style task boxes. Reading a checklist as [ ] and [x] in a terminal
	// works, but the boxes are what make a long list scannable.
	if box, rest, ok := taskBox(text); ok {
		bullet = box
		text = rest
	}

	pad := strings.Repeat(" ", indent)
	head := pad + r.st.bullet.Render(bullet) + " "
	hang := pad + strings.Repeat(" ", ansi.StringWidth(bullet)+1)

	segs := r.wrap(r.inline(text), r.width-indent-ansi.StringWidth(bullet)-1)
	if len(segs) == 0 {
		return []string{head}
	}
	out := []string{head + segs[0]}
	for _, seg := range segs[1:] {
		out = append(out, hang+seg)
	}
	return out
}

// table renders a GFM table starting at lines[i], if that is what it is. It
// reports the index of the first line after the table.
func (r renderer) table(lines []string, i int) ([]string, int, bool) {
	if i+1 >= len(lines) || !strings.Contains(lines[i], "|") || !isTableDelimiter(lines[i+1]) {
		return nil, i, false
	}

	header := tableCells(lines[i])
	aligns := tableAligns(lines[i+1])
	var rows [][]string
	j := i + 2
	for ; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" || !strings.Contains(lines[j], "|") {
			break
		}
		rows = append(rows, tableCells(lines[j]))
	}

	cols := len(header)
	for _, row := range rows {
		cols = max(cols, len(row))
	}
	if cols == 0 {
		return nil, i, false
	}

	// Width each column to its widest cell, then shrink the widest columns
	// until the table fits. Truncating every column equally would mangle a
	// narrow one to protect a wide one.
	widths := make([]int, cols)
	for c := range cols {
		widths[c] = ansi.StringWidth(r.plain(cellAt(header, c)))
		for _, row := range rows {
			widths[c] = max(widths[c], ansi.StringWidth(r.plain(cellAt(row, c))))
		}
		widths[c] = max(widths[c], 3)
	}
	shrinkToFit(widths, r.width-(3*cols+1))

	out := []string{r.tableRow(header, widths, aligns, true)}
	var rule strings.Builder
	rule.WriteString("├")
	for c, w := range widths {
		if c > 0 {
			rule.WriteString("┼")
		}
		rule.WriteString(strings.Repeat("─", w+2))
	}
	rule.WriteString("┤")
	out = append(out, r.st.tableRule.Render(rule.String()))
	for _, row := range rows {
		out = append(out, r.tableRow(row, widths, aligns, false))
	}
	return out, j, true
}

func (r renderer) tableRow(cells []string, widths []int, aligns []byte, header bool) string {
	var b strings.Builder
	b.WriteString(r.st.tableRule.Render("│"))
	for c, w := range widths {
		text := cellAt(cells, c)
		plain := r.plain(text)
		if ansi.StringWidth(plain) > w {
			plain = ansi.Truncate(plain, w, "…")
		}
		body := padTo(plain, w, alignAt(aligns, c))
		if header {
			body = r.st.tableHead.Render(body)
		}
		b.WriteString(" " + body + " ")
		b.WriteString(r.st.tableRule.Render("│"))
	}
	return b.String()
}

// wrap word-wraps already-styled text, and returns at least one line.
func (r renderer) wrap(s string, width int) []string {
	if width < 4 {
		width = 4
	}
	// Wrap breaks on spaces and hyphens, and hard-breaks anything longer than
	// the width, which is what keeps a pasted URL inside the pane.
	wrapped := ansi.Wrap(s, width, " -")
	return strings.Split(wrapped, "\n")
}

// hardwrap splits a line at exactly width columns.
func hardwrap(s string, width int) []string {
	if width < 4 {
		width = 4
	}
	if s == "" {
		return []string{""}
	}
	return strings.Split(ansi.Hardwrap(s, width, false), "\n")
}

// segment is one run of inline text and the style it carries, or no style.
type segment struct {
	text  string
	style *lipgloss.Style
}

// inline renders inline markdown with each styled run as a leaf.
func (r renderer) inline(s string) string {
	var b strings.Builder
	for _, seg := range r.segments(s) {
		if seg.style == nil {
			b.WriteString(seg.text)
			continue
		}
		b.WriteString(seg.style.Render(seg.text))
	}
	return b.String()
}

// plain strips inline markers without styling anything, for the places where an
// outer style owns the whole line.
func (r renderer) plain(s string) string {
	var b strings.Builder
	for _, seg := range r.segments(s) {
		b.WriteString(seg.text)
	}
	return b.String()
}

// segments splits inline markdown into styled runs.
func (r renderer) segments(s string) []segment {
	var out []segment
	var lit strings.Builder

	flush := func() {
		if lit.Len() > 0 {
			out = append(out, segment{text: lit.String()})
			lit.Reset()
		}
	}
	emit := func(text string, style *lipgloss.Style) {
		flush()
		out = append(out, segment{text: text, style: style})
	}

	for i := 0; i < len(s); {
		rest := s[i:]

		// A backslash escape is the writer saying "this is not a marker".
		if s[i] == '\\' && i+1 < len(s) {
			lit.WriteByte(s[i+1])
			i += 2
			continue
		}

		// Code spans win over everything: their contents are literal by
		// definition, markers included.
		if s[i] == '`' {
			if end := strings.IndexByte(rest[1:], '`'); end >= 0 {
				emit(rest[1:1+end], &r.st.code)
				i += end + 2
				continue
			}
		}

		if strings.HasPrefix(rest, "![[") {
			if end := strings.Index(rest, "]]"); end > 0 {
				emit("![["+wikiDisplay(rest[3:end])+"]]", &r.st.link)
				i += end + 2
				continue
			}
		}
		if strings.HasPrefix(rest, "[[") {
			if end := strings.Index(rest, "]]"); end > 0 {
				emit("[["+wikiDisplay(rest[2:end])+"]]", &r.st.wikilink)
				i += end + 2
				continue
			}
		}
		// [text](url): the text is what a reader wants; the URL is noise in a
		// pane this narrow.
		if s[i] == '[' {
			if text, width, ok := mdLink(rest); ok {
				emit(text, &r.st.link)
				i += width
				continue
			}
		}

		if m, ok := emphasis(rest, "**"); ok {
			emit(m.text, &r.st.bold)
			i += m.width
			continue
		}
		if m, ok := emphasis(rest, "__"); ok {
			emit(m.text, &r.st.bold)
			i += m.width
			continue
		}
		if m, ok := emphasis(rest, "~~"); ok {
			emit(m.text, &r.st.strike)
			i += m.width
			continue
		}
		if s[i] == '*' {
			if m, ok := emphasis(rest, "*"); ok {
				emit(m.text, &r.st.italic)
				i += m.width
				continue
			}
		}
		// An underscore only opens emphasis at a word boundary, so
		// snake_case_names survive.
		if s[i] == '_' && atBoundary(s, i) {
			if m, ok := emphasis(rest, "_"); ok {
				emit(m.text, &r.st.italic)
				i += m.width
				continue
			}
		}

		if s[i] == '#' && atBoundary(s, i) {
			if tag := readTag(rest); tag != "" {
				emit(tag, &r.st.tag)
				i += len(tag)
				continue
			}
		}

		lit.WriteByte(s[i])
		i++
	}
	flush()
	return out
}

// match is one matched emphasis run: its contents and how much of the input it
// consumed.
type match struct {
	text  string
	width int
}

// emphasis matches a run delimited by the same marker at both ends. An empty
// run, or one that opens on a space, is not emphasis: "a * b * c" is arithmetic
// or a bullet, not italics.
func emphasis(s, marker string) (match, bool) {
	if !strings.HasPrefix(s, marker) {
		return match{}, false
	}
	rest := s[len(marker):]
	if rest == "" || rest[0] == ' ' {
		return match{}, false
	}
	end := strings.Index(rest, marker)
	if end <= 0 {
		return match{}, false
	}
	if rest[end-1] == ' ' {
		return match{}, false
	}
	return match{text: rest[:end], width: len(marker)*2 + end}, true
}

// mdLink matches [text](url) and reports the text and the whole width.
func mdLink(s string) (string, int, bool) {
	if !strings.HasPrefix(s, "[") {
		return "", 0, false
	}
	close := strings.IndexByte(s, ']')
	if close < 0 || close+1 >= len(s) || s[close+1] != '(' {
		return "", 0, false
	}
	end := strings.IndexByte(s[close:], ')')
	if end < 0 {
		return "", 0, false
	}
	text := s[1:close]
	if text == "" {
		text = s[close+2 : close+end]
	}
	return text, close + end + 1, true
}

// wikiDisplay reduces a wikilink target to what should be shown: the alias if
// there is one, and never the anchor.
func wikiDisplay(target string) string {
	if _, alias, ok := strings.Cut(target, "|"); ok {
		return strings.TrimSpace(alias)
	}
	if p, _, ok := strings.Cut(target, "#"); ok && p != "" {
		return strings.TrimSpace(p)
	}
	return strings.TrimSpace(target)
}

// readTag returns the #tag starting at s, or "" if this is a heading marker or
// a bare hash.
func readTag(s string) string {
	i := 1
	for i < len(s) && isTagByte(s[i]) {
		i++
	}
	if i == 1 {
		return ""
	}
	// A tag has to contain something that is not a digit, or "#1" in prose
	// becomes a tag.
	if !strings.ContainsFunc(s[1:i], func(r rune) bool { return r < '0' || r > '9' }) {
		return ""
	}
	return s[:i]
}

func isTagByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '/'
}

// atBoundary reports whether position i starts a word.
func atBoundary(s string, i int) bool {
	if i == 0 {
		return true
	}
	switch s[i-1] {
	case ' ', '\t', '(', '[', '{', '>', '"', '\'', '*', '_', ',', ';', ':':
		return true
	}
	return false
}

// codeFence reports the fence marker opening or closing a code block.
func codeFence(line string) (string, bool) {
	t := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(t, "```"):
		return "```", true
	case strings.HasPrefix(t, "~~~"):
		return "~~~", true
	}
	return "", false
}

// heading splits an ATX heading into its level and text.
func heading(t string) (int, string, bool) {
	level := 0
	for level < len(t) && t[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	if level < len(t) && t[level] != ' ' {
		return 0, "", false
	}
	return level, strings.TrimSpace(t[level:]), true
}

// isRule reports whether the line is a thematic break.
func isRule(t string) bool {
	if len(t) < 3 {
		return false
	}
	c := t[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := range len(t) {
		if t[i] != c && t[i] != ' ' {
			return false
		}
	}
	return strings.Count(t, string(c)) >= 3
}

// parseListItem splits a list line into its indent, marker, and text.
func parseListItem(line string) (int, string, string, bool) {
	indent := 0
	for indent < len(line) && (line[indent] == ' ' || line[indent] == '\t') {
		indent++
	}
	rest := line[indent:]
	if rest == "" {
		return 0, "", "", false
	}

	switch rest[0] {
	case '-', '*', '+':
		if len(rest) > 1 && rest[1] == ' ' {
			return indent, rest[:1], strings.TrimSpace(rest[2:]), true
		}
	}

	// An ordered marker is digits then '.' or ')'.
	d := 0
	for d < len(rest) && rest[d] >= '0' && rest[d] <= '9' {
		d++
	}
	if d > 0 && d+1 < len(rest) && (rest[d] == '.' || rest[d] == ')') && rest[d+1] == ' ' {
		return indent, rest[:d+1], strings.TrimSpace(rest[d+2:]), true
	}
	return 0, "", "", false
}

// taskBox turns a leading [ ] or [x] into a box, and returns the rest.
func taskBox(text string) (string, string, bool) {
	if len(text) < 3 || text[0] != '[' || text[2] != ']' {
		return "", text, false
	}
	switch text[1] {
	case ' ':
		return "☐", strings.TrimSpace(text[3:]), true
	case 'x', 'X':
		return "☑", strings.TrimSpace(text[3:]), true
	}
	return "", text, false
}

// isTableDelimiter reports whether a line is the |---|:--:| row that turns the
// line above it into a table header.
func isTableDelimiter(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "-") || !strings.Contains(t, "|") {
		return false
	}
	for i := range len(t) {
		switch t[i] {
		case '-', ':', '|', ' ':
		default:
			return false
		}
	}
	return true
}

// tableCells splits a table row into cells.
func tableCells(line string) []string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	cells := strings.Split(t, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// tableAligns reads the alignment of each column from the delimiter row.
func tableAligns(line string) []byte {
	cells := tableCells(line)
	out := make([]byte, len(cells))
	for i, c := range cells {
		left, right := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case left && right:
			out[i] = 'c'
		case right:
			out[i] = 'r'
		default:
			out[i] = 'l'
		}
	}
	return out
}

func cellAt(cells []string, i int) string {
	if i < len(cells) {
		return cells[i]
	}
	return ""
}

func alignAt(aligns []byte, i int) byte {
	if i < len(aligns) {
		return aligns[i]
	}
	return 'l'
}

// padTo pads text to width, honouring the column's alignment.
func padTo(text string, width int, align byte) string {
	gap := width - ansi.StringWidth(text)
	if gap <= 0 {
		return text
	}
	switch align {
	case 'r':
		return strings.Repeat(" ", gap) + text
	case 'c':
		left := gap / 2
		return strings.Repeat(" ", left) + text + strings.Repeat(" ", gap-left)
	default:
		return text + strings.Repeat(" ", gap)
	}
}

// shrinkToFit narrows the widest columns until the total fits the budget.
func shrinkToFit(widths []int, budget int) {
	if budget < len(widths)*3 {
		budget = len(widths) * 3
	}
	total := 0
	for _, w := range widths {
		total += w
	}
	for total > budget {
		widest, at := 0, -1
		for i, w := range widths {
			if w > widest {
				widest, at = w, i
			}
		}
		if at < 0 || widths[at] <= 3 {
			return
		}
		widths[at]--
		total--
	}
}
