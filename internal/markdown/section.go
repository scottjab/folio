package markdown

import (
	"bytes"
	"strings"
)

// Section returns the part of a note under one heading, which is what
// ![[Note#Heading]] embeds.
//
// The span runs from the heading line to the next heading of the same or higher
// level, and the heading itself is included, matching what Obsidian renders. A
// heading found under a deeper one is nested content and stays in.
//
// Matching is on the heading's text rather than its slug, case-insensitively,
// because that is what a reader typed inside the brackets. A slug is also
// accepted, so a link copied out of a rendered page still lands.
//
// This lives in the markdown package rather than in either editor because the
// browser and the terminal both have to answer it, and two implementations of
// "where does this section end" is two answers the day a note has an H3 under
// an H2.
func Section(body []byte, heading string) ([]byte, bool) {
	want := strings.ToLower(strings.TrimSpace(strings.TrimLeft(heading, "#")))
	want = strings.TrimSpace(want)
	if want == "" {
		return nil, false
	}

	lines := splitLinesKeepEnds(body)
	start, level := -1, 0
	inFence := false

	for i, line := range lines {
		if isCodeFence(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lv, text, ok := atxHeading(line)
		if !ok {
			continue
		}
		if start < 0 {
			lower := strings.ToLower(text)
			if lower == want || Slugify(text) == want {
				start, level = i, lv
			}
			continue
		}
		// Past the heading we wanted: the section ends at the next one that is
		// not nested underneath it.
		if lv <= level {
			return bytes.Join(lines[start:i], nil), true
		}
	}
	if start < 0 {
		return nil, false
	}
	return bytes.Join(lines[start:], nil), true
}

// atxHeading parses "## Text" into its level and text. Setext headings are not
// recognized: they are vanishingly rare in notes, and a two-line lookbehind here
// would have to be duplicated in the fence tracking to stay correct.
func atxHeading(line []byte) (level int, text string, ok bool) {
	trimmed := bytes.TrimRight(line, "\r\n")
	// Up to three leading spaces still makes a heading in CommonMark; four makes
	// it an indented code block.
	stripped := bytes.TrimLeft(trimmed, " ")
	if len(trimmed)-len(stripped) > 3 {
		return 0, "", false
	}
	n := 0
	for n < len(stripped) && stripped[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, "", false
	}
	rest := stripped[n:]
	if len(rest) > 0 && rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false // "#tag" is not a heading
	}
	// A closing run of hashes is decoration, not part of the text.
	body := strings.TrimSpace(string(rest))
	body = strings.TrimRight(body, "#")
	return n, strings.TrimSpace(body), true
}

// isCodeFence reports whether a line opens or closes a ``` or ~~~ block. A
// heading inside one is an example, not a section boundary.
func isCodeFence(line []byte) bool {
	s := bytes.TrimSpace(line)
	return bytes.HasPrefix(s, []byte("```")) || bytes.HasPrefix(s, []byte("~~~"))
}

// splitLinesKeepEnds splits on "\n" without dropping it, so joining a range of
// lines reproduces the original bytes exactly.
func splitLinesKeepEnds(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}
