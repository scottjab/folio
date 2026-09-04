package tui

import (
	"slices"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scottjab/folio/internal/client"
	"github.com/scottjab/folio/internal/vaultpath"
)

// The key table is data rather than a switch, for the same reason the
// subcommand table in cmd/folio is: the help screen is generated from it, so it
// cannot drift from what the keys actually do.
type keyGroup int

const (
	gMove keyGroup = iota
	gNote
	gFind
	gShare
	gApp
)

var groupNames = map[keyGroup]string{
	gMove:  "Moving around",
	gNote:  "The open note",
	gFind:  "Finding things",
	gShare: "Sharing",
	gApp:   "Everything else",
}

type binding struct {
	keys  []string
	label string
	desc  string
	group keyGroup
	run   func(*model) tea.Cmd
}

var (
	bindingsOnce sync.Once
	bindingsList []binding
)

// bindings are the keys that apply when no overlay, prompt, or editor has the
// keyboard. They are built on first use rather than at package initialisation
// because the help screen is generated from this table, and a package variable
// holding it would be an initialisation cycle.
func bindings() []binding {
	bindingsOnce.Do(func() { bindingsList = allBindings() })
	return bindingsList
}

func allBindings() []binding {
	return []binding{
		{
			keys: []string{"tab"}, label: "tab", desc: "switch between the list and the note",
			group: gMove,
			run: func(m *model) tea.Cmd {
				if m.focus == paneList {
					m.focus = paneNote
				} else {
					m.focus = paneList
				}
				return nil
			},
		},
		{
			keys: []string{"down", "j"}, label: "j / ↓", desc: "down", group: gMove,
			run: func(m *model) tea.Cmd { return m.moveCursor(1) },
		},
		{
			keys: []string{"up", "k"}, label: "k / ↑", desc: "up", group: gMove,
			run: func(m *model) tea.Cmd { return m.moveCursor(-1) },
		},
		{
			keys: []string{"pgdown", " "}, label: "space / pgdn", desc: "page down", group: gMove,
			run: func(m *model) tea.Cmd { return m.moveCursor(m.pageSize()) },
		},
		{
			keys: []string{"pgup", "b"}, label: "b / pgup", desc: "page up", group: gMove,
			run: func(m *model) tea.Cmd { return m.moveCursor(-m.pageSize()) },
		},
		{
			keys: []string{"ctrl+d"}, label: "ctrl+d", desc: "half a page down", group: gMove,
			run: func(m *model) tea.Cmd { return m.moveCursor(m.pageSize() / 2) },
		},
		{
			keys: []string{"ctrl+u"}, label: "ctrl+u", desc: "half a page up", group: gMove,
			run: func(m *model) tea.Cmd { return m.moveCursor(-m.pageSize() / 2) },
		},
		{
			keys: []string{"home", "g"}, label: "g / home", desc: "to the top", group: gMove,
			run: func(m *model) tea.Cmd { return m.jump(true) },
		},
		{
			keys: []string{"end", "G"}, label: "G / end", desc: "to the bottom", group: gMove,
			run: func(m *model) tea.Cmd { return m.jump(false) },
		},
		{
			keys: []string{"enter"}, label: "enter", desc: "open the selected note, or start editing",
			group: gMove,
			run: func(m *model) tea.Cmd {
				if m.focus == paneList {
					return m.openSelected()
				}
				return m.startEdit()
			},
		},

		{
			keys: []string{"i"}, label: "i", desc: "edit here", group: gNote,
			run: func(m *model) tea.Cmd { return m.startEdit() },
		},
		{
			keys: []string{"e"}, label: "e", desc: "edit in $EDITOR, then save", group: gNote,
			run: func(m *model) tea.Cmd { return m.launchEditor() },
		},
		{
			keys: []string{"p"}, label: "p", desc: "toggle rendered and raw markdown", group: gNote,
			run: func(m *model) tea.Cmd {
				m.raw = !m.raw
				m.renderBody()
				if m.raw {
					return m.setStatus("raw markdown", false)
				}
				return m.setStatus("rendered", false)
			},
		},
		{
			keys: []string{"ctrl+s"}, label: "ctrl+s", desc: "save", group: gNote,
			run: func(m *model) tea.Cmd { return m.save(false) },
		},
		{
			keys: []string{"a"}, label: "a", desc: "append a line to this note", group: gNote,
			run: func(m *model) tea.Cmd {
				if !m.requireNote() {
					return m.setStatus("no note open", true)
				}
				pr := newTextPrompt(prAppend, "append to "+m.note.Path+":", "")
				// The note is captured now, so an event that moves it while the
				// question is up cannot redirect the text somewhere else.
				pr.vault, pr.path = m.note.Vault, m.note.Path
				m.pr = pr
				return m.pr.input.Focus()
			},
		},
		{
			keys: []string{"n"}, label: "n", desc: "new note", group: gNote,
			run: func(m *model) tea.Cmd {
				if m.vault != m.me.Vault {
					return m.setStatus("you can only create notes in your own vault (v to switch back)", true)
				}
				folder := m.query.folder
				if folder == "" {
					if it, ok := m.current(); ok {
						folder = folderOf(it.path)
					}
				}
				prefill := ""
				if folder != "" {
					prefill = folder + "/"
				}
				m.pr = newTextPrompt(prNew, "new note:", prefill)
				return m.pr.input.Focus()
			},
		},
		{
			keys: []string{"m"}, label: "m", desc: "rename or move", group: gNote,
			run: func(m *model) tea.Cmd {
				it, ok := m.target()
				if !ok {
					return m.setStatus("no note selected", true)
				}
				p := newTextPrompt(prRename, "rename to:", it.path)
				p.vault, p.path = it.vault, it.path
				m.pr = p
				return m.pr.input.Focus()
			},
		},
		{
			keys: []string{"x"}, label: "x", desc: "delete (to the vault's trash)", group: gNote,
			run: func(m *model) tea.Cmd {
				it, ok := m.target()
				if !ok {
					return m.setStatus("no note selected", true)
				}
				p := newKeyPrompt(prDelete, "delete "+it.path+"?", "yn", "[y] delete · [n] keep")
				p.vault, p.path = it.vault, it.path
				m.pr = p
				return nil
			},
		},
		{
			keys: []string{"D"}, label: "D", desc: "today's daily note", group: gNote,
			run: func(m *model) tea.Cmd { return m.guard(loadDaily(m.ctx, m.cl, m.vault)) },
		},
		{
			keys: []string{"A"}, label: "A", desc: "attach a file to this note", group: gNote,
			run: func(m *model) tea.Cmd { return m.startAttach() },
		},
		{
			keys: []string{"o"}, label: "o", desc: "open this note in a browser", group: gNote,
			run: func(m *model) tea.Cmd {
				it, ok := m.target()
				if !ok {
					return m.setStatus("no note selected", true)
				}
				return openBrowser(noteURL(m.cl.Server(), it.vault, it.path))
			},
		},

		{
			keys: []string{"/", "ctrl+k"}, label: "/ or ctrl+k", desc: "search every vault you can read",
			group: gFind,
			run: func(m *model) tea.Cmd {
				cmd := m.showPicker(pickSearch, "Search", nil)
				if q := m.searchQuery; q != "" {
					m.pick.input.SetValue(q)
					m.pick.input.CursorEnd()
					m.pick.seq++
					return tea.Batch(cmd, runSearch(m.ctx, m.cl, q, m.pick.seq))
				}
				return cmd
			},
		},
		{
			keys: []string{"t"}, label: "t", desc: "filter by tag", group: gFind,
			run: func(m *model) tea.Cmd { return loadTags(m.ctx, m.cl) },
		},
		{
			keys: []string{"f"}, label: "f", desc: "filter by folder", group: gFind,
			run: func(m *model) tea.Cmd { return loadFolders(m.ctx, m.cl) },
		},
		{
			keys: []string{"v"}, label: "v", desc: "switch vault", group: gFind,
			run: func(m *model) tea.Cmd {
				items := make([]pickItem, 0, len(m.vaults))
				for _, v := range m.vaults {
					label := v.Vault
					detail := v.OwnerLogin
					if v.IsMine {
						detail = m.st.muted.Render("yours")
					}
					items = append(items, pickItem{label: label, detail: detail, text: v.Vault})
				}
				return m.showPicker(pickVault, "Vaults", items)
			},
		},
		{
			keys: []string{"B"}, label: "B", desc: "notes linking to this one", group: gFind,
			run: func(m *model) tea.Cmd { return m.showBacklinks() },
		},
		{
			keys: []string{"L"}, label: "L", desc: "links out of this note", group: gFind,
			run: func(m *model) tea.Cmd { return m.showLinks() },
		},

		{
			keys: []string{"s"}, label: "s", desc: "shares, both directions", group: gShare,
			run: func(m *model) tea.Cmd { return loadShares(m.ctx, m.cl, "") },
		},
		{
			keys: []string{"S"}, label: "S", desc: "share this note with someone", group: gShare,
			run: func(m *model) tea.Cmd { return m.startShare() },
		},

		{
			keys: []string{"r"}, label: "r", desc: "reload from the server", group: gApp,
			run: func(m *model) tea.Cmd {
				cmds := []tea.Cmd{m.refreshList()}
				status := "reloaded"
				if m.hasNote && !m.dirty {
					cmds = append(cmds, loadNote(m.ctx, m.cl, m.note.Vault, m.note.Path, false))
				} else if m.dirty {
					// Saying "reloaded" while quietly skipping the note would be
					// a lie, and the unsaved buffer is what must not be lost.
					status = "reloaded the list; the note has unsaved edits, so it was left alone"
				}
				return tea.Batch(append(cmds, m.setStatus(status, false))...)
			},
		},
		{
			keys: []string{"M"}, label: "M", desc: "mouse on or off (off frees the mouse to select text)",
			group: gApp,
			run: func(m *model) tea.Cmd {
				m.mouse = !m.mouse
				if m.mouse {
					return tea.Batch(tea.EnableMouseCellMotion, m.setStatus("mouse on", false))
				}
				return tea.Batch(tea.DisableMouse, m.setStatus("mouse off: the terminal has the pointer back", false))
			},
		},
		{
			keys: []string{","}, label: ",", desc: "settings: where attachments go", group: gApp,
			run: func(m *model) tea.Cmd { return m.showSettings() },
		},
		{
			keys: []string{"?"}, label: "?", desc: "this help", group: gApp,
			run: func(m *model) tea.Cmd { return m.showHelp() },
		},
		{
			keys: []string{"esc"}, label: "esc", desc: "back: clear a filter, close an overlay", group: gApp,
			run: func(m *model) tea.Cmd { return m.back() },
		},
		{
			keys: []string{"q", "ctrl+c"}, label: "q", desc: "quit", group: gApp,
			run: func(m *model) tea.Cmd { return m.quit() },
		},
	}
}

// handleKey routes a keypress to whatever currently owns the keyboard.
func (m *model) handleKey(msg tea.KeyMsg) tea.Cmd {
	key := msg.String()
	switch {
	case m.pr.active():
		return m.promptKey(msg, key)
	case m.pick != nil:
		return m.pickerKey(msg, key)
	case m.editing:
		return m.editKey(msg, key)
	}
	return m.normalKey(key)
}

func (m *model) normalKey(key string) tea.Cmd {
	for _, b := range bindings() {
		if slices.Contains(b.keys, key) {
			return b.run(m)
		}
	}
	return nil
}

// editKey handles the keys that mean something while typing into a note. Every
// other key belongs to the textarea.
func (m *model) editKey(msg tea.KeyMsg, key string) tea.Cmd {
	switch key {
	case "esc":
		return m.stopEdit()
	case "ctrl+s":
		return m.save(false)
	case "ctrl+c":
		return m.quit()
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.dirty = m.ta.Value() != m.note.Content

	// Typing the second bracket is what opens completion, which is the habit
	// anyone arriving from Obsidian already has. Only a literal "[" does it, so
	// arrowing back over an existing "[[" does not reopen the list.
	if key == "[" && m.justTypedOpenBracket() {
		return tea.Batch(cmd, m.startComplete())
	}
	return cmd
}

// pickerKey drives an overlay list.
func (m *model) pickerKey(msg tea.KeyMsg, key string) tea.Cmd {
	p := m.pick
	switch key {
	case "esc", "ctrl+c":
		m.closePicker()
		return nil
	case "enter":
		return m.activate()
	case "down", "ctrl+n":
		p.move(1)
		return nil
	case "up", "ctrl+p":
		p.move(-1)
		return nil
	case "pgdown":
		p.move(8)
		return nil
	case "pgup":
		p.move(-8)
		return nil
	case "ctrl+x":
		if p.kind == pickShare {
			return m.revokeSelected()
		}
		return nil
	case "ctrl+s":
		if p.kind == pickFolder {
			return m.shareFolder()
		}
		return nil
	}

	// Without a filter box, the list takes vim keys too.
	if !p.takesText() {
		switch key {
		case "j":
			p.move(1)
			return nil
		case "k":
			p.move(-1)
			return nil
		}
		return nil
	}

	cmd := p.update(msg)
	if p.live {
		p.seq++
		return tea.Batch(cmd, debounceSearch(p.seq))
	}
	return cmd
}

// promptKey answers the question on the status line.
func (m *model) promptKey(msg tea.KeyMsg, key string) tea.Cmd {
	if key == "esc" || key == "ctrl+c" {
		return m.cancelPrompt()
	}
	if m.pr.textual() {
		if key == "enter" {
			return m.submitPrompt()
		}
		return m.pr.update(msg)
	}
	if !m.pr.accepts(key) {
		return nil
	}
	return m.answerPrompt(key)
}

func (m *model) cancelPrompt() tea.Cmd {
	kind := m.pr.kind
	m.pr = prompt{}
	if kind == prConflict {
		return m.setStatus("still unsaved; your version is also in the conflict file", true)
	}
	return nil
}

func (m *model) submitPrompt() tea.Cmd {
	p := m.pr
	value := strings.TrimSpace(p.input.Value())
	m.pr = prompt{}
	if value == "" {
		return nil
	}

	switch p.kind {
	case prNew:
		return createNote(m.ctx, m.cl, m.vault, vaultpath.EnsureMarkdown(value), "")
	case prRename:
		if value == p.path {
			return nil
		}
		return moveNote(m.ctx, m.cl, p.vault, p.path, vaultpath.EnsureMarkdown(value))
	case prAppend:
		return appendNote(m.ctx, m.cl, p.vault, p.path, value, "")
	case prAttach:
		return uploadFile(m.ctx, m.cl, p.vault, p.path, value)
	case prAttachFolder:
		next := m.prefs
		next.AttachmentMode, next.AttachmentFolder = p.text, value
		return savePrefs(m.ctx, m.cl, next)
	}
	return nil
}

func (m *model) answerPrompt(key string) tea.Cmd {
	p := m.pr
	m.pr = prompt{}

	switch p.kind {
	case prDelete:
		if key == "y" {
			return deleteNote(m.ctx, m.cl, p.vault, p.path)
		}
	case prRevoke:
		if key == "y" {
			return revokeShare(m.ctx, m.cl, p.id, p.path)
		}
	case prSwitch:
		switch key {
		case "s":
			// Queueing the interrupted action behind the save means a conflict
			// stops both rather than losing the buffer.
			m.pendingCmd = p.then
			return m.save(false)
		case "d":
			m.dirty = false
			m.stale = false
			return p.then
		}
	case prQuit:
		switch key {
		case "s":
			m.quitting = true
			return m.save(false)
		case "d":
			return tea.Quit
		}
	case prConflict:
		switch key {
		case "o":
			// Overwrite: the server already stashed our rejected content in the
			// conflict file, so the other version is recoverable either way.
			return m.save(true)
		case "r":
			m.dirty = false
			m.stale = false
			return tea.Batch(
				loadNote(m.ctx, m.cl, m.note.Vault, m.note.Path, false),
				m.setStatus("loaded the server's version; yours is in the conflict file", false),
			)
		}
	case prPerm:
		perm := client.PermRead
		if key == "w" {
			perm = client.PermWrite
		}
		// A path without a markdown extension is a folder grant, which covers
		// everything under it.
		return grantShare(m.ctx, m.cl, p.path, !vaultpath.IsMarkdown(p.path), p.text, perm)
	}
	return nil
}

// activate does whatever enter means for the open picker.
func (m *model) activate() tea.Cmd {
	p := m.pick
	it, ok := p.current()
	if !ok {
		return nil
	}

	switch p.kind {
	case pickSearch:
		// The results become the sidebar listing, so the next result is one
		// keypress away rather than a re-search.
		m.searchQuery = p.input.Value()
		m.label = "search: " + m.searchQuery
		items := make([]listItem, 0, len(p.items))
		for _, r := range p.items {
			items = append(items, listItem{
				vault: r.vault, path: r.path, title: r.label,
				detail: "", shared: r.vault != m.me.Vault,
			})
		}
		m.closePicker()
		m.setItems(items)
		return m.openNote(it.vault, it.path)

	case pickVault:
		m.closePicker()
		m.vault = it.text
		m.searchQuery = ""
		m.query = listQuery{vault: it.text}
		return tea.Batch(m.refreshList(), m.setStatus("vault "+it.text, false))

	case pickTag:
		m.closePicker()
		m.searchQuery = ""
		m.query = listQuery{vault: m.vault, tag: it.text}
		return m.refreshList()

	case pickFolder:
		m.closePicker()
		m.searchQuery = ""
		m.query = listQuery{vault: m.vault, folder: it.text}
		return m.refreshList()

	case pickBacklink, pickLink, pickShare:
		if it.path == "" {
			return nil
		}
		vault := it.vault
		if vault == "" {
			vault = m.vault
		}
		m.closePicker()
		if !vaultpath.IsMarkdown(it.path) {
			// A folder share: show what is in it, which is the only useful
			// thing enter can mean here.
			m.vault = vault
			m.searchQuery = ""
			m.query = listQuery{vault: vault, folder: it.path}
			return m.refreshList()
		}
		return m.openNote(vault, it.path)

	case pickUser:
		path := m.pendingShare
		m.closePicker()
		pr := newKeyPrompt(prPerm, "share "+path+" with "+it.text+":", "rw",
			"[r] read · [w] write · esc cancel")
		pr.path, pr.text = path, it.text
		m.pr = pr
		return nil

	case pickComplete:
		return m.insertCompletion(it.text)

	case pickSetting:
		return m.chooseAttachmentMode(it.text)

	case pickHelp:
		m.closePicker()
	}
	return nil
}

func (m *model) revokeSelected() tea.Cmd {
	it, ok := m.pick.current()
	if !ok {
		return nil
	}
	if !it.mine {
		return m.setStatus("that share is not yours to revoke", true)
	}
	m.closePicker()
	p := newKeyPrompt(prRevoke, "revoke access to "+it.path+"?", "yn", "[y] revoke · [n] keep")
	p.id, p.path = it.id, it.path
	m.pr = p
	return nil
}

// back is escape: undo the most recent narrowing, one step at a time.
func (m *model) back() tea.Cmd {
	switch {
	case m.searchQuery != "":
		m.searchQuery = ""
		return m.refreshList()
	case m.query.tag != "" || m.query.folder != "":
		m.query = listQuery{vault: m.vault}
		return m.refreshList()
	case m.focus == paneNote:
		m.focus = paneList
		return nil
	}
	return nil
}

func (m *model) quit() tea.Cmd {
	if m.dirty {
		m.pr = newKeyPrompt(prQuit, "unsaved changes to "+m.note.Path+".", "sd",
			"[s] save and quit · [d] discard · esc cancel")
		return nil
	}
	return tea.Quit
}
