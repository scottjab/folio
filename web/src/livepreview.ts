// Live preview: the editor renders markdown in place while the buffer stays the
// raw source, byte for byte.
//
// This is how Obsidian works, and the reason to copy it is fidelity. A WYSIWYG
// editor built on a rich-text document has to serialize back to markdown on
// every save, which quietly rewrites your files: emphasis markers change, list
// bullets normalize, long paragraphs reflow. Here the document *is* the
// markdown, so what you save is exactly what you typed, and switching to source
// mode is nothing more than turning these decorations off.
//
// The rule that makes it feel right: a line the cursor is on shows its raw
// syntax. You get a rendered document to read and the real characters to edit,
// without a mode switch.

import { syntaxTree } from "@codemirror/language";
import { Range, RangeSet, EditorState } from "@codemirror/state";
import {
  Decoration,
  DecorationSet,
  EditorView,
  ViewPlugin,
  ViewUpdate,
  WidgetType,
} from "@codemirror/view";
import type { SyntaxNodeRef } from "@lezer/common";

/** What an ![[embed]] turned out to point at, once the server has resolved it. */
export interface EmbedResult {
  kind: "note" | "attachment" | "missing";
  path?: string;
  title?: string;
  content?: string;
  truncated?: boolean;
  /** Where to fetch an attachment from. Only set when kind is "attachment". */
  href?: string;
}

/** What a click on a rendered wikilink or tag reports back to the app. */
export interface LivePreviewHandlers {
  openLink(target: string, anchor: string): void;
  openTag(tag: string): void;
  /** The image URL for a target that names one, or null for anything else. */
  resolveEmbed(target: string): string | null;
  isResolved(target: string): boolean;

  /**
   * Resolves an ![[embed]] that is not an image. Resolution and section
   * extraction happen on the server so the browser and the terminal client
   * inline the same span of text.
   */
  loadEmbed(target: string, anchor: string): Promise<EmbedResult>;

  /**
   * Renders markdown into host using this same live preview, read only, and
   * returns a teardown.
   *
   * An embedded note has to look like a note, which means running the real
   * renderer over it rather than a second, simpler one that drifts. stack is
   * the chain of notes already being rendered, and is how a note that embeds
   * itself stops instead of recursing.
   */
  renderEmbedded(host: HTMLElement, content: string, stack: string[]): () => void;
}

/**
 * How deep transclusions may nest before they stop expanding.
 *
 * Three is enough for a note that pulls in a section that pulls in a snippet,
 * and shallow enough that a page cannot quietly mount dozens of editors.
 */
export const maxEmbedDepth = 3;

/** Hides a range entirely: the markup characters of a rendered element. */
const hide = Decoration.replace({});

/** Applies a class to a range without changing the text. */
const mark = (className: string) => Decoration.mark({ class: className });

const headingMarks = [
  "cm-fol-h1", "cm-fol-h2", "cm-fol-h3", "cm-fol-h4", "cm-fol-h5", "cm-fol-h6",
];

/**
 * The DOM for a rendered wikilink.
 *
 * Exported because table cells build their contents by hand rather than through
 * decorations, and a link inside a table has to look and behave exactly like one
 * in a paragraph.
 */
export function wikilinkElement(
  label: string,
  target: string,
  anchor: string,
  resolved: boolean,
  handlers: LivePreviewHandlers,
): HTMLAnchorElement {
  const a = document.createElement("a");
  a.className = "cm-fol-wikilink" + (resolved ? "" : " cm-fol-unresolved");
  a.textContent = label;
  a.title = resolved ? target : `${target} does not exist yet; click to create it`;
  a.addEventListener("mousedown", (e) => {
    e.preventDefault();
    e.stopPropagation();
    handlers.openLink(target, anchor);
  });
  return a;
}

/** The DOM for a rendered #tag pill. Shared with the table renderer. */
export function tagElement(tag: string, handlers: LivePreviewHandlers): HTMLSpanElement {
  const el = document.createElement("span");
  el.className = "cm-fol-tag";
  el.textContent = "#" + tag;
  el.addEventListener("mousedown", (e) => {
    e.preventDefault();
    e.stopPropagation();
    handlers.openTag(tag);
  });
  return el;
}

/** A rendered wikilink. Clicking it navigates; the text stays selectable. */
class WikilinkWidget extends WidgetType {
  constructor(
    readonly label: string,
    readonly target: string,
    readonly anchor: string,
    readonly resolved: boolean,
    readonly handlers: LivePreviewHandlers,
  ) {
    super();
  }

  eq(other: WikilinkWidget) {
    return (
      other.label === this.label &&
      other.target === this.target &&
      other.anchor === this.anchor &&
      other.resolved === this.resolved
    );
  }

  toDOM() {
    return wikilinkElement(this.label, this.target, this.anchor, this.resolved, this.handlers);
  }

  ignoreEvent() {
    return true;
  }
}

/** A rendered #tag, styled as a pill and clickable to filter by it. */
class TagWidget extends WidgetType {
  constructor(readonly tag: string, readonly handlers: LivePreviewHandlers) {
    super();
  }

  eq(other: TagWidget) {
    return other.tag === this.tag;
  }

  toDOM() {
    return tagElement(this.tag, this.handlers);
  }

  ignoreEvent() {
    return true;
  }
}

/**
 * The display size in an embed's alias slot: `![[shot.png|300]]` or
 * `![[shot.png|300x200]]`, Obsidian's syntax.
 *
 * Anything not of that shape is alt text rather than a size, so it returns null
 * and the image renders at its natural width.
 */
export function parseEmbedSize(alias: string): { width: number; height?: number } | null {
  const m = /^\s*(\d+)\s*(?:x\s*(\d+)\s*)?$/.exec(alias);
  if (!m) return null;
  const width = Number(m[1]);
  if (!width) return null;
  const height = m[2] ? Number(m[2]) : undefined;
  return height ? { width, height } : { width };
}

/** An embedded image, from ![[file.png]] or ![alt](file.png). */
class ImageWidget extends WidgetType {
  constructor(
    readonly src: string,
    readonly alt: string,
    readonly size: { width: number; height?: number } | null = null,
  ) {
    super();
  }

  eq(other: ImageWidget) {
    return (
      other.src === this.src &&
      other.alt === this.alt &&
      other.size?.width === this.size?.width &&
      other.size?.height === this.size?.height
    );
  }

  toDOM() {
    const wrap = document.createElement("div");
    wrap.className = "cm-fol-embed";
    const img = document.createElement("img");
    img.src = this.src;
    img.alt = this.alt;
    img.loading = "lazy";
    if (this.size) {
      // A width attribute rather than a style, so the stylesheet's max-width
      // still wins on a narrow screen and the image never overflows the page.
      img.width = this.size.width;
      if (this.size.height) img.height = this.size.height;
    }
    wrap.appendChild(img);
    return wrap;
  }
}

/**
 * An embedded note: ![[Note]] or ![[Note#Heading]], rendered in place.
 *
 * The content is fetched rather than read from the open buffer because the
 * target is usually a different note, and the server is what decides which note
 * a target resolves to and where a heading's section ends.
 */
class TransclusionWidget extends WidgetType {
  private teardown: (() => void) | null = null;

  constructor(
    readonly target: string,
    readonly anchor: string,
    readonly stack: string[],
    readonly handlers: LivePreviewHandlers,
  ) {
    super();
  }

  eq(other: TransclusionWidget) {
    return (
      other.target === this.target &&
      other.anchor === this.anchor &&
      other.stack.join("\u0000") === this.stack.join("\u0000")
    );
  }

  toDOM() {
    const wrap = document.createElement("div");
    wrap.className = "cm-fol-transclusion";
    wrap.append(h("div", "cm-fol-transclusion-loading", "Loading..."));

    void this.handlers
      .loadEmbed(this.target, this.anchor)
      .then((res) => this.fill(wrap, res))
      .catch(() => {
        wrap.replaceChildren(
          h("div", "cm-fol-transclusion-missing", `Could not load ${this.label()}`),
        );
      });
    return wrap;
  }

  private label() {
    return this.anchor ? `${this.target}#${this.anchor}` : this.target;
  }

  private fill(wrap: HTMLElement, res: EmbedResult) {
    if (res.kind === "missing") {
      const el = h("div", "cm-fol-transclusion-missing", `${this.label()} does not exist yet`);
      el.addEventListener("mousedown", (e) => {
        e.preventDefault();
        this.handlers.openLink(this.target, this.anchor);
      });
      wrap.replaceChildren(el);
      return;
    }

    if (res.kind === "attachment") {
      // A PDF or a zip cannot render inline, but it should still be reachable
      // rather than silently nothing.
      const a = document.createElement("a");
      a.className = "cm-fol-file-chip";
      a.href = res.href ?? "#";
      a.target = "_blank";
      a.rel = "noreferrer";
      a.textContent = res.path ?? this.target;
      wrap.replaceChildren(a);
      return;
    }

    const path = res.path ?? this.target;

    // A note that embeds itself, directly or through a chain, would otherwise
    // mount editors until the tab dies.
    if (this.stack.includes(path)) {
      wrap.replaceChildren(
        h("div", "cm-fol-transclusion-missing", `${this.label()} embeds itself`),
      );
      return;
    }

    const head = h("div", "cm-fol-transclusion-head", res.title || path);
    head.addEventListener("mousedown", (e) => {
      e.preventDefault();
      this.handlers.openLink(this.target, this.anchor);
    });

    const body = h("div", "cm-fol-transclusion-body", "");
    wrap.replaceChildren(head, body);

    if (this.stack.length >= maxEmbedDepth) {
      // Past the limit the note is still named and still clickable; it just
      // stops expanding.
      body.append(h("div", "cm-fol-transclusion-deep", "Embedded too deeply to expand"));
      return;
    }

    this.teardown = this.handlers.renderEmbedded(body, res.content ?? "", [...this.stack, path]);
    if (res.truncated) {
      body.append(h("div", "cm-fol-transclusion-deep", "Truncated; open the note to read the rest"));
    }
  }

  destroy() {
    this.teardown?.();
    this.teardown = null;
  }

  ignoreEvent() {
    return true;
  }
}

/** A small element helper, so widget DOM reads as structure rather than noise. */
function h(tag: string, className: string, text: string): HTMLElement {
  const el = document.createElement(tag);
  el.className = className;
  el.textContent = text;
  return el;
}

/** A real checkbox for a `- [ ]` task, which writes back to the document. */
class TaskWidget extends WidgetType {
  constructor(readonly checked: boolean, readonly pos: number) {
    super();
  }

  eq(other: TaskWidget) {
    return other.checked === this.checked && other.pos === this.pos;
  }

  toDOM(view: EditorView) {
    const box = document.createElement("input");
    box.type = "checkbox";
    box.className = "cm-fol-task";
    box.checked = this.checked;
    box.addEventListener("mousedown", (e) => e.preventDefault());
    box.addEventListener("change", () => {
      // Toggling rewrites the single character between the brackets, so the
      // rest of the line, including any trailing metadata, is untouched.
      view.dispatch({
        changes: { from: this.pos, to: this.pos + 1, insert: box.checked ? "x" : " " },
      });
    });
    return box;
  }

  ignoreEvent(event: Event) {
    return event.type !== "mousedown";
  }
}

/** A rendered horizontal rule. */
class RuleWidget extends WidgetType {
  eq() {
    return true;
  }
  toDOM() {
    const el = document.createElement("hr");
    el.className = "cm-fol-rule";
    return el;
  }
}

/** A rendered list bullet, replacing the `-` or `*` the author typed. */
class BulletWidget extends WidgetType {
  eq() {
    return true;
  }
  toDOM() {
    const el = document.createElement("span");
    el.className = "cm-fol-bullet";
    el.textContent = "•";
    return el;
  }
}

/**
 * Returns the set of line ranges the selection touches.
 *
 * Decorations that would hide characters are skipped inside these ranges, which
 * is what reveals the raw markdown on the line you are editing.
 */
export function activeLineRanges(state: EditorState): Array<{ from: number; to: number }> {
  const ranges: Array<{ from: number; to: number }> = [];
  for (const r of state.selection.ranges) {
    const first = state.doc.lineAt(r.from);
    const last = r.empty ? first : state.doc.lineAt(r.to);
    ranges.push({ from: first.from, to: last.to });
  }
  return ranges;
}

/** Reports whether [from, to) overlaps any range in ranges. */
export function overlaps(
  ranges: Array<{ from: number; to: number }>,
  from: number,
  to: number,
): boolean {
  for (const r of ranges) {
    if (from <= r.to && to >= r.from) return true;
  }
  return false;
}

/**
 * Builds the decoration set for the visible document.
 *
 * Only the visible ranges are walked. A vault note is small, but a pasted log
 * file is not, and decorating the whole document on every keystroke is how these
 * editors get slow.
 */
function buildDecorations(
  view: EditorView,
  handlers: LivePreviewHandlers,
  stack: string[],
): DecorationSet {
  const active = activeLineRanges(view.state);
  const out: Range<Decoration>[] = [];

  // Hiding a range only when the cursor is elsewhere is the whole live-preview
  // trick, so it goes through one helper rather than being repeated per node.
  const conceal = (from: number, to: number) => {
    if (from >= to) return;
    if (overlaps(active, from, to)) return;
    out.push(hide.range(from, to));
  };
  const replaceWith = (from: number, to: number, deco: Decoration) => {
    if (overlaps(active, from, to)) return;
    out.push(deco.range(from, to));
  };

  for (const { from, to } of view.visibleRanges) {
    syntaxTree(view.state).iterate({
      from,
      to,
      enter(node: SyntaxNodeRef) {
        return decorateNode(view, node, out, conceal, replaceWith, handlers, stack);
      },
    });
  }

  // RangeSet requires sorted ranges, and widgets can be produced out of order.
  out.sort((a, b) => a.from - b.from || a.value.startSide - b.value.startSide);
  return RangeSet.of(out, true);
}

function decorateNode(
  view: EditorView,
  node: SyntaxNodeRef,
  out: Range<Decoration>[],
  conceal: (from: number, to: number) => void,
  replaceWith: (from: number, to: number, deco: Decoration) => void,
  handlers: LivePreviewHandlers,
  stack: string[],
): boolean | void {
  const doc = view.state.doc;
  const name = node.name;

  // A table is rendered whole, by the state field in table.ts, because a block
  // decoration cannot come from a view plugin. Decorating its innards here would
  // put two decorations over the same range, so the field owns tables outright
  // and this walk stops at the boundary.
  if (name === "Table") return false;

  // Headings: hide the hashes, scale the text. The line keeps its real content,
  // so backspacing at the start still deletes a "#".
  const headingMatch = /^ATXHeading(\d)$/.exec(name);
  if (headingMatch) {
    const level = Number(headingMatch[1]);
    out.push(
      Decoration.line({ class: headingMarks[level - 1] }).range(doc.lineAt(node.from).from),
    );
    const markNode = node.node.firstChild;
    if (markNode?.name === "HeaderMark") {
      // Include the space after the hashes, or the heading is indented by one.
      const end = Math.min(markNode.to + 1, node.to);
      conceal(markNode.from, end);
    }
    return;
  }

  switch (name) {
    case "StrongEmphasis":
    case "Emphasis":
    case "Strikethrough": {
      const cls =
        name === "StrongEmphasis"
          ? "cm-fol-strong"
          : name === "Emphasis"
            ? "cm-fol-em"
            : "cm-fol-strike";
      out.push(mark(cls).range(node.from, node.to));
      for (let c = node.node.firstChild; c; c = c.nextSibling) {
        if (c.name === "EmphasisMark" || c.name === "StrikethroughMark") {
          conceal(c.from, c.to);
        }
      }
      return;
    }

    case "InlineCode": {
      out.push(mark("cm-fol-code").range(node.from, node.to));
      for (let c = node.node.firstChild; c; c = c.nextSibling) {
        if (c.name === "CodeMark") conceal(c.from, c.to);
      }
      return;
    }

    case "Wikilink": {
      const parts = wikilinkParts(view.state, node);
      if (!parts) return;
      replaceWith(
        node.from,
        node.to,
        Decoration.replace({
          widget: new WikilinkWidget(
            parts.label,
            parts.target,
            parts.anchor,
            handlers.isResolved(parts.target),
            handlers,
          ),
        }),
      );
      return;
    }

    case "WikilinkEmbed": {
      const parts = wikilinkParts(view.state, node);
      if (!parts) return;
      const src = handlers.resolveEmbed(parts.target);
      if (src) {
        // An image resolves without asking the server, which keeps the common
        // case off the network.
        replaceWith(
          node.from,
          node.to,
          Decoration.replace({
            widget: new ImageWidget(src, parts.target, parseEmbedSize(parts.alias)),
            block: false,
          }),
        );
        return;
      }
      // Everything else is a note to transclude or a file to link, and only the
      // server can say which.
      replaceWith(
        node.from,
        node.to,
        Decoration.replace({
          widget: new TransclusionWidget(parts.target, parts.anchor, stack, handlers),
          block: false,
        }),
      );
      return;
    }

    case "Hashtag": {
      const text = doc.sliceString(node.from, node.to);
      replaceWith(
        node.from,
        node.to,
        Decoration.replace({ widget: new TagWidget(text.slice(1), handlers) }),
      );
      return;
    }

    case "Link": {
      // Render as just the link text: hide the brackets and the URL.
      const marks: Array<{ from: number; to: number }> = [];
      let urlNode: { from: number; to: number } | null = null;
      for (let c = node.node.firstChild; c; c = c.nextSibling) {
        if (c.name === "LinkMark") marks.push({ from: c.from, to: c.to });
        if (c.name === "URL") urlNode = { from: c.from, to: c.to };
      }
      if (marks.length >= 2 && urlNode) {
        out.push(mark("cm-fol-link").range(node.from, node.to));
        conceal(marks[0].from, marks[0].to);
        // From the closing "]" through the ")" is everything but the label.
        conceal(marks[1].from, node.to);
      }
      return;
    }

    case "Image": {
      let url = "";
      let alt = "";
      for (let c = node.node.firstChild; c; c = c.nextSibling) {
        if (c.name === "URL") url = doc.sliceString(c.from, c.to);
      }
      const label = /!\[([^\]]*)\]/.exec(doc.sliceString(node.from, node.to));
      if (label) alt = label[1];
      if (url && !/^[a-z]+:/i.test(url)) {
        const src = handlers.resolveEmbed(url);
        if (src) replaceWith(node.from, node.to, Decoration.replace({ widget: new ImageWidget(src, alt) }));
      }
      return;
    }

    case "ListMark": {
      const text = doc.sliceString(node.from, node.to);
      // Numbered lists keep their numbers; only bullets become a glyph.
      if (text === "-" || text === "*" || text === "+") {
        replaceWith(node.from, node.to, Decoration.replace({ widget: new BulletWidget() }));
      }
      return;
    }

    case "TaskMarker": {
      const text = doc.sliceString(node.from, node.to);
      const checked = /\[[xX]\]/.test(text);
      replaceWith(
        node.from,
        node.to,
        Decoration.replace({ widget: new TaskWidget(checked, node.from + 1) }),
      );
      return;
    }

    case "Blockquote": {
      for (let line = doc.lineAt(node.from).number; ; line++) {
        const l = doc.line(line);
        out.push(Decoration.line({ class: "cm-fol-quote" }).range(l.from));
        if (l.to >= node.to) break;
      }
      return;
    }

    case "QuoteMark": {
      conceal(node.from, Math.min(node.to + 1, doc.lineAt(node.from).to));
      return;
    }

    case "HorizontalRule": {
      replaceWith(node.from, node.to, Decoration.replace({ widget: new RuleWidget() }));
      return;
    }

    case "FencedCode": {
      // Code stays visible and highlighted, the same as Obsidian. Hiding the
      // fences would make it impossible to tell where a block ends.
      for (let line = doc.lineAt(node.from).number; ; line++) {
        const l = doc.line(line);
        out.push(Decoration.line({ class: "cm-fol-fence" }).range(l.from));
        if (l.to >= node.to) break;
      }
      return;
    }
  }
}

/** Reads a wikilink node's target, anchor, and display label. */
export function wikilinkParts(
  state: EditorState,
  node: SyntaxNodeRef,
): { target: string; anchor: string; label: string; alias: string } | null {
  let target = "";
  let anchor = "";
  let alias = "";
  for (let c = node.node.firstChild; c; c = c.nextSibling) {
    const text = state.doc.sliceString(c.from, c.to);
    if (c.name === "WikilinkTarget") target = text;
    if (c.name === "WikilinkAnchor") anchor = text.replace(/^#/, "");
    if (c.name === "WikilinkAlias") alias = text.replace(/^\|/, "");
  }
  if (!target && !anchor) return null;
  const label = alias || (target ? target.split("/").pop()! : anchor);
  return { target, anchor, label, alias };
}

/**
 * The live-preview extension. Add it to make the editor render; remove it, via
 * the mode compartment, to see raw markdown.
 */
export function livePreview(handlers: LivePreviewHandlers, stack: string[] = []) {
  return ViewPlugin.fromClass(
    class {
      decorations: DecorationSet;

      constructor(view: EditorView) {
        this.decorations = buildDecorations(view, handlers, stack);
      }

      update(update: ViewUpdate) {
        // The selection matters as much as the document: moving the cursor onto
        // a line has to reveal that line's syntax.
        if (update.docChanged || update.selectionSet || update.viewportChanged) {
          this.decorations = buildDecorations(update.view, handlers, stack);
        }
      }
    },
    {
      decorations: (v) => v.decorations,
      // Without this, clicking a rendered widget would move the cursor into the
      // hidden markup and flash the raw source.
      eventHandlers: {
        mousedown(event) {
          const target = event.target as HTMLElement;
          return target.closest(".cm-fol-wikilink, .cm-fol-tag") !== null;
        },
      },
    },
  );
}
