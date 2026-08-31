package mcpsrv

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottjab/folio/internal/notes"
)

// resourceScheme addresses a note as folio://<vault>/<path>.
const resourceScheme = "folio://"

// addResources exposes notes as MCP resources in addition to tools.
//
// Resources and tools overlap deliberately: a client that browses resources gets
// the vault as a navigable tree, while a client that reasons with tools gets
// search. Offering both means we do not have to guess which style a given agent
// prefers.
func (sess *session) addResources(srv *mcp.Server) {
	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "note",
		Title:       "Note",
		URITemplate: "folio://{vault}/{+path}",
		Description: "A markdown note. vault is a vault directory name, or \"me\" for your own; path is vault-relative, for example Daily/2026-08-30.md",
		MIMEType:    "text/markdown",
	}, sess.readResource)

	srv.AddResource(&mcp.Resource{
		Name:        "index",
		Title:       "Vault index",
		URI:         "folio://me/",
		Description: "A listing of every note in your vault, with its title and tags.",
		MIMEType:    "text/markdown",
	}, sess.readIndexResource)
}

// parseResourceURI splits folio://<vault>/<path> into its parts.
func parseResourceURI(uri string) (vault, path string, err error) {
	rest, found := strings.CutPrefix(uri, resourceScheme)
	if !found {
		return "", "", fmt.Errorf("resource URI must start with %s, got %q", resourceScheme, uri)
	}
	vault, path, _ = strings.Cut(rest, "/")
	if vault == "" {
		return "", "", fmt.Errorf("resource URI %q names no vault", uri)
	}
	return vault, path, nil
}

func (sess *session) readResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	vaultName, path, err := parseResourceURI(req.Params.URI)
	if err != nil {
		return nil, err
	}
	sc, err := sess.scope(ctx, vaultName)
	if err != nil {
		return nil, toolErr(err)
	}
	if path == "" {
		return sess.vaultIndex(ctx, sc, req.Params.URI)
	}

	n, err := sess.Notes.Read(ctx, sc, path)
	if err != nil {
		return nil, toolErr(err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "text/markdown",
			Text:     n.Content,
		}},
	}, nil
}

func (sess *session) readIndexResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	sc, err := sess.scope(ctx, "me")
	if err != nil {
		return nil, toolErr(err)
	}
	return sess.vaultIndex(ctx, sc, req.Params.URI)
}

// vaultIndex renders a vault listing as markdown, which is both readable by a
// person and cheap for a model to scan.
func (sess *session) vaultIndex(ctx context.Context, sc notes.Scope, uri string) (*mcp.ReadResourceResult, error) {
	list, err := sess.Notes.List(ctx, sc, notes.ListOptions{Limit: 500})
	if err != nil {
		return nil, toolErr(err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s (%d notes)\n\n", sc.Dir, len(list))
	for _, s := range list {
		fmt.Fprintf(&b, "- [[%s]] %s", strings.TrimSuffix(s.Path, ".md"), s.Title)
		if len(s.Tags) > 0 {
			fmt.Fprintf(&b, " (#%s)", strings.Join(s.Tags, " #"))
		}
		fmt.Fprintf(&b, " — updated %s\n", s.UpdatedAt.Format(time.DateOnly))
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: "text/markdown",
			Text:     b.String(),
		}},
	}, nil
}

// NoteURI builds the resource URI for a note, which is what a change
// notification names.
func NoteURI(vault, path string) string {
	return resourceScheme + vault + "/" + path
}
