package tui

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/scottjab/folio/internal/client"
)

// A picker is the overlay behind every list this UI puts in front of you:
// search, vaults, tags, folders, backlinks, outgoing links, shares, and the
// people you can share with. They differ in what they list and what enter does,
// which is a table and a switch, not eight components.
type pickerKind int

const (
	pickNone pickerKind = iota
	pickSearch
	pickVault
	pickTag
	pickFolder
	pickBacklink
	pickLink
	pickShare
	pickUser
	pickHelp
	// pickComplete is the `[[` link completion, and is the only picker that
	// appears while editing rather than instead of it.
	pickComplete
	pickSetting
)

// pickItem is one row. Only the fields the kind cares about are set.
type pickItem struct {
	label  string
	detail string
	// segs renders a search snippet with its matched runs highlighted.
	segs []client.SnippetSegment

	vault string
	path  string
	id    string
	// text is the generic payload: a tag, a folder, a login.
	text string
	perm client.Perm
	// mine marks a share the caller granted, and so may revoke.
	mine bool
}

type picker struct {
	kind  pickerKind
	title string
	hint  string
	items []pickItem
	// view indexes items, after the filter box has had its say.
	view []int
	sel  int
	top  int

	input textinput.Model
	// live means the server answers each keystroke, so the filter box must not
	// also filter locally.
	live    bool
	twoLine bool
	// seq drops search responses that arrived after the query moved on.
	seq int
	// empty is what to say when there is nothing to show.
	empty string
	// detailRight puts the detail in a right-hand column, which suits a count or
	// a permission. Help text reads better next to its key.
	detailRight bool
}

func newPicker(kind pickerKind, title string) *picker {
	in := textinput.New()
	in.Prompt = "› "
	in.Placeholder = "filter"
	in.CharLimit = 200
	in.Focus()

	p := &picker{kind: kind, title: title, input: in, empty: "nothing here", detailRight: true}
	switch kind {
	case pickSearch:
		p.live = true
		p.twoLine = true
		p.input.Placeholder = `search: words, "phrases", tag:go, path:Daily, -not`
		p.hint = "enter open · esc close"
		p.empty = "type to search"
	case pickShare:
		p.twoLine = true
		p.hint = "d revoke · esc close"
	case pickBacklink, pickLink:
		p.twoLine = true
		p.hint = "enter open · esc close"
	case pickComplete, pickSetting:
		p.twoLine = true
		p.hint = "enter insert · esc cancel"
	case pickHelp:
		p.hint = "esc close"
		p.detailRight = false
	default:
		p.hint = "enter select · esc close"
	}
	return p
}

// setItems replaces the contents and reapplies the filter.
func (p *picker) setItems(items []pickItem) {
	p.items = items
	p.refilter()
}

// refilter recomputes which rows are visible.
func (p *picker) refilter() {
	q := strings.ToLower(strings.TrimSpace(p.input.Value()))
	p.view = p.view[:0]
	for i, it := range p.items {
		if p.live || q == "" || strings.Contains(strings.ToLower(it.label+" "+it.detail+" "+it.path), q) {
			p.view = append(p.view, i)
		}
	}
	if p.sel >= len(p.view) {
		p.sel = max(0, len(p.view)-1)
	}
	p.scrollIntoView(0)
}

func (p *picker) move(delta int) {
	if len(p.view) == 0 {
		return
	}
	p.sel = clamp(p.sel+delta, 0, len(p.view)-1)
}

// scrollIntoView keeps the selection on screen for a viewport of h rows.
func (p *picker) scrollIntoView(h int) {
	visible := max(1, h/p.rowsPerItem())
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+visible {
		p.top = p.sel - visible + 1
	}
	p.top = clamp(p.top, 0, max(0, len(p.view)-1))
}

// current returns the selected item.
func (p *picker) current() (pickItem, bool) {
	if p.sel < 0 || p.sel >= len(p.view) {
		return pickItem{}, false
	}
	return p.items[p.view[p.sel]], true
}

// takesText reports whether the filter box should see ordinary keystrokes.
func (p *picker) takesText() bool { return p.kind != pickHelp }

// rowsPerItem is how many screen lines one item occupies.
func (p *picker) rowsPerItem() int {
	if p.twoLine {
		return 2
	}
	return 1
}

// listTopRow is where the first item is drawn, counting the box's top border as
// row zero. Drawing and clicking both go through it.
func (p *picker) listTopRow() int {
	if p.takesText() {
		return 4 // border, title, filter, blank
	}
	return 2 // border, title
}

// update feeds a key to the filter box.
func (p *picker) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	if !p.live {
		p.refilter()
	}
	return cmd
}

// render draws the picker into a box exactly width by height, border included.
//
// The arithmetic is worth being careful about: lipgloss counts padding inside a
// style's Width and the border outside it, so a row built at the box's own width
// wraps, and every wrapped row makes the box a line taller than the screen has
// room for.
func (p *picker) render(st *styles, width, height int) string {
	const border, padding = 2, 2
	inner := max(4, width-border-padding)

	var b strings.Builder
	b.WriteString(st.overlayTitle.Render(truncate(p.title, inner)))
	b.WriteByte('\n')

	if p.takesText() {
		b.WriteString(truncate(p.input.View(), inner))
		b.WriteString("\n\n")
	}

	// What is left after the title, the filter box, the footer, and the border.
	listHeight := max(1, height-p.listTopRow()-1-border/2)
	p.scrollIntoView(listHeight)

	if len(p.view) == 0 {
		b.WriteString(st.muted.Render(p.empty))
		b.WriteByte('\n')
	}

	perRow := p.rowsPerItem()
	shown := 0
	for i := p.top; i < len(p.view) && shown+perRow <= listHeight; i++ {
		it := p.items[p.view[i]]
		selected := i == p.sel

		label := it.label
		switch {
		case p.twoLine:
			label = fit(truncate(label, inner), inner)
		case p.detailRight && it.detail != "":
			// A count or a permission reads as a column, so it goes hard right.
			left := max(4, inner-ansi.StringWidth(it.detail)-1)
			label = fit(truncate(label, left), left) + " " + it.detail
			label = fit(truncate(label, inner), inner)
		case it.detail != "":
			label = fit(truncate(label+"  "+it.detail, inner), inner)
		default:
			label = fit(truncate(label, inner), inner)
		}
		if selected {
			b.WriteString(st.itemSelected.Render(label))
		} else {
			b.WriteString(label)
		}
		b.WriteByte('\n')
		shown++

		if p.twoLine {
			detail := it.detail
			if len(it.segs) > 0 {
				detail = renderSnippet(st, it.segs)
			}
			b.WriteString("  " + truncate(detail, inner-2))
			b.WriteByte('\n')
			shown++
		}
	}

	for ; shown < listHeight; shown++ {
		b.WriteByte('\n')
	}

	footer := st.muted.Render(p.hint)
	if n := len(p.view); n > 0 {
		pos := strconv.Itoa(p.sel+1) + "/" + strconv.Itoa(n)
		footer = st.muted.Render(pos) + "  " + footer
	}
	b.WriteString(truncate(footer, inner))

	// Width covers the padding but not the border, so this comes out at exactly
	// the width asked for.
	return st.overlay.Width(inner + padding).Render(b.String())
}

// renderSnippet styles a search snippet, highlighting what matched.
func renderSnippet(st *styles, segs []client.SnippetSegment) string {
	var b strings.Builder
	for _, seg := range segs {
		if seg.Match {
			b.WriteString(st.match.Render(seg.Text))
			continue
		}
		b.WriteString(st.muted.Render(seg.Text))
	}
	return b.String()
}
