import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { Editor } from "./editor";
import type { LivePreviewHandlers } from "./livepreview";
import { parseAlign } from "./table";

/**
 * These go through a real editor rather than calling the renderer directly.
 *
 * A table is the one thing live preview draws from a state field instead of the
 * view plugin, because CodeMirror rejects block decorations from a plugin. That
 * rejection happens at render time, inside the view, so only a mounted editor
 * can catch it coming back.
 */
const mounted: Editor[] = [];

function mount(doc = "", handlers: Partial<LivePreviewHandlers> = {}) {
  const parent = document.createElement("div");
  document.body.append(parent);

  const editor = new Editor({
    parent,
    onChange: () => {},
    onSaveRequest: () => {},
    completeNotes: () => [],
    completeTags: () => [],
    openLink: () => {},
    openTag: () => {},
    isResolved: () => true,
    resolveEmbed: () => null,
    ...handlers,
  });
  mounted.push(editor);
  editor.setContent(doc);
  return { editor, parent };
}

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

/** The rendered table, or null if the source is still showing. */
function table(parent: HTMLElement): HTMLTableElement | null {
  return parent.querySelector("table.cm-fol-table");
}

/** Every cell of a rendered table, as text, row by row. */
function cells(parent: HTMLElement): string[][] {
  return [...(table(parent)?.rows ?? [])].map((row) =>
    [...row.cells].map((c) => c.textContent ?? ""),
  );
}

// setContent leaves the cursor at position 0, so every table here is preceded by
// a line of prose: a table the cursor is sitting in shows its source on purpose.
const INTRO = "intro\n\n";

describe("parseAlign", () => {
  it("reads each column's colons", () => {
    expect(parseAlign("|:---|---:|:--:|---|")).toEqual(["left", "right", "center", null]);
  });

  it("accepts a row without outer pipes", () => {
    expect(parseAlign("--- | ---:")).toEqual([null, "right"]);
  });

  it("handles a single column", () => {
    expect(parseAlign("|:-:|")).toEqual(["center"]);
  });
});

describe("table live preview", () => {
  beforeEach(() => {
    document.body.innerHTML = "";
  });

  it("renders a table as a real table", () => {
    const { parent } = mount(INTRO + "| Metric | Value |\n|---|---|\n| Cash | $432.36 |\n");
    expect(cells(parent)).toEqual([
      ["Metric", "Value"],
      ["Cash", "$432.36"],
    ]);
  });

  it("puts the first row in a header", () => {
    const { parent } = mount(INTRO + "| A | B |\n|---|---|\n| c | d |\n");
    const t = table(parent)!;
    expect(t.tHead?.rows[0].cells[0].tagName).toBe("TH");
    expect(t.tBodies[0].rows[0].cells[0].tagName).toBe("TD");
  });

  it("carries column alignment onto the cells", () => {
    const { parent } = mount(INTRO + "| A | B | C |\n|:--|--:|:-:|\n| a | b | c |\n");
    const row = table(parent)!.tBodies[0].rows[0];
    expect([...row.cells].map((c) => c.className)).toEqual([
      "cm-fol-al-left",
      "cm-fol-al-right",
      "cm-fol-al-center",
    ]);
  });

  it("renders markup inside a cell", () => {
    const { parent } = mount(INTRO + "| A | B |\n|---|---|\n| **bold** | `code` |\n");
    const row = table(parent)!.tBodies[0].rows[0];
    expect(row.cells[0].querySelector("strong")?.textContent).toBe("bold");
    expect(row.cells[1].querySelector("code.cm-fol-code")?.textContent).toBe("code");
  });

  it("renders a link as its text, without the URL", () => {
    const { parent } = mount(INTRO + "| A |\n|---|\n| [the docs](http://x/y) |\n");
    const cell = table(parent)!.tBodies[0].rows[0].cells[0];
    expect(cell.textContent).toBe("the docs");
  });

  it("unescapes a pipe written as \\|", () => {
    const { parent } = mount(INTRO + "| A |\n|---|\n| a \\| b |\n");
    expect(cells(parent)[1]).toEqual(["a | b"]);
  });

  it("makes a wikilink in a cell clickable", () => {
    const opened: Array<[string, string]> = [];
    const { parent } = mount(INTRO + "| A |\n|---|\n| [[Some/Note#Bit]] |\n", {
      openLink: (target, anchor) => opened.push([target, anchor]),
    });
    const link = table(parent)!.querySelector<HTMLElement>("a.cm-fol-wikilink")!;
    expect(link.textContent).toBe("Note");
    link.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    expect(opened).toEqual([["Some/Note", "Bit"]]);
  });

  it("makes a tag in a cell clickable", () => {
    const opened: string[] = [];
    const { parent } = mount(INTRO + "| A |\n|---|\n| #maker here |\n", {
      openTag: (t) => opened.push(t),
    });
    const tag = table(parent)!.querySelector<HTMLElement>(".cm-fol-tag")!;
    tag.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    expect(opened).toEqual(["maker"]);
  });

  it("falls back to alt text for an image it cannot serve", () => {
    const { parent } = mount(INTRO + "| A |\n|---|\n| ![a chart](http://x/y.png) |\n");
    const cell = table(parent)!.tBodies[0].rows[0].cells[0];
    expect(cell.querySelector("img")).toBeNull();
    expect(cell.textContent).toBe("a chart");
  });

  it("shows the source while the cursor is inside the table", () => {
    const doc = INTRO + "| A | B |\n|---|---|\n| c | d |\n";
    const { editor, parent } = mount(doc);
    expect(table(parent)).not.toBeNull();

    editor.setCursor(doc.indexOf("| c |") + 3);
    expect(table(parent)).toBeNull();
    expect(parent.querySelector(".cm-content")?.textContent).toContain("|---|---|");
  });

  it("renders again once the cursor leaves", () => {
    const doc = INTRO + "| A | B |\n|---|---|\n| c | d |\n";
    const { editor, parent } = mount(doc);
    editor.setCursor(doc.indexOf("| c |") + 3);
    expect(table(parent)).toBeNull();

    editor.setCursor(0);
    expect(table(parent)).not.toBeNull();
  });

  it("clicking a cell puts the cursor in it, revealing the source", () => {
    const doc = INTRO + "| A | B |\n|---|---|\n| c | d |\n";
    const { editor, parent } = mount(doc);
    const cell = table(parent)!.tBodies[0].rows[0].cells[1];

    cell.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    expect(editor.cursor()).toBe(doc.indexOf("| c | d |") + 6);
    expect(table(parent)).toBeNull();
  });

  it("stops rendering in source mode", () => {
    const { editor, parent } = mount(INTRO + "| A | B |\n|---|---|\n| c | d |\n");
    expect(table(parent)).not.toBeNull();

    editor.setMode("source");
    expect(table(parent)).toBeNull();
    expect(parent.querySelector(".cm-content")?.textContent).toContain("| A | B |");
  });

  it("never rewrites the document", () => {
    const doc = INTRO + "|A|B|\n|:-|-:|\n| c |  d |\n";
    const { editor } = mount(doc);
    editor.setCursor(doc.length);
    expect(editor.content()).toBe(doc);
  });

  it("leaves a table inside a blockquote as source", () => {
    // A block decoration has to cover whole lines, and this one would start
    // after the "> ". Rendering it anyway is a render-time crash.
    const { parent } = mount(INTRO + "> | A | B |\n> |---|---|\n> | c | d |\n");
    expect(table(parent)).toBeNull();
    expect(parent.querySelector(".cm-content")?.textContent).toContain("| A | B |");
  });

  it("does not render a table inside a fenced block", () => {
    const { parent } = mount(INTRO + "```\n| A | B |\n|---|---|\n```\n");
    expect(table(parent)).toBeNull();
  });

  it("renders two tables in one note", () => {
    const { parent } = mount(
      INTRO + "| A |\n|---|\n| a |\n\n## Next\n\n| B |\n|---|\n| b |\n",
    );
    expect(parent.querySelectorAll("table.cm-fol-table")).toHaveLength(2);
  });
});
