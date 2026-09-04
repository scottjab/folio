package mcpsrv

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottjab/folio/internal/notes"
	"github.com/scottjab/folio/internal/prefs"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/vaultpath"
)

// maxAttachmentBytes caps what attach_file will take, matching the HTTP API's
// limit so an agent and a browser hit the same wall.
const maxAttachmentBytes = 64 << 20 // 64 MiB

// Every tool's input and output is a plain Go struct. mcp.AddTool derives the
// JSON schema from it, so the struct tags below are the contract the model sees;
// the jsonschema descriptions are what it reads to decide which tool to call.

type searchInput struct {
	Query string `json:"query" jsonschema:"Search text. Supports \"quoted phrases\", prefix* matching, -exclusions, and tag:, path:, and title: filters. An empty query lists the most recently updated notes."`
	Limit int    `json:"limit,omitzero" jsonschema:"Maximum results to return. Defaults to 20."`
}

type searchHit struct {
	Vault      string   `json:"vault" jsonschema:"Which vault the note is in. Pass this back to other tools when it is not your own."`
	Path       string   `json:"path" jsonschema:"Vault-relative path, for example Daily/2026-08-30.md"`
	Title      string   `json:"title"`
	Snippet    string   `json:"snippet" jsonschema:"Matching text with the query terms marked."`
	Tags       []string `json:"tags"`
	OwnerLogin string   `json:"ownerLogin" jsonschema:"The tailnet user who owns the note."`
	UpdatedAt  string   `json:"updatedAt"`
}

type searchOutput struct {
	Hits    []searchHit `json:"hits"`
	HasMore bool        `json:"hasMore"`
}

func (sess *session) addTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_notes",
		Description: "Full-text search across every note this user can read, including notes shared with them. This is the fastest way to find a note when you do not already know its path.",
	}, sess.searchNotes)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_notes",
		Description: "Browse notes by folder, tag, or recency. Use this to explore the vault's structure; use search_notes when you know roughly what you are looking for.",
	}, sess.listNotes)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_note",
		Description: "Read a note's full markdown content, along with its tags and the notes that link to it.",
	}, sess.readNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_note",
		Description: "Create a new note. Fails if one already exists at that path, so it can never overwrite anything.",
	}, sess.createNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "update_note",
		Description: "Replace a note's entire content. Prefer edit_note or append_note for targeted changes; use this only when rewriting the whole note is genuinely what you mean. Pass baseSha from a previous read to avoid clobbering a concurrent edit.",
	}, sess.updateNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "edit_note",
		Description: "Change specific passages in a note by find-and-replace, leaving everything else untouched. Each search string must appear exactly once unless replaceAll is set. This is the preferred way to modify an existing note.",
	}, sess.editNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "append_note",
		Description: "Add text to the end of a note, or to the end of the section under a named heading. Use this to add an item to a list without rewriting the file.",
	}, sess.appendNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_note",
		Description: "Move a note to the vault's trash. It is recoverable from .folio/trash on disk, but is no longer searchable or linkable.",
	}, sess.deleteNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "move_note",
		Description: "Rename or move a note, rewriting every wikilink elsewhere in the vault that named it, so nothing ends up pointing at a missing note.",
	}, sess.moveNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_backlinks",
		Description: "List the notes that link to a given note. Useful for understanding how an idea connects to the rest of the vault.",
	}, sess.getBacklinks)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_tags",
		Description: "List every tag in use with how many notes carry it, most used first.",
	}, sess.listTags)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_folders",
		Description: "List every folder that contains notes.",
	}, sess.listFolders)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_daily_note",
		Description: "Get the daily note for a date, creating it from a template if it does not exist yet. Defaults to today.",
	}, sess.getDailyNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_vaults",
		Description: "List the vaults this user can read: their own, plus anyone else's notes that have been shared with them.",
	}, sess.listVaults)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "vault_stats",
		Description: "Summarise a vault: how many notes, tags, and links it contains.",
	}, sess.vaultStats)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_shares",
		Description: "List the shares this user has granted to other people, and the notes other people have shared with them.",
	}, sess.listShares)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "share_note",
		Description: "Share a note or folder from this user's own vault with another tailnet user, at read or write access.",
	}, sess.shareNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "unshare_note",
		Description: "Revoke a share by its id. Takes effect immediately.",
	}, sess.unshareNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_attachment",
		Description: "Read a non-markdown file from a vault, such as an image embedded with ![[diagram.png]]. Returns base64.",
	}, sess.readAttachment)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_attachments",
		Description: "List the non-markdown files in a vault: images, PDFs, anything that is not a note. Use this to find what an ![[embed]] could point at.",
	}, sess.listAttachments)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "attach_file",
		Description: "Add a file to a vault and get back the link to write for it. The folder and any renaming to avoid a collision are decided by the user's own attachment setting, exactly as they are when someone drops a file into the browser, so do not try to choose a path yourself. Embed the returned link with ![[link]] for an image and [[link]] for anything else.",
	}, sess.attachFile)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "resolve_link",
		Description: "Work out what a [[wikilink]] or ![[embed]] actually points at, and return the text it would render. Use this rather than guessing which note a bare name means: the answer follows the same rule the editor and the index use, and it handles a #Heading by returning just that section.",
	}, sess.resolveLink)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_settings",
		Description: "Read this user's folio settings, currently where new attachments are filed. Worth checking before attach_file if you want to tell them where a file will land.",
	}, sess.getSettings)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_settings",
		Description: "Change where this user's new attachments are filed. This moves nothing that already exists, it only decides where the next one goes, and it changes the behaviour of their browser and terminal too. Only call it when the user has asked you to.",
	}, sess.setSettings)
}

func (sess *session) searchNotes(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	hits, hasMore, err := sess.Notes.Search(ctx, sess.user, in.Query, limit, 0)
	if err != nil {
		return nil, searchOutput{}, toolErr(err)
	}

	out := searchOutput{Hits: make([]searchHit, 0, len(hits)), HasMore: hasMore}
	for _, h := range hits {
		out.Hits = append(out.Hits, searchHit{
			Vault: h.Vault, Path: h.Path, Title: h.Title,
			// The index marks matches with control characters so a note
			// containing literal markup cannot forge a highlight. Models read
			// plain text, so strip them rather than translating to HTML.
			Snippet:    plainSnippet(h.Snippet),
			Tags:       h.Tags,
			OwnerLogin: h.OwnerLogin,
			UpdatedAt:  h.UpdatedAt.Format(time.RFC3339),
		})
	}
	return nil, out, nil
}

type listInput struct {
	Vault  string `json:"vault,omitzero" jsonschema:"Which vault to list. Defaults to this user's own."`
	Folder string `json:"folder,omitzero" jsonschema:"Restrict to a folder and everything beneath it, for example Daily"`
	Tag    string `json:"tag,omitzero" jsonschema:"Restrict to notes carrying this tag."`
	Limit  int    `json:"limit,omitzero" jsonschema:"Maximum notes to return. Defaults to 100."`
	Offset int    `json:"offset,omitzero"`
}

type noteSummary struct {
	Vault     string   `json:"vault"`
	Path      string   `json:"path"`
	Title     string   `json:"title"`
	Tags      []string `json:"tags"`
	UpdatedAt string   `json:"updatedAt"`
}

type listOutput struct {
	Notes []noteSummary `json:"notes"`
}

func (sess *session) listNotes(ctx context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, listOutput{}, toolErr(err)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	list, err := sess.Notes.List(ctx, sc, notes.ListOptions{
		Folder: in.Folder, Tag: in.Tag, Limit: limit, Offset: in.Offset,
	})
	if err != nil {
		return nil, listOutput{}, toolErr(err)
	}

	out := listOutput{Notes: make([]noteSummary, 0, len(list))}
	for _, s := range list {
		out.Notes = append(out.Notes, noteSummary{
			Vault: s.Vault, Path: s.Path, Title: s.Title, Tags: s.Tags,
			UpdatedAt: s.UpdatedAt.Format(time.RFC3339),
		})
	}
	return nil, out, nil
}

type readInput struct {
	Path  string `json:"path" jsonschema:"Vault-relative path, for example Projects/folio.md"`
	Vault string `json:"vault,omitzero" jsonschema:"Which vault. Defaults to this user's own."`
}

type linkOut struct {
	Path  string `json:"path"`
	Title string `json:"title"`
}

type readOutput struct {
	Vault      string    `json:"vault"`
	Path       string    `json:"path"`
	Title      string    `json:"title"`
	Content    string    `json:"content" jsonschema:"The note's full markdown, frontmatter included."`
	Tags       []string  `json:"tags"`
	Sha        string    `json:"sha" jsonschema:"Content hash. Pass this back as baseSha on update_note to avoid overwriting a concurrent edit."`
	Perm       string    `json:"perm" jsonschema:"Either read or write. A read-only note cannot be modified."`
	OwnerLogin string    `json:"ownerLogin"`
	Backlinks  []linkOut `json:"backlinks" jsonschema:"Notes that link to this one."`
}

func (sess *session) readNote(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, readOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, readOutput{}, toolErr(err)
	}
	n, err := sess.Notes.Read(ctx, sc, in.Path)
	if err != nil {
		return nil, readOutput{}, toolErr(err)
	}
	links, err := sess.Notes.Backlinks(ctx, sc, n.Path)
	if err != nil {
		return nil, readOutput{}, toolErr(err)
	}

	out := readOutput{
		Vault: n.Vault, Path: n.Path, Title: n.Title, Content: n.Content,
		Tags: n.Tags, Sha: n.SHA256, Perm: string(n.Perm), OwnerLogin: n.OwnerLogin,
		Backlinks: make([]linkOut, 0, len(links)),
	}
	for _, l := range links {
		out.Backlinks = append(out.Backlinks, linkOut{Path: l.Path, Title: l.Title})
	}
	return nil, out, nil
}

type createInput struct {
	Path    string `json:"path" jsonschema:"Vault-relative path. A .md extension is added if you omit it."`
	Content string `json:"content" jsonschema:"The note's markdown. May start with a YAML frontmatter block delimited by ---."`
	Vault   string `json:"vault,omitzero"`
}

type writeOutput struct {
	Vault string `json:"vault"`
	Path  string `json:"path"`
	Sha   string `json:"sha"`
}

func (sess *session) createNote(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, writeOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, writeOutput{}, toolErr(err)
	}
	n, err := sess.Notes.Create(ctx, sc, in.Path, in.Content)
	if err != nil {
		return nil, writeOutput{}, toolErr(err)
	}
	return nil, writeOutput{Vault: n.Vault, Path: n.Path, Sha: n.SHA256}, nil
}

type updateInput struct {
	Path    string `json:"path"`
	Content string `json:"content" jsonschema:"The note's complete new markdown. This replaces everything."`
	BaseSha string `json:"baseSha,omitzero" jsonschema:"The sha from a previous read_note. If the note has changed since, the write is refused and your content is saved to a conflict file instead of overwriting."`
	Vault   string `json:"vault,omitzero"`
}

func (sess *session) updateNote(ctx context.Context, _ *mcp.CallToolRequest, in updateInput) (*mcp.CallToolResult, writeOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, writeOutput{}, toolErr(err)
	}
	n, err := sess.Notes.Update(ctx, sc, in.Path, in.Content, in.BaseSha)
	if err != nil {
		return nil, writeOutput{}, toolErr(err)
	}
	return nil, writeOutput{Vault: n.Vault, Path: n.Path, Sha: n.SHA256}, nil
}

type editSpec struct {
	Old        string `json:"old" jsonschema:"Exact text to find, including whitespace. Must appear exactly once unless replaceAll is true; include surrounding lines to disambiguate."`
	New        string `json:"new" jsonschema:"Replacement text. Use an empty string to delete."`
	ReplaceAll bool   `json:"replaceAll,omitzero" jsonschema:"Replace every occurrence rather than requiring exactly one."`
}

type editInput struct {
	Path  string     `json:"path"`
	Edits []editSpec `json:"edits" jsonschema:"Edits applied in order. If any one fails, none are applied."`
	Vault string     `json:"vault,omitzero"`
}

func (sess *session) editNote(ctx context.Context, _ *mcp.CallToolRequest, in editInput) (*mcp.CallToolResult, writeOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, writeOutput{}, toolErr(err)
	}
	edits := make([]notes.Edit, len(in.Edits))
	for i, e := range in.Edits {
		edits[i] = notes.Edit{Old: e.Old, New: e.New, ReplaceAll: e.ReplaceAll}
	}
	n, err := sess.Notes.Edit(ctx, sc, in.Path, edits)
	if err != nil {
		return nil, writeOutput{}, toolErr(err)
	}
	return nil, writeOutput{Vault: n.Vault, Path: n.Path, Sha: n.SHA256}, nil
}

type appendInput struct {
	Path         string `json:"path"`
	Text         string `json:"text" jsonschema:"Markdown to add."`
	UnderHeading string `json:"underHeading,omitzero" jsonschema:"Append at the end of the section under this heading rather than the end of the file. Match the heading text, with or without its leading #."`
	Vault        string `json:"vault,omitzero"`
}

func (sess *session) appendNote(ctx context.Context, _ *mcp.CallToolRequest, in appendInput) (*mcp.CallToolResult, writeOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, writeOutput{}, toolErr(err)
	}
	n, err := sess.Notes.Append(ctx, sc, in.Path, in.Text, in.UnderHeading)
	if err != nil {
		return nil, writeOutput{}, toolErr(err)
	}
	return nil, writeOutput{Vault: n.Vault, Path: n.Path, Sha: n.SHA256}, nil
}

type deleteInput struct {
	Path  string `json:"path"`
	Vault string `json:"vault,omitzero"`
}

type okOutput struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitzero"`
}

func (sess *session) deleteNote(ctx context.Context, _ *mcp.CallToolRequest, in deleteInput) (*mcp.CallToolResult, okOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, okOutput{}, toolErr(err)
	}
	if err := sess.Notes.Delete(ctx, sc, in.Path); err != nil {
		return nil, okOutput{}, toolErr(err)
	}
	return nil, okOutput{OK: true, Message: "moved to trash; recoverable from .folio/trash"}, nil
}

type moveInput struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Vault string `json:"vault,omitzero"`
}

func (sess *session) moveNote(ctx context.Context, _ *mcp.CallToolRequest, in moveInput) (*mcp.CallToolResult, writeOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, writeOutput{}, toolErr(err)
	}
	n, err := sess.Notes.Move(ctx, sc, in.From, in.To)
	if err != nil {
		return nil, writeOutput{}, toolErr(err)
	}
	return nil, writeOutput{Vault: n.Vault, Path: n.Path, Sha: n.SHA256}, nil
}

type backlinksOutput struct {
	Backlinks []linkOut `json:"backlinks"`
}

func (sess *session) getBacklinks(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, backlinksOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, backlinksOutput{}, toolErr(err)
	}
	links, err := sess.Notes.Backlinks(ctx, sc, in.Path)
	if err != nil {
		return nil, backlinksOutput{}, toolErr(err)
	}
	out := backlinksOutput{Backlinks: make([]linkOut, 0, len(links))}
	for _, l := range links {
		out.Backlinks = append(out.Backlinks, linkOut{Path: l.Path, Title: l.Title})
	}
	return nil, out, nil
}

type vaultInput struct {
	Vault string `json:"vault,omitzero"`
}

type tagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

type tagsOutput struct {
	Tags []tagCount `json:"tags"`
}

func (sess *session) listTags(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, tagsOutput, error) {
	ids, err := sess.Shares.ReadableVaults(ctx, sess.user)
	if err != nil {
		return nil, tagsOutput{}, toolErr(err)
	}
	tags, err := sess.Index.Tags(ctx, ids)
	if err != nil {
		return nil, tagsOutput{}, toolErr(err)
	}
	out := tagsOutput{Tags: make([]tagCount, 0, len(tags))}
	for _, t := range tags {
		out.Tags = append(out.Tags, tagCount{Tag: t.Tag, Count: t.Count})
	}
	return nil, out, nil
}

type foldersOutput struct {
	Folders []string `json:"folders"`
}

func (sess *session) listFolders(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, foldersOutput, error) {
	ids, err := sess.Shares.ReadableVaults(ctx, sess.user)
	if err != nil {
		return nil, foldersOutput{}, toolErr(err)
	}
	folders, err := sess.Index.Folders(ctx, ids)
	if err != nil {
		return nil, foldersOutput{}, toolErr(err)
	}
	if folders == nil {
		folders = []string{}
	}
	return nil, foldersOutput{Folders: folders}, nil
}

type dailyInput struct {
	Date     string `json:"date,omitzero" jsonschema:"Date as YYYY-MM-DD. Defaults to today."`
	Create   bool   `json:"create,omitzero" jsonschema:"Create the note from a template if it does not exist. Defaults to true."`
	NoCreate bool   `json:"noCreate,omitzero" jsonschema:"Set to true to return an error instead of creating a missing daily note."`
	Vault    string `json:"vault,omitzero"`
}

func (sess *session) getDailyNote(ctx context.Context, _ *mcp.CallToolRequest, in dailyInput) (*mcp.CallToolResult, readOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, readOutput{}, toolErr(err)
	}
	day := now()
	if in.Date != "" {
		day, err = time.Parse("2006-01-02", in.Date)
		if err != nil {
			return nil, readOutput{}, fmt.Errorf("date must be YYYY-MM-DD: %w", err)
		}
	}
	n, err := sess.Notes.DailyNote(ctx, sc, day, !in.NoCreate)
	if err != nil {
		return nil, readOutput{}, toolErr(err)
	}
	return nil, readOutput{
		Vault: n.Vault, Path: n.Path, Title: n.Title, Content: n.Content,
		Tags: n.Tags, Sha: n.SHA256, Perm: string(n.Perm), OwnerLogin: n.OwnerLogin,
		Backlinks: []linkOut{},
	}, nil
}

type vaultInfo struct {
	Vault      string `json:"vault"`
	OwnerLogin string `json:"ownerLogin"`
	IsMine     bool   `json:"isMine"`
}

type vaultsOutput struct {
	Vaults []vaultInfo `json:"vaults"`
}

func (sess *session) listVaults(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, vaultsOutput, error) {
	ids, err := sess.Shares.ReadableVaults(ctx, sess.user)
	if err != nil {
		return nil, vaultsOutput{}, toolErr(err)
	}
	out := vaultsOutput{Vaults: make([]vaultInfo, 0, len(ids))}
	for _, id := range ids {
		owner, err := sess.Identity.ByVaultID(ctx, id)
		if err != nil {
			continue
		}
		out.Vaults = append(out.Vaults, vaultInfo{
			Vault: owner.VaultDir, OwnerLogin: owner.Login, IsMine: owner.ID == sess.user.ID,
		})
	}
	return nil, out, nil
}

type statsOutput struct {
	Vault string `json:"vault"`
	Notes int    `json:"notes"`
	Tags  int    `json:"tags"`
	Links int    `json:"links"`
}

func (sess *session) vaultStats(ctx context.Context, _ *mcp.CallToolRequest, in vaultInput) (*mcp.CallToolResult, statsOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, statsOutput{}, toolErr(err)
	}
	if err := sess.Shares.Check(ctx, sc.User, sc.VaultID, ".", share.Read); err != nil {
		return nil, statsOutput{}, toolErr(err)
	}
	st, err := sess.Index.VaultStats(ctx, sc.VaultID)
	if err != nil {
		return nil, statsOutput{}, toolErr(err)
	}
	return nil, statsOutput{Vault: sc.Dir, Notes: st.Notes, Tags: st.Tags, Links: st.Links}, nil
}

type shareInfo struct {
	ID           string `json:"id"`
	Vault        string `json:"vault"`
	Path         string `json:"path"`
	IsFolder     bool   `json:"isFolder"`
	OwnerLogin   string `json:"ownerLogin"`
	GranteeLogin string `json:"granteeLogin"`
	Perm         string `json:"perm"`
}

type sharesOutput struct {
	Granted      []shareInfo `json:"granted" jsonschema:"Shares this user has given to other people."`
	SharedWithMe []shareInfo `json:"sharedWithMe" jsonschema:"Notes other people have shared with this user."`
}

func (sess *session) listShares(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, sharesOutput, error) {
	mine, err := sess.Shares.SharesIMade(ctx, sess.user)
	if err != nil {
		return nil, sharesOutput{}, toolErr(err)
	}
	theirs, err := sess.Shares.SharedWithMe(ctx, sess.user)
	if err != nil {
		return nil, sharesOutput{}, toolErr(err)
	}
	return nil, sharesOutput{
		Granted:      sess.shareInfos(ctx, mine),
		SharedWithMe: sess.shareInfos(ctx, theirs),
	}, nil
}

func (sess *session) shareInfos(ctx context.Context, in []share.Share) []shareInfo {
	out := make([]shareInfo, 0, len(in))
	for _, s := range in {
		dir := ""
		if owner, err := sess.Identity.ByVaultID(ctx, s.VaultID); err == nil {
			dir = owner.VaultDir
		}
		out = append(out, shareInfo{
			ID: s.ID, Vault: dir, Path: s.Path, IsFolder: s.IsFolder,
			OwnerLogin: s.OwnerLogin, GranteeLogin: s.GranteeLogin, Perm: string(s.Perm),
		})
	}
	return out
}

type shareInput struct {
	Path     string `json:"path" jsonschema:"Note or folder in this user's own vault."`
	Grantee  string `json:"grantee" jsonschema:"The tailnet login to share with, for example alice@github. They must have used folio at least once."`
	Perm     string `json:"perm,omitzero" jsonschema:"Either read or write. Defaults to read."`
	IsFolder bool   `json:"isFolder,omitzero" jsonschema:"Share a whole folder and everything beneath it."`
}

func (sess *session) shareNote(ctx context.Context, _ *mcp.CallToolRequest, in shareInput) (*mcp.CallToolResult, shareInfo, error) {
	perm := share.Perm(in.Perm)
	if perm == "" {
		perm = share.Read
	}
	path := in.Path
	if !in.IsFolder {
		path = vaultpath.EnsureMarkdown(path)
	}
	s, err := sess.Shares.Grant(ctx, sess.user, path, in.IsFolder, in.Grantee, perm)
	if err != nil {
		return nil, shareInfo{}, toolErr(err)
	}
	return nil, sess.shareInfos(ctx, []share.Share{s})[0], nil
}

type unshareInput struct {
	ID string `json:"id" jsonschema:"The share id from list_shares or share_note."`
}

func (sess *session) unshareNote(ctx context.Context, _ *mcp.CallToolRequest, in unshareInput) (*mcp.CallToolResult, okOutput, error) {
	if err := sess.Shares.Revoke(ctx, sess.user, in.ID); err != nil {
		return nil, okOutput{}, toolErr(err)
	}
	return nil, okOutput{OK: true, Message: "share revoked"}, nil
}

type attachmentInput struct {
	Path  string `json:"path" jsonschema:"Vault-relative path to a non-markdown file, for example attachments/diagram.png"`
	Vault string `json:"vault,omitzero"`
}

type attachmentOutput struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Base64   string `json:"base64"`
	MimeType string `json:"mimeType"`
}

func (sess *session) readAttachment(ctx context.Context, _ *mcp.CallToolRequest, in attachmentInput) (*mcp.CallToolResult, attachmentOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, attachmentOutput{}, toolErr(err)
	}
	clean, err := vaultpath.Clean(in.Path)
	if err != nil {
		return nil, attachmentOutput{}, err
	}
	if err := sess.Shares.Check(ctx, sc.User, sc.VaultID, clean, share.Read); err != nil {
		return nil, attachmentOutput{}, toolErr(err)
	}
	content, n, err := sc.Vault.Read(clean)
	if err != nil {
		return nil, attachmentOutput{}, toolErr(err)
	}
	return nil, attachmentOutput{
		Path: n.Path, Size: n.Size,
		Base64:   base64.StdEncoding.EncodeToString(content),
		MimeType: mimeFor(clean),
	}, nil
}

func mimeFor(p string) string {
	switch strings.ToLower(vaultpath.Ext(p)) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "svg":
		return "image/svg+xml"
	case "pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

// plainSnippet strips the index's highlight markers. A model reading a snippet
// wants the text, not the markup.
func plainSnippet(s string) string {
	return strings.NewReplacer(
		indexHighlightOpen, "",
		indexHighlightClose, "",
	).Replace(s)
}

// --- attachments, links, and settings ---
//
// These exist because an agent is a first-class folio client, not a reader with
// a keyboard taped to it. Everything a person can do in the browser or the
// terminal, an agent can do here, through the same operations layer, so none of
// the three can drift from the other two.

type listAttachmentsInput struct {
	Vault string `json:"vault,omitzero" jsonschema:"Which vault to list. Defaults to this user's own."`
}

type attachmentInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type listAttachmentsOutput struct {
	Vault       string           `json:"vault"`
	Attachments []attachmentInfo `json:"attachments"`
}

func (sess *session) listAttachments(ctx context.Context, _ *mcp.CallToolRequest, in listAttachmentsInput) (*mcp.CallToolResult, listAttachmentsOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, listAttachmentsOutput{}, toolErr(err)
	}
	list, err := sess.Notes.ListAttachments(ctx, sc)
	if err != nil {
		return nil, listAttachmentsOutput{}, toolErr(err)
	}

	out := listAttachmentsOutput{Vault: sc.Dir, Attachments: make([]attachmentInfo, 0, len(list))}
	for _, a := range list {
		out.Attachments = append(out.Attachments, attachmentInfo{Path: a.Path, Size: a.Size})
	}
	return nil, out, nil
}

type attachInput struct {
	Base64 string `json:"base64" jsonschema:"The file's bytes, base64 encoded."`
	Name   string `json:"name,omitzero" jsonschema:"The file's name, for example diagram.png. Only the filename is used; any folder in it is ignored, because where the file goes is the user's setting. Leave it out for a generated \"Pasted image ...\" name."`
	Note   string `json:"note,omitzero" jsonschema:"The note this file is being attached to, for example Daily/2026-08-30.md. It decides the folder under the two note-relative settings, and it is the note the returned link is shortened for."`
	Vault  string `json:"vault,omitzero"`
	// MimeType only matters for a file arriving without a name, where it picks
	// the extension. Naming it explicitly beats sniffing bytes we were handed.
	MimeType string `json:"mimeType,omitzero" jsonschema:"The file's type, for example image/png. Only used to pick an extension when no name is given."`
}

type attachOutput struct {
	Vault string `json:"vault"`
	Path  string `json:"path" jsonschema:"Where the file actually landed, which may differ from the name you sent if something was already called that."`
	Size  int64  `json:"size"`
	Link  string `json:"link" jsonschema:"What to write inside brackets to reference the file: ![[link]] for an image, [[link]] for anything else."`
}

func (sess *session) attachFile(ctx context.Context, _ *mcp.CallToolRequest, in attachInput) (*mcp.CallToolResult, attachOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, attachOutput{}, toolErr(err)
	}
	// Base64 rather than raw bytes because MCP arguments are JSON. A model that
	// sends something that is not base64 gets told that, rather than having its
	// prose written to disk as a PNG.
	content, err := base64.StdEncoding.DecodeString(in.Base64)
	if err != nil {
		return nil, attachOutput{}, fmt.Errorf("base64 is not valid: %w", err)
	}
	if len(content) > maxAttachmentBytes {
		return nil, attachOutput{}, fmt.Errorf("attachment is %d bytes, over the %d byte limit", len(content), maxAttachmentBytes)
	}

	at, err := sess.Notes.Attach(ctx, sc, notes.Upload{
		Note:        in.Note,
		Name:        in.Name,
		ContentType: in.MimeType,
		Content:     content,
	})
	if err != nil {
		return nil, attachOutput{}, toolErr(err)
	}
	return nil, attachOutput{Vault: at.Vault, Path: at.Path, Size: at.Size, Link: at.Link}, nil
}

type resolveInput struct {
	Target string `json:"target" jsonschema:"What is written between the brackets, for example \"Projects/folio\", \"folio\", or \"Meeting#Actions\". Leave off the brackets and any leading exclamation mark."`
	From   string `json:"from,omitzero" jsonschema:"The note the link is written in. A bare name can resolve to different notes depending on where it sits, so pass this whenever you know it."`
	Vault  string `json:"vault,omitzero"`
}

type resolveOutput struct {
	Kind      string `json:"kind" jsonschema:"\"note\" if it points at a note, \"attachment\" if at a file, \"missing\" if at nothing yet."`
	Vault     string `json:"vault"`
	Path      string `json:"path,omitzero" jsonschema:"The note or file it resolves to."`
	Title     string `json:"title,omitzero"`
	Anchor    string `json:"anchor,omitzero"`
	Content   string `json:"content,omitzero" jsonschema:"The text this link would render, narrowed to the anchor's section when there was one. Empty for an attachment; use read_attachment for those."`
	Truncated bool   `json:"truncated,omitzero"`
}

func (sess *session) resolveLink(ctx context.Context, _ *mcp.CallToolRequest, in resolveInput) (*mcp.CallToolResult, resolveOutput, error) {
	sc, err := sess.scope(ctx, in.Vault)
	if err != nil {
		return nil, resolveOutput{}, toolErr(err)
	}
	// Tolerate a model that pasted the whole link rather than its interior. It
	// is a small kindness that costs nothing and saves a retry.
	target := strings.TrimSpace(in.Target)
	target = strings.TrimPrefix(target, "!")
	target = strings.TrimSuffix(strings.TrimPrefix(target, "[["), "]]")
	if strings.TrimSpace(target) == "" {
		return nil, resolveOutput{}, fmt.Errorf("target is empty")
	}

	em, err := sess.Notes.Embed(ctx, sc, in.From, target)
	if err != nil {
		return nil, resolveOutput{}, toolErr(err)
	}
	return nil, resolveOutput{
		Kind: string(em.Kind), Vault: em.Vault, Path: em.Path,
		Title: em.Title, Anchor: em.Anchor, Content: em.Content, Truncated: em.Truncated,
	}, nil
}

type settingsInput struct{}

type settingsOutput struct {
	AttachmentMode   string `json:"attachmentMode" jsonschema:"Where new attachments go: \"folder\" (all in one folder), \"vault\" (the vault root), \"current\" (beside the note), or \"subfolder\" (a named folder beside the note)."`
	AttachmentFolder string `json:"attachmentFolder" jsonschema:"The folder name, used by the \"folder\" and \"subfolder\" modes."`
}

func (sess *session) getSettings(ctx context.Context, _ *mcp.CallToolRequest, _ settingsInput) (*mcp.CallToolResult, settingsOutput, error) {
	p, err := sess.Notes.Prefs.Get(ctx, sess.user.ID)
	if err != nil {
		return nil, settingsOutput{}, toolErr(err)
	}
	return nil, settingsOutput{
		AttachmentMode:   string(p.Attachments.Mode),
		AttachmentFolder: p.Attachments.Folder,
	}, nil
}

type setSettingsInput struct {
	AttachmentMode   string `json:"attachmentMode" jsonschema:"One of \"folder\", \"vault\", \"current\", or \"subfolder\"."`
	AttachmentFolder string `json:"attachmentFolder,omitzero" jsonschema:"The folder name. Required by \"folder\" and \"subfolder\", ignored by the other two."`
}

func (sess *session) setSettings(ctx context.Context, _ *mcp.CallToolRequest, in setSettingsInput) (*mcp.CallToolResult, settingsOutput, error) {
	folder := in.AttachmentFolder
	if folder == "" {
		// The folder is kept across a mode change, so a caller switching to a
		// mode that does not use one must not wipe what was there.
		if current, err := sess.Notes.Prefs.Get(ctx, sess.user.ID); err == nil {
			folder = current.Attachments.Folder
		}
	}
	next := prefs.Prefs{Attachments: prefs.Attachments{
		Mode:   prefs.AttachmentMode(in.AttachmentMode),
		Folder: folder,
	}}
	if err := sess.Notes.Prefs.Set(ctx, sess.user.ID, next); err != nil {
		return nil, settingsOutput{}, toolErr(err)
	}
	return nil, settingsOutput{
		AttachmentMode:   string(next.Attachments.Mode),
		AttachmentFolder: next.Attachments.Folder,
	}, nil
}
