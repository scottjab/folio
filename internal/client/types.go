package client

import "time"

// The types in this file are the wire shapes of the JSON API, one for one with
// the response structs in internal/httpapi. They are duplicated rather than
// shared on purpose: they are a published contract, and a client that reached
// into the server's internals would make every field rename a wire break.

// Perm is a share permission.
type Perm string

const (
	PermRead  Perm = "read"
	PermWrite Perm = "write"
)

// CanWrite reports whether the permission allows editing.
func (p Perm) CanWrite() bool { return p == PermWrite }

// Me is the caller's own identity, as the tailnet reports it.
type Me struct {
	Login       string `json:"login"`
	DisplayName string `json:"displayName"`
	ProfilePic  string `json:"profilePic"`
	// Vault is the caller's own vault directory name.
	Vault   string `json:"vault"`
	IsAgent bool   `json:"isAgent"`
}

// Vault is one vault the caller can read.
type Vault struct {
	Vault      string `json:"vault"`
	OwnerLogin string `json:"ownerLogin"`
	IsMine     bool   `json:"isMine"`
}

// Note is one note with its content.
type Note struct {
	Vault      string    `json:"vault"`
	OwnerLogin string    `json:"ownerLogin"`
	Path       string    `json:"path"`
	Content    string    `json:"content"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	ModTime    time.Time `json:"modTime"`
	Title      string    `json:"title"`
	Tags       []string  `json:"tags"`
	Perm       Perm      `json:"perm"`
	Backlinks  []Link    `json:"backlinks"`
}

// Summary is a note without its content, as the list and write endpoints return
// it.
type Summary struct {
	Vault      string    `json:"vault"`
	OwnerLogin string    `json:"ownerLogin"`
	Path       string    `json:"path"`
	Title      string    `json:"title"`
	Tags       []string  `json:"tags"`
	SHA256     string    `json:"sha256"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Link is one wikilink or markdown link pointing at a note.
type Link struct {
	Path   string `json:"path"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Alias  string `json:"alias"`
	Anchor string `json:"anchor"`
}

// Hit is one search result. Snippet contains <mark> tags around the matched
// terms and is HTML-escaped otherwise; see [StripMarks] for rendering it in a
// terminal.
type Hit struct {
	Vault      string    `json:"vault"`
	OwnerLogin string    `json:"ownerLogin"`
	Path       string    `json:"path"`
	Title      string    `json:"title"`
	Snippet    string    `json:"snippet"`
	Tags       []string  `json:"tags"`
	Score      float64   `json:"score"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Results is one page of search results.
type Results struct {
	Query   string `json:"query"`
	Hits    []Hit  `json:"hits"`
	HasMore bool   `json:"hasMore"`
}

// Tag is a tag and how many notes carry it.
type Tag struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// User is another tailnet user, for the share picker.
type User struct {
	Login       string `json:"login"`
	DisplayName string `json:"displayName"`
}

// Share is one grant of access to a note or folder.
type Share struct {
	ID           string    `json:"id"`
	Vault        string    `json:"vault"`
	OwnerLogin   string    `json:"ownerLogin"`
	Path         string    `json:"path"`
	IsFolder     bool      `json:"isFolder"`
	GranteeLogin string    `json:"granteeLogin"`
	Perm         Perm      `json:"perm"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Edit is one find-and-replace against a note's text. The server refuses an
// ambiguous match rather than guessing, and applies every edit or none.
type Edit struct {
	Old        string `json:"old"`
	New        string `json:"new"`
	ReplaceAll bool   `json:"replaceAll,omitzero"`
}

// Attachment is a binary file stored beside the notes.
type Attachment struct {
	Path        string
	ContentType string
	SHA256      string
	Data        []byte
}

// ListOptions filters a note listing.
type ListOptions struct {
	Folder string
	Tag    string
	Limit  int
	Offset int
}
