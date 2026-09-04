package notes

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/scottjab/folio/internal/markdown"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/vault"
	"github.com/scottjab/folio/internal/vaultpath"
)

// ErrNotAttachment means someone tried to upload markdown as a binary. Notes go
// through Create and Update so they are parsed, indexed, and given an id.
var ErrNotAttachment = errors.New("markdown belongs in a note, not an attachment")

// maxNameAttempts bounds the search for a free filename. Reaching it means a
// folder holds thousands of files differing only by suffix, which is a runaway
// loop somewhere rather than a real vault.
const maxNameAttempts = 10000

// pastedImagePrefix is what an image arriving without a filename is called.
// Obsidian's format exactly, so a vault opened in both does not end up with two
// naming schemes side by side.
const pastedImagePrefix = "Pasted image"

// Upload is a file on its way into a vault.
type Upload struct {
	// Note is the note the file is being inserted into. It is what the two
	// note-relative attachment modes are relative to, and it is the vantage
	// point the returned Link is shortened from. Empty is allowed: it means the
	// vault root for those modes.
	Note string

	// Name is the file's original name, and is empty for a paste. Only the
	// basename is used, so a browser that hands over a full path cannot decide
	// what folder the file lands in.
	Name string

	// ContentType names the file's type, and is only consulted to pick an
	// extension when Name is empty.
	ContentType string

	Content []byte
}

// Attachment is a stored file.
type Attachment struct {
	Vault  string
	Path   string
	Size   int64
	SHA256 string

	// Link is what to write inside [[ ]] to reference this file from Upload.Note:
	// the bare filename when that is unambiguous in the vault, the full path when
	// it is not. This is the same "shortest path when possible" rule Obsidian
	// uses, and it is computed here rather than in each client so the browser and
	// the terminal cannot disagree about it.
	Link string
}

// Attach stores an uploaded file and reports where it went.
//
// The destination is not the caller's to choose. It comes from the user's
// attachment preference applied to up.Note, which is what keeps a drop in the
// browser and an attach in the terminal filing things the same way. A name
// already in use is suffixed rather than overwritten, so dropping two photos
// called IMG_0001.jpg keeps both.
func (s *Service) Attach(ctx context.Context, sc Scope, up Upload) (Attachment, error) {
	p, err := s.Prefs.Get(ctx, sc.User.ID)
	if err != nil {
		return Attachment{}, err
	}

	notePath := up.Note
	if notePath != "" {
		if notePath, err = vaultpath.Clean(notePath); err != nil {
			return Attachment{}, fmt.Errorf("note: %w", err)
		}
	}

	name := attachmentName(up.Name, up.ContentType, time.Now())
	if vaultpath.IsMarkdown(name) {
		return Attachment{}, fmt.Errorf("%s: %w", name, ErrNotAttachment)
	}

	dir := p.Attachments.Dir(notePath)
	stored, err := s.writeUnique(ctx, sc, dir, name, up.Content)
	if err != nil {
		return Attachment{}, err
	}

	return Attachment{
		Vault:  sc.Dir,
		Path:   stored.Path,
		Size:   stored.Size,
		SHA256: stored.SHA256,
		Link:   s.linkFor(sc, notePath, stored.Path),
	}, nil
}

// writeUnique writes content into dir under name, suffixing the name until it
// finds one nothing is using.
//
// The existence check and the write are not one atomic step, so the loop leans
// on Vault.Create refusing to overwrite: a name that was free a moment ago and
// is taken by the time we write comes back as ErrExists and we simply try the
// next one. Checking first is still worth it, because without it every upload
// into a folder of n files would attempt n writes.
func (s *Service) writeUnique(ctx context.Context, sc Scope, dir, name string, content []byte) (vault.Note, error) {
	stem, ext := splitExt(name)
	for i := range maxNameAttempts {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s %d%s", stem, i, ext)
		}
		clean, err := vaultpath.Clean(path.Join(dir, candidate))
		if err != nil {
			return vault.Note{}, err
		}
		// Permission is checked per candidate rather than once for the folder,
		// because a share can cover a single path.
		if err := s.Shares.Check(ctx, sc.User, sc.VaultID, clean, share.Write); err != nil {
			return vault.Note{}, err
		}
		if sc.Vault.Exists(clean) {
			continue
		}
		n, err := sc.Vault.Create(clean, content)
		if errors.Is(err, vault.ErrExists) {
			continue // lost the race; the next suffix is still free
		}
		if err != nil {
			return vault.Note{}, err
		}
		return n, nil
	}
	return vault.Note{}, fmt.Errorf("no free name for %q in %q after %d tries", name, dir, maxNameAttempts)
}

// linkFor returns the shortest wikilink target that still resolves to p when
// written in notePath. It falls back to the full path, which always resolves.
func (s *Service) linkFor(sc Scope, notePath, p string) string {
	entries, err := sc.Vault.List()
	if err != nil {
		return p
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	if base := path.Base(p); markdown.ResolveWikilink(paths, notePath, base) == p {
		return base
	}
	return p
}

// ListAttachments returns every non-markdown file the caller may read in a
// vault. The editor needs it to tell an embed that points at a real image from
// one that points at nothing, which it cannot do from the note listing alone.
func (s *Service) ListAttachments(ctx context.Context, sc Scope) ([]Attachment, error) {
	entries, err := sc.Vault.List()
	if err != nil {
		return nil, err
	}
	out := make([]Attachment, 0, len(entries))
	for _, e := range entries {
		if vaultpath.IsMarkdown(e.Path) {
			continue
		}
		if err := s.Shares.Check(ctx, sc.User, sc.VaultID, e.Path, share.Read); err != nil {
			continue
		}
		out = append(out, Attachment{Vault: sc.Dir, Path: e.Path, Size: e.Size})
	}
	return out, nil
}

// attachmentName decides what to store a file as.
//
// A file that arrived with a name keeps it, minus any directory part. A paste
// has no name, so it gets one built from the clock, which is what Obsidian does
// and is far more useful than "image.png" repeated forty times.
func attachmentName(name, contentType string, now time.Time) string {
	name = strings.TrimSpace(path.Base(strings.ReplaceAll(name, `\`, "/")))
	// A leading dot would upload happily and then vanish: hidden paths are never
	// listed and never indexed, so the file would exist, be unreachable from any
	// listing, and look to the user like the upload silently failed. Dropping the
	// dots keeps the name recognisable and the file visible.
	name = strings.TrimLeft(name, ".")
	if name != "" && name != "/" {
		return name
	}
	return fmt.Sprintf("%s %s%s", pastedImagePrefix, now.Format("20060102150405"), extForType(contentType))
}

// pastedExts maps the types a clipboard actually produces to an extension.
//
// mime.ExtensionsByType is not usable here: it returns every registered
// extension in sorted order, so image/jpeg yields ".jpe" first, and pasting a
// screenshot would produce a file no other tool wants to open.
var pastedExts = map[string]string{
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"image/svg+xml":   ".svg",
	"image/avif":      ".avif",
	"image/bmp":       ".bmp",
	"image/tiff":      ".tiff",
	"application/pdf": ".pdf",
}

func extForType(contentType string) string {
	base, _, _ := strings.Cut(contentType, ";")
	if ext, ok := pastedExts[strings.ToLower(strings.TrimSpace(base))]; ok {
		return ext
	}
	return ".bin"
}

// splitExt divides "photo.tar.gz" into "photo.tar" and ".gz", and a name with no
// extension into itself and "". The suffix number goes between the two, so
// "photo 1.gz" keeps opening in the right application.
func splitExt(name string) (stem, ext string) {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i], name[i:]
	}
	return name, ""
}
