package events

import "time"

// Kinds of note change. These strings are part of the SSE contract with the
// browser and the MCP resource-notification contract with agents, so they are
// declared once here rather than spelled out at each emit site.
const (
	NoteCreated = "note.created"
	NoteUpdated = "note.updated"
	NoteDeleted = "note.deleted"
	NoteMoved   = "note.moved"
)

// NoteChanged is emitted whenever a note is written, deleted, or moved, whether
// by the web editor, an MCP agent, or an external tool the file watcher noticed.
type NoteChanged struct {
	// ID is a UUIDv7, which sorts by time and doubles as the SSE event id so a
	// reconnecting browser can say where it left off.
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	VaultID int64  `json:"-"`
	Vault   string `json:"vault"`
	Path    string `json:"path"`
	// OldPath is set on a move.
	OldPath string `json:"oldPath,omitzero"`
	SHA256  string `json:"sha256,omitzero"`
	// ByLogin is empty for a change made outside tsnotes, which is how the UI
	// tells "you saved this" from "Obsidian saved this".
	ByLogin string    `json:"byLogin,omitzero"`
	At      time.Time `json:"at"`
}
