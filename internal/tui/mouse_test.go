package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The mouse tests use literal coordinates rather than asking the model where
// things are. A hit test that agrees with the layout it was derived from proves
// nothing; these click at columns and rows counted off the drawn screen.
//
// At 120x40: the title bar is row 0, the sidebar is columns 0-39, the divider is
// column 40, the note starts at column 41, and each pane's first row is 2
// because row 1 is its heading.
const (
	testWidth   = 120
	testHeight  = 40
	sidebarCols = 40
	noteX       = 41
	firstRow    = 2
)

func mouseClick(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
}

func mouseWheel(x, y int, up bool) tea.MouseMsg {
	button := tea.MouseButtonWheelDown
	if up {
		button = tea.MouseButtonWheelUp
	}
	return tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: button}
}

func (d *driver) click(x, y int) {
	d.send(mouseClick(x, y))
	d.settle()
}

// The geometry the coordinates above assume has to actually be the geometry, or
// every test in this file is clicking somewhere it did not mean to.
func TestLayoutIsWhereTheTestsThinkItIs(t *testing.T) {
	d := newDriver(t)
	l := d.m.layout()

	if l.bodyTop != 1 {
		t.Errorf("body starts at row %d, want 1", l.bodyTop)
	}
	if l.sidebarW != sidebarCols {
		t.Errorf("sidebar is %d columns, want %d", l.sidebarW, sidebarCols)
	}
	if l.noteX != noteX {
		t.Errorf("note pane starts at column %d, want %d", l.noteX, noteX)
	}
	if l.statusRow != testHeight-1 {
		t.Errorf("status line is row %d, want %d", l.statusRow, testHeight-1)
	}
}

func TestClickOpensANote(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Projects/clicked.md", "# Clicked\n\nThe body of the note.\n")

	d := driverWith(t, cl)
	row := d.rowOf("Projects/clicked.md")
	if row < 0 {
		t.Fatalf("Projects/clicked.md is not listed: %v", d.paths())
	}

	d.click(2, firstRow+row)

	if d.m.note.Path != "Projects/clicked.md" {
		t.Fatalf("open note = %q, want the one that was clicked", d.m.note.Path)
	}
	if d.m.focus != paneNote {
		t.Error("clicking a note should move the keyboard to it, as enter does")
	}
	if !strings.Contains(d.view(), "The body of the note.") {
		t.Error("the clicked note is not on screen")
	}
}

// Clicking past the end of the list must not open whatever was last, and must
// not panic on an index that is not there.
func TestClickBelowTheListDoesNothing(t *testing.T) {
	d := newDriver(t)
	before := d.m.note.Path

	d.click(2, testHeight-3)

	if d.m.note.Path != before {
		t.Errorf("open note changed to %q", d.m.note.Path)
	}
	if d.m.focus != paneList {
		t.Error("the click should still have moved the keyboard to the list")
	}
}

func TestWheelMovesTheListAndScrollsTheNote(t *testing.T) {
	cl := startServer(t)
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		seed(t, cl, "Notes/"+n+".md", "# "+n+"\n")
	}
	// A note long enough to scroll.
	seed(t, cl, "Notes/long.md", strings.Repeat("a paragraph of text\n\n", 60))

	d := driverWith(t, cl)
	d.m.focus = paneList
	d.m.sel = 0

	d.send(mouseWheel(2, 10, false))
	if d.m.sel != wheelLines {
		t.Errorf("selection = %d after one notch down, want %d", d.m.sel, wheelLines)
	}
	d.send(mouseWheel(2, 10, true))
	if d.m.sel != 0 {
		t.Errorf("selection = %d after scrolling back up, want 0", d.m.sel)
	}
	// The wheel over the list must not steal the keyboard.
	if d.m.focus != paneList {
		t.Error("the wheel changed the focus")
	}

	d.openPath("Notes/long.md")
	if d.m.vp.YOffset != 0 {
		t.Fatalf("a freshly opened note starts at offset %d", d.m.vp.YOffset)
	}
	d.send(mouseWheel(noteX+5, 10, false))
	if d.m.vp.YOffset == 0 {
		t.Error("the wheel over the note did not scroll it")
	}
	at := d.m.vp.YOffset
	d.send(mouseWheel(noteX+5, 10, true))
	if d.m.vp.YOffset >= at {
		t.Errorf("scrolling back up left the offset at %d, was %d", d.m.vp.YOffset, at)
	}
}

func TestClickFollowsAWikilink(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Projects/target.md", "# The target\n")
	seed(t, cl, "Notes/source.md", "Worked on [[Projects/target]] today.\n")

	d := driverWith(t, cl)
	d.openPath("Notes/source.md")

	if !d.clickText(t, "[[Projects/target]]") {
		t.Fatal("the link is not on screen")
	}
	if d.m.note.Path != "Projects/target.md" {
		t.Errorf("open note = %q, want the link's target", d.m.note.Path)
	}
}

// A link with an alias shows the alias, so following it has to go back to the
// source for the path rather than trusting what is drawn.
func TestClickFollowsAnAliasedWikilink(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Projects/target.md", "# The target\n")
	seed(t, cl, "Notes/aliased.md", "See [[Projects/target|the indexer]] for more.\n")

	d := driverWith(t, cl)
	d.openPath("Notes/aliased.md")

	if !d.clickText(t, "[[the indexer]]") {
		t.Fatal("the aliased link is not on screen")
	}
	if d.m.note.Path != "Projects/target.md" {
		t.Errorf("open note = %q, want Projects/target.md", d.m.note.Path)
	}
}

func TestClickingPlainTextOpensNothing(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/plain.md", "Just some words, and a [[Link]] over here.\n")

	d := driverWith(t, cl)
	d.openPath("Notes/plain.md")

	if !d.clickText(t, "Just") {
		t.Fatal("the text is not on screen")
	}
	if d.m.note.Path != "Notes/plain.md" {
		t.Errorf("clicking plain text opened %q", d.m.note.Path)
	}
	if d.m.status != "" {
		t.Errorf("clicking plain text said %q", d.m.status)
	}
}

func TestOverlayClicks(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/tagged.md", "---\ntags: [clickable]\n---\n\n# Tagged\n")

	d := driverWith(t, cl)
	d.key("t")
	if d.m.pick == nil {
		t.Fatal("t should open the tag list")
	}

	// Clicking away from the box closes it, as a dialog does everywhere else.
	d.click(1, testHeight-3)
	if d.m.pick != nil {
		t.Fatal("clicking outside the overlay should have closed it")
	}

	// And clicking a row picks it.
	d.key("t")
	d.typeText("clickable")
	l := d.m.layout()
	d.click(l.box.x+3, l.box.y+d.m.pick.listTopRow())

	if d.m.pick != nil {
		t.Error("choosing a row should have closed the overlay")
	}
	if d.m.query.tag != "clickable" {
		t.Errorf("tag filter = %q, want the row that was clicked", d.m.query.tag)
	}
}

// Turning the mouse off has to actually stop the UI reacting, or "off" is a
// label rather than a state.
func TestMouseCanBeTurnedOff(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/quiet.md", "# Quiet\n")

	d := driverWith(t, cl)
	d.key("M")
	if d.m.mouse {
		t.Fatal("M should have turned the mouse off")
	}

	before := d.m.note.Path
	row := d.rowOf("Notes/quiet.md")
	if row < 0 {
		t.Fatal("Notes/quiet.md is not listed")
	}
	d.click(2, firstRow+row)
	if d.m.note.Path != before {
		t.Errorf("a click was acted on with the mouse off: opened %q", d.m.note.Path)
	}

	d.key("M")
	if !d.m.mouse {
		t.Fatal("M should have turned it back on")
	}
	d.click(2, firstRow+row)
	if d.m.note.Path != "Notes/quiet.md" {
		t.Errorf("open note = %q, want the click to work again", d.m.note.Path)
	}
}

// Clicking another note with a draft in hand asks the same question the keyboard
// asks, rather than quietly dropping it.
func TestClickAsksAboutUnsavedWork(t *testing.T) {
	cl := startServer(t)
	seed(t, cl, "Notes/one.md", "one\n")
	seed(t, cl, "Notes/two.md", "two\n")

	d := driverWith(t, cl)
	d.openPath("Notes/one.md")
	d.key("i")
	d.typeText("draft ")
	d.key("esc")

	row := d.rowOf("Notes/two.md")
	if row < 0 {
		t.Fatal("Notes/two.md is not listed")
	}
	d.click(2, firstRow+row)

	if d.m.pr.kind != prSwitch {
		t.Fatalf("prompt = %v, want to be asked about the draft", d.m.pr.kind)
	}
	if d.m.note.Path != "Notes/one.md" {
		t.Error("it switched anyway")
	}
}

func TestLinkAt(t *testing.T) {
	line := "See [[Projects/folio]] and ![[diagram.png]] here."
	tests := []struct {
		col     int
		want    string
		isEmbed bool
		found   bool
	}{
		{0, "", false, false},               // "S"
		{4, "Projects/folio", false, true},  // the first "["
		{10, "Projects/folio", false, true}, // inside
		{21, "Projects/folio", false, true}, // the last "]"
		{22, "", false, false},              // just past it
		{27, "", false, false},              // the "!" of the embed
		{30, "diagram.png", true, true},     // inside the embed
		{len(line) - 1, "", false, false},   // the full stop
		{len(line) + 50, "", false, false},  // past the end
		{-1, "", false, false},              // before the start
	}
	for _, tc := range tests {
		got, embed, ok := linkAt(line, tc.col)
		if got != tc.want || embed != tc.isEmbed || ok != tc.found {
			t.Errorf("linkAt(col %d) = (%q, %v, %v), want (%q, %v, %v)",
				tc.col, got, embed, ok, tc.want, tc.isEmbed, tc.found)
		}
	}

	// A link split by wrapping has no complete span, and half a target is worse
	// than none.
	if _, _, ok := linkAt("a line ending in [[Projects/fol", 20); ok {
		t.Error("half a link should not be clickable")
	}

	// Wide characters have to be counted in columns, not bytes.
	wide := "日本語 [[Note]]"
	if got, _, ok := linkAt(wide, 8); !ok || got != "Note" {
		t.Errorf("linkAt over wide text = (%q, %v), want Note", got, ok)
	}
	if _, _, ok := linkAt(wide, 2); ok {
		t.Error("column 2 is inside the wide text, not the link")
	}
}

// ---------------------------------------------------------------- helpers

// rowOf returns the sidebar row a note is drawn on, or -1.
func (d *driver) rowOf(path string) int {
	for i, it := range d.m.items {
		if it.path == path {
			if i < d.m.top {
				return -1
			}
			return i - d.m.top
		}
	}
	return -1
}

// clickText finds text in the drawn note and clicks its first column, the way
// someone reading the screen would.
func (d *driver) clickText(t *testing.T, needle string) bool {
	t.Helper()
	for i, line := range d.m.body {
		plain := ansi.Strip(line)
		at := strings.Index(plain, needle)
		if at < 0 {
			continue
		}
		row := i - d.m.vp.YOffset
		if row < 0 || row >= d.m.vp.Height {
			return false // scrolled out of view
		}
		col := ansi.StringWidth(plain[:at])
		d.click(noteX+col, firstRow+row)
		return true
	}
	return false
}
