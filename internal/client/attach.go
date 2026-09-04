package client

import (
	"context"
	"net/http"
	"net/url"
)

// Upload stores a file and reports where the server filed it.
//
// The destination is not the caller's to pick. The server applies the user's
// attachment preference to note, which is what keeps an attach in the terminal
// and a drop in the browser landing in the same folder. name is the file's
// original name; leaving it empty asks for a "Pasted image ..." name built from
// the clock, which is what the browser does for a clipboard paste.
func (c *Client) Upload(ctx context.Context, vault, note, name, contentType string, data []byte) (Upload, error) {
	q := url.Values{}
	if note != "" {
		q.Set("note", note)
	}
	if name != "" {
		q.Set("name", name)
	}
	h := http.Header{}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	h.Set("Content-Type", contentType)

	out, _, err := c.do[Upload](ctx, request{
		Method: http.MethodPost,
		Path:   "/api/vaults/" + vault + "/upload",
		Query:  q, Header: h, Body: data,
	})
	return out, err
}

// ListAttachments returns the non-markdown files in a vault.
func (c *Client) ListAttachments(ctx context.Context, vault string) ([]AttachmentInfo, error) {
	out, _, err := c.do[struct {
		Attachments []AttachmentInfo `json:"attachments"`
	}](ctx, request{
		Method: http.MethodGet,
		Path:   "/api/vaults/" + vault + "/attachments",
	})
	return out.Attachments, err
}

// Embed resolves one ![[target]] written inside the note at from.
//
// The resolution and the section extraction happen on the server, so the
// terminal renders the same span of text the browser does.
func (c *Client) Embed(ctx context.Context, vault, from, target string) (Embed, error) {
	q := url.Values{"target": {target}}
	if from != "" {
		q.Set("from", from)
	}
	out, _, err := c.do[Embed](ctx, request{
		Method: http.MethodGet,
		Path:   "/api/vaults/" + vault + "/embed",
		Query:  q,
	})
	return out, err
}

// Prefs reads the caller's settings.
func (c *Client) Prefs(ctx context.Context) (Prefs, error) {
	out, _, err := c.do[Prefs](ctx, request{Method: http.MethodGet, Path: "/api/prefs"})
	return out, err
}

// SetPrefs replaces the caller's settings. It is a full replace, so read them
// first and send back the whole thing.
func (c *Client) SetPrefs(ctx context.Context, p Prefs) (Prefs, error) {
	out, _, err := c.do[Prefs](ctx, request{
		Method: http.MethodPut, Path: "/api/prefs", Body: p,
	})
	return out, err
}
