package notes

import (
	"fmt"
	"strings"
)

// appendToNote inserts text at the end of a note, or at the end of the section
// under a named heading.
//
// "Under a heading" means immediately before the next heading of the same or
// higher level, which is where a person would type it. Appending to the very end
// of the file would put a task meant for "## Tasks" underneath "## Notes".
func appendToNote(content, text, heading string) (string, error) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return content, nil
	}

	if heading == "" {
		return joinBlocks(content, text), nil
	}

	lines := strings.Split(content, "\n")
	start, level := findHeading(lines, heading)
	if start < 0 {
		return "", fmt.Errorf("no heading %q in this note", heading)
	}

	// Walk to the end of the section: the next heading at the same level or
	// shallower, or the end of the file.
	end := len(lines)
	inFence := false
	for i := start + 1; i < len(lines); i++ {
		if isFenceLine(lines[i]) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if l := headingLevel(lines[i]); l > 0 && l <= level {
			end = i
			break
		}
	}

	// Skip back over trailing blank lines so the insert sits against the
	// section's content rather than after a gap.
	insert := end
	for insert > start+1 && strings.TrimSpace(lines[insert-1]) == "" {
		insert--
	}

	newLines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines)+len(newLines)+1)
	out = append(out, lines[:insert]...)
	// Continue a list rather than splitting it: appending "- two" after "- one"
	// should land on the next line, while appending a paragraph after a heading
	// or another paragraph wants a blank line between them.
	if insert > 0 && strings.TrimSpace(lines[insert-1]) != "" &&
		!(isListItem(lines[insert-1]) && isListItem(newLines[0])) {
		out = append(out, "")
	}
	out = append(out, newLines...)
	out = append(out, lines[insert:]...)
	return strings.Join(out, "\n"), nil
}

// joinBlocks appends text to content with exactly one blank line between them.
func joinBlocks(content, text string) string {
	trimmed := strings.TrimRight(content, "\n")
	if trimmed == "" {
		return text + "\n"
	}
	return trimmed + "\n\n" + text + "\n"
}

// findHeading locates a heading by its text, case-insensitively, ignoring any
// leading "#" the caller included.
func findHeading(lines []string, heading string) (idx, level int) {
	want := strings.ToLower(strings.TrimSpace(strings.TrimLeft(heading, "# ")))
	inFence := false
	for i, line := range lines {
		if isFenceLine(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		l := headingLevel(line)
		if l == 0 {
			continue
		}
		if strings.ToLower(strings.TrimSpace(strings.TrimLeft(line, "# "))) == want {
			return i, l
		}
	}
	return -1, 0
}

// headingLevel returns the ATX heading level of a line, or 0 if it is not one.
func headingLevel(line string) int {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0
	}
	// A heading needs whitespace and then some text after the hashes. Without
	// that, "#hashtag" is a tag and a bare "#" is nothing at all.
	if n >= len(line) || (line[n] != ' ' && line[n] != '\t') {
		return 0
	}
	return n
}

// isListItem reports whether a line is a bullet, task, or numbered list item.
func isListItem(line string) bool {
	t := strings.TrimLeft(line, " \t")
	for _, p := range []string{"- ", "* ", "+ ", "- [", "* ["} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	// A numbered item: digits, then "." or ")", then a space.
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(t) && (t[i] == '.' || t[i] == ')') && t[i+1] == ' '
}

// isFenceLine reports whether a line opens or closes a fenced code block, so a
// "# comment" inside a shell snippet is never mistaken for a heading.
func isFenceLine(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}
