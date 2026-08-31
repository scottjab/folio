package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scottjab/folio/internal/client"
)

// Every server call is a tea.Cmd returning one of these messages. Nothing in
// this file touches the model: a command runs on its own goroutine, and the only
// thing it is allowed to do is come back with a message.

type meMsg struct {
	me     client.Me
	vaults []client.Vault
	err    error
}

type notesMsg struct {
	label string
	from  listQuery
	notes []client.Summary
	err   error
}

type noteMsg struct {
	note client.Note
	// focus moves to the note pane when the note was opened deliberately, and
	// stays put when it was reloaded underneath.
	focus bool
	err   error
}

type savedMsg struct {
	sum client.Summary
	// content is what was sent, so the model can adopt it as the saved text
	// without trusting a buffer that may have been typed into since.
	content string
	// reload means the server changed the file in a way the client cannot
	// reconstruct, so the note has to be read back.
	reload bool
	err    error
}

type createdMsg struct {
	sum client.Summary
	err error
}

type movedMsg struct {
	from string
	sum  client.Summary
	err  error
}

type deletedMsg struct {
	vault string
	path  string
	err   error
}

type searchMsg struct {
	seq int
	res client.Results
	err error
}

type tagsMsg struct {
	tags []client.Tag
	err  error
}

type foldersMsg struct {
	folders []string
	err     error
}

type usersMsg struct {
	users []client.User
	err   error
}

type sharesMsg struct {
	mine   []client.Share
	toMe   []client.Share
	err    error
	notify string
}

type shareChangedMsg struct {
	text string
	err  error
}

type eventMsg struct{ ev client.Event }

type editorDoneMsg struct {
	content string
	err     error
}

type statusMsg struct {
	text  string
	isErr bool
}

// clearStatusMsg expires a transient status line, so an error from ten minutes
// ago is not still sitting there implying it just happened.
type clearStatusMsg struct{ seq int }

// searchDueMsg fires after the keystroke debounce; seq drops it if more has
// been typed since.
type searchDueMsg struct{ seq int }

// listQuery is what the sidebar is currently listing.
type listQuery struct {
	vault  string
	folder string
	tag    string
}

func loadMe(ctx context.Context, cl *client.Client) tea.Cmd {
	return func() tea.Msg {
		me, err := cl.Me(ctx)
		if err != nil {
			return meMsg{err: err}
		}
		vaults, err := cl.Vaults(ctx)
		return meMsg{me: me, vaults: vaults, err: err}
	}
}

func loadNotes(ctx context.Context, cl *client.Client, q listQuery, label string) tea.Cmd {
	return func() tea.Msg {
		notes, err := cl.ListNotes(ctx, q.vault, client.ListOptions{
			Folder: q.folder, Tag: q.tag, Limit: 500,
		})
		return notesMsg{label: label, from: q, notes: notes, err: err}
	}
}

func loadNote(ctx context.Context, cl *client.Client, vault, path string, focus bool) tea.Cmd {
	return func() tea.Msg {
		n, err := cl.Note(ctx, vault, path)
		return noteMsg{note: n, focus: focus, err: err}
	}
}

func loadDaily(ctx context.Context, cl *client.Client, vault string) tea.Cmd {
	return func() tea.Msg {
		n, err := cl.DailyNote(ctx, vault, time.Time{}, true)
		return noteMsg{note: n, focus: true, err: err}
	}
}

func saveNote(ctx context.Context, cl *client.Client, vault, path, content, baseSHA string) tea.Cmd {
	return func() tea.Msg {
		sum, err := cl.UpdateNote(ctx, vault, path, content, baseSHA)
		return savedMsg{sum: sum, content: content, err: err}
	}
}

func createNote(ctx context.Context, cl *client.Client, vault, path, content string) tea.Cmd {
	return func() tea.Msg {
		sum, err := cl.CreateNote(ctx, vault, path, content)
		return createdMsg{sum: sum, err: err}
	}
}

func appendNote(ctx context.Context, cl *client.Client, vault, path, text, heading string) tea.Cmd {
	return func() tea.Msg {
		sum, err := cl.AppendNote(ctx, vault, path, text, heading)
		// The server decided where the text landed, so read the note back rather
		// than guessing at the result.
		return savedMsg{sum: sum, reload: true, err: err}
	}
}

func moveNote(ctx context.Context, cl *client.Client, vault, from, to string) tea.Cmd {
	return func() tea.Msg {
		sum, err := cl.MoveNote(ctx, vault, from, to)
		return movedMsg{from: from, sum: sum, err: err}
	}
}

func deleteNote(ctx context.Context, cl *client.Client, vault, path string) tea.Cmd {
	return func() tea.Msg {
		err := cl.DeleteNote(ctx, vault, path)
		return deletedMsg{vault: vault, path: path, err: err}
	}
}

func runSearch(ctx context.Context, cl *client.Client, query string, seq int) tea.Cmd {
	return func() tea.Msg {
		res, err := cl.Search(ctx, query, 60, 0)
		return searchMsg{seq: seq, res: res, err: err}
	}
}

func loadTags(ctx context.Context, cl *client.Client) tea.Cmd {
	return func() tea.Msg {
		tags, err := cl.Tags(ctx)
		return tagsMsg{tags: tags, err: err}
	}
}

func loadFolders(ctx context.Context, cl *client.Client) tea.Cmd {
	return func() tea.Msg {
		folders, err := cl.Folders(ctx)
		return foldersMsg{folders: folders, err: err}
	}
}

func loadUsers(ctx context.Context, cl *client.Client) tea.Cmd {
	return func() tea.Msg {
		users, err := cl.Users(ctx)
		return usersMsg{users: users, err: err}
	}
}

func loadShares(ctx context.Context, cl *client.Client, notify string) tea.Cmd {
	return func() tea.Msg {
		mine, err := cl.Shares(ctx)
		if err != nil {
			return sharesMsg{err: err}
		}
		toMe, err := cl.SharedWithMe(ctx)
		return sharesMsg{mine: mine, toMe: toMe, err: err, notify: notify}
	}
}

func grantShare(ctx context.Context, cl *client.Client, path string, isFolder bool, grantee string, perm client.Perm) tea.Cmd {
	return func() tea.Msg {
		s, err := cl.Grant(ctx, path, isFolder, grantee, perm)
		if err != nil {
			return shareChangedMsg{err: err}
		}
		return shareChangedMsg{text: fmt.Sprintf("shared %s with %s (%s)", s.Path, s.GranteeLogin, s.Perm)}
	}
}

func revokeShare(ctx context.Context, cl *client.Client, id, label string) tea.Cmd {
	return func() tea.Msg {
		if err := cl.Revoke(ctx, id); err != nil {
			return shareChangedMsg{err: err}
		}
		return shareChangedMsg{text: "revoked " + label}
	}
}

// waitForEvent turns the next server-sent event into a message. It is reissued
// on every event, which is how a channel becomes part of the update loop.
func waitForEvent(ch <-chan client.Event) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return eventMsg{ev: ev}
	}
}

// openInEditor hands the note to $EDITOR and comes back with what was saved.
//
// The temporary file keeps the note's own file name so the editor picks the
// right syntax highlighting and the status line says something recognisable.
func openInEditor(editor, name, content string) tea.Cmd {
	dir, err := os.MkdirTemp("", "folio-edit-")
	if err != nil {
		return func() tea.Msg { return editorDoneMsg{err: err} }
	}
	path := filepath.Join(dir, safeFilename(name))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		os.RemoveAll(dir)
		return func() tea.Msg { return editorDoneMsg{err: err} }
	}

	name, args := splitEditor(editor)
	cmd := exec.Command(name, append(args, path)...)
	return tea.ExecProcess(cmd, func(runErr error) tea.Msg {
		defer os.RemoveAll(dir)
		if runErr != nil {
			return editorDoneMsg{err: fmt.Errorf("%s: %w", editor, runErr)}
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return editorDoneMsg{err: err}
		}
		return editorDoneMsg{content: string(b)}
	})
}

// expireStatus clears a status line after a while.
func expireStatus(seq int) tea.Cmd {
	return tea.Tick(6*time.Second, func(time.Time) tea.Msg { return clearStatusMsg{seq: seq} })
}

// debounceSearch waits out a burst of typing before querying the server.
func debounceSearch(seq int) tea.Cmd {
	return tea.Tick(160*time.Millisecond, func(time.Time) tea.Msg { return searchDueMsg{seq: seq} })
}
