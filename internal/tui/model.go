// Package tui is folio's terminal client.
//
// It is a full client, not a viewer: everything the browser app can do to a
// note, this can do, because both go through the same JSON API. Reading,
// searching, writing, renaming, deleting, daily notes, backlinks, sharing, and
// the live event stream are all here.
//
// Identity works exactly as it does everywhere else in folio. The TUI runs on
// your machine, so its requests reach the server from your tailnet address and
// WhoIs says who you are. There is nothing to log into.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/scottjab/folio/internal/client"
	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/vaultpath"
)

// Options configures a run of the terminal client.
type Options struct {
	// Client talks to the folio server. Required.
	Client *client.Client
	// Editor overrides $VISUAL and $EDITOR for the handoff.
	Editor string
	// Note, if set, is the note to open at startup, as "path" in the caller's
	// own vault or "vault/path" elsewhere.
	Note string
}

// pane is which half of the screen has the keyboard.
type pane int

const (
	paneList pane = iota
	paneNote
)

// listItem is one row of the sidebar. Search hits and note listings are
// flattened into this so the sidebar has one shape to draw.
type listItem struct {
	vault  string
	path   string
	title  string
	detail string
	// shared marks a note in someone else's vault.
	shared bool
}

type model struct {
	ctx    context.Context
	cl     *client.Client
	st     *styles
	editor string

	width, height int
	ready         bool

	me     client.Me
	vaults []client.Vault
	vault  string

	// The sidebar listing, and what produced it.
	label       string
	items       []listItem
	sel, top    int
	query       listQuery
	searchQuery string

	// The open note.
	note    client.Note
	hasNote bool
	body    []string
	vp      viewport.Model
	raw     bool

	// Editing state. baseSHA is what the buffer was built from, and is what
	// makes a save a compare-and-swap.
	editing bool
	ta      textarea.Model
	dirty   bool
	baseSHA string
	// stale means the note changed on the server while we hold unsaved edits.
	stale bool

	focus pane

	pick *picker
	pr   prompt

	// Transient status line. statusSeq expires an old message without cancelling
	// a newer one.
	status    string
	statusErr bool
	statusSeq int

	connected bool
	events    <-chan client.Event

	// mouse is whether the terminal is reporting pointer events. It can be
	// turned off, because a terminal that is reporting them cannot also be used
	// to select text.
	mouse bool

	// pendingShare holds the note being shared while the user picks a grantee.
	pendingShare string
	// startup is the note named on the command line, opened once identity is
	// known and we can tell whose vault it is in.
	startup string
	// pendingCmd is what to do once the save in flight has landed: the "save
	// first" half of every question about unsaved work.
	pendingCmd tea.Cmd

	quitting bool
}

// Run starts the terminal client and blocks until it exits.
func Run(ctx context.Context, opts Options) error {
	if opts.Client == nil {
		return errors.New("tui: no client")
	}
	m := newModel(ctx, opts)
	p := tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithContext(ctx),
		// Cell motion rather than all motion: folio wants clicks and the wheel,
		// and reporting every pointer move would be traffic nothing reads.
		tea.WithMouseCellMotion(),
	)
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		// Ctrl-C at the shell, or a SIGTERM. Not a failure.
		return nil
	}
	return err
}

func newModel(ctx context.Context, opts Options) *model {
	st := newStyles()

	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// The defaults cap a textarea at 99 rows and 500 columns, which a maximised
	// terminal on a big screen runs straight into.
	ta.MaxHeight = 0
	ta.MaxWidth = 0
	ta.Placeholder = "write something"

	editor := opts.Editor
	if editor == "" {
		editor = defaultEditor()
	}

	m := &model{
		ctx:    ctx,
		cl:     opts.Client,
		st:     st,
		editor: editor,
		ta:     ta,
		vp:     viewport.New(80, 20),
		label:  "Notes",
		mouse:  true,
	}
	m.startup = opts.Note
	// A sensible size before the terminal reports one, so the first frame is the
	// app rather than a placeholder.
	m.resize(80, 24)
	return m
}

func (m *model) Init() tea.Cmd {
	m.events = m.cl.Watch(m.ctx)
	return tea.Batch(loadMe(m.ctx, m.cl), waitForEvent(m.events))
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil

	case tea.KeyMsg:
		return m, m.handleKey(msg)

	case tea.MouseMsg:
		return m, m.handleMouse(msg)

	case meMsg:
		return m, m.onMe(msg)

	case notesMsg:
		return m, m.onNotes(msg)

	case noteMsg:
		return m, m.onNote(msg)

	case savedMsg:
		return m, m.onSaved(msg)

	case createdMsg:
		if msg.err != nil {
			return m, m.fail("create", msg.err)
		}
		return m, tea.Batch(
			m.setStatus("created "+msg.sum.Path, false),
			loadNote(m.ctx, m.cl, msg.sum.Vault, msg.sum.Path, true),
			m.refreshList(),
		)

	case movedMsg:
		if msg.err != nil {
			return m, m.fail("rename", msg.err)
		}
		return m, tea.Batch(
			m.setStatus(fmt.Sprintf("%s → %s", msg.from, msg.sum.Path), false),
			loadNote(m.ctx, m.cl, msg.sum.Vault, msg.sum.Path, false),
			m.refreshList(),
		)

	case deletedMsg:
		if msg.err != nil {
			return m, m.fail("delete", msg.err)
		}
		if m.hasNote && m.note.Vault == msg.vault && m.note.Path == msg.path {
			m.closeNote()
		}
		return m, tea.Batch(m.setStatus("deleted "+msg.path+" (it is in the vault's trash)", false), m.refreshList())

	case searchMsg:
		return m, m.onSearch(msg)

	case searchDueMsg:
		if m.pick != nil && m.pick.kind == pickSearch && m.pick.seq == msg.seq {
			return m, runSearch(m.ctx, m.cl, m.pick.input.Value(), msg.seq)
		}
		return m, nil

	case tagsMsg:
		if msg.err != nil {
			return m, m.fail("tags", msg.err)
		}
		items := make([]pickItem, 0, len(msg.tags))
		for _, t := range msg.tags {
			items = append(items, pickItem{
				label:  m.st.tag.Render("#" + t.Tag),
				detail: m.st.muted.Render(fmt.Sprintf("%d", t.Count)),
				text:   t.Tag,
			})
		}
		return m, m.showPicker(pickTag, "Tags", items)

	case foldersMsg:
		if msg.err != nil {
			return m, m.fail("folders", msg.err)
		}
		items := []pickItem{{label: "(all notes)", text: ""}}
		for _, f := range msg.folders {
			items = append(items, pickItem{label: f + "/", text: f})
		}
		return m, m.showPicker(pickFolder, "Folders", items)

	case usersMsg:
		if msg.err != nil {
			return m, m.fail("users", msg.err)
		}
		items := make([]pickItem, 0, len(msg.users))
		for _, u := range msg.users {
			label := u.Login
			if u.DisplayName != "" && u.DisplayName != u.Login {
				label = fmt.Sprintf("%s (%s)", u.Login, u.DisplayName)
			}
			items = append(items, pickItem{label: label, text: u.Login})
		}
		p := m.showPicker(pickUser, "Share "+m.pendingShare+" with", items)
		if len(items) == 0 {
			m.pick.empty = "no other tailnet users are known to this server yet"
		}
		return m, p

	case sharesMsg:
		return m, m.onShares(msg)

	case shareChangedMsg:
		if msg.err != nil {
			return m, m.fail("share", msg.err)
		}
		cmd := m.setStatus(msg.text, false)
		if m.pick != nil && m.pick.kind == pickShare {
			return m, tea.Batch(cmd, loadShares(m.ctx, m.cl, ""))
		}
		return m, cmd

	case editorDoneMsg:
		return m, m.onEditorDone(msg)

	case eventMsg:
		return m, m.onEvent(msg)

	case statusMsg:
		return m, m.setStatus(msg.text, msg.isErr)

	case clearStatusMsg:
		if msg.seq == m.statusSeq {
			m.status, m.statusErr = "", false
		}
		return m, nil
	}

	// Anything else (cursor blinks, viewport internals) goes to whatever has
	// the keyboard.
	return m, m.forward(msg)
}

// forward passes a message to the focused sub-component.
func (m *model) forward(msg tea.Msg) tea.Cmd {
	switch {
	case m.pr.textual():
		return m.pr.update(msg)
	case m.pick != nil && m.pick.takesText():
		return m.pick.update(msg)
	case m.editing:
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return cmd
	}
	return nil
}

// resize recomputes the layout. Every pane size in the UI is derived here and
// nowhere else.
func (m *model) resize(w, h int) {
	// A terminal that cannot say how big it is still has to be usable: without
	// this, a zero size leaves the UI on its loading line forever. Anything that
	// does not know its own size is almost certainly 80x24.
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	m.width, m.height = w, h
	m.ready = true

	body := max(1, h-2) // title bar, status line
	noteW := m.notePaneWidth()

	m.vp.Width = noteW
	m.vp.Height = max(1, body-1) // the note's own header line

	m.ta.SetWidth(noteW)
	m.ta.SetHeight(max(1, body-1))

	m.renderBody()
}

// sidebarWidth is how wide the note list is, or zero when the window is too
// narrow to show both panes at once.
func (m *model) sidebarWidth() int {
	if m.width < 62 {
		return 0
	}
	return clamp(m.width/3, 26, 46)
}

func (m *model) notePaneWidth() int { return m.layout().renderW }

// renderBody rebuilds the note pane's lines from the note's content.
func (m *model) renderBody() {
	if !m.hasNote {
		m.body = nil
		m.vp.SetContent("")
		return
	}
	if m.editing {
		return // the textarea owns the pane
	}
	// An unsaved buffer is what the reader should see; the note's own content is
	// only the server's last word on it.
	content := m.note.Content
	if m.dirty {
		content = m.ta.Value()
	}
	if m.raw {
		var lines []string
		for _, l := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
			lines = append(lines, hardwrap(l, max(8, m.notePaneWidth()))...)
		}
		m.body = lines
	} else {
		r := renderer{st: m.st, width: max(8, m.notePaneWidth())}
		m.body = r.render(content)
	}
	m.vp.SetContent(strings.Join(m.body, "\n"))
}

// ---------------------------------------------------------------- messages

func (m *model) onMe(msg meMsg) tea.Cmd {
	if msg.err != nil {
		// Without an identity there is nothing to show, so this one is fatal
		// rather than a status line nobody can act on.
		return tea.Sequence(m.fail("who am I", msg.err), tea.Quit)
	}
	m.me = msg.me
	m.vaults = msg.vaults
	m.vault = msg.me.Vault
	m.query = listQuery{vault: m.vault}

	startup := m.startup
	m.startup = ""

	cmds := []tea.Cmd{m.refreshList()}
	switch {
	case startup != "":
		vault, path := m.vault, startup
		// "vault/path" addresses someone else's note; a bare path is your own.
		for _, v := range msg.vaults {
			if strings.HasPrefix(startup, v.Vault+"/") {
				vault, path = v.Vault, strings.TrimPrefix(startup, v.Vault+"/")
				break
			}
		}
		cmds = append(cmds, loadNote(m.ctx, m.cl, vault, vaultpath.EnsureMarkdown(path), true))
	default:
		// Opening on today's daily note is what the web app does, and it means
		// the first thing on screen is somewhere you can write.
		cmds = append(cmds, loadDaily(m.ctx, m.cl, m.vault))
	}
	return tea.Batch(cmds...)
}

func (m *model) onNotes(msg notesMsg) tea.Cmd {
	if msg.err != nil {
		return m.fail("list notes", msg.err)
	}
	// A listing that arrived after the user moved on is dropped, or the sidebar
	// flickers back to the previous filter.
	if msg.from != m.query || m.searchQuery != "" {
		return nil
	}
	m.label = msg.label
	m.setItems(summariesToItems(msg.notes, m.vault))
	return nil
}

func (m *model) onNote(msg noteMsg) tea.Cmd {
	if msg.err != nil {
		if errors.Is(msg.err, client.ErrNotFound) {
			return m.setStatus("no such note", true)
		}
		return m.fail("open note", msg.err)
	}
	m.setNote(msg.note)
	if msg.focus {
		m.focus = paneNote
		m.selectPath(msg.note.Vault, msg.note.Path)
	}
	return nil
}

func (m *model) onSaved(msg savedMsg) tea.Cmd {
	if msg.err != nil {
		// A failed save cancels whatever was queued behind it.
		m.quitting = false
		m.pendingCmd = nil
		if conflict := client.ConflictPath(msg.err); conflict != "" {
			// The server has already written our version beside the original, so
			// nothing is lost whichever way this goes.
			m.stale = true
			m.pr = newKeyPrompt(prConflict,
				"changed on the server. yours is saved at "+conflict+".",
				"or", "[o] overwrite theirs · [r] load theirs · esc keep editing")
			return nil
		}
		return m.fail("save", msg.err)
	}

	m.baseSHA = msg.sum.SHA256
	m.note.SHA256 = msg.sum.SHA256
	m.note.Path = msg.sum.Path
	m.note.Title = msg.sum.Title
	m.note.Tags = msg.sum.Tags
	m.stale = false

	cmds := []tea.Cmd{m.setStatus("saved "+msg.sum.Path, false), m.refreshList()}
	switch {
	case msg.reload:
		cmds = append(cmds, loadNote(m.ctx, m.cl, msg.sum.Vault, msg.sum.Path, false))
	default:
		m.note.Content = msg.content
		// Typing carries on while a save is in flight, so the buffer is compared
		// against what was actually written rather than assumed to match it.
		m.dirty = m.editing && m.ta.Value() != msg.content
		m.renderBody()
	}
	if m.quitting && !m.dirty {
		return tea.Sequence(tea.Batch(cmds...), tea.Quit)
	}
	m.quitting = false

	if next := m.pendingCmd; next != nil {
		m.pendingCmd = nil
		cmds = append(cmds, next)
	}
	return tea.Batch(cmds...)
}

func (m *model) onSearch(msg searchMsg) tea.Cmd {
	if m.pick == nil || m.pick.kind != pickSearch || msg.seq != m.pick.seq {
		return nil // a stale response for a query that has moved on
	}
	if msg.err != nil {
		m.pick.setItems(nil)
		m.pick.empty = "search failed: " + msg.err.Error()
		return nil
	}
	items := make([]pickItem, 0, len(msg.res.Hits))
	for _, h := range msg.res.Hits {
		label := h.Title
		if h.OwnerLogin != "" && h.Vault != m.me.Vault {
			label += m.st.itemShared.Render("  " + h.OwnerLogin)
		}
		items = append(items, pickItem{
			label: label, segs: client.ParseSnippet(h.Snippet),
			vault: h.Vault, path: h.Path,
		})
	}
	m.pick.setItems(items)
	m.pick.empty = "nothing matched"
	if msg.res.HasMore {
		m.pick.hint = "enter open · esc close · more results than shown"
	}
	return nil
}

func (m *model) onShares(msg sharesMsg) tea.Cmd {
	if msg.err != nil {
		return m.fail("shares", msg.err)
	}
	var items []pickItem
	for _, s := range msg.mine {
		kind := "note"
		if s.IsFolder {
			kind = "folder"
		}
		items = append(items, pickItem{
			label: fmt.Sprintf("→ %s", s.Path),
			detail: m.st.muted.Render(fmt.Sprintf("%s to %s · %s · granted %s",
				kind, s.GranteeLogin, s.Perm, s.CreatedAt.Format("2006-01-02"))),
			id: s.ID, path: s.Path, perm: s.Perm, mine: true, vault: s.Vault,
		})
	}
	for _, s := range msg.toMe {
		items = append(items, pickItem{
			label:  fmt.Sprintf("← %s", s.Path),
			detail: m.st.muted.Render(fmt.Sprintf("from %s · %s", s.OwnerLogin, s.Perm)),
			id:     s.ID, path: s.Path, perm: s.Perm, vault: s.Vault,
		})
	}
	cmd := m.showPicker(pickShare, "Shares", items)
	m.pick.empty = "nothing is shared, in either direction"
	if msg.notify != "" {
		return tea.Batch(cmd, m.setStatus(msg.notify, false))
	}
	return cmd
}

// onEvent handles one server-sent event: another client, an agent over MCP, or
// Obsidian on the desktop, changing a note.
func (m *model) onEvent(msg eventMsg) tea.Cmd {
	next := waitForEvent(m.events)

	if msg.ev.Change == nil {
		was := m.connected
		m.connected = msg.ev.Connected
		switch {
		case was && !m.connected:
			return tea.Batch(next, m.setStatus("lost the event stream, reconnecting", true))
		case !was && m.connected && m.ready && m.hasNote:
			// Reconnecting means we may have missed something while away, so
			// take the list and the open note again rather than trusting them.
			cmds := []tea.Cmd{next, m.refreshList()}
			if !m.dirty {
				cmds = append(cmds, loadNote(m.ctx, m.cl, m.note.Vault, m.note.Path, false))
			}
			return tea.Batch(cmds...)
		}
		return next
	}

	e := msg.ev.Change
	cmds := []tea.Cmd{next}

	if e.Vault == m.query.vault {
		cmds = append(cmds, m.refreshList())
	}

	if m.hasNote && e.Vault == m.note.Vault && (e.Path == m.note.Path || e.OldPath == m.note.Path) {
		switch {
		case e.Kind == events.NoteDeleted:
			m.closeNote()
			cmds = append(cmds, m.setStatus("this note was deleted", true))
		case e.SHA256 != "" && e.SHA256 == m.note.SHA256:
			// The echo of our own save. The hash is the only reliable way to
			// tell: the login is our own whenever the other writer is us in
			// another window, or Obsidian on this same machine.
		case m.dirty:
			// Reloading would throw away what is being typed. Say so instead,
			// and let the save conflict handle it if it comes to that.
			m.stale = true
			cmds = append(cmds, m.setStatus("changed on the server by "+byWhom(e.ByLogin)+"; your edits are still here", true))
		default:
			path := e.Path
			cmds = append(cmds,
				loadNote(m.ctx, m.cl, e.Vault, path, false),
				m.setStatus("reloaded, changed by "+byWhom(e.ByLogin), false))
		}
	}
	return tea.Batch(cmds...)
}

func byWhom(login string) string {
	if login == "" {
		// No login means it did not come through folio: Obsidian, an editor, a
		// script writing the file directly.
		return "another program"
	}
	return login
}

func (m *model) onEditorDone(msg editorDoneMsg) tea.Cmd {
	if msg.err != nil {
		return m.fail("editor", msg.err)
	}
	if !m.hasNote {
		return nil
	}
	if msg.content == m.note.Content {
		return m.setStatus("no changes", false)
	}
	m.note.Content = msg.content
	m.ta.SetValue(msg.content)
	m.dirty = true
	m.renderBody()
	// Saving straight away is what someone who just wrote and quit their editor
	// expects; the compare-and-swap still protects the file.
	return m.save(false)
}

// ---------------------------------------------------------------- state

func (m *model) setNote(n client.Note) {
	m.note = n
	m.hasNote = true
	m.baseSHA = n.SHA256
	m.dirty = false
	m.stale = false
	if m.editing {
		m.ta.SetValue(n.Content)
	}
	m.vp.GotoTop()
	m.renderBody()
}

func (m *model) closeNote() {
	m.note = client.Note{}
	m.hasNote = false
	m.editing = false
	m.dirty = false
	m.stale = false
	m.body = nil
	m.vp.SetContent("")
	m.focus = paneList
}

func (m *model) setItems(items []listItem) {
	// Keep the selection on the same note across a refresh where possible: an
	// event-driven reload should not move the cursor out from under someone.
	var selPath, selVault string
	if it, ok := m.current(); ok {
		selPath, selVault = it.path, it.vault
	}
	m.items = items
	m.sel = 0
	for i, it := range items {
		if it.path == selPath && it.vault == selVault {
			m.sel = i
			break
		}
	}
	m.clampList()
}

func (m *model) current() (listItem, bool) {
	if m.sel < 0 || m.sel >= len(m.items) {
		return listItem{}, false
	}
	return m.items[m.sel], true
}

// selectPath moves the sidebar cursor to a note, if it is in the list.
func (m *model) selectPath(vault, path string) {
	for i, it := range m.items {
		if it.vault == vault && it.path == path {
			m.sel = i
			m.clampList()
			return
		}
	}
}

func (m *model) clampList() {
	if len(m.items) == 0 {
		m.sel, m.top = 0, 0
		return
	}
	m.sel = clamp(m.sel, 0, len(m.items)-1)
	h := m.listHeight()
	if m.sel < m.top {
		m.top = m.sel
	}
	if m.sel >= m.top+h {
		m.top = m.sel - h + 1
	}
	m.top = clamp(m.top, 0, max(0, len(m.items)-1))
}

// listHeight is how many note rows fit, below the sidebar's own header.
func (m *model) listHeight() int { return max(1, m.height-2-1) }

func (m *model) refreshList() tea.Cmd {
	if m.searchQuery != "" {
		// A search listing is a snapshot of a query, not a live folder. Leave it
		// alone rather than silently replacing it with something else.
		return nil
	}
	return loadNotes(m.ctx, m.cl, m.query, m.listLabel())
}

func (m *model) listLabel() string {
	switch {
	case m.query.tag != "":
		return "#" + m.query.tag
	case m.query.folder != "":
		return m.query.folder + "/"
	default:
		return "All notes"
	}
}

func summariesToItems(list []client.Summary, ownVault string) []listItem {
	items := make([]listItem, 0, len(list))
	for _, s := range list {
		title := s.Title
		if title == "" {
			title = vaultpath.TitleFor(s.Path)
		}
		items = append(items, listItem{
			vault: s.Vault, path: s.Path, title: title,
			detail: relTime(s.UpdatedAt), shared: s.Vault != ownVault,
		})
	}
	return items
}

// setStatus shows a transient message and schedules its expiry.
func (m *model) setStatus(text string, isErr bool) tea.Cmd {
	m.status, m.statusErr = text, isErr
	m.statusSeq++
	return expireStatus(m.statusSeq)
}

// fail reports a failed operation, translating the errors a user can act on.
func (m *model) fail(what string, err error) tea.Cmd {
	switch {
	case errors.Is(err, client.ErrForbidden):
		return m.setStatus(what+": not allowed", true)
	case errors.Is(err, client.ErrUnavailable):
		return m.setStatus(what+": the server cannot reach tailscaled right now", true)
	case errors.Is(err, client.ErrNotFound):
		return m.setStatus(what+": not found", true)
	}
	var apiErr *client.Error
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		return m.setStatus(what+": "+apiErr.Message, true)
	}
	return m.setStatus(what+": "+err.Error(), true)
}

// showPicker opens an overlay list.
func (m *model) showPicker(kind pickerKind, title string, items []pickItem) tea.Cmd {
	p := newPicker(kind, title)
	p.setItems(items)
	m.pick = p
	return nil
}

func (m *model) closePicker() { m.pick = nil }
