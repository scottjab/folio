import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { Editor } from "./editor";
import type { LivePreviewHandlers } from "./livepreview";

/**
 * Mounts a real editor.
 *
 * These tests exist because the app failed as a blank page once: markdown-ext
 * threw while loading, every import of it failed, and nothing rendered. Unit
 * tests over pure helpers cannot see that. Constructing the editor for real can.
 */
const mounted: Editor[] = [];

function mount(doc = "", handlers: Partial<LivePreviewHandlers> = {}) {
  const parent = document.createElement("div");
  document.body.append(parent);

  const editor = new Editor({
    parent,
    onChange: () => {},
    onSaveRequest: () => {},
    completeNotes: () => [{ path: "Projects/folio.md", title: "folio", insert: "folio" }],
    completeTags: () => ["go", "daily"],
    openLink: () => {},
    openTag: () => {},
    isResolved: () => true,
    resolveEmbed: () => null,
    // Nothing transcludes unless a test says so; "missing" is the quiet answer.
    loadEmbed: async () => ({ kind: "missing" as const }),
    ...handlers,
  });
  mounted.push(editor);
  editor.setContent(doc);
  return { editor, parent };
}

// Tearing the editors down stops their pending measurement callbacks, which
// would otherwise run after the test that created them has finished.
afterEach(() => {
  for (const e of mounted.splice(0)) {
    try {
      e.destroy();
    } catch {
      // Already destroyed by the test itself.
    }
  }
  document.body.innerHTML = "";
});

/** The rendered text of the editor's content area. */
function rendered(parent: HTMLElement): string {
  return parent.querySelector(".cm-content")?.textContent ?? "";
}

describe("Editor", () => {
  beforeEach(() => {
    document.body.innerHTML = "";
  });

  it("constructs and renders without throwing", () => {
    const { parent } = mount("# Hello\n\nSome text.\n");
    expect(parent.querySelector(".cm-editor")).not.toBeNull();
    expect(rendered(parent)).toContain("Hello");
  });

  it("round-trips content byte for byte", () => {
    // The whole design rests on the buffer being the file. Any rewriting here
    // would show up as a diff on every save.
    const doc = "---\ntags: [a]\n---\n# H\n\ntrailing spaces   \n\n\ttab\n\n- [ ] task\n";
    const { editor } = mount(doc);
    expect(editor.content()).toBe(doc);
  });

  it("starts in live preview and toggles to source and back", () => {
    const { editor } = mount("# Hello\n");
    expect(editor.currentMode()).toBe("preview");

    editor.setMode("source");
    expect(editor.currentMode()).toBe("source");

    editor.setMode("preview");
    expect(editor.currentMode()).toBe("preview");
  });

  it("does not change the document when the mode changes", () => {
    const doc = "# Hello\n\n**bold** and [[a link]] and #tag\n";
    const { editor } = mount(doc);

    editor.setMode("source");
    expect(editor.content()).toBe(doc);
    editor.setMode("preview");
    expect(editor.content()).toBe(doc);
  });

  it("shows the raw markers in source mode", () => {
    const { editor, parent } = mount("# Heading\n\n**bold**\n");
    editor.setMode("source");
    const text = rendered(parent);
    expect(text).toContain("#");
    expect(text).toContain("**");
  });

  it("inserts at the cursor", () => {
    const { editor } = mount("");
    editor.insertAtCursor("![[diagram.png]]");
    expect(editor.content()).toBe("![[diagram.png]]");
  });

  it("survives being destroyed", () => {
    const { editor } = mount("# x\n");
    expect(() => editor.destroy()).not.toThrow();
  });

  it("handles an empty document", () => {
    const { editor, parent } = mount("");
    expect(editor.content()).toBe("");
    expect(parent.querySelector(".cm-editor")).not.toBeNull();
  });
});

describe("live preview rendering", () => {
  beforeEach(() => {
    document.body.innerHTML = "";
  });

  it("hides heading hashes when the cursor is elsewhere", () => {
    // setContent puts the cursor at position 0, which is on the heading line,
    // so the heading reveals itself. A second line is where the effect shows.
    const { editor, parent } = mount("intro\n\n# Heading\n");
    void editor;
    const text = rendered(parent);
    expect(text).toContain("Heading");
    expect(text).not.toContain("# Heading");
  });

  it("renders a wikilink as its label", () => {
    const { parent } = mount("intro\n\nSee [[Projects/folio|the project]].\n");
    const link = parent.querySelector(".cm-fol-wikilink");
    expect(link).not.toBeNull();
    expect(link?.textContent).toBe("the project");
  });

  it("marks an unresolved wikilink differently", () => {
    const { parent } = mount("intro\n\nSee [[Nope]].\n", { isResolved: () => false });
    expect(parent.querySelector(".cm-fol-wikilink.cm-fol-unresolved")).not.toBeNull();
  });

  it("renders a tag as a pill", () => {
    const { parent } = mount("intro\n\nTagged #go127 here.\n");
    const tag = parent.querySelector(".cm-fol-tag");
    expect(tag?.textContent).toBe("#go127");
  });

  it("renders a task as a real checkbox", () => {
    const { parent } = mount("intro\n\n- [x] done\n");
    const box = parent.querySelector<HTMLInputElement>("input.cm-fol-task");
    expect(box).not.toBeNull();
    expect(box?.checked).toBe(true);
  });

  it("ticking a checkbox rewrites only that character", () => {
    const { editor, parent } = mount("intro\n\n- [ ] a task\n");
    const box = parent.querySelector<HTMLInputElement>("input.cm-fol-task")!;
    box.checked = true;
    box.dispatchEvent(new Event("change"));
    expect(editor.content()).toBe("intro\n\n- [x] a task\n");
  });

  it("keeps fenced code visible, as Obsidian does", () => {
    const { parent } = mount("intro\n\n```go\nfunc main() {}\n```\n");
    expect(rendered(parent)).toContain("func main() {}");
  });

  it("does not render a wikilink inside code", () => {
    const { parent } = mount("intro\n\n```\n[[not a link]]\n```\n");
    expect(parent.querySelector(".cm-fol-wikilink")).toBeNull();
  });

  it("stops rendering entirely in source mode", () => {
    const { editor, parent } = mount("intro\n\nSee [[Target]] and #tag.\n");
    expect(parent.querySelector(".cm-fol-wikilink")).not.toBeNull();

    editor.setMode("source");
    expect(parent.querySelector(".cm-fol-wikilink")).toBeNull();
    expect(parent.querySelector(".cm-fol-tag")).toBeNull();
    expect(rendered(parent)).toContain("[[Target]]");
  });

  it("clicking a wikilink reports the target", () => {
    const opened: Array<[string, string]> = [];
    const { parent } = mount("intro\n\nSee [[Projects/folio#Design]].\n", {
      openLink: (target, anchor) => opened.push([target, anchor]),
    });
    parent.querySelector<HTMLElement>(".cm-fol-wikilink")!.dispatchEvent(
      new MouseEvent("mousedown", { bubbles: true }),
    );
    expect(opened).toEqual([["Projects/folio", "Design"]]);
  });

  it("clicking a tag reports it", () => {
    const opened: string[] = [];
    const { parent } = mount("intro\n\nTagged #go127.\n", { openTag: (t) => opened.push(t) });
    parent.querySelector<HTMLElement>(".cm-fol-tag")!.dispatchEvent(
      new MouseEvent("mousedown", { bubbles: true }),
    );
    expect(opened).toEqual(["go127"]);
  });

  it("renders an image embed when the target resolves to one", () => {
    const { parent } = mount("intro\n\n![[diagram.png]]\n", {
      resolveEmbed: (t) => `/api/vaults/me/attachments/${t}`,
    });
    const img = parent.querySelector<HTMLImageElement>(".cm-fol-embed img");
    expect(img?.getAttribute("src")).toBe("/api/vaults/me/attachments/diagram.png");
  });

  it("sizes an image embed from the alias, Obsidian style", () => {
    const { parent } = mount("intro\n\n![[diagram.png|300x200]]\n", {
      resolveEmbed: (t) => `/api/vaults/me/attachments/${t}`,
    });
    const img = parent.querySelector<HTMLImageElement>(".cm-fol-embed img")!;
    expect(img.getAttribute("width")).toBe("300");
    expect(img.getAttribute("height")).toBe("200");
  });

  it("treats a non-numeric alias as alt text, not a size", () => {
    const { parent } = mount("intro\n\n![[diagram.png|a diagram]]\n", {
      resolveEmbed: (t) => `/api/vaults/me/attachments/${t}`,
    });
    const img = parent.querySelector<HTMLImageElement>(".cm-fol-embed img")!;
    expect(img.hasAttribute("width")).toBe(false);
  });

  it("transcludes an embedded note", async () => {
    const { parent } = mount("intro\n\n![[Some Note]]\n", {
      resolveEmbed: () => null, // not an image, so it is a note to transclude
      loadEmbed: async () => ({
        kind: "note" as const,
        path: "Some Note.md",
        title: "Some Note",
        content: "# Heading\n\nEmbedded body.\n",
      }),
    });
    await flush();

    const block = parent.querySelector(".cm-fol-transclusion")!;
    expect(block.querySelector(".cm-fol-transclusion-head")?.textContent).toBe("Some Note");
    // The embedded text goes through the same renderer, so its heading is a
    // rendered heading rather than a line starting with a hash.
    expect(block.textContent).toContain("Embedded body.");
    expect(block.querySelector(".cm-fol-h1")).not.toBeNull();
  });

  it("says so rather than expanding a note that embeds itself", async () => {
    const { parent } = mount("intro\n\n![[Self]]\n", {
      resolveEmbed: () => null,
      // The embedded note embeds itself, which is the shape that would recurse.
      loadEmbed: async () => ({
        kind: "note" as const,
        path: "Self.md",
        content: "loop\n\n![[Self]]\n",
      }),
    });
    await flush();

    // The outer level expands, because Self.md is not on the stack until it
    // does. The copy of the embed inside it is what has to stop.
    expect(parent.textContent).toContain("loop");
    expect(parent.textContent).toContain("embeds itself");
  });

  it("offers a link rather than nothing for an embedded file", async () => {
    const { parent } = mount("intro\n\n![[report.pdf]]\n", {
      resolveEmbed: () => null,
      loadEmbed: async () => ({
        kind: "attachment" as const,
        path: "attachments/report.pdf",
        href: "/api/vaults/me/attachments/attachments/report.pdf",
      }),
    });
    await flush();

    const chip = parent.querySelector<HTMLAnchorElement>(".cm-fol-file-chip")!;
    expect(chip.getAttribute("href")).toBe("/api/vaults/me/attachments/attachments/report.pdf");
  });

  it("marks an embed of a note that does not exist", async () => {
    const { parent } = mount("intro\n\n![[Nope]]\n", {
      resolveEmbed: () => null,
      loadEmbed: async () => ({ kind: "missing" as const }),
    });
    await flush();
    expect(parent.querySelector(".cm-fol-transclusion-missing")?.textContent).toContain(
      "does not exist yet",
    );
  });
});

/**
 * Lets the embed promises settle and the widgets repaint.
 *
 * A transclusion fills itself in from a promise, so asserting straight after
 * mounting only ever sees the loading state.
 */
async function flush() {
  for (let i = 0; i < 5; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}
