package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/scottjab/folio/internal/vaultpath"
)

// The verbs behind the key table. Each one either changes local state and
// returns nil, or returns the command that will do the work on the server.

// moveCursor moves the list selection or scrolls the note, depending on which
// pane has the keyboard.
func (m *model) moveCursor(delta int) tea.Cmd {
	if m.focus == paneList {
		if len(m.items) == 0 {
			return nil
		}
		m.sel = clamp(m.sel+delta, 0, len(m.items)-1)
		m.clampList()
		return nil
	}
	if delta > 0 {
		m.vp.ScrollDown(delta)
	} else {
		m.vp.ScrollUp(-delta)
	}
	return nil
}

func (m *model) jump(toTop bool) tea.Cmd {
	if m.focus == paneList {
		if len(m.items) == 0 {
			return nil
		}
		if toTop {
			m.sel = 0
		} else {
			m.sel = len(m.items) - 1
		}
		m.clampList()
		return nil
	}
	if toTop {
		m.vp.GotoTop()
	} else {
		m.vp.GotoBottom()
	}
	return nil
}

// pageSize is how far a page key moves in the focused pane.
func (m *model) pageSize() int {
	if m.focus == paneList {
		return m.listHeight()
	}
	return max(1, m.vp.Height-1)
}

// openSelected loads the note the sidebar cursor is on.
func (m *model) openSelected() tea.Cmd {
	it, ok := m.current()
	if !ok {
		return nil
	}
	return m.openNote(it.vault, it.path)
}

// openNote opens a note, asking about unsaved work first.
func (m *model) openNote(vault, path string) tea.Cmd {
	if m.hasNote && vault == m.note.Vault && path == m.note.Path {
		// Already open. Re-reading it would throw away a draft to show the same
		// note back.
		m.focus = paneNote
		return nil
	}
	return m.guard(loadNote(m.ctx, m.cl, vault, path, true))
}

// guard runs next, unless there is unsaved work, in which case it asks first.
//
// Losing a draft has to be a decision rather than a side effect of pressing
// enter or clicking something, but refusing outright leaves no way to abandon
// one, so the question offers both.
func (m *model) guard(next tea.Cmd) tea.Cmd {
	if !m.dirty {
		return next
	}
	pr := newKeyPrompt(prSwitch, "unsaved changes to "+m.note.Path+".", "sd",
		"[s] save first · [d] discard · esc stay")
	pr.then = next
	m.pr = pr
	return nil
}

// target is the note a command acts on: the open one when the note pane has the
// keyboard, and the selected one otherwise.
func (m *model) target() (listItem, bool) {
	if m.focus == paneNote && m.hasNote {
		return listItem{vault: m.note.Vault, path: m.note.Path, title: m.noteTitle()}, true
	}
	if it, ok := m.current(); ok {
		return it, true
	}
	if m.hasNote {
		return listItem{vault: m.note.Vault, path: m.note.Path, title: m.noteTitle()}, true
	}
	return listItem{}, false
}

func (m *model) requireNote() bool { return m.hasNote }

// buffer is the text as it currently stands, edited or not.
func (m *model) buffer() string {
	if m.dirty || m.editing {
		return m.ta.Value()
	}
	return m.note.Content
}

// startEdit puts the cursor in the note.
func (m *model) startEdit() tea.Cmd {
	if !m.hasNote {
		return m.setStatus("no note open", true)
	}
	if !m.note.Perm.CanWrite() {
		return m.setStatus("read-only: you have read access to this note", true)
	}
	if m.editing {
		return nil
	}
	if !m.dirty {
		// Re-entering with unsaved edits must keep them; only a clean buffer is
		// reloaded from the note.
		m.ta.SetValue(m.note.Content)
	}
	m.editing = true
	m.focus = paneNote
	return m.ta.Focus()
}

// stopEdit leaves editing without discarding what was typed.
func (m *model) stopEdit() tea.Cmd {
	if !m.editing {
		return nil
	}
	m.editing = false
	m.ta.Blur()
	m.dirty = m.ta.Value() != m.note.Content
	m.renderBody()
	if m.dirty {
		return m.setStatus("still unsaved: ctrl+s to save", true)
	}
	return nil
}

// launchEditor hands the note to $EDITOR.
func (m *model) launchEditor() tea.Cmd {
	if !m.hasNote {
		return m.setStatus("no note open", true)
	}
	if !m.note.Perm.CanWrite() {
		return m.setStatus("read-only: you have read access to this note", true)
	}
	return openInEditor(m.editor, m.note.Path, m.buffer())
}

// save writes the buffer back, as a compare-and-swap unless force is set.
func (m *model) save(force bool) tea.Cmd {
	if !m.hasNote {
		return m.setStatus("no note open", true)
	}
	if !m.note.Perm.CanWrite() {
		return m.setStatus("read-only: you cannot write to this note", true)
	}
	content := m.buffer()
	if content == m.note.Content && !force {
		m.dirty = false
		if m.quitting {
			return tea.Quit
		}
		return m.setStatus("nothing to save", false)
	}
	base := m.baseSHA
	if force {
		// An empty base is an unconditional write, which is what "overwrite
		// anyway" means.
		base = ""
	}
	return saveNote(m.ctx, m.cl, m.note.Vault, m.note.Path, content, base)
}

// showBacklinks lists what links here. The backlinks came with the note, so
// this needs no round trip.
func (m *model) showBacklinks() tea.Cmd {
	if !m.hasNote {
		return m.setStatus("no note open", true)
	}
	items := make([]pickItem, 0, len(m.note.Backlinks))
	for _, l := range m.note.Backlinks {
		detail := l.Path
		if l.Alias != "" {
			detail += "  ·  linked as " + l.Alias
		}
		if l.Anchor != "" {
			detail += "  ·  #" + l.Anchor
		}
		items = append(items, pickItem{
			label:  l.Title,
			detail: m.st.muted.Render(detail),
			vault:  m.note.Vault,
			path:   l.Path,
		})
	}
	cmd := m.showPicker(pickBacklink, "Links to "+m.noteTitle(), items)
	m.pick.empty = "nothing links here yet"
	return cmd
}

// showLinks lists the wikilinks in the note, which is how you follow one
// without a mouse.
func (m *model) showLinks() tea.Cmd {
	if !m.hasNote {
		return m.setStatus("no note open", true)
	}
	targets := outgoingLinks(m.buffer())
	items := make([]pickItem, 0, len(targets))
	for _, t := range targets {
		items = append(items, pickItem{
			label:  t,
			detail: m.st.muted.Render(vaultpath.EnsureMarkdown(t)),
			vault:  m.note.Vault,
			path:   vaultpath.EnsureMarkdown(t),
		})
	}
	cmd := m.showPicker(pickLink, "Links out of "+m.noteTitle(), items)
	m.pick.empty = "this note links nowhere"
	return cmd
}

// shareFolder begins granting someone access to a whole folder, from the folder
// list. A folder grant covers everything under it, now and later.
func (m *model) shareFolder() tea.Cmd {
	it, ok := m.pick.current()
	if !ok || it.text == "" {
		return m.setStatus("pick a folder first", true)
	}
	if m.vault != m.me.Vault {
		return m.setStatus("you can only share folders in your own vault", true)
	}
	m.closePicker()
	m.pendingShare = it.text
	return loadUsers(m.ctx, m.cl)
}

// startShare begins granting someone access to the selected note.
func (m *model) startShare() tea.Cmd {
	it, ok := m.target()
	if !ok {
		return m.setStatus("no note selected", true)
	}
	// The server only ever grants out of the caller's own vault, so say so here
	// rather than letting the request come back as a confusing 403.
	if it.vault != m.me.Vault {
		return m.setStatus("you can only share notes in your own vault", true)
	}
	m.pendingShare = it.path
	return loadUsers(m.ctx, m.cl)
}
