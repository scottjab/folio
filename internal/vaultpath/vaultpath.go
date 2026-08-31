// Package vaultpath owns every rule about what a path inside a vault may look
// like. Nothing else in tsnotes is allowed to hand-roll path handling: if it
// touches a vault-relative path, it goes through here first.
//
// Paths are always vault-relative, always forward-slash separated, always NFC
// normalized, and can never escape the vault. The last guarantee is belt and
// braces with os.Root over in internal/vault, which enforces confinement at the
// syscall layer even if something here were wrong.
package vaultpath

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// reservedDir is our own sidecar directory. Callers may never address it
// directly; internal/vault reaches it through unexported helpers instead.
const reservedDir = ".tsnotes"

// ErrInvalidPath is the base for every rejection out of [Clean]. Callers can
// test with errors.Is and still surface the specific reason to the user.
var ErrInvalidPath = errors.New("invalid vault path")

// markdownExts are the extensions we treat as notes. Everything else in a vault
// is an attachment.
var markdownExts = map[string]bool{"md": true, "markdown": true}

// Clean validates p and returns it in canonical form: vault-relative, forward
// slashes, no "." or ".." segments, NFC normalized, no trailing slash.
func Clean(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidPath)
	}
	for _, r := range p {
		if r == 0 || (unicode.IsControl(r) && r != '\t') {
			return "", fmt.Errorf("%w: control character %q", ErrInvalidPath, r)
		}
	}

	// Windows-style separators reach us from clients and from vaults synced off
	// other machines. Normalize before path.Clean, which only knows "/".
	p = strings.ReplaceAll(p, `\`, "/")
	p = norm.NFC.String(p)

	cleaned := path.Clean("/" + p)
	cleaned = strings.TrimPrefix(cleaned, "/")

	if cleaned == "" || cleaned == "." {
		return "", fmt.Errorf("%w: resolves to the vault root", ErrInvalidPath)
	}
	// path.Clean("/"+p) can never leave a ".." behind, but an input that tried
	// to escape resolves to something shorter than it should. Catch the attempt
	// explicitly so the caller gets a real error instead of a silent rewrite.
	if hasDotDot(p) {
		return "", fmt.Errorf("%w: %q escapes the vault", ErrInvalidPath, p)
	}
	if first, _, _ := strings.Cut(cleaned, "/"); first == reservedDir {
		return "", fmt.Errorf("%w: %s is reserved", ErrInvalidPath, reservedDir)
	}
	return cleaned, nil
}

// hasDotDot reports whether any segment of p is exactly "..".
func hasDotDot(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// Ext returns the lowercased extension of p without the dot, or "" if it has none.
func Ext(p string) string {
	base := path.Base(p)
	name, ext, found := strings.CutLast(base, ".")
	if !found || name == "" {
		return ""
	}
	return strings.ToLower(ext)
}

// IsMarkdown reports whether p names a note rather than an attachment.
func IsMarkdown(p string) bool { return markdownExts[Ext(p)] }

// EnsureMarkdown appends ".md" unless p already carries a markdown extension.
func EnsureMarkdown(p string) string {
	if IsMarkdown(p) {
		return p
	}
	return p + ".md"
}

// TitleFor derives a fallback display title from p: the basename with any
// markdown extension removed. Frontmatter and the first H1 both outrank this;
// it is what we use when a note offers neither.
func TitleFor(p string) string {
	base := path.Base(p)
	if name, ext, found := strings.CutLast(base, "."); found && name != "" && markdownExts[strings.ToLower(ext)] {
		return name
	}
	return base
}

// SplitAnchor separates a link target from its heading anchor. The first "#"
// wins, so a heading containing "#" survives in the anchor.
func SplitAnchor(target string) (p, anchor string) {
	p, anchor, _ = strings.Cut(target, "#")
	return p, anchor
}

// Dir returns the parent folder of p, or "" when p sits at the vault root.
func Dir(p string) string {
	d := path.Dir(p)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// Within reports whether p sits inside folder. The empty folder means the vault
// root and therefore contains everything. Sibling folders that share a prefix
// ("Projects" vs "Projects2") do not match, which is the whole point of having
// this instead of strings.HasPrefix at each call site.
func Within(folder, p string) bool {
	if folder == "" {
		return true
	}
	return p == folder || strings.HasPrefix(p, folder+"/")
}

// IsHidden reports whether any segment of p starts with a dot. Hidden paths are
// preserved on disk (Obsidian keeps real state in .obsidian) but never indexed
// and never served.
func IsHidden(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// Slug turns an arbitrary identifier, in practice a tailnet login like
// "scottjab@github", into a single filesystem-safe path segment. It is
// deliberately lossy and deliberately idempotent: Slug(Slug(x)) == Slug(x).
//
// Dots survive so "alice@example.com" stays readable, but runs of them collapse
// so no slug can ever be "..".
func Slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(norm.NFC.String(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()

	// Collapse dot runs before dash runs: "weird/../slashes" is "weird-..-slashes"
	// at this point and has to end up as "weird-slashes".
	out = collapse(out, '.')
	out = collapse(out, '-')
	out = strings.Trim(out, "-.")

	if out == "" {
		return "unnamed"
	}
	return out
}

// collapse rewrites runs of two or more c into a single '-'.
func collapse(s string, c byte) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != c {
			b.WriteByte(s[i])
			continue
		}
		j := i
		for j < len(s) && s[j] == c {
			j++
		}
		if j-i == 1 {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
		i = j - 1
	}
	return b.String()
}
