// Live preview for GFM tables.
//
// Every other block live preview renders is decorated in place: the characters
// stay where they are and CSS makes them look like a heading or a quote. A table
// cannot work that way. Lining up columns means laying out a real grid, which
// means replacing several lines with one widget, and CodeMirror refuses block
// decorations from a view plugin ("Block decorations may not be specified via
// plugins") because they change line heights it has already measured.
//
// So tables get their own state field, and livepreview.ts stops its walk at the
// Table node so the two never decorate the same range.
//
// The reveal rule is the same as everywhere else: put the cursor inside a table
// and it turns back into the pipes you typed, ready to edit.

import { syntaxTree } from "@codemirror/language";
import { EditorState, Extension, Range, StateField } from "@codemirror/state";
import { Decoration, DecorationSet, EditorView, WidgetType } from "@codemirror/view";
import type { SyntaxNode } from "@lezer/common";

import {
  activeLineRanges,
  overlaps,
  tagElement,
  wikilinkElement,
  wikilinkParts,
} from "./livepreview";
import type { LivePreviewHandlers } from "./livepreview";

/**
 * One piece of a rendered cell.
 *
 * Cells are extracted into plain data rather than DOM or syntax nodes because a
 * widget outlives the state that built it: CodeMirror keeps the old DOM when
 * eq() says nothing changed, and a widget holding a stale EditorState would
 * render positions that have since moved.
 */
export type Inline =
  | { kind: "text"; text: string }
  | { kind: "style"; tag: "strong" | "em" | "del"; body: Inline[] }
  | { kind: "code"; text: string }
  | { kind: "link"; body: Inline[] }
  | { kind: "wikilink"; target: string; anchor: string; label: string; resolved: boolean }
  | { kind: "tag"; tag: string }
  | { kind: "image"; src: string; alt: string };

/** A column's alignment, from the `:---:` row. null means the default. */
export type Align = "left" | "center" | "right" | null;

export interface TableCellModel {
  /** Document offset of the cell's first character, for click-to-edit. */
  from: number;
  body: Inline[];
}

export interface TableRowModel {
  header: boolean;
  cells: TableCellModel[];
}

export interface TableModel {
  from: number;
  align: Align[];
  rows: TableRowModel[];
}

/**
 * Syntax nodes whose text is markup rather than content.
 *
 * Dropping them by name is what lets one walk handle emphasis, code, links and
 * images: the renderer emits the gaps between a node's children, so anything
 * named here disappears along with the characters it covers.
 */
const DROP = new Set([
  "EmphasisMark",
  "StrikethroughMark",
  "CodeMark",
  "LinkMark",
  "LinkTitle",
  "URL",
  "WikilinkMark",
  "TableDelimiter",
]);

/**
 * Reads column alignment out of the `|:---|---:|` row.
 *
 * The row is one node covering the whole line, so this is string work rather
 * than a tree walk. GFM allows the outer pipes to be left off, hence the
 * conditional strip rather than a blind slice.
 */
export function parseAlign(line: string): Align[] {
  const trimmed = line.trim().replace(/^\|/, "").replace(/\|$/, "");
  return trimmed.split("|").map((cell) => {
    const s = cell.trim();
    const left = s.startsWith(":");
    const right = s.endsWith(":");
    if (left && right) return "center";
    if (right) return "right";
    if (left) return "left";
    return null;
  });
}

/** Turns one Table node into the data the widget renders from. */
export function readTable(
  state: EditorState,
  table: SyntaxNode,
  handlers: LivePreviewHandlers,
): TableModel {
  const rows: TableRowModel[] = [];
  let align: Align[] = [];

  for (let row = table.firstChild; row; row = row.nextSibling) {
    // The only TableDelimiter that is a direct child of Table is the alignment
    // row; the ones separating cells hang off the rows themselves.
    if (row.name === "TableDelimiter") {
      align = parseAlign(state.doc.sliceString(row.from, row.to));
      continue;
    }
    if (row.name !== "TableHeader" && row.name !== "TableRow") continue;

    const cells: TableCellModel[] = [];
    for (let cell = row.firstChild; cell; cell = cell.nextSibling) {
      if (cell.name !== "TableCell") continue;
      cells.push({ from: cell.from, body: inlines(state, cell, cell.from, cell.to, handlers) });
    }
    rows.push({ header: row.name === "TableHeader", cells });
  }

  return { from: table.from, align, rows };
}

/**
 * Renders the inline content of one node into the model.
 *
 * Walking children and emitting the text between them, rather than only the Text
 * nodes, is what keeps the output faithful: lezer does not give every run of
 * plain characters a node of its own.
 */
function inlines(
  state: EditorState,
  node: SyntaxNode,
  from: number,
  to: number,
  handlers: LivePreviewHandlers,
): Inline[] {
  const out: Inline[] = [];
  const text = (a: number, b: number) => {
    if (b > a) out.push({ kind: "text", text: state.doc.sliceString(a, b) });
  };

  let pos = from;
  for (let child = node.firstChild; child; child = child.nextSibling) {
    if (child.from >= to) break;
    if (child.to <= pos) continue;
    text(pos, child.from);
    const piece = convert(state, child, handlers);
    if (piece) out.push(piece);
    pos = child.to;
  }
  text(pos, to);
  return out;
}

/** Maps one inline syntax node to its model piece, or null to drop it. */
function convert(
  state: EditorState,
  node: SyntaxNode,
  handlers: LivePreviewHandlers,
): Inline | null {
  const doc = state.doc;
  if (DROP.has(node.name)) return null;

  switch (node.name) {
    case "StrongEmphasis":
    case "Emphasis":
    case "Strikethrough": {
      const tag = node.name === "StrongEmphasis" ? "strong" : node.name === "Emphasis" ? "em" : "del";
      return { kind: "style", tag, body: inlines(state, node, node.from, node.to, handlers) };
    }

    case "InlineCode": {
      // Code is literal: what is between the backticks is not markup.
      const first = node.firstChild;
      const last = node.lastChild;
      const from = first?.name === "CodeMark" ? first.to : node.from;
      const to = last?.name === "CodeMark" ? last.from : node.to;
      return { kind: "code", text: doc.sliceString(from, to) };
    }

    case "Escape":
      // "\|" is how a pipe gets into a cell at all, so the backslash has to go.
      return { kind: "text", text: doc.sliceString(node.from + 1, node.to) };

    case "Link":
      // Styled but not navigable, matching how live preview renders a markdown
      // link everywhere else.
      return { kind: "link", body: inlines(state, node, node.from, node.to, handlers) };

    case "Image": {
      let url = "";
      for (let c = node.firstChild; c; c = c.nextSibling) {
        if (c.name === "URL") url = doc.sliceString(c.from, c.to);
      }
      const alt = /^!\[([^\]]*)\]/.exec(doc.sliceString(node.from, node.to))?.[1] ?? "";
      // An off-site image would be blocked by the page's img-src anyway, so an
      // absolute URL falls back to its alt text rather than a broken icon.
      const src = /^[a-z]+:/i.test(url) ? null : handlers.resolveEmbed(url);
      return src ? { kind: "image", src, alt } : { kind: "text", text: alt };
    }

    case "Wikilink": {
      const parts = wikilinkParts(state, node);
      if (!parts) return null;
      return { kind: "wikilink", ...parts, resolved: handlers.isResolved(parts.target) };
    }

    case "WikilinkEmbed": {
      const parts = wikilinkParts(state, node);
      const src = parts ? handlers.resolveEmbed(parts.target) : null;
      if (!src || !parts) return { kind: "text", text: doc.sliceString(node.from, node.to) };
      return { kind: "image", src, alt: parts.target };
    }

    case "Hashtag":
      return { kind: "tag", tag: doc.sliceString(node.from, node.to).slice(1) };

    default:
      return { kind: "text", text: doc.sliceString(node.from, node.to) };
  }
}

/** Builds the DOM for a cell's contents. */
function fill(parent: Node, body: Inline[], handlers: LivePreviewHandlers) {
  for (const piece of body) {
    switch (piece.kind) {
      case "text":
        parent.appendChild(document.createTextNode(piece.text));
        break;
      case "style": {
        const el = document.createElement(piece.tag);
        fill(el, piece.body, handlers);
        parent.appendChild(el);
        break;
      }
      case "code": {
        const el = document.createElement("code");
        el.className = "cm-fol-code";
        el.textContent = piece.text;
        parent.appendChild(el);
        break;
      }
      case "link": {
        const el = document.createElement("span");
        el.className = "cm-fol-link";
        fill(el, piece.body, handlers);
        parent.appendChild(el);
        break;
      }
      case "wikilink":
        parent.appendChild(
          wikilinkElement(piece.label, piece.target, piece.anchor, piece.resolved, handlers),
        );
        break;
      case "tag":
        parent.appendChild(tagElement(piece.tag, handlers));
        break;
      case "image": {
        const img = document.createElement("img");
        img.className = "cm-fol-table-img";
        img.src = piece.src;
        img.alt = piece.alt;
        img.loading = "lazy";
        parent.appendChild(img);
        break;
      }
    }
  }
}

/**
 * Builds a rendered table.
 *
 * Exported so tests can assert on the DOM without standing up an editor, which
 * is also why view is optional: only click-to-edit needs it.
 */
export function renderTable(
  model: TableModel,
  handlers: LivePreviewHandlers,
  view?: EditorView,
): HTMLElement {
  const wrap = document.createElement("div");
  wrap.className = "cm-fol-table-wrap";

  const table = document.createElement("table");
  table.className = "cm-fol-table";
  wrap.appendChild(table);

  let head: HTMLTableSectionElement | null = null;
  let body: HTMLTableSectionElement | null = null;

  for (const row of model.rows) {
    const section = row.header ? (head ??= table.createTHead()) : (body ??= table.createTBody());
    const tr = document.createElement("tr");
    section.appendChild(tr);

    row.cells.forEach((cell, i) => {
      const td = document.createElement(row.header ? "th" : "td");
      const align = model.align[i];
      if (align) td.classList.add(`cm-fol-al-${align}`);
      td.dataset.pos = String(cell.from);
      fill(td, cell.body, handlers);
      tr.appendChild(td);
    });
  }

  if (view) wrap.addEventListener("click", (e) => clickIntoCell(e, view));
  return wrap;
}

/**
 * Puts the cursor in the cell that was clicked, which reveals the markdown
 * behind the table so it can be edited.
 *
 * Without this a rendered table would be a dead end in preview mode: the widget
 * is not editable, so clicking it only parks the cursor beside the whole block.
 * A click that finished a drag is left alone, so selecting and copying out of a
 * rendered table still works.
 */
function clickIntoCell(e: MouseEvent, view: EditorView) {
  const target = e.target as HTMLElement;
  if (target.closest(".cm-fol-wikilink, .cm-fol-tag")) return;
  if (!window.getSelection()?.isCollapsed) return;

  const pos = target.closest<HTMLElement>("th, td")?.dataset.pos;
  if (pos === undefined) return;

  view.dispatch({ selection: { anchor: Number(pos) } });
  view.focus();
}

/** The whole table, standing in for the lines that describe it. */
class TableWidget extends WidgetType {
  private readonly key: string;

  constructor(
    readonly model: TableModel,
    readonly handlers: LivePreviewHandlers,
  ) {
    super();
    // The model is small and rebuilt on every keystroke anyway, so comparing it
    // whole is cheaper to get right than a field-by-field eq. The offset is part
    // of it because the cell positions the click handler captured are only valid
    // while the table sits where it did.
    this.key = JSON.stringify(model);
  }

  eq(other: TableWidget) {
    return other.key === this.key;
  }

  toDOM(view: EditorView) {
    return renderTable(this.model, this.handlers, view);
  }

  ignoreEvent() {
    return true;
  }
}

function buildTables(state: EditorState, handlers: LivePreviewHandlers): DecorationSet {
  const active = activeLineRanges(state);
  const out: Range<Decoration>[] = [];

  syntaxTree(state).iterate({
    enter(node) {
      if (node.name !== "Table") return;
      if (overlaps(active, node.from, node.to)) return false;
      // A block decoration has to cover whole lines. A table nested in a
      // blockquote or a list item does not, so it stays as source rather than
      // throwing at render time.
      if (state.doc.lineAt(node.from).from !== node.from) return false;
      if (state.doc.lineAt(node.to).to !== node.to) return false;

      out.push(
        Decoration.replace({
          widget: new TableWidget(readTable(state, node.node, handlers), handlers),
          block: true,
        }).range(node.from, node.to),
      );
      return false;
    },
  });

  return Decoration.set(out, true);
}

/**
 * The table half of live preview. Pair it with livePreview(); on its own it
 * renders tables and nothing else.
 */
export function tableRendering(handlers: LivePreviewHandlers): Extension {
  return StateField.define<DecorationSet>({
    create: (state) => buildTables(state, handlers),
    update(value, tr) {
      // The selection matters as much as the document, because moving the cursor
      // into a table is what turns it back into source. The tree comparison
      // catches the parser finishing a document too large to parse in one go.
      if (
        tr.docChanged ||
        tr.selection ||
        syntaxTree(tr.state) !== syntaxTree(tr.startState)
      ) {
        return buildTables(tr.state, handlers);
      }
      return value;
    },
    provide: (f) => EditorView.decorations.from(f),
  });
}
