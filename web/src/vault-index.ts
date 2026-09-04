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
  private files: string[] = [];
  private byPath = new Map<string, IndexedNote>();
  private byBasename = new Map<string, IndexedNote[]>();

  /**
   * Replaces what the index knows.
   *
   * Attachments go into the same lookup tables as notes, because ![[diagram.png]]
   * resolves by exactly the rule [[Some Note]] does. Keeping them out is what
   * made a shortest-form embed dangle: the file was really at
   * attachments/diagram.png, the index had never heard of it, and the editor
   * asked the server for a diagram.png at the vault root.
   */
  replace(notes: IndexedNote[], files: string[] = []) {
    this.notes = notes;
    this.files = files;
    this.byPath = new Map();
    this.byBasename = new Map();

    const add = (n: IndexedNote) => {
      this.byPath.set(n.path.toLowerCase(), n);
      const base = basename(n.path).toLowerCase();
      const list = this.byBasename.get(base) ?? [];
      list.push(n);
      this.byBasename.set(base, list);
    };
    for (const n of notes) add(n);
    const vault = notes[0]?.vault ?? "";
    for (const p of files) add({ vault, path: p, title: basename(p), tags: [] });
  }

  /** Just the notes, which is what `[[` completion should offer. */
  all(): IndexedNote[] {
    return this.notes;
  }

  /** Just the attachments. */
  attachments(): string[] {
    return this.files;
  }

  /**
   * Returns the shortest form of path that still resolves back to it, which is
   * what Obsidian writes when it inserts a link: the bare name when it is
   * unambiguous in the vault, the full path when it is not.
   *
   * Notes lose their .md and attachments keep their extension, because that is
   * what each looks like inside brackets.
   */
  shortest(path: string, fromPath = ""): string {
    const bare = (p: string) => (p.endsWith(".md") ? p.slice(0, -3) : p);
    const base = basename(path);
    if (this.resolve(base, fromPath) === path) return bare(base);
    return bare(path);
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

    // Otherwise the shallowest, then by code unit, so the answer is stable.
    //
    // Not localeCompare: the server breaks this same tie with Go's byte
    // comparison, where every uppercase letter sorts before every lowercase
    // one, while ICU collation folds case and orders "attachments" before
    // "Daily". That disagreement is invisible until two folders differ only in
    // case at the deciding character, and then the editor and the indexer point
    // the same link at two different files.
    return [...candidates].sort(
      (a, b) => depth(a.path) - depth(b.path) || compareBytes(a.path, b.path),
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

/**
 * Orders two paths the way Go's `cmp.Compare` does, which is what the server
 * uses to break a resolution tie. Both sides have to agree or a wikilink means
 * different things in the editor and in the index.
 */
function compareBytes(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** True if a path names an image the editor can render inline. */
export function isImagePath(p: string): boolean {
  return /\.(png|jpe?g|gif|webp|svg|avif)$/i.test(p);
}
