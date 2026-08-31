package tui

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// defaultEditor picks the editor to hand a note to.
func defaultEditor() string {
	for _, k := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	// Not vi: this UI already has an editor, so the fallback only matters for
	// someone who explicitly asked for a handoff and has nothing configured.
	// Naming vi anyway is the least surprising answer on a Unix box.
	return "vi"
}

// splitEditor splits an editor setting like `code --wait` into its parts.
func splitEditor(editor string) (string, []string) {
	fields := strings.Fields(editor)
	if len(fields) == 0 {
		return "vi", nil
	}
	return fields[0], fields[1:]
}

// safeFilename reduces a vault path to a plain file name for a temp file.
func safeFilename(p string) string {
	base := path.Base(p)
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '-' || r == '_' || r == ' ':
			return r
		default:
			return '-'
		}
	}, base)
	if base == "" || base == "." || base == ".." {
		return "note.md"
	}
	return base
}

// noteURL builds the browser URL for a note, matching web/src/router.ts.
func noteURL(server, vault, notePath string) string {
	enc := func(s string) string {
		parts := strings.Split(s, "/")
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		return strings.Join(parts, "/")
	}
	return strings.TrimSuffix(server, "/") + "/n/" + enc(vault) + "/" + enc(notePath)
}

// attachmentURL builds the URL that serves a binary file out of a vault. It is
// what an embedded image resolves to when there is no way to draw it here.
func attachmentURL(server, vault, path string) string {
	enc := func(s string) string {
		parts := strings.Split(s, "/")
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		return strings.Join(parts, "/")
	}
	return strings.TrimSuffix(server, "/") + "/api/vaults/" + enc(vault) + "/attachments/" + enc(path)
}

// openBrowser hands a URL to the desktop, which is the only way to see an
// attachment or an embedded image from a terminal.
func openBrowser(target string) tea.Cmd {
	return func() tea.Msg {
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("open", target)
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
		default:
			cmd = exec.Command("xdg-open", target)
		}
		if err := cmd.Start(); err != nil {
			return statusMsg{text: fmt.Sprintf("could not open a browser: %v", err), isErr: true}
		}
		// Reaping the child keeps a zombie off the process table for the life of
		// the UI; nothing depends on what it returns.
		go cmd.Wait()
		return statusMsg{text: "opened " + target}
	}
}

// truncate shortens styled text to width, ANSI sequences not counted.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}

// pad right-pads styled text to width.
func pad(s string, width int) string {
	gap := width - ansi.StringWidth(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}

// fit truncates and then pads, so every row in a column is exactly as wide as
// the column. Rows of differing width make a reverse-video selection look
// ragged.
func fit(s string, width int) string {
	return pad(truncate(s, width), width)
}

// relTime is a short, human "when", for a list column that has room for six
// characters.
func relTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dy", int(d.Hours()/24/365))
	}
}

// folderOf returns the folder part of a vault path, or "" at the root.
func folderOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return ""
}

// outgoingLinks lists the wikilink targets in a note, in order, without
// repeats. It is the terminal's answer to clicking a link.
func outgoingLinks(md string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range wikiLinks(md) {
		if l.target != "" && !seen[l.target] {
			seen[l.target] = true
			out = append(out, l.target)
		}
	}
	return out
}

// clamp confines a value to a range.
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// lipglossWidth is the printed width of styled text.
func lipglossWidth(s string) int { return ansi.StringWidth(s) }

// ansiStrip removes styling, for the places that need the plain characters.
func ansiStrip(s string) string { return ansi.Strip(s) }
