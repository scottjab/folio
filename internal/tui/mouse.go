package tui

import (
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/scottjab/folio/internal/vaultpath"
)

// wheelLines is how far one notch of the wheel moves. Three is what terminals
// and the viewport component already assume, so matching it makes the TUI feel
// like the rest of the desktop rather than like itself.
const wheelLines = 3

// layout is where each part of the screen is, in terminal coordinates.
//
// Drawing and hit-testing both read it. Computing the geometry twice, once to
// paint and once to work out what was clicked, is how a UI ends up opening the
// note above the one you pointed at.
type layout struct {
	width, height int

	// bodyTop is the first row below the title bar; statusRow is the last row.
	bodyTop    int
	bodyHeight int
	statusRow  int

	// sidebarW is zero when the window is too narrow to show the list, and the
	// same goes for noteW when the list has the screen to itself.
	sidebarX, sidebarW int
	noteX, noteW       int
	// renderW is the width the note is wrapped to, which is the full width when
	// the note pane is hidden rather than zero.
	renderW int

	// box is the overlay, when a picker is open.
	box struct{ x, y, w, h int }
}

// layout computes the geometry of the current screen.
func (m *model) layout() layout {
	l := layout{width: m.width, height: m.height, bodyTop: 1}
	l.bodyHeight = max(1, m.height-2)
	l.statusRow = l.bodyTop + l.bodyHeight

	sw := m.sidebarWidth()
	switch {
	case sw == 0 && m.focus == paneList:
		// The list has the screen to itself, but the note is still rendered, so
		// switching back does not show a stale wrap.
		l.sidebarX, l.sidebarW = 0, m.width
		l.renderW = max(8, m.width)
	case sw == 0:
		l.noteX, l.noteW = 0, m.width
		l.renderW = max(8, m.width)
	default:
		l.sidebarX, l.sidebarW = 0, sw
		// One column between them for the divider.
		l.noteX, l.noteW = sw+1, max(8, m.width-sw-1)
		l.renderW = l.noteW
	}

	if m.pick != nil {
		l.box.w = min(m.width, clamp(m.width-8, 40, 108))
		l.box.h = min(l.bodyHeight, clamp(l.bodyHeight-4, 8, 40))
		l.box.x = (m.width - l.box.w) / 2
		l.box.y = l.bodyTop + (l.bodyHeight-l.box.h)/2
	}
	return l
}

// handleMouse routes a mouse event to whatever is under the pointer.
func (m *model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	// A question on the status line owns the screen: what is behind it is not
	// something to be clicking on.
	if !m.mouse || m.pr.active() {
		return nil
	}
	l := m.layout()

	if m.pick != nil {
		return m.mouseOverlay(msg, l)
	}
	switch {
	case msg.Y < l.bodyTop || msg.Y >= l.statusRow:
		return nil
	case l.sidebarW > 0 && msg.X < l.sidebarW:
		return m.mouseSidebar(msg, l)
	case l.noteW > 0 && msg.X >= l.noteX:
		return m.mouseNote(msg, l)
	}
	return nil
}

// mouseSidebar handles the pointer over the note list.
func (m *model) mouseSidebar(msg tea.MouseMsg, l layout) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return m.scrollList(-wheelLines)
	case tea.MouseButtonWheelDown:
		return m.scrollList(wheelLines)
	}
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return nil
	}

	// Row zero of the pane is its heading.
	row := msg.Y - l.bodyTop - 1
	if row < 0 {
		return nil
	}
	idx := m.top + row
	if idx >= len(m.items) {
		// Below the last note: take the keyboard, but do not open anything.
		m.focus = paneList
		return nil
	}
	m.focus = paneList
	m.sel = idx
	m.clampList()
	// Clicking a note is the mouse's way of pressing enter on it, unsaved-work
	// question included.
	return m.openSelected()
}

// scrollList moves the selection, which is what scrolling a list of notes
// means here: the cursor and the window are the same thing.
func (m *model) scrollList(delta int) tea.Cmd {
	if len(m.items) == 0 {
		return nil
	}
	m.sel = clamp(m.sel+delta, 0, len(m.items)-1)
	m.clampList()
	return nil
}

// mouseNote handles the pointer over the note.
func (m *model) mouseNote(msg tea.MouseMsg, l layout) tea.Cmd {
	// MouseMsg is a distinct type from the event it wraps, and only the event
	// carries the wheel test.
	if tea.MouseEvent(msg).IsWheel() {
		if m.editing {
			// The textarea can only scroll by moving the caret, so that is what
			// the wheel does while editing. It is visible, which a silent wheel
			// is not.
			for range wheelLines {
				if msg.Button == tea.MouseButtonWheelUp {
					m.ta.CursorUp()
				} else if msg.Button == tea.MouseButtonWheelDown {
					m.ta.CursorDown()
				}
			}
			return nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return cmd
	}
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return nil
	}

	m.focus = paneNote
	if m.editing || !m.hasNote {
		return nil
	}

	// Row zero of the pane is the note's own header line.
	row := msg.Y - l.bodyTop - 1
	if row < 0 {
		return nil
	}
	line := m.vp.YOffset + row
	if line >= len(m.body) {
		return nil
	}

	target, embed, ok := linkAt(ansiStrip(m.body[line]), msg.X-l.noteX)
	if !ok {
		return nil
	}
	return m.follow(target, embed)
}

// follow opens what a clicked link points at.
func (m *model) follow(display string, embed bool) tea.Cmd {
	// The rendered text is the alias when there is one, so the target has to
	// come back from the source rather than from the screen.
	target := display
	for _, l := range wikiLinks(m.buffer()) {
		if l.display == display {
			target = l.target
			break
		}
	}
	if embed {
		// An embed is a picture or a PDF. A terminal cannot show it, but the
		// browser on the other end of the same tailnet can.
		return openBrowser(attachmentURL(m.cl.Server(), m.note.Vault, target))
	}
	return m.openNote(m.note.Vault, vaultpath.EnsureMarkdown(target))
}

// mouseOverlay handles the pointer over a picker.
func (m *model) mouseOverlay(msg tea.MouseMsg, l layout) tea.Cmd {
	p := m.pick
	inside := msg.X >= l.box.x && msg.X < l.box.x+l.box.w &&
		msg.Y >= l.box.y && msg.Y < l.box.y+l.box.h

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		p.move(-wheelLines)
		return nil
	case tea.MouseButtonWheelDown:
		p.move(wheelLines)
		return nil
	}
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return nil
	}
	if !inside {
		// Clicking away from a dialog closes it, everywhere else.
		m.closePicker()
		return nil
	}

	row := msg.Y - l.box.y - p.listTopRow()
	if row < 0 {
		return nil
	}
	idx := p.top + row/p.rowsPerItem()
	if idx >= len(p.view) {
		return nil
	}
	p.sel = idx
	return m.activate()
}

// wikiLink is one link in a note: what it points at, and what the reader sees.
type wikiLink struct {
	target  string
	display string
}

// wikiLinks lists the wikilinks in a note in order.
func wikiLinks(md string) []wikiLink {
	var out []wikiLink
	for i := 0; i < len(md); i++ {
		if md[i] != '[' || i+1 >= len(md) || md[i+1] != '[' {
			continue
		}
		end := strings.Index(md[i:], "]]")
		if end < 0 {
			break
		}
		inner := md[i+2 : i+end]
		out = append(out, wikiLink{target: linkTarget(inner), display: wikiDisplay(inner)})
		i += end + 1
	}
	return out
}

// linkTarget strips the alias and the anchor from a wikilink, leaving the note.
func linkTarget(inner string) string {
	if p, _, ok := strings.Cut(inner, "|"); ok {
		inner = p
	}
	if p, _, ok := strings.Cut(inner, "#"); ok && p != "" {
		inner = p
	}
	return strings.TrimSpace(inner)
}

// linkAt reports the wikilink covering display column col of a rendered line,
// and whether it is an embed.
//
// Only a link whose brackets are both on this line counts. One split across a
// wrap has no complete span, and guessing at half of one would open the wrong
// note.
func linkAt(line string, col int) (string, bool, bool) {
	if col < 0 {
		return "", false, false
	}
	at := 0 // display column of the byte at i
	for i := 0; i < len(line); {
		if strings.HasPrefix(line[i:], "[[") {
			if j := strings.Index(line[i:], "]]"); j > 0 {
				span := line[i : i+j+2]
				width := ansi.StringWidth(span)
				if col >= at && col < at+width {
					embed := i > 0 && line[i-1] == '!'
					return strings.TrimSpace(line[i+2 : i+j]), embed, true
				}
				at += width
				i += j + 2
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		at += ansi.StringWidth(string(r))
		i += size
	}
	return "", false, false
}
