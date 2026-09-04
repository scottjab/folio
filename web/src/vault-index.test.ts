import { beforeEach, describe, expect, it } from "vitest";
import { basename, dirname, isImagePath, VaultIndex } from "./vault-index";

describe("VaultIndex.resolve", () => {
  let idx: VaultIndex;

  beforeEach(() => {
    idx = new VaultIndex();
    idx.replace(
      [
        "Projects/folio.md",
        "Archive/folio.md",
        "Notes/unique.md",
        "Daily/2026-08-30.md",
        "attachments/diagram.png",
      ].map((path) => ({ vault: "me", path, title: basename(path), tags: [] })),
    );
  });

  it("prefers an exact path", () => {
    expect(idx.resolve("Projects/folio.md")).toBe("Projects/folio.md");
    expect(idx.resolve("Projects/folio")).toBe("Projects/folio.md");
  });

  it("resolves a unique basename from anywhere", () => {
    expect(idx.resolve("unique", "Daily/2026-08-30.md")).toBe("Notes/unique.md");
  });

  it("prefers the linking note's own folder when a basename is ambiguous", () => {
    // This mirrors internal/markdown.ResolveWikilink on the server. If the two
    // disagreed, a link would render as broken and then resolve on save.
    expect(idx.resolve("folio", "Archive/y.md")).toBe("Archive/folio.md");
    expect(idx.resolve("folio", "Projects/y.md")).toBe("Projects/folio.md");
  });

  it("is deterministic when nothing else breaks the tie", () => {
    const first = idx.resolve("folio", "Elsewhere/y.md");
    for (let i = 0; i < 5; i++) {
      expect(idx.resolve("folio", "Elsewhere/y.md")).toBe(first);
    }
  });

  it("resolves attachments without adding .md", () => {
    expect(idx.resolve("attachments/diagram.png")).toBe("attachments/diagram.png");
  });

  it("returns null for a dangling link", () => {
    expect(idx.resolve("Nope")).toBeNull();
    expect(idx.has("Nope")).toBe(false);
    expect(idx.has("unique")).toBe(true);
  });

  it("ignores whitespace and a leading slash", () => {
    expect(idx.resolve("  Projects/folio  ")).toBe("Projects/folio.md");
    expect(idx.resolve("/Projects/folio")).toBe("Projects/folio.md");
  });

  it("treats an empty target as dangling rather than throwing", () => {
    expect(idx.resolve("")).toBeNull();
    expect(idx.resolve("   ")).toBeNull();
  });

  it("matches case-insensitively, as Obsidian does", () => {
    expect(idx.resolve("projects/FOLIO")).toBe("Projects/folio.md");
  });
});

describe("VaultIndex.tags", () => {
  it("collects and sorts every tag", () => {
    const idx = new VaultIndex();
    idx.replace([
      { vault: "me", path: "a.md", title: "A", tags: ["go", "daily"] },
      { vault: "me", path: "b.md", title: "B", tags: ["go", "work"] },
    ]);
    expect(idx.tags()).toEqual(["daily", "go", "work"]);
  });
});

describe("path helpers", () => {
  it("splits paths", () => {
    expect(basename("a/b/c.md")).toBe("c.md");
    expect(basename("c.md")).toBe("c.md");
    expect(dirname("a/b/c.md")).toBe("a/b");
    expect(dirname("c.md")).toBe("");
  });

  it("recognises renderable images", () => {
    for (const p of ["a.png", "a.JPG", "x/y.jpeg", "a.svg", "a.webp"]) {
      expect(isImagePath(p)).toBe(true);
    }
    for (const p of ["a.md", "a.pdf", "a", "a.png.md"]) {
      expect(isImagePath(p)).toBe(false);
    }
  });
});

describe("VaultIndex.shortest", () => {
  let idx: VaultIndex;

  beforeEach(() => {
    idx = new VaultIndex();
    idx.replace(
      ["Projects/folio.md", "Archive/folio.md", "Notes/unique.md"].map((path) => ({
        vault: "me",
        path,
        title: basename(path),
        tags: [],
      })),
      ["attachments/diagram.png", "Daily/shot.png", "attachments/shot.png"],
    );
  });

  it("uses the bare name when it is unambiguous", () => {
    // Obsidian's "shortest path when possible", which is what makes a vault of
    // links readable instead of a wall of folder names.
    expect(idx.shortest("Notes/unique.md")).toBe("unique");
    expect(idx.shortest("attachments/diagram.png")).toBe("attachments/diagram.png".split("/")[1]);
  });

  it("falls back to the full path when the bare name would land elsewhere", () => {
    // Two notes are called folio.md. The question is not "is the name unique"
    // but "does the bare name resolve back to this note", because resolution
    // breaks the tie deterministically. From an unrelated folder [[folio]] wins
    // for Archive/folio.md, so that one keeps the short form and Projects does
    // not: writing [[folio]] there would quietly point at the other note.
    expect(idx.resolve("folio", "Elsewhere/y.md")).toBe("Archive/folio.md");
    expect(idx.shortest("Archive/folio.md", "Elsewhere/y.md")).toBe("folio");
    expect(idx.shortest("Projects/folio.md", "Elsewhere/y.md")).toBe("Projects/folio");

    // Same rule for attachments, which keep their extension.
    expect(idx.shortest("attachments/shot.png", "Elsewhere/y.md")).toBe("attachments/shot.png");
    expect(idx.shortest("Daily/shot.png", "Elsewhere/y.md")).toBe("shot.png");
  });

  it("keeps the short form when the linking note's own folder decides it", () => {
    // Resolution prefers the linking note's folder, so from Archive the bare
    // name really does land on Archive/folio.md.
    expect(idx.shortest("Archive/folio.md", "Archive/y.md")).toBe("folio");
  });

  it("drops .md from notes but keeps an attachment's extension", () => {
    expect(idx.shortest("Notes/unique.md")).toBe("unique");
    expect(idx.shortest("Daily/shot.png", "Daily/y.md")).toBe("shot.png");
  });
});

describe("VaultIndex with attachments", () => {
  it("resolves an embed written in its short form", () => {
    // The bug this covers: attachments were never in the index, so
    // ![[diagram.png]] dangled and the editor asked for a diagram.png at the
    // vault root while the file sat in attachments/.
    const idx = new VaultIndex();
    idx.replace([{ vault: "me", path: "n.md", title: "n", tags: [] }], [
      "attachments/diagram.png",
    ]);
    expect(idx.resolve("diagram.png")).toBe("attachments/diagram.png");
    expect(idx.has("diagram.png")).toBe(true);
  });

  it("keeps attachments out of the note completions", () => {
    const idx = new VaultIndex();
    idx.replace([{ vault: "me", path: "n.md", title: "n", tags: [] }], ["a.png"]);
    expect(idx.all().map((n) => n.path)).toEqual(["n.md"]);
    expect(idx.attachments()).toEqual(["a.png"]);
  });
});

describe("VaultIndex tie-breaking matches the server", () => {
  it("orders by code unit, not by locale collation", () => {
    // internal/markdown.ResolveWikilink breaks this tie with Go's byte
    // comparison, where "Daily" sorts before "attachments" because 'D' is 68
    // and 'a' is 97. localeCompare folds case and answers the other way, which
    // would point the same [[shot.png]] at two different files depending on
    // whether the editor or the indexer was asked.
    const idx = new VaultIndex();
    idx.replace([{ vault: "me", path: "n.md", title: "n", tags: [] }], [
      "attachments/shot.png",
      "Daily/shot.png",
    ]);
    expect(idx.resolve("shot.png", "Elsewhere/y.md")).toBe("Daily/shot.png");
  });
});
