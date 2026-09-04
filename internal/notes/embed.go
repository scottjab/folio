package notes

import (
	"context"
	"errors"

	"github.com/scottjab/folio/internal/markdown"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/vault"
	"github.com/scottjab/folio/internal/vaultpath"
)

// EmbedKind says what an ![[embed]] turned out to point at.
type EmbedKind string

const (
	// EmbedNote is another note, whose text renders in place.
	EmbedNote EmbedKind = "note"
	// EmbedAttachment is a file. The client fetches the bytes itself and decides
	// whether it can show them.
	EmbedAttachment EmbedKind = "attachment"
	// EmbedMissing is a target that resolves to nothing. It is not an error: a
	// link to a note you have not written yet is normal, and the editor draws it
	// as an invitation rather than a failure.
	EmbedMissing EmbedKind = "missing"
)

// Embedded is what an ![[embed]] resolved to.
type Embedded struct {
	Kind   EmbedKind
	Vault  string
	Path   string
	Title  string
	Anchor string

	// Content is the embedded text, frontmatter stripped, narrowed to the
	// anchor's section when there was one. Empty for anything but a note.
	Content string

	// Truncated is set when Content was cut to the size limit, so the client can
	// say so instead of silently showing half a note.
	Truncated bool
}

// maxEmbedBytes caps how much of a note is inlined into another.
//
// An embed is a preview, not a transport: past this the reader is better served
// by following the link. It also bounds what one note can cost to render when it
// embeds a dozen others.
const maxEmbedBytes = 128 << 10

// Embed resolves an ![[target]] written inside the note at from.
//
// Resolution is [markdown.ResolveWikilink], the same rule the indexer uses, so
// an embed and the backlink it produces can never disagree about which note they
// mean. Extracting the section for a #anchor is [markdown.Section], and both
// live in the markdown package for the same reason: the browser and the terminal
// have to render the identical span of text.
func (s *Service) Embed(ctx context.Context, sc Scope, from, target string) (Embedded, error) {
	bare, anchor := vaultpath.SplitAnchor(target)
	out := Embedded{Kind: EmbedMissing, Vault: sc.Dir, Anchor: anchor}

	if from != "" {
		if clean, err := vaultpath.Clean(from); err == nil {
			from = clean
		}
	}

	entries, err := sc.Vault.List()
	if err != nil {
		return Embedded{}, err
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}

	resolved := markdown.ResolveWikilink(paths, from, bare)
	if resolved == "" {
		return out, nil
	}
	out.Path = resolved

	// A share can cover part of a vault, so a target that exists is still not
	// necessarily one this caller may see. Reporting it as missing rather than
	// forbidden is deliberate: the alternative tells the caller a note exists at
	// a path they cannot read.
	if err := s.Shares.Check(ctx, sc.User, sc.VaultID, resolved, share.Read); err != nil {
		return Embedded{Kind: EmbedMissing, Vault: sc.Dir, Anchor: anchor}, nil
	}

	if !vaultpath.IsMarkdown(resolved) {
		out.Kind = EmbedAttachment
		out.Title = vaultpath.TitleFor(resolved)
		return out, nil
	}

	content, _, err := sc.Vault.Read(resolved)
	if errors.Is(err, vault.ErrNotFound) {
		// The listing and the read raced against a delete.
		return Embedded{Kind: EmbedMissing, Vault: sc.Dir, Anchor: anchor}, nil
	}
	if err != nil {
		return Embedded{}, err
	}

	// Frontmatter is metadata about the embedded note, not part of it. Leaving
	// it in means every transclusion opens with a YAML block.
	_, body, _ := markdown.SplitFrontmatter(content)

	if anchor != "" {
		section, ok := markdown.Section(body, anchor)
		if !ok {
			// The note is there but the heading is not. Saying so beats
			// silently inlining the whole note the reader did not ask for.
			return Embedded{Kind: EmbedMissing, Vault: sc.Dir, Path: resolved, Anchor: anchor}, nil
		}
		body = section
	}

	if len(body) > maxEmbedBytes {
		body = body[:maxEmbedBytes]
		out.Truncated = true
	}

	out.Kind = EmbedNote
	out.Content = string(body)
	out.Title = vaultpath.TitleFor(resolved)
	if e, err := s.Index.Get(ctx, sc.VaultID, resolved); err == nil && e.Title != "" {
		out.Title = e.Title
	}
	return out, nil
}
