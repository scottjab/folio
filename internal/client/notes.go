package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Me returns the caller's identity, as the tailnet reports it to the server.
func (c *Client) Me(ctx context.Context) (Me, error) {
	me, _, err := c.do[Me](ctx, request{Method: http.MethodGet, Path: "/api/me"})
	return me, err
}

// Vaults lists every vault the caller can read, their own included.
func (c *Client) Vaults(ctx context.Context) ([]Vault, error) {
	out, _, err := c.do[struct {
		Vaults []Vault `json:"vaults"`
	}](ctx, request{Method: http.MethodGet, Path: "/api/vaults"})
	return out.Vaults, err
}

// ListNotes lists the notes in a vault, optionally filtered to a folder or a
// tag.
func (c *Client) ListNotes(ctx context.Context, vault string, opts ListOptions) ([]Summary, error) {
	q := url.Values{}
	if opts.Folder != "" {
		q.Set("folder", opts.Folder)
	}
	if opts.Tag != "" {
		q.Set("tag", opts.Tag)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	out, _, err := c.do[struct {
		Notes []Summary `json:"notes"`
	}](ctx, request{Method: http.MethodGet, Path: notesPath(vault), Query: q})
	return out.Notes, err
}

// Note reads one note, with its backlinks.
func (c *Client) Note(ctx context.Context, vault, path string) (Note, error) {
	n, _, err := c.do[Note](ctx, request{Method: http.MethodGet, Path: notePath(vault, path)})
	return n, err
}

// CreateNote writes a new note. It fails with [ErrConflict] if one already
// exists at that path.
func (c *Client) CreateNote(ctx context.Context, vault, path, content string) (Summary, error) {
	body := struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{path, content}
	s, _, err := c.do[Summary](ctx, request{
		Method: http.MethodPost, Path: notesPath(vault), Body: body,
	})
	return s, err
}

// UpdateNote saves a note.
//
// baseSHA is the hash the caller last read, which makes the write a
// compare-and-swap: if the note changed underneath, the save fails with
// [ErrConflict] and the server has written the caller's content to a file named
// by [ConflictPath] rather than picking a winner. Passing an empty baseSHA
// overwrites unconditionally, which is what a deliberate "save anyway" wants.
func (c *Client) UpdateNote(ctx context.Context, vault, path, content, baseSHA string) (Summary, error) {
	h := http.Header{}
	if baseSHA != "" {
		h.Set("If-Match", strconv.Quote(baseSHA))
	}
	s, _, err := c.do[Summary](ctx, request{
		Method: http.MethodPut, Path: notePath(vault, path), Header: h,
		Body: struct {
			Content string `json:"content"`
		}{content},
	})
	return s, err
}

// DeleteNote moves a note to the vault's trash.
func (c *Client) DeleteNote(ctx context.Context, vault, path string) error {
	_, _, err := c.do[struct{}](ctx, request{Method: http.MethodDelete, Path: notePath(vault, path)})
	return err
}

// MoveNote renames or moves a note, rewriting the links that point at it.
func (c *Client) MoveNote(ctx context.Context, vault, from, to string) (Summary, error) {
	body := struct {
		From string `json:"from"`
		To   string `json:"to"`
	}{from, to}
	s, _, err := c.do[Summary](ctx, request{
		Method: http.MethodPost, Path: "/api/vaults/" + vault + "/move", Body: body,
	})
	return s, err
}

// AppendNote adds text to the end of a note, or to the end of one section when
// underHeading is set. It creates the note if it does not exist.
func (c *Client) AppendNote(ctx context.Context, vault, path, text, underHeading string) (Summary, error) {
	body := struct {
		Path         string `json:"path"`
		Text         string `json:"text"`
		UnderHeading string `json:"underHeading,omitzero"`
	}{path, text, underHeading}
	s, _, err := c.do[Summary](ctx, request{
		Method: http.MethodPost, Path: "/api/vaults/" + vault + "/append", Body: body,
	})
	return s, err
}

// EditNote applies find-and-replace edits without rewriting the whole file.
func (c *Client) EditNote(ctx context.Context, vault, path string, edits []Edit) (Summary, error) {
	body := struct {
		Path  string `json:"path"`
		Edits []Edit `json:"edits"`
	}{path, edits}
	s, _, err := c.do[Summary](ctx, request{
		Method: http.MethodPost, Path: "/api/vaults/" + vault + "/edit", Body: body,
	})
	return s, err
}

// DailyNote returns the daily note for a day, creating it from the template
// unless create is false. A zero day means today.
func (c *Client) DailyNote(ctx context.Context, vault string, day time.Time, create bool) (Note, error) {
	q := url.Values{}
	if !day.IsZero() {
		q.Set("date", day.Format("2006-01-02"))
	}
	if !create {
		q.Set("create", "false")
	}
	n, _, err := c.do[Note](ctx, request{
		Method: http.MethodGet, Path: "/api/vaults/" + vault + "/daily", Query: q,
	})
	return n, err
}

// Backlinks lists the notes linking to this one.
func (c *Client) Backlinks(ctx context.Context, vault, path string) ([]Link, error) {
	out, _, err := c.do[struct {
		Backlinks []Link `json:"backlinks"`
	}](ctx, request{
		Method: http.MethodGet,
		Path:   "/api/vaults/" + vault + "/backlinks/" + path,
	})
	return out.Backlinks, err
}

// Attachment downloads a binary file from a vault.
func (c *Client) Attachment(ctx context.Context, vault, path string) (Attachment, error) {
	a := Attachment{Path: path}
	_, _, err := c.do[struct{}](ctx, request{
		Method: http.MethodGet,
		Path:   "/api/vaults/" + vault + "/attachments/" + path,
		Raw: func(resp *http.Response) error {
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("reading attachment %s: %w", path, err)
			}
			a.Data = data
			a.ContentType = resp.Header.Get("Content-Type")
			a.SHA256 = etag(resp.Header)
			return nil
		},
	})
	return a, err
}

// PutAttachment uploads a binary file. The server refuses markdown here; use
// [Client.CreateNote] for that.
func (c *Client) PutAttachment(ctx context.Context, vault, path string, data []byte, contentType string) (Attachment, error) {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	out, hdr, err := c.do[struct {
		Vault  string `json:"vault"`
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}](ctx, request{
		Method: http.MethodPost,
		Path:   "/api/vaults/" + vault + "/attachments/" + path,
		Header: h, Body: data,
	})
	if err != nil {
		return Attachment{}, err
	}
	sha := out.SHA256
	if sha == "" {
		sha = etag(hdr)
	}
	return Attachment{Path: out.Path, SHA256: sha, ContentType: contentType}, nil
}

func notesPath(vault string) string { return "/api/vaults/" + vault + "/notes" }

func notePath(vault, path string) string {
	return notesPath(vault) + "/" + path
}
