package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/scottjab/folio/internal/vaultpath"
)

// View draws the whole screen: a title bar, the two panes, and one line at the
// bottom that is either a status message, a question, or the keys you can press.
func (m *model) View() string {
	if !m.ready {
		return "starting folio…"
	}

	bodyH := max(1, m.height-2)
	var body string
	if m.pick != nil {
		body = m.overlayView(bodyH)
	} else {
		body = m.panesView(bodyH)
	}

	return strings.Join([]string{m.titleBar(), body, m.statusLine()}, "\n")
}

func (m *model) titleBar() string {
	left := m.st.title.Render("folio")
	if m.me.Login != "" {
		left += m.st.muted.Render("  " + m.me.Login)
	}

	var right []string
	if m.vault != "" {
		label := m.vault
		if m.vault != m.me.Vault {
			label = m.st.itemShared.Render(label)
		}
		right = append(right, label)
	}
	if m.connected {
		right = append(right, m.st.okText.Render("● live"))
	} else {
		right = append(right, m.st.muted.Render("○ offline"))
	}
	r := strings.Join(right, m.st.muted.Render(" · "))

	gap := m.width - lipglossWidth(left) - lipglossWidth(r)
	if gap < 1 {
		// Not enough room for both. The right-hand side is the live indicator
		// and which vault you are in, which is worth more than the login.
		left = truncate(left, max(0, m.width-lipglossWidth(r)-1))
		gap = m.width - lipglossWidth(left) - lipglossWidth(r)
		if gap < 0 {
			return fit(truncate(r, m.width), m.width)
		}
	}
	return left + strings.Repeat(" ", gap) + r
}

func (m *model) panesView(height int) string {
	noteW := m.notePaneWidth()
	note := m.notePane(noteW, height)

	sw := m.sidebarWidth()
	if sw == 0 {
		// Too narrow for both: show whichever pane has the keyboard, the way the
		// web app collapses its sidebar into a drawer on a phone.
		if m.focus == paneList {
			return m.sidebar(m.width, height)
		}
		return note
	}
	divider := m.dividerColumn(height)
	return lipgloss.JoinHorizontal(lipgloss.Top, m.sidebar(sw, height), divider, note)
}

func (m *model) dividerColumn(height int) string {
	col := make([]string, height)
	for i := range col {
		col[i] = m.st.divider.Render("│")
	}
	return strings.Join(col, "\n")
}

// sidebar draws the note list.
func (m *model) sidebar(width, height int) string {
	lines := make([]string, 0, height)

	header := m.label
	if n := len(m.items); n > 0 {
		header += m.st.muted.Render(fmt.Sprintf("  %d", n))
	}
	lines = append(lines, fit(m.st.paneTitle.Render(truncate(header, width)), width))

	rows := max(1, height-1)
	// Recompute the window here rather than trusting a stale scroll offset: the
	// height can change under us on a resize.
	top := clamp(m.top, 0, max(0, len(m.items)-1))
	if m.sel < top {
		top = m.sel
	}
	if m.sel >= top+rows {
		top = m.sel - rows + 1
	}
	m.top = top

	for i := 0; i < rows; i++ {
		idx := top + i
		if idx >= len(m.items) {
			lines = append(lines, strings.Repeat(" ", width))
			continue
		}
		lines = append(lines, m.sidebarRow(m.items[idx], idx == m.sel, width))
	}

	if len(m.items) == 0 {
		msg := "no notes here"
		if m.searchQuery != "" {
			msg = "nothing matched"
		}
		lines[1] = fit(m.st.muted.Render(truncate(msg, width)), width)
	}
	return strings.Join(lines, "\n")
}

func (m *model) sidebarRow(it listItem, selected bool, width int) string {
	detail := it.detail
	titleW := max(4, width-lipglossWidth(detail)-2)

	title := truncate(it.title, titleW)
	if it.shared {
		title = m.st.itemShared.Render(title)
	}
	body := fit(title, titleW)
	if detail != "" {
		body += " " + m.st.itemDetail.Render(detail)
	}

	switch {
	case selected && m.focus == paneList:
		return m.st.itemSelected.Render(fit(" "+body, width))
	case selected:
		// The list still shows which note is open when the keyboard is in the
		// note pane, but quietly: a full reverse-video bar reads as "this is
		// where your keystrokes are going", and they are not.
		return fit(m.st.key.Render("▌")+body, width)
	default:
		return fit(" "+body, width)
	}
}

// notePane draws the open note, its header, and the editor when one is open.
func (m *model) notePane(width, height int) string {
	if !m.hasNote {
		return m.emptyPane(width, height)
	}

	head := m.noteHeader(width)
	m.vp.Width = width
	m.vp.Height = max(1, height-1)
	m.ta.SetWidth(width)
	m.ta.SetHeight(max(1, height-1))

	var body string
	if m.editing {
		body = m.ta.View()
	} else {
		body = m.vp.View()
	}

	lines := append([]string{head}, strings.Split(body, "\n")...)
	for len(lines) < height {
		lines = append(lines, "")
	}
	lines = lines[:height]
	for i := range lines {
		lines[i] = fit(lines[i], width)
	}
	return strings.Join(lines, "\n")
}

func (m *model) noteHeader(width int) string {
	var badges []string
	if m.editing {
		badges = append(badges, m.st.okText.Render("editing"))
	}
	if m.dirty {
		badges = append(badges, m.st.errorText.Render("● unsaved"))
	}
	if m.stale {
		badges = append(badges, m.st.errorText.Render("changed on the server"))
	}
	if !m.note.Perm.CanWrite() {
		badges = append(badges, m.st.muted.Render("read-only"))
	}
	if m.raw && !m.editing {
		badges = append(badges, m.st.muted.Render("raw"))
	}
	if m.note.Vault != m.me.Vault {
		badges = append(badges, m.st.itemShared.Render(m.note.OwnerLogin))
	}
	if !m.editing && len(m.note.Tags) > 0 {
		badges = append(badges, m.st.tag.Render("#"+strings.Join(m.note.Tags, " #")))
	}

	right := strings.Join(badges, m.st.muted.Render(" · "))
	left := m.st.bold.Render(m.note.Path)

	gap := width - lipglossWidth(left) - lipglossWidth(right)
	if gap < 1 {
		return fit(truncate(left, width), width)
	}
	return left + strings.Repeat(" ", gap) + right
}

// emptyPane is what fills the note side before anything is open.
func (m *model) emptyPane(width, height int) string {
	lines := []string{
		"",
		m.st.muted.Render("  Nothing open."),
		"",
		"  " + m.st.key.Render("enter") + m.st.muted.Render("  open the selected note"),
		"  " + m.st.key.Render("n") + m.st.muted.Render("      new note"),
		"  " + m.st.key.Render("D") + m.st.muted.Render("      today's daily note"),
		"  " + m.st.key.Render("/") + m.st.muted.Render("      search"),
		"  " + m.st.key.Render("?") + m.st.muted.Render("      all the keys"),
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = fit(lines[i], width)
	}
	return strings.Join(lines[:height], "\n")
}

// overlayView draws a picker over the body area, at the position [model.layout]
// says it is. Placing it by hand rather than with lipgloss.Place is what lets
// the mouse work out which row was clicked.
func (m *model) overlayView(height int) string {
	l := m.layout()
	box := strings.Split(m.pick.render(m.st, l.box.w, l.box.h), "\n")
	indent := strings.Repeat(" ", l.box.x)

	rows := make([]string, height)
	for i := range rows {
		row := i - (l.box.y - l.bodyTop)
		if row >= 0 && row < len(box) {
			rows[i] = fit(indent+box[row], m.width)
			continue
		}
		rows[i] = strings.Repeat(" ", m.width)
	}
	return strings.Join(rows, "\n")
}

// statusLine is the bottom row: a question if one is pending, then a message if
// there is one, and otherwise the keys worth knowing right now.
func (m *model) statusLine() string {
	if m.pr.active() {
		return m.pr.render(m.st, m.width)
	}
	if m.status != "" {
		if m.statusErr {
			return truncate(m.st.errorText.Render(m.status), m.width)
		}
		return truncate(m.st.okText.Render(m.status), m.width)
	}

	var hints [][2]string
	switch {
	case m.editing:
		hints = [][2]string{{"ctrl+s", "save"}, {"esc", "stop editing"}, {"e", "$EDITOR"}}
		if m.dirty {
			hints = append(hints, [2]string{"", "unsaved"})
		}
	case m.pick != nil:
		hints = [][2]string{{"enter", "select"}, {"esc", "close"}}
	case m.focus == paneList:
		hints = [][2]string{{"enter", "open"}, {"/", "search"}, {"n", "new"}, {"t", "tags"}, {"?", "help"}, {"q", "quit"}}
	default:
		hints = [][2]string{{"i", "edit"}, {"e", "$EDITOR"}, {"p", "raw"}, {"B", "backlinks"}, {"tab", "list"}, {"?", "help"}}
	}

	var parts []string
	for _, h := range hints {
		if h[0] == "" {
			parts = append(parts, m.st.muted.Render(h[1]))
			continue
		}
		parts = append(parts, m.st.key.Render(h[0])+m.st.muted.Render(" "+h[1]))
	}
	return truncate(strings.Join(parts, m.st.muted.Render("  ")), m.width)
}

// helpKeyColumn is how wide the key column in the help is, so the descriptions
// line up under each other.
const helpKeyColumn = 14

// showHelp lists every key, generated from the binding table.
func (m *model) showHelp() tea.Cmd {
	var items []pickItem
	last := keyGroup(-1)
	for _, b := range bindings() {
		if b.group != last {
			items = append(items, pickItem{label: m.st.paneTitle.Render(groupNames[b.group])})
			last = b.group
		}
		items = append(items, pickItem{
			label:  "  " + m.st.key.Render(fit(b.label, helpKeyColumn)),
			detail: m.st.muted.Render(b.desc),
		})
	}
	extra := []struct{ group, key, desc string }{
		{"While editing", "ctrl+s", "save, as a compare-and-swap against the note's hash"},
		{"While editing", "esc", "stop editing, keeping what you typed"},
		{"In a list overlay", "↑ ↓", "move, and so do ctrl+p and ctrl+n"},
		{"In a list overlay", "ctrl+x", "revoke, in the shares list"},
		{"In a list overlay", "ctrl+s", "share the whole folder, in the folder list"},
		{"With the mouse", "click", "open a note in the list, or follow a [[link]] in one"},
		{"With the mouse", "wheel", "move through the list, or scroll the note"},
		{"With the mouse", "click away", "close an overlay"},
	}
	group := ""
	for _, e := range extra {
		if e.group != group {
			items = append(items, pickItem{label: m.st.paneTitle.Render(e.group)})
			group = e.group
		}
		items = append(items, pickItem{
			label:  "  " + m.st.key.Render(fit(e.key, helpKeyColumn)),
			detail: m.st.muted.Render(e.desc),
		})
	}
	return m.showPicker(pickHelp, "Keys", items)
}

// noteTitle is what to call the open note in a message.
func (m *model) noteTitle() string {
	if m.note.Title != "" {
		return m.note.Title
	}
	return vaultpath.TitleFor(m.note.Path)
}
