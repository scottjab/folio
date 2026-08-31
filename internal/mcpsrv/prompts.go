package mcpsrv

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addPrompts registers the ready-made requests a client can offer as commands.
//
// Each one does the tedious gathering itself, so the model starts with the notes
// already in hand rather than spending three tool calls working out what to
// read.
func (sess *session) addPrompts(srv *mcp.Server) {
	srv.AddPrompt(&mcp.Prompt{
		Name:        "daily_review",
		Title:       "Daily review",
		Description: "Review today's and yesterday's daily notes and pull out what is still outstanding.",
		Arguments: []*mcp.PromptArgument{
			{Name: "date", Description: "Date as YYYY-MM-DD. Defaults to today."},
		},
	}, sess.dailyReviewPrompt)

	srv.AddPrompt(&mcp.Prompt{
		Name:        "summarize_note",
		Title:       "Summarize a note",
		Description: "Summarize a note and say what it connects to.",
		Arguments: []*mcp.PromptArgument{
			{Name: "path", Description: "Vault-relative path to the note.", Required: true},
			{Name: "vault", Description: "Vault name. Defaults to your own."},
		},
	}, sess.summarizePrompt)

	srv.AddPrompt(&mcp.Prompt{
		Name:        "find_related",
		Title:       "Find related notes",
		Description: "Find notes related to one you name, using both its backlinks and a full-text search on its title.",
		Arguments: []*mcp.PromptArgument{
			{Name: "path", Description: "Vault-relative path to the note.", Required: true},
			{Name: "vault", Description: "Vault name. Defaults to your own."},
		},
	}, sess.findRelatedPrompt)
}

func userMessage(text string) *mcp.PromptMessage {
	return &mcp.PromptMessage{Role: "user", Content: &mcp.TextContent{Text: text}}
}

func (sess *session) dailyReviewPrompt(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	day := now()
	if s := req.Params.Arguments["date"]; s != "" {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			return nil, fmt.Errorf("date must be YYYY-MM-DD: %w", err)
		}
		day = d
	}
	sc, err := sess.scope(ctx, "")
	if err != nil {
		return nil, toolErr(err)
	}

	var b strings.Builder
	b.WriteString("Review these daily notes. List what is still outstanding, what got finished, ")
	b.WriteString("and anything that looks like it needs a decision. Be brief.\n\n")

	for _, d := range []time.Time{day.AddDate(0, 0, -1), day} {
		// Read only; a review should never create tomorrow's note as a side
		// effect of being asked about it.
		n, err := sess.Notes.DailyNote(ctx, sc, d, false)
		if err != nil {
			fmt.Fprintf(&b, "## %s\n\n(no daily note)\n\n", d.Format(time.DateOnly))
			continue
		}
		fmt.Fprintf(&b, "## %s (%s)\n\n%s\n\n", d.Format(time.DateOnly), n.Path, n.Content)
	}

	return &mcp.GetPromptResult{
		Description: "Daily review for " + day.Format(time.DateOnly),
		Messages:    []*mcp.PromptMessage{userMessage(b.String())},
	}, nil
}

func (sess *session) summarizePrompt(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	path := req.Params.Arguments["path"]
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	sc, err := sess.scope(ctx, req.Params.Arguments["vault"])
	if err != nil {
		return nil, toolErr(err)
	}
	n, err := sess.Notes.Read(ctx, sc, path)
	if err != nil {
		return nil, toolErr(err)
	}
	links, _ := sess.Notes.Backlinks(ctx, sc, n.Path)

	var b strings.Builder
	fmt.Fprintf(&b, "Summarize this note in a few sentences, then say what it connects to.\n\n")
	fmt.Fprintf(&b, "# %s (%s)\n\n%s\n", n.Title, n.Path, n.Content)
	if len(links) > 0 {
		b.WriteString("\n## Notes that link here\n\n")
		for _, l := range links {
			fmt.Fprintf(&b, "- %s (%s)\n", l.Title, l.Path)
		}
	}

	return &mcp.GetPromptResult{
		Description: "Summary of " + n.Path,
		Messages:    []*mcp.PromptMessage{userMessage(b.String())},
	}, nil
}

func (sess *session) findRelatedPrompt(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	path := req.Params.Arguments["path"]
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	sc, err := sess.scope(ctx, req.Params.Arguments["vault"])
	if err != nil {
		return nil, toolErr(err)
	}
	n, err := sess.Notes.Read(ctx, sc, path)
	if err != nil {
		return nil, toolErr(err)
	}

	links, _ := sess.Notes.Backlinks(ctx, sc, n.Path)
	// Searching on the title finds notes that discuss the same thing without
	// linking to it, which is the half backlinks miss.
	hits, _, _ := sess.Notes.Search(ctx, sess.user, n.Title, 10, 0)

	var b strings.Builder
	fmt.Fprintf(&b, "Which of these notes are genuinely related to %q, and why? Ignore coincidental matches.\n\n", n.Title)
	fmt.Fprintf(&b, "## The note\n\n%s\n\n", n.Content)

	if len(links) > 0 {
		b.WriteString("## Notes that link to it\n\n")
		for _, l := range links {
			fmt.Fprintf(&b, "- %s (%s)\n", l.Title, l.Path)
		}
		b.WriteString("\n")
	}
	if len(hits) > 0 {
		b.WriteString("## Notes mentioning similar terms\n\n")
		for _, h := range hits {
			if h.Path == n.Path && h.Vault == n.Vault {
				continue
			}
			fmt.Fprintf(&b, "- %s (%s): %s\n", h.Title, h.Path, plainSnippet(h.Snippet))
		}
	}

	return &mcp.GetPromptResult{
		Description: "Notes related to " + n.Path,
		Messages:    []*mcp.PromptMessage{userMessage(b.String())},
	}, nil
}
