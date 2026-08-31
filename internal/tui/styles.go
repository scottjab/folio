package tui

import "github.com/charmbracelet/lipgloss"

// The palette is built from the ANSI colour indices rather than hex values, so
// folio's terminal UI comes out in whatever colours the user's terminal theme
// already uses. A hard-coded purple looks wrong in half the terminals it lands
// in; "colour 5" looks right in all of them.
var (
	colAccent = lipgloss.AdaptiveColor{Light: "4", Dark: "12"} // blue
	colHead   = lipgloss.AdaptiveColor{Light: "5", Dark: "13"} // magenta
	colLink   = lipgloss.AdaptiveColor{Light: "6", Dark: "14"} // cyan
	colTag    = lipgloss.AdaptiveColor{Light: "3", Dark: "11"} // yellow
	colCode   = lipgloss.AdaptiveColor{Light: "2", Dark: "10"} // green
	colErr    = lipgloss.AdaptiveColor{Light: "1", Dark: "9"}  // red
	colMuted  = lipgloss.AdaptiveColor{Light: "8", Dark: "8"}  // bright black
)

// styles holds every lipgloss style the UI draws with. They are built once, in
// [newStyles], because lipgloss resolves adaptive colours against the detected
// terminal background each time a style is created.
type styles struct {
	// chrome
	title     lipgloss.Style
	key       lipgloss.Style
	muted     lipgloss.Style
	errorText lipgloss.Style
	okText    lipgloss.Style
	divider   lipgloss.Style

	// sidebar
	paneTitle    lipgloss.Style
	itemSelected lipgloss.Style
	itemDetail   lipgloss.Style
	itemShared   lipgloss.Style

	// note body
	h1, h2, h3 lipgloss.Style
	bold       lipgloss.Style
	italic     lipgloss.Style
	strike     lipgloss.Style
	code       lipgloss.Style
	codeBlock  lipgloss.Style
	quote      lipgloss.Style
	bullet     lipgloss.Style
	link       lipgloss.Style
	wikilink   lipgloss.Style
	tag        lipgloss.Style
	rule       lipgloss.Style
	frontmat   lipgloss.Style
	tableHead  lipgloss.Style
	tableRule  lipgloss.Style
	match      lipgloss.Style

	// overlays
	overlay      lipgloss.Style
	overlayTitle lipgloss.Style
	prompt       lipgloss.Style
}

func newStyles() *styles {
	s := &styles{}

	s.muted = lipgloss.NewStyle().Foreground(colMuted)
	s.errorText = lipgloss.NewStyle().Foreground(colErr)
	s.okText = lipgloss.NewStyle().Foreground(colCode)
	s.divider = lipgloss.NewStyle().Foreground(colMuted)

	s.title = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	s.key = lipgloss.NewStyle().Bold(true).Foreground(colAccent)

	s.paneTitle = lipgloss.NewStyle().Bold(true).Foreground(colMuted)
	// Reverse video for the selection, rather than a background colour: it is
	// the one highlight that is legible on every terminal theme.
	s.itemSelected = lipgloss.NewStyle().Reverse(true)
	s.itemDetail = s.muted
	s.itemShared = lipgloss.NewStyle().Foreground(colLink)

	s.h1 = lipgloss.NewStyle().Bold(true).Foreground(colHead).Underline(true)
	s.h2 = lipgloss.NewStyle().Bold(true).Foreground(colHead)
	s.h3 = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	s.bold = lipgloss.NewStyle().Bold(true)
	s.italic = lipgloss.NewStyle().Italic(true)
	s.strike = lipgloss.NewStyle().Strikethrough(true)
	s.code = lipgloss.NewStyle().Foreground(colCode)
	s.codeBlock = lipgloss.NewStyle().Foreground(colCode)
	s.quote = lipgloss.NewStyle().Foreground(colMuted).Italic(true)
	s.bullet = lipgloss.NewStyle().Foreground(colAccent)
	s.link = lipgloss.NewStyle().Foreground(colLink).Underline(true)
	s.wikilink = lipgloss.NewStyle().Foreground(colLink)
	s.tag = lipgloss.NewStyle().Foreground(colTag)
	s.rule = lipgloss.NewStyle().Foreground(colMuted)
	s.frontmat = lipgloss.NewStyle().Foreground(colMuted)
	s.tableHead = lipgloss.NewStyle().Bold(true)
	s.tableRule = lipgloss.NewStyle().Foreground(colMuted)
	s.match = lipgloss.NewStyle().Reverse(true)

	s.overlay = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colAccent).
		Padding(0, 1)
	s.overlayTitle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	s.prompt = lipgloss.NewStyle().Foreground(colAccent)

	return s
}
