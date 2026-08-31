package client

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Search runs a full-text query across every vault the caller can read.
//
// The query language is the server's: bare words, "exact phrases", prefix*,
// -exclusions, and the tag: and path: filters.
func (c *Client) Search(ctx context.Context, query string, limit, offset int) (Results, error) {
	q := url.Values{}
	q.Set("q", query)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	r, _, err := c.do[Results](ctx, request{Method: http.MethodGet, Path: "/api/search", Query: q})
	return r, err
}

// Tags lists every tag in the readable vaults, with its note count.
func (c *Client) Tags(ctx context.Context) ([]Tag, error) {
	out, _, err := c.do[struct {
		Tags []Tag `json:"tags"`
	}](ctx, request{Method: http.MethodGet, Path: "/api/tags"})
	return out.Tags, err
}

// Folders lists every folder in the readable vaults.
func (c *Client) Folders(ctx context.Context) ([]string, error) {
	out, _, err := c.do[struct {
		Folders []string `json:"folders"`
	}](ctx, request{Method: http.MethodGet, Path: "/api/folders"})
	return out.Folders, err
}

// Users lists the other tailnet users known to the server, for the share picker.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	out, _, err := c.do[struct {
		Users []User `json:"users"`
	}](ctx, request{Method: http.MethodGet, Path: "/api/users"})
	return out.Users, err
}

// SnippetSegment is a run of search-result text, flagged if the query matched
// it.
type SnippetSegment struct {
	Text  string
	Match bool
}

// ParseSnippet turns a search snippet into styleable segments.
//
// The server hands the browser HTML: entity-escaped text with <mark> around each
// hit. A terminal wants neither, so this undoes the escaping and reports where
// the marks were, leaving the choice of how to highlight to the caller.
func ParseSnippet(s string) []SnippetSegment {
	var segs []SnippetSegment
	add := func(text string, match bool) {
		if text == "" {
			return
		}
		text = html.UnescapeString(text)
		// Snippets are single-line by definition; a stray newline from the note
		// would otherwise break the list layout.
		text = strings.ReplaceAll(text, "\n", " ")
		if n := len(segs); n > 0 && segs[n-1].Match == match {
			segs[n-1].Text += text
			return
		}
		segs = append(segs, SnippetSegment{Text: text, Match: match})
	}

	for {
		open := strings.Index(s, "<mark>")
		if open < 0 {
			add(s, false)
			return segs
		}
		add(s[:open], false)
		s = s[open+len("<mark>"):]

		close := strings.Index(s, "</mark>")
		if close < 0 {
			// Unterminated, which should not happen. Treat the rest as a match
			// rather than dropping it.
			add(s, true)
			return segs
		}
		add(s[:close], true)
		s = s[close+len("</mark>"):]
	}
}

// PlainSnippet is [ParseSnippet] flattened back to text, for places that cannot
// style anything.
func PlainSnippet(s string) string {
	var b strings.Builder
	for _, seg := range ParseSnippet(s) {
		b.WriteString(seg.Text)
	}
	return b.String()
}
