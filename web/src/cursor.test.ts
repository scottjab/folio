import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { Editor } from "./editor";
import type { LivePreviewHandlers } from "./livepreview";

// Knowing where you are typing is not a nicety. Live preview hides syntax on
// every line except the one you are on, so if the cursor is hard to find the
// editor is genuinely hard to use.

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
      /* already gone */
    }
  }
  document.body.innerHTML = "";
});

beforeEach(() => {
  document.body.innerHTML = "";
});

/**
 * Focuses the editor and waits for CodeMirror to notice.
 *
 * Focus handling is debounced: the .cm-focused class, and with it the cursor,
 * only appear on a later tick. test-setup.ts also has to tell jsdom the document
 * is focused at all, since it reports otherwise and CodeMirror checks.
 */
async function focused(editor: Editor) {
  editor.focus();
  await new Promise((r) => setTimeout(r, 60));
}

describe("the cursor is visible", () => {
  it("draws a cursor element rather than relying on the native caret", async () => {
    // The native contenteditable caret is a hairline, and it competes with a
    // document full of replaced ranges and widgets. CodeMirror's drawSelection
    // renders a real element we can style boldly and, incidentally, assert on.
    const { editor, parent } = mount("# Heading\n\nSome text.\n");
    await focused(editor);

    expect(parent.querySelector(".cm-cursorLayer")).not.toBeNull();
    expect(parent.querySelector(".cm-cursor")).not.toBeNull();
    expect(parent.querySelector(".cm-cursor-primary")).not.toBeNull();
  });

  it("keeps drawing one in source mode", async () => {
    const { editor, parent } = mount("# Heading\n\nSome text.\n");
    editor.setMode("source");
    await focused(editor);
    expect(parent.querySelector(".cm-cursor")).not.toBeNull();
  });

  it("has a cursor layer even before it is focused", () => {
    // The layer is what drawSelection contributes, so its presence is the
    // direct check that the extension is configured at all. That was the actual
    // defect: without it, .cm-cursor never exists and the CSS for it is dead.
    const { parent } = mount("text\n");
    expect(parent.querySelector(".cm-cursorLayer")).not.toBeNull();
    expect(parent.querySelector(".cm-selectionLayer")).not.toBeNull();
  });

  it("marks the line the cursor is on", async () => {
    // Live preview reveals raw syntax on the active line, so highlighting that
    // line explains why it looks different from the others.
    const { editor, parent } = mount("first\nsecond\nthird\n");
    await focused(editor);

    const active = parent.querySelectorAll(".cm-activeLine");
    expect(active.length).toBe(1);
    expect(active[0].textContent).toBe("first");
  });

  it("moves the active line with the cursor", async () => {
    const { editor, parent } = mount("first\nsecond\nthird\n");
    await focused(editor);
    editor.setCursor(Number.MAX_SAFE_INTEGER); // end of the document

    const active = parent.querySelector(".cm-activeLine");
    expect(active?.textContent).toBe("");
  });

  it("reports where the cursor is", () => {
    const { editor } = mount("hello\n");
    expect(editor.cursor()).toBe(0);
    editor.setCursor(3);
    expect(editor.cursor()).toBe(3);
  });

  it("clamps a cursor position outside the document", () => {
    const { editor } = mount("hello\n");
    editor.setCursor(-5);
    expect(editor.cursor()).toBe(0);
    editor.setCursor(9999);
    expect(editor.cursor()).toBe(6);
  });

  it("puts the cursor after text inserted at it", () => {
    const { editor } = mount("");
    editor.insertAtCursor("![[diagram.png]]");
    expect(editor.cursor()).toBe("![[diagram.png]]".length);
  });

  it("can jump to a heading, which is what a #anchor link needs", async () => {
    const doc = "# One\n\ntext\n\n## Two\n\nmore\n";
    const { editor, parent } = mount(doc);
    await focused(editor);

    expect(editor.goToHeading("Two")).toBe(true);
    expect(parent.querySelector(".cm-activeLine")?.textContent).toBe("## Two");

    expect(editor.goToHeading("Nope")).toBe(false);
  });
});
