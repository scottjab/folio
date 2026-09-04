package tui

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scottjab/folio/internal/client"
	"github.com/scottjab/folio/internal/markdown"
	"github.com/scottjab/folio/internal/vaultpath"
)

// This file is the terminal half of what the browser gained: linking with
// completion, attaching a file, transcluding an embed, and the setting that
// says where an attachment goes.
//
// Where the browser reimplements a rule in TypeScript, the terminal client does
// not have to. It is Go, so it calls markdown.ResolveWikilink directly, which is
// the same function the indexer uses. There is no second implementation here to
// drift.

// linkIndex is every path in the vault, notes and attachments both. It is what
// turns a note's full path into the shortest link that still finds it.
type linkIndex struct {
	vault string
	paths []string
}

// shortest returns the shortest wikilink target that resolves back to path when
// written inside from: the bare name when it is unambiguous, the full path when
// it is not. Notes lose their extension; attachments keep theirs.
//
// This is Obsidian's "shortest path when possible", and it is the reason a vault
// of links reads as prose rather than as a directory listing.
func (ix linkIndex) shortest(path, from string) string {
	bare := func(p string) string {
		if vaultpath.IsMarkdown(p) {
			return strings.TrimSuffix(p, filepath.Ext(p))
		}
		return p
	}
	if base := filepath.Base(path); markdown.ResolveWikilink(ix.paths, from, base) == path {
		return bare(base)
	}
	return bare(path)
}

type linkIndexMsg struct {
	vault string
	paths []string
	err   error
}

// loadLinkIndex fetches every path in a vault, which completion and the embed
// renderer both need.
func loadLinkIndex(ctx context.Context, cl *client.Client, vault string) tea.Cmd {
	return func() tea.Msg {
		notes, err := cl.ListNotes(ctx, vault, client.ListOptions{})
		if err != nil {
			return linkIndexMsg{vault: vault, err: err}
		}
		paths := make([]string, 0, len(notes))
		for _, n := range notes {
			paths = append(paths, n.Path)
		}
		// An attachment listing failing is not worth losing note completion
		// over; it only costs the short form of an embed.
		if files, err := cl.ListAttachments(ctx, vault); err == nil {
			for _, f := range files {
				paths = append(paths, f.Path)
			}
		}
		return linkIndexMsg{vault: vault, paths: paths}
	}
}

// --- completion ---

// startComplete opens the note picker over the editor, so a link can be written
// without remembering the path.
//
// It is triggered by typing "[[", which is what Obsidian does and what the
// fingers of anyone coming from it will do anyway. The two brackets are already
// in the buffer by the time we get here; accepting a completion writes the
// target and the closing pair after them.
func (m *model) startComplete() tea.Cmd {
	items := make([]pickItem, 0, len(m.links.paths))
	from := m.note.Path
	for _, p := range m.links.paths {
		short := m.links.shortest(p, from)
		items = append(items, pickItem{
			label:  short,
			detail: m.st.muted.Render(p),
			path:   p,
			text:   short,
		})
	}
	cmd := m.showPicker(pickComplete, "Link to", items)
	m.pick.empty = "nothing to link to yet"
	m.pick.hint = "enter insert · esc cancel"
	return cmd
}

// insertCompletion writes the chosen target and closes the brackets.
func (m *model) insertCompletion(target string) tea.Cmd {
	m.closePicker()
	if !m.editing {
		return nil
	}
	m.insertAtCursor(target + "]]")
	return nil
}

// insertAtCursor writes text into the buffer where the cursor is.
//
// The textarea has no insert method, so this rebuilds the value around the
// cursor's offset and puts the cursor back after what was written. Working in
// runes rather than bytes matters: a note with an emoji in it would otherwise
// split a character in half.
func (m *model) insertAtCursor(text string) {
	value := []rune(m.ta.Value())
	at := m.cursorOffset()
	if at > len(value) {
		at = len(value)
	}
	next := string(value[:at]) + text + string(value[at:])
	m.ta.SetValue(next)

	// SetValue parks the cursor at the end, so walk it back to just after the
	// insertion.
	m.setCursorOffset(at + len([]rune(text)))
	m.dirty = m.ta.Value() != m.note.Content
}

// cursorOffset is the cursor's position as a rune offset into the whole buffer.
// The textarea reports a row and a column, and everything here works in offsets.
//
// The column comes from StartColumn plus ColumnOffset rather than from
// CharOffset. CharOffset is a display width, so one CJK character or emoji
// before the cursor counts as two, and an insertion would land a character early.
func (m *model) cursorOffset() int {
	row := m.ta.Line()
	li := m.ta.LineInfo()
	col := li.StartColumn + li.ColumnOffset

	lines := strings.Split(m.ta.Value(), "\n")
	off := 0
	for i := 0; i < row && i < len(lines); i++ {
		off += len([]rune(lines[i])) + 1 // +1 for the newline
	}
	if row < len(lines) {
		off += min(col, len([]rune(lines[row])))
	}
	return off
}

// setCursorOffset moves the cursor to a rune offset in the buffer.
func (m *model) setCursorOffset(off int) {
	lines := strings.Split(m.ta.Value(), "\n")
	row := 0
	for row < len(lines)-1 && off > len([]rune(lines[row])) {
		off -= len([]rune(lines[row])) + 1
		row++
	}

	// There is no setter for the logical row, so the cursor has to be walked
	// there. Two things make that fiddlier than it looks: CursorStart moves to
	// the start of the current row rather than of the buffer, so getting to the
	// top means pressing up; and CursorDown moves a *visual* row, so a paragraph
	// wider than the pane takes several presses to leave. Both are why this
	// watches Line() rather than counting presses.
	//
	// The bound is one press per visual row at worst, and there can never be
	// more visual rows than the buffer has runes plus lines. A fixed bound
	// rather than a clever one, because the cost of getting it wrong is a hung
	// UI.
	maxSteps := len([]rune(m.ta.Value())) + len(lines) + 8
	for guard := 0; m.ta.Line() > 0 && guard < maxSteps; guard++ {
		m.ta.CursorUp()
	}
	for guard := 0; m.ta.Line() < row && guard < maxSteps; guard++ {
		m.ta.CursorDown()
	}
	m.ta.SetCursor(off)
}

// justTypedOpenBracket reports whether the text immediately before the cursor is
// "[[", which is the moment completion should appear.
func (m *model) justTypedOpenBracket() bool {
	value := []rune(m.ta.Value())
	at := m.cursorOffset()
	return at >= 2 && at <= len(value) && value[at-2] == '[' && value[at-1] == '['
}

// --- attaching ---

// startAttach asks for a file to upload into the open note.
func (m *model) startAttach() tea.Cmd {
	if !m.hasNote {
		return m.setStatus("open a note first", true)
	}
	if m.note.Perm == client.PermRead {
		return m.setStatus("this note is read only", true)
	}
	pr := newTextPrompt(prAttach, "attach file:", "")
	pr.vault, pr.path = m.note.Vault, m.note.Path
	m.pr = pr
	return nil
}

type attachedMsg struct {
	up  client.Upload
	err error
}

// uploadFile reads a local file and hands it to the server, which decides where
// in the vault it lands and what to call it.
func uploadFile(ctx context.Context, cl *client.Client, vault, note, name string) tea.Cmd {
	return func() tea.Msg {
		expanded, err := expandHome(name)
		if err != nil {
			return attachedMsg{err: err}
		}
		data, err := os.ReadFile(expanded)
		if err != nil {
			return attachedMsg{err: err}
		}
		ct := mime.TypeByExtension(filepath.Ext(expanded))
		up, err := cl.Upload(ctx, vault, note, filepath.Base(expanded), ct, data)
		return attachedMsg{up: up, err: err}
	}
}

// expandHome turns a leading ~ into the user's home directory, because that is
// how a path gets typed at a terminal and nothing else will do it for us.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expanding %q: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// --- settings ---

type prefsMsg struct {
	prefs client.Prefs
	// saved distinguishes reading the settings from writing them, so only a
	// write reports back on the status line.
	saved bool
	err   error
}

func loadPrefs(ctx context.Context, cl *client.Client) tea.Cmd {
	return func() tea.Msg {
		p, err := cl.Prefs(ctx)
		return prefsMsg{prefs: p, err: err}
	}
}

func savePrefs(ctx context.Context, cl *client.Client, p client.Prefs) tea.Cmd {
	return func() tea.Msg {
		out, err := cl.SetPrefs(ctx, p)
		return prefsMsg{prefs: out, saved: true, err: err}
	}
}

// attachmentModes are the four places an attachment can go, by the names
// Obsidian uses, with the wording a terminal has room for.
var attachmentModes = []struct{ mode, label, detail string }{
	{"folder", "In one folder", "every attachment in the same folder"},
	{"vault", "In the vault root", "no folder at all"},
	{"current", "Beside the note", "in the note's own folder"},
	{"subfolder", "In a subfolder beside the note", "a named folder under the note's own"},
}

// showSettings lists the attachment modes.
func (m *model) showSettings() tea.Cmd {
	items := make([]pickItem, 0, len(attachmentModes))
	for _, a := range attachmentModes {
		label := a.label
		if a.mode == m.prefs.AttachmentMode {
			label += "  ✓"
		}
		items = append(items, pickItem{label: label, detail: m.st.muted.Render(a.detail), text: a.mode})
	}
	cmd := m.showPicker(pickSetting, "New attachments go", items)
	m.pick.hint = "enter choose · esc close"
	return cmd
}

// chooseAttachmentMode applies a mode, asking for a folder name when the mode
// needs one.
func (m *model) chooseAttachmentMode(mode string) tea.Cmd {
	m.closePicker()
	if mode == "vault" || mode == "current" {
		next := m.prefs
		next.AttachmentMode = mode
		return savePrefs(m.ctx, m.cl, next)
	}

	folder := m.prefs.AttachmentFolder
	if folder == "" {
		folder = "attachments"
	}
	pr := newTextPrompt(prAttachFolder, "folder for attachments:", folder)
	pr.text = mode
	m.pr = pr
	return nil
}

// --- transclusion ---

// embedKey identifies one resolved embed. The note it was written in is part of
// it because resolution depends on where the link sits.
type embedKey struct{ vault, from, target string }

type embedMsg struct {
	key embedKey
	res client.Embed
	err error
}

// loadEmbed resolves one ![[target]] on the server.
func loadEmbed(ctx context.Context, cl *client.Client, key embedKey) tea.Cmd {
	return func() tea.Msg {
		res, err := cl.Embed(ctx, key.vault, key.from, key.target)
		return embedMsg{key: key, res: res, err: err}
	}
}

// fetchEmbeds asks for every embed in the open note that we have not already
// resolved.
//
// A note is re-rendered on every keystroke while editing, so this deliberately
// only fires for targets missing from the cache; the cache is dropped when the
// vault changes, which is the only thing that can make one stale.
func (m *model) fetchEmbeds() tea.Cmd {
	if !m.hasNote {
		return nil
	}
	var cmds []tea.Cmd
	for _, target := range embedTargets(m.buffer()) {
		key := embedKey{vault: m.note.Vault, from: m.note.Path, target: target}
		if _, done := m.embeds[key]; done {
			continue
		}
		if m.embedsInFlight[key] {
			continue
		}
		if m.embedsInFlight == nil {
			m.embedsInFlight = map[embedKey]bool{}
		}
		m.embedsInFlight[key] = true
		cmds = append(cmds, loadEmbed(m.ctx, m.cl, key))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// embedTargets lists the ![[...]] targets in a note, in order and deduplicated.
func embedTargets(md string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i+2 < len(md); i++ {
		if md[i] != '!' || md[i+1] != '[' || md[i+2] != '[' {
			continue
		}
		end := strings.Index(md[i:], "]]")
		if end < 0 {
			break
		}
		inner := strings.TrimSpace(md[i+3 : i+end])
		// The alias is a display size for an image, never part of the target.
		if p, _, ok := strings.Cut(inner, "|"); ok {
			inner = strings.TrimSpace(p)
		}
		if inner != "" && !seen[inner] {
			seen[inner] = true
			out = append(out, inner)
		}
		i += end + 1
	}
	return out
}

// rerender redraws the note pane, which an embed arriving has to trigger: the
// body was rendered before its content was known.
func (m *model) rerender() {
	if m.hasNote {
		m.renderBody()
	}
}

// onAttached inserts a link to what was just uploaded.
func (m *model) onAttached(msg attachedMsg) tea.Cmd {
	if msg.err != nil {
		return m.fail("attach", msg.err)
	}
	// An image embeds, anything else links: a terminal cannot show a PDF inline
	// and neither can a browser mid-paragraph.
	text := "[[" + msg.up.Link + "]]"
	if isImagePath(msg.up.Path) {
		text = "!" + text
	}

	// Inserting at the cursor only makes sense while editing. Otherwise the
	// link goes on the end, which is what "attach this to the note" means when
	// there is no cursor to speak of.
	if m.editing {
		m.insertAtCursor(text)
	} else {
		return tea.Batch(
			m.setStatus("attached "+msg.up.Path, false),
			appendNote(m.ctx, m.cl, msg.up.Vault, m.note.Path, text, ""),
			loadLinkIndex(m.ctx, m.cl, msg.up.Vault),
		)
	}
	return tea.Batch(
		m.setStatus("attached "+msg.up.Path, false),
		loadLinkIndex(m.ctx, m.cl, msg.up.Vault),
	)
}

// isImagePath reports whether a path names something a browser renders inline.
// The terminal cannot show any of them, but the note it writes is read in both.
func isImagePath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".avif":
		return true
	}
	return false
}

// describeAttachments says where attachments will land, for the status line.
func describeAttachments(p client.Prefs) string {
	for _, a := range attachmentModes {
		if a.mode == p.AttachmentMode {
			if p.AttachmentMode == "folder" || p.AttachmentMode == "subfolder" {
				return strings.ToLower(a.label) + " " + p.AttachmentFolder + "/"
			}
			return strings.ToLower(a.label)
		}
	}
	return p.AttachmentMode
}

// lookupEmbed is what the renderer asks for a transclusion's content. A target
// we have not resolved yet reports false, and the source line stays on screen
// until the answer arrives and triggers a re-render.
func (m *model) lookupEmbed(target string) (client.Embed, bool) {
	res, ok := m.embeds[embedKey{vault: m.note.Vault, from: m.note.Path, target: target}]
	return res, ok
}
