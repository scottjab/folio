package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// A prompt is the one-line question at the bottom of the screen: a path to
// type, or a yes/no to answer. It lives on the status line rather than in a
// dialog box because that is where a terminal user looks for a question, and
// because it leaves the note itself on screen while you answer.
type promptKind int

const (
	prNone promptKind = iota
	prNew
	prRename
	prAppend
	prDelete
	prQuit
	prConflict
	prRevoke
	prPerm
	prSwitch
	prAttach
	prAttachFolder
)

type prompt struct {
	kind  promptKind
	label string
	input textinput.Model
	// choices holds the single keys a keyed prompt accepts. Empty means the
	// prompt takes typed text.
	choices string
	hint    string

	// payload, meaning whatever the answer will be applied to.
	vault string
	path  string
	id    string
	text  string
	// then is what to run once the question is answered, for the flows that
	// interrupt something to ask about unsaved work.
	then tea.Cmd
}

// newTextPrompt asks for a line of text, pre-filled with value.
func newTextPrompt(kind promptKind, label, value string) prompt {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 512
	in.SetValue(value)
	in.CursorEnd()
	in.Focus()
	return prompt{kind: kind, label: label, input: in, hint: "enter confirm · esc cancel"}
}

// newKeyPrompt asks a question answered by a single key.
func newKeyPrompt(kind promptKind, label, choices, hint string) prompt {
	return prompt{kind: kind, label: label, choices: choices, hint: hint}
}

func (p prompt) active() bool  { return p.kind != prNone }
func (p prompt) textual() bool { return p.active() && p.choices == "" }

// accepts reports whether a key answers a keyed prompt.
func (p prompt) accepts(key string) bool {
	return len(key) == 1 && strings.Contains(p.choices, key)
}

func (p *prompt) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	return cmd
}

// render draws the prompt across the status line.
func (p prompt) render(st *styles, width int) string {
	label := st.prompt.Render(p.label + " ")
	body := p.input.View()
	if !p.textual() {
		body = st.muted.Render(p.hint)
	} else if p.hint != "" {
		body = body + "  " + st.muted.Render(p.hint)
	}
	return truncate(label+body, width)
}
