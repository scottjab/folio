package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scottjab/folio/internal/client"
	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/httpapi"
	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/index"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/store"
	"github.com/scottjab/folio/internal/vault"
)

// These tests drive the model against a real folio server: real SQLite, real
// vault directory, real HTTP handlers, real event stream. The alternative, a
// mocked client, would pass happily while the thing it stands for had changed
// underneath, and the bugs this UI can actually have are all at that seam.

// driver runs a model the way bubbletea would, minus the terminal.
type driver struct {
	t    *testing.T
	m    *model
	msgs chan tea.Msg
	cl   *client.Client

	mu   sync.Mutex
	quit bool
}

func newDriver(t *testing.T) *driver {
	t.Helper()
	cl := startServer(t)

	d := &driver{
		t:    t,
		cl:   cl,
		msgs: make(chan tea.Msg, 256),
		m: newModel(t.Context(), Options{
			Client: cl,
			Editor: "true", // never actually launched in these tests
		}),
	}
	d.exec(d.m.Init())
	d.send(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	d.settle()
	return d
}

// exec runs a command the way the runtime does: on its own goroutine, with
// batches unpacked. A blocked command (the event stream waiting for something to
// happen) simply never reports, which is exactly what it does in the real
// program.
func (d *driver) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		switch v := msg.(type) {
		case nil:
			return
		case tea.BatchMsg:
			for _, c := range v {
				d.exec(c)
			}
			return
		}
		select {
		case d.msgs <- msg:
		case <-d.m.ctx.Done():
		}
	}()
}

// send delivers one message and runs whatever it produces.
func (d *driver) send(msg tea.Msg) {
	if _, ok := msg.(tea.QuitMsg); ok {
		d.mu.Lock()
		d.quit = true
		d.mu.Unlock()
		return
	}
	_, cmd := d.m.Update(msg)
	d.exec(cmd)
}

// key delivers a keypress by name, the way bubbletea reports one.
func (d *driver) key(name string) {
	d.send(keyMsg(name))
	d.settle()
}

// typeText sends each rune as its own key event.
func (d *driver) typeText(s string) {
	for _, r := range s {
		d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	d.settle()
}

// settle processes messages until nothing has arrived for a while.
func (d *driver) settle() {
	d.t.Helper()
	const idle = 250 * time.Millisecond
	deadline := time.After(15 * time.Second)
	for {
		select {
		case msg := <-d.msgs:
			d.send(msg)
		case <-time.After(idle):
			return
		case <-deadline:
			d.t.Fatal("the UI never settled")
		}
	}
}

// view renders the screen with styling stripped, for assertions about what is
// actually on it.
func (d *driver) view() string { return ansiStrip(d.m.View()) }

func keyMsg(name string) tea.KeyMsg {
	switch name {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "ctrl+x":
		return tea.KeyMsg{Type: tea.KeyCtrlX}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
}

// startServer wires up a complete folio and returns a client for it.
func startServer(t *testing.T) *client.Client {
	t.Helper()
	dir := t.TempDir()

	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	vaults := vault.NewSet(filepath.Join(dir, "vaults"))
	t.Cleanup(func() { vaults.Close() })

	whois := func(context.Context, string) (identity.WhoIs, error) {
		return identity.WhoIs{UserID: 1, Login: "alice@github", DisplayName: "Alice"}, nil
	}

	api := httpapi.New(httpapi.Deps{
		DB:       db,
		Index:    index.New(db),
		Vaults:   vaults,
		Identity: identity.NewResolver(db, whois, identity.Options{}),
		Shares:   share.NewResolver(db),
		Bus:      events.NewBus(),
		PeerAddr: func(*http.Request) string { return "100.64.0.1:40000" },
	})
	t.Cleanup(api.Close)

	srv := httptest.NewTestServer(t, api)
	srv.Start()
	t.Cleanup(srv.Close)

	cl, err := client.New(srv.URL, client.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return cl
}

// seed writes a note straight through the API, standing in for the browser, an
// agent, or Obsidian having put it there.
func seed(t *testing.T, cl *client.Client, path, content string) {
	t.Helper()
	if _, err := cl.CreateNote(t.Context(), "alice-github", path, content); err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}
}

func TestStartsOnTheDailyNote(t *testing.T) {
	d := newDriver(t)

	if d.m.me.Login != "alice@github" {
		t.Errorf("login = %q, want alice@github", d.m.me.Login)
	}
	if d.m.vault != "alice-github" {
		t.Errorf("vault = %q, want alice-github", d.m.vault)
	}
	if !d.m.hasNote {
		t.Fatal("no note open; the daily note should have been created")
	}
	if !strings.HasPrefix(d.m.note.Path, "Daily/") {
		t.Errorf("opened %q, want a note under Daily/", d.m.note.Path)
	}
	if len(d.m.items) == 0 {
		t.Error("the sidebar is empty; the daily note should be listed")
	}
	if !d.m.connected {
		t.Error("the event stream never connected")
	}

	view := d.view()
	if !strings.Contains(view, "alice@github") || !strings.Contains(view, "live") {
		t.Errorf("title bar is missing something:\n%s", firstLines(view, 2))
	}
}

func TestOpenNoteFromTheList(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Projects/folio.md", "# folio\n\nShipped the indexer.\n")

	d := driverWith(t, cl)
	d.m.focus = paneList

	// Find the seeded note in the sidebar and open it.
	if !d.selectNote("Projects/folio.md") {
		t.Fatalf("Projects/folio.md is not in the list: %v", d.paths())
	}
	d.key("enter")

	if d.m.note.Path != "Projects/folio.md" {
		t.Fatalf("open note = %q", d.m.note.Path)
	}
	if d.m.focus != paneNote {
		t.Error("opening a note should move the keyboard to it")
	}
	if !strings.Contains(d.view(), "Shipped the indexer.") {
		t.Errorf("the note body is not on screen:\n%s", d.view())
	}
}

func TestEditAndSaveWritesThrough(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/edit.md", "original\n")

	d := driverWith(t, cl)
	d.openPath("Notes/edit.md")

	d.key("i")
	if !d.m.editing {
		t.Fatal("i should start editing")
	}
	d.typeText("hello ")
	if !d.m.dirty {
		t.Error("typing should mark the buffer unsaved")
	}
	if !strings.Contains(d.view(), "unsaved") {
		t.Error("the header should say the note is unsaved")
	}

	d.key("ctrl+s")
	if d.m.dirty {
		t.Error("still unsaved after ctrl+s")
	}

	// The note on the server is what matters, not what the model believes.
	got, err := cl.Note(t.Context(), "alice-github", "Notes/edit.md")
	if err != nil {
		t.Fatalf("re-reading the note: %v", err)
	}
	// Editing starts with the cursor at the end of the note, which is where
	// someone adding to a daily note wants it.
	if !strings.HasSuffix(got.Content, "original\nhello ") {
		t.Errorf("saved content = %q, want the typing at the cursor", got.Content)
	}
	if d.m.baseSHA != got.SHA256 {
		t.Errorf("the model kept a stale hash: %q, server says %q", d.m.baseSHA, got.SHA256)
	}
}

// A save whose base hash is out of date must not silently win. The server
// stashes the rejected text and the UI has to say where it went.
func TestSaveConflictOffersAWayOut(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/race.md", "first\n")

	d := driverWith(t, cl)
	d.openPath("Notes/race.md")

	d.key("i")
	d.typeText("mine ")

	// Someone else writes the note while we are typing.
	if _, err := cl.UpdateNote(t.Context(), "alice-github", "Notes/race.md", "theirs\n", ""); err != nil {
		t.Fatalf("competing write: %v", err)
	}
	d.settle()

	d.key("ctrl+s")
	if d.m.pr.kind != prConflict {
		t.Fatalf("prompt = %v, want the conflict prompt", d.m.pr.kind)
	}
	if !strings.Contains(d.m.pr.label, "conflict") {
		t.Errorf("the prompt should name the conflict file: %q", d.m.pr.label)
	}

	// Overwriting is a deliberate second choice, and must then succeed.
	d.key("o")
	if d.m.pr.active() {
		t.Errorf("the prompt is still up: %+v", d.m.pr)
	}
	got, err := cl.Note(t.Context(), "alice-github", "Notes/race.md")
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if !strings.Contains(got.Content, "mine ") || strings.Contains(got.Content, "theirs") {
		t.Errorf("content = %q, want ours after an overwrite", got.Content)
	}
}

func TestSearchOpensAResult(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Projects/indexer.md", "# The indexer\n\nBM25 ranking over FTS5.\n")
	seed(t, cl, "Projects/other.md", "nothing to see\n")

	d := driverWith(t, cl)
	d.key("/")
	if d.m.pick == nil || d.m.pick.kind != pickSearch {
		t.Fatal("/ should open the search palette")
	}
	d.typeText("BM25")
	d.settle() // the debounce, then the query

	if len(d.m.pick.view) == 0 {
		t.Fatalf("no results for BM25; empty message is %q", d.m.pick.empty)
	}
	if !strings.Contains(d.view(), "BM25") {
		t.Errorf("the snippet should show the match:\n%s", d.view())
	}

	d.key("enter")
	if d.m.note.Path != "Projects/indexer.md" {
		t.Errorf("opened %q, want Projects/indexer.md", d.m.note.Path)
	}
	// The results stay as the listing, so the next hit is one key away.
	if d.m.searchQuery != "BM25" {
		t.Errorf("searchQuery = %q, want the results to become the listing", d.m.searchQuery)
	}
	if !strings.Contains(d.m.label, "BM25") {
		t.Errorf("sidebar label = %q", d.m.label)
	}

	// Escape backs out of the search listing to everything again.
	d.key("esc")
	d.key("esc")
	if d.m.searchQuery != "" {
		t.Error("esc should clear the search listing")
	}
}

func TestNewRenameAndDelete(t *testing.T) {
	d := newDriver(t)

	d.key("n")
	if d.m.pr.kind != prNew {
		t.Fatalf("n should ask for a path, got %v", d.m.pr.kind)
	}
	// The prompt is prefilled with the folder being browsed, so clear it first.
	d.clearPrompt()
	d.typeText("Inbox/thought")
	d.key("enter")

	if d.m.note.Path != "Inbox/thought.md" {
		t.Fatalf("created note = %q, want Inbox/thought.md (with the extension added)", d.m.note.Path)
	}

	d.key("m")
	if d.m.pr.kind != prRename {
		t.Fatalf("m should ask for a new path, got %v", d.m.pr.kind)
	}
	d.clearPrompt()
	d.typeText("Inbox/second thought.md")
	d.key("enter")

	if d.m.note.Path != "Inbox/second thought.md" {
		t.Fatalf("after rename the open note is %q", d.m.note.Path)
	}
	if _, err := d.cl.Note(t.Context(), "alice-github", "Inbox/second thought.md"); err != nil {
		t.Errorf("the renamed note is not on the server: %v", err)
	}

	d.key("x")
	if d.m.pr.kind != prDelete {
		t.Fatalf("x should ask to confirm, got %v", d.m.pr.kind)
	}
	d.key("n") // changed my mind
	if _, err := d.cl.Note(t.Context(), "alice-github", "Inbox/second thought.md"); err != nil {
		t.Fatalf("answering no deleted the note anyway: %v", err)
	}

	d.key("x")
	d.key("y")
	if d.m.hasNote {
		t.Error("the deleted note should have been closed")
	}
	if _, err := d.cl.Note(t.Context(), "alice-github", "Inbox/second thought.md"); err == nil {
		t.Error("the note is still on the server after a confirmed delete")
	}
}

func TestAppendAddsALine(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/log.md", "# Log\n")

	d := driverWith(t, cl)
	d.openPath("Notes/log.md")

	d.key("a")
	if d.m.pr.kind != prAppend {
		t.Fatalf("a should ask what to append, got %v", d.m.pr.kind)
	}
	d.typeText("another entry")
	d.key("enter")

	got, err := cl.Note(t.Context(), "alice-github", "Notes/log.md")
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if !strings.Contains(got.Content, "another entry") {
		t.Errorf("content = %q, want the appended line", got.Content)
	}
	// The model must have re-read the note rather than assuming what happened.
	if !strings.Contains(d.m.note.Content, "another entry") {
		t.Errorf("the model still shows %q", d.m.note.Content)
	}
}

// An edit made anywhere else, by the browser, an agent, or Obsidian, has to
// arrive over the event stream without a keypress.
func TestLiveUpdateReloadsTheOpenNote(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/live.md", "before\n")

	d := driverWith(t, cl)
	d.openPath("Notes/live.md")

	if _, err := cl.UpdateNote(t.Context(), "alice-github", "Notes/live.md", "after\n", ""); err != nil {
		t.Fatalf("outside write: %v", err)
	}
	d.settle()

	if !strings.Contains(d.m.note.Content, "after") {
		t.Errorf("the open note was not reloaded: %q", d.m.note.Content)
	}
	if !strings.Contains(d.view(), "after") {
		t.Errorf("the screen still shows the old text:\n%s", d.view())
	}
}

// The same event, but with unsaved edits in the buffer: nothing may be thrown
// away behind the user's back.
func TestLiveUpdateKeepsUnsavedEdits(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/keep.md", "before\n")

	d := driverWith(t, cl)
	d.openPath("Notes/keep.md")
	d.key("i")
	d.typeText("mine ")

	if _, err := cl.UpdateNote(t.Context(), "alice-github", "Notes/keep.md", "theirs\n", ""); err != nil {
		t.Fatalf("outside write: %v", err)
	}
	d.settle()

	if !strings.Contains(d.m.ta.Value(), "mine ") {
		t.Fatalf("the buffer lost the edit: %q", d.m.ta.Value())
	}
	if !d.m.stale {
		t.Error("the note should be flagged as changed on the server")
	}
	if !d.m.dirty {
		t.Error("the buffer should still be unsaved")
	}
}

func TestTagFilterNarrowsTheList(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/tagged.md", "---\ntags: [go]\n---\n\n# Tagged\n")
	seed(t, cl, "Notes/plain.md", "# Plain\n")

	d := driverWith(t, cl)
	d.key("t")
	if d.m.pick == nil || d.m.pick.kind != pickTag {
		t.Fatal("t should open the tag list")
	}
	if len(d.m.pick.view) == 0 {
		t.Fatal("no tags listed")
	}
	// The daily note carries a tag of its own, so narrow to the one we want.
	d.typeText("go")
	d.key("enter")

	if d.m.query.tag != "go" {
		t.Fatalf("tag filter = %q, want go", d.m.query.tag)
	}
	if got := d.paths(); len(got) != 1 || got[0] != "Notes/tagged.md" {
		t.Errorf("filtered list = %v, want just the tagged note", got)
	}
	if !strings.Contains(d.view(), "#go") {
		t.Errorf("the sidebar should say what it is filtered to:\n%s", firstLines(d.view(), 3))
	}

	d.key("esc")
	if d.m.query.tag != "" {
		t.Error("esc should clear the tag filter")
	}
}

func TestBacklinksAndOutgoingLinks(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Projects/folio.md", "# folio\n")
	seed(t, cl, "Daily/today.md", "Worked on [[Projects/folio]] again.\n")

	d := driverWith(t, cl)
	d.openPath("Projects/folio.md")

	d.key("B")
	if d.m.pick == nil || d.m.pick.kind != pickBacklink {
		t.Fatal("B should list backlinks")
	}
	if len(d.m.pick.view) == 0 {
		t.Fatalf("no backlinks found; %q should link here", "Daily/today.md")
	}
	d.key("enter")
	if d.m.note.Path != "Daily/today.md" {
		t.Fatalf("opened %q, want the linking note", d.m.note.Path)
	}

	d.key("L")
	if d.m.pick == nil || d.m.pick.kind != pickLink {
		t.Fatal("L should list outgoing links")
	}
	d.key("enter")
	if d.m.note.Path != "Projects/folio.md" {
		t.Errorf("following the link opened %q", d.m.note.Path)
	}
}

func TestReadOnlyNoteRefusesEditing(t *testing.T) {
	d := newDriver(t)
	d.m.note.Perm = client.PermRead

	d.key("i")
	if d.m.editing {
		t.Error("a read-only note must not open for editing")
	}
	if !strings.Contains(d.m.status, "read-only") {
		t.Errorf("status = %q, want it to explain why", d.m.status)
	}
}

func TestQuitAsksAboutUnsavedChanges(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/quit.md", "x\n")

	d := driverWith(t, cl)
	d.openPath("Notes/quit.md")
	d.key("i")
	d.typeText("more ")
	d.key("esc")

	d.key("q")
	if d.m.pr.kind != prQuit {
		t.Fatalf("q with unsaved changes should ask first, got %v", d.m.pr.kind)
	}
	if d.didQuit() {
		t.Fatal("it quit anyway")
	}

	d.key("s") // save and quit
	got, err := cl.Note(t.Context(), "alice-github", "Notes/quit.md")
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if !strings.HasSuffix(got.Content, "more ") {
		t.Errorf("content = %q, want the buffer to have been saved", got.Content)
	}
}

func TestHelpListsEveryBinding(t *testing.T) {
	d := newDriver(t)
	d.key("?")
	if d.m.pick == nil || d.m.pick.kind != pickHelp {
		t.Fatal("? should open the help")
	}
	// Every binding has to appear, since the help is generated from the same
	// table that dispatches the keys.
	rows := make(map[string]bool, len(d.m.pick.items))
	for _, it := range d.m.pick.items {
		rows[strings.TrimSpace(ansiStrip(it.label))] = true
	}
	for _, b := range bindings() {
		if !rows[b.label] {
			t.Errorf("the help does not mention %q (%s)", b.label, b.desc)
		}
	}
	d.key("esc")
	if d.m.pick != nil {
		t.Error("esc should close the help")
	}
}

func TestNarrowTerminalShowsOnePane(t *testing.T) {
	d := newDriver(t)
	d.send(tea.WindowSizeMsg{Width: 50, Height: 20})
	d.settle()

	if d.m.sidebarWidth() != 0 {
		t.Error("at 50 columns the sidebar should collapse")
	}
	for _, line := range strings.Split(d.view(), "\n") {
		if lipglossWidth(line) > 50 {
			t.Fatalf("a line is %d columns wide in a 50 column terminal: %q", lipglossWidth(line), line)
		}
	}
}

// ---------------------------------------------------------------- helpers

func driverWith(t *testing.T, cl *client.Client) *driver {
	t.Helper()
	d := &driver{
		t:    t,
		cl:   cl,
		msgs: make(chan tea.Msg, 256),
		m:    newModel(t.Context(), Options{Client: cl, Editor: "true"}),
	}
	d.exec(d.m.Init())
	d.send(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	d.settle()
	return d
}

func (d *driver) didQuit() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.quit
}

// paths lists what the sidebar is showing.
func (d *driver) paths() []string {
	out := make([]string, 0, len(d.m.items))
	for _, it := range d.m.items {
		out = append(out, it.path)
	}
	return out
}

// selectNote moves the sidebar cursor onto a path.
func (d *driver) selectNote(path string) bool {
	for i, it := range d.m.items {
		if it.path == path {
			d.m.sel = i
			return true
		}
	}
	return false
}

// openPath opens a note the way a user would: select it, press enter.
func (d *driver) openPath(path string) {
	d.t.Helper()
	d.m.focus = paneList
	if !d.selectNote(path) {
		d.t.Fatalf("%s is not in the list: %v", path, d.paths())
	}
	d.key("enter")
	if d.m.note.Path != path {
		d.t.Fatalf("opened %q, want %q", d.m.note.Path, path)
	}
}

// clearPrompt empties a prefilled text prompt.
func (d *driver) clearPrompt() {
	for range len(d.m.pr.input.Value()) {
		d.send(keyMsg("backspace"))
	}
	d.settle()
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// Nothing may be wider than the terminal. A single line that overruns wraps and
// pushes the whole layout down a row, and it keeps doing it on every repaint.
func TestEveryLineFitsTheTerminal(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Projects/a note with quite a long title indeed.md",
		"---\ntags: [go, notes]\n---\n\n# Heading\n\n| Feature | Used for |\n|---|---:|\n| generics | `All[T]` |\n\n- [ ] a task whose text runs on and on and on\n")

	d := driverWith(t, cl)

	sizes := []struct{ w, h int }{{40, 12}, {50, 20}, {62, 24}, {80, 24}, {100, 30}, {130, 45}}
	screens := map[string]func(){
		"note":      func() {},
		"help":      func() { d.key("?") },
		"search":    func() { d.key("/") },
		"tags":      func() { d.key("t") },
		"shares":    func() { d.key("s") },
		"backlinks": func() { d.key("B") },
		"editing":   func() { d.key("i") },
		"prompt":    func() { d.key("n") },
	}

	for name, open := range screens {
		d.key("esc")
		d.key("esc")
		open()
		for _, size := range sizes {
			// A resize produces no commands, so there is nothing to wait for.
			d.send(tea.WindowSizeMsg{Width: size.w, Height: size.h})

			lines := strings.Split(d.m.View(), "\n")
			for i, line := range lines {
				if w := lipglossWidth(line); w > size.w {
					t.Errorf("%s at %dx%d: line %d is %d columns wide:\n%q",
						name, size.w, size.h, i, w, ansiStrip(line))
				}
			}
			if len(lines) > size.h {
				t.Errorf("%s at %dx%d: %d lines, more than the terminal has", name, size.w, size.h, len(lines))
			}
			// The title bar is the one line that must fill the width exactly, or
			// the right-hand status sits in the wrong place.
			if got := lipglossWidth(lines[0]); got != size.w {
				t.Errorf("%s at %dx%d: title bar is %d columns, want %d: %q",
					name, size.w, size.h, got, size.w, ansiStrip(lines[0]))
			}
		}
	}
}

// Opening another note with a draft in hand must neither throw the draft away
// nor leave you stuck with no way out of it.
func TestSwitchingNotesWithUnsavedEdits(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/one.md", "one\n")
	seed(t, cl, "Notes/two.md", "two\n")

	d := driverWith(t, cl)
	d.openPath("Notes/one.md")
	d.key("i")
	d.typeText("draft ")
	d.key("esc")

	d.m.focus = paneList
	if !d.selectNote("Notes/two.md") {
		t.Fatal("Notes/two.md is not listed")
	}
	d.key("enter")

	if d.m.pr.kind != prSwitch {
		t.Fatalf("prompt = %v, want to be asked about the unsaved note", d.m.pr.kind)
	}
	if d.m.note.Path != "Notes/one.md" {
		t.Error("it switched anyway")
	}

	// Saving first must land the draft and then open the other note.
	d.key("s")
	if d.m.note.Path != "Notes/two.md" {
		t.Fatalf("open note = %q, want Notes/two.md after save-and-open", d.m.note.Path)
	}
	got, err := cl.Note(t.Context(), "alice-github", "Notes/one.md")
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if !strings.Contains(got.Content, "draft ") {
		t.Errorf("the draft was not saved: %q", got.Content)
	}

	// And discarding must work too, leaving the file as it was.
	d.key("i")
	d.typeText("thrown away ")
	d.key("esc")
	d.m.focus = paneList
	if !d.selectNote("Notes/one.md") {
		t.Fatal("Notes/one.md is not listed")
	}
	d.key("enter")
	d.key("d")

	if d.m.note.Path != "Notes/one.md" || d.m.dirty {
		t.Errorf("after discarding: note = %q, dirty = %v", d.m.note.Path, d.m.dirty)
	}
	after, err := cl.Note(t.Context(), "alice-github", "Notes/two.md")
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if strings.Contains(after.Content, "thrown away") {
		t.Errorf("the discarded text was written anyway: %q", after.Content)
	}
}
