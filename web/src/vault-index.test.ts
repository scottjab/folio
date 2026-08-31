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
