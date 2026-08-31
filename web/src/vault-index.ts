// A client-side view of what is in the vault, used for two things the editor
// needs to answer instantly: whether a wikilink resolves, and what to offer as
// completions when you type `[[`.
//
// Resolution mirrors the server's rule in internal/markdown so a link is not
// drawn as broken in the editor and then resolved on save, or the reverse.

export interface IndexedNote {
  vault: string;
  path: string;
  title: string;
  tags: string[];
}

export class VaultIndex {
  private notes: IndexedNote[] = [];
  private byPath = new Map<string, IndexedNote>();
  private byBasename = new Map<string, IndexedNote[]>();

  replace(notes: IndexedNote[]) {
    this.notes = notes;
    this.byPath = new Map();
    this.byBasename = new Map();
    for (const n of notes) {
      this.byPath.set(n.path.toLowerCase(), n);
      const base = basename(n.path).toLowerCase();
      const list = this.byBasename.get(base) ?? [];
      list.push(n);
      this.byBasename.set(base, list);
    }
  }

  all(): IndexedNote[] {
    return this.notes;
  }

  tags(): string[] {
    const seen = new Set<string>();
    for (const n of this.notes) for (const t of n.tags) seen.add(t);
    return [...seen].sort((a, b) => a.localeCompare(b));
  }

  /**
   * Resolves a wikilink target to a real path, or null if it dangles.
   *
   * Tries an exact path, then the same path with .md, then a unique basename,
   * preferring a candidate in the linking note's own folder.
   */
  resolve(target: string, fromPath = ""): string | null {
    const clean = target.trim().replace(/^\/+/, "");
    if (!clean) return null;

    const exact = this.byPath.get(clean.toLowerCase());
    if (exact) return exact.path;

    const withExt = this.byPath.get((clean + ".md").toLowerCase());
    if (withExt) return withExt.path;

    const candidates = [
      ...(this.byBasename.get(basename(clean).toLowerCase()) ?? []),
      ...(this.byBasename.get((basename(clean) + ".md").toLowerCase()) ?? []),
    ];
    if (candidates.length === 0) return null;
    if (candidates.length === 1) return candidates[0].path;

    const fromDir = dirname(fromPath);
    const sameFolder = candidates.find((c) => dirname(c.path) === fromDir);
    if (sameFolder) return sameFolder.path;

    // Otherwise the shallowest, then alphabetical, so the answer is stable.
    return [...candidates].sort(
      (a, b) => depth(a.path) - depth(b.path) || a.path.localeCompare(b.path),
    )[0].path;
  }

  has(target: string, fromPath = ""): boolean {
    return this.resolve(target, fromPath) !== null;
  }
}

export function basename(p: string): string {
  const i = p.lastIndexOf("/");
  return i < 0 ? p : p.slice(i + 1);
}

export function dirname(p: string): string {
  const i = p.lastIndexOf("/");
  return i < 0 ? "" : p.slice(0, i);
}

function depth(p: string): number {
  return p.split("/").length;
}

/** True if a path names an image the editor can render inline. */
export function isImagePath(p: string): boolean {
  return /\.(png|jpe?g|gif|webp|svg|avif)$/i.test(p);
}
