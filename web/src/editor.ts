// The editor: one CodeMirror instance holding raw markdown, with live preview
// swapped in and out through a compartment.
//
// Switching to "markdown view" is exactly that compartment reconfiguration. The
// document never changes, the cursor stays put, and there is no serialization
// step that could rewrite the file.

import { autocompletion, CompletionContext, CompletionResult } from "@codemirror/autocomplete";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { highlightSelectionMatches, searchKeymap } from "@codemirror/search";
import { Compartment, EditorSelection, EditorState, Extension } from "@codemirror/state";
import {
  drawSelection,
  dropCursor,
  EditorView,
  highlightActiveLine,
  keymap,
  placeholder,
  rectangularSelection,
  ViewUpdate,
} from "@codemirror/view";
import { tags as t } from "@lezer/highlight";

import { livePreview, LivePreviewHandlers } from "./livepreview";
import { hashtagTag, folioMarkdown, wikilinkTag, wikilinkMarkTag } from "./markdown-ext";
import { tableRendering } from "./table";

/** The two ways to look at the same buffer. */
export type Mode = "preview" | "source";

/**
 * Everything the editor needs from the app around it.
 *
 * renderEmbedded is deliberately not in here: mounting a nested read-only view
 * is the editor's own job, and asking the app for it would mean exporting the
 * extension list so the app could rebuild it.
 */
export interface EditorOptions extends Omit<LivePreviewHandlers, "renderEmbedded"> {
  parent: HTMLElement;
  /** Called on every document change, for the autosave timer. */
  onChange(content: string): void;
  /**
   * Notes to offer when the user types `[[`.
   *
   * insert is what actually goes in the brackets: the shortest form that still
   * resolves, which is what Obsidian writes. The app computes it because only it
   * has the vault index to know whether a bare name is ambiguous.
   */
  completeNotes(): Array<{ path: string; title: string; insert: string }>;
  /** Tags to offer when the user types `#`. */
  completeTags(): string[];
  /** Cmd/Ctrl-S: save immediately rather than waiting for the timer. */
  onSaveRequest(): void;
}

const highlight = HighlightStyle.define([
  { tag: t.heading1, class: "cm-fol-tok-h1" },
  { tag: t.heading2, class: "cm-fol-tok-h2" },
  { tag: t.heading3, class: "cm-fol-tok-h3" },
  { tag: [t.heading4, t.heading5, t.heading6], class: "cm-fol-tok-h4" },
  { tag: t.strong, class: "cm-fol-tok-strong" },
  { tag: t.emphasis, class: "cm-fol-tok-em" },
  { tag: t.strikethrough, class: "cm-fol-tok-strike" },
  { tag: t.monospace, class: "cm-fol-tok-code" },
  { tag: t.link, class: "cm-fol-tok-link" },
  { tag: t.url, class: "cm-fol-tok-url" },
  { tag: t.quote, class: "cm-fol-tok-quote" },
  { tag: t.list, class: "cm-fol-tok-list" },
  { tag: t.processingInstruction, class: "cm-fol-tok-mark" },
  { tag: t.comment, class: "cm-fol-tok-comment" },
  { tag: t.keyword, class: "cm-fol-tok-keyword" },
  { tag: t.string, class: "cm-fol-tok-string" },
  { tag: t.number, class: "cm-fol-tok-number" },
  { tag: wikilinkTag, class: "cm-fol-tok-wikilink" },
  { tag: wikilinkMarkTag, class: "cm-fol-tok-mark" },
  { tag: hashtagTag, class: "cm-fol-tok-tag" },
]);

/** Completion for `[[` and `#`, driven by what the app already has loaded. */
function completions(opts: EditorOptions) {
  return (context: CompletionContext): CompletionResult | null => {
    const wiki = context.matchBefore(/\[\[[^\]\n]*/);
    if (wiki) {
      const typed = wiki.text.slice(2).toLowerCase();
      const options = opts
        .completeNotes()
        .filter((n) => n.path.toLowerCase().includes(typed) || n.title.toLowerCase().includes(typed))
        .slice(0, 50)
        .map((n) => ({
          label: n.insert,
          detail: n.title,
          type: "text",
          // Close the brackets for the user; typing "]]" by hand is tedious.
          apply: (view: EditorView, _c: unknown, from: number, to: number) => {
            const insert = n.insert + "]]";
            view.dispatch({
              changes: { from, to, insert },
              selection: { anchor: from + insert.length },
            });
          },
        }));
      return { from: wiki.from + 2, options, validFor: /^[^\]\n]*$/ };
    }

    const tag = context.matchBefore(/(^|\s)#[\p{L}\p{N}_/-]*/u);
    if (tag) {
      const at = tag.text.indexOf("#");
      const typed = tag.text.slice(at + 1).toLowerCase();
      return {
        from: tag.from + at + 1,
        options: opts
          .completeTags()
          .filter((x) => x.toLowerCase().includes(typed))
          .slice(0, 50)
          .map((x) => ({ label: x, type: "keyword" })),
        validFor: /^[\p{L}\p{N}_/-]*$/u,
      };
    }
    return null;
  };
}

/** The editor, wrapping a CodeMirror view and its mode compartment. */
export class Editor {
  private view: EditorView;
  private modeCompartment = new Compartment();
  private mode: Mode = "preview";
  private silencing = false;

  constructor(private opts: EditorOptions) {
    this.view = new EditorView({
      parent: opts.parent,
      state: EditorState.create({
        doc: "",
        extensions: this.extensions(),
      }),
    });
  }

  private extensions(): Extension[] {
    return [
      history(),
      keymap.of([
        {
          key: "Mod-s",
          preventDefault: true,
          run: () => {
            this.opts.onSaveRequest();
            return true;
          },
        },
        ...defaultKeymap,
        ...historyKeymap,
        ...searchKeymap,
        indentWithTab,
      ]),
      // Cursor and selection rendering. drawSelection replaces the browser's
      // native caret with an element of CodeMirror's own, which is what lets it
      // be styled to stand out against a page full of rendered markdown; the
      // native hairline caret is far too easy to lose. highlightActiveLine then
      // says which line you are on, which matters more here than in a plain text
      // editor because live preview deliberately makes that line look different
      // from every other one.
      drawSelection({ cursorBlinkRate: 1000 }),
      dropCursor(),
      highlightActiveLine(),
      highlightSelectionMatches(),
      rectangularSelection(),

      markdown({ base: markdownLanguage, extensions: folioMarkdown, codeLanguages: [] }),
      syntaxHighlighting(highlight),
      autocompletion({ override: [completions(this.opts)], closeOnBlur: true }),
      EditorView.lineWrapping,
      placeholder("Start writing. [[ links to a note, # adds a tag."),
      this.modeCompartment.of(this.preview()),
      EditorView.updateListener.of((u: ViewUpdate) => {
        if (u.docChanged && !this.silencing) {
          this.opts.onChange(u.state.doc.toString());
        }
      }),
      EditorView.theme({ "&": { height: "100%" } }),
    ];
  }

  /**
   * Everything that renders markdown, as one extension.
   *
   * Two pieces, because a table has to replace whole lines and CodeMirror only
   * accepts block decorations from a state field, never from a view plugin.
   */
  private preview(stack: string[] = []): Extension {
    const handlers = this.handlers();
    return [livePreview(handlers, stack), tableRendering(handlers)];
  }

  /**
   * The live-preview handlers, which are the app's plus the one the editor
   * supplies for itself.
   */
  private handlers(): LivePreviewHandlers {
    return {
      ...this.opts,
      renderEmbedded: (host, content, stack) => this.mountEmbedded(host, content, stack),
    };
  }

  /**
   * Mounts a read-only view of content inside host, using this same renderer.
   *
   * An embedded note has to look like a note. Rendering it with a second,
   * simpler markdown pass is how ![[Note]] ends up showing a table as pipes
   * while the note itself shows a table, so it gets the real thing: the same
   * parser, the same decorations, the same table renderer. stack carries the
   * chain of notes already on screen so a cycle stops rather than recursing.
   */
  private mountEmbedded(host: HTMLElement, content: string, stack: string[]): () => void {
    const view = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: content,
        extensions: [
          markdown({ base: markdownLanguage, extensions: folioMarkdown, codeLanguages: [] }),
          syntaxHighlighting(highlight),
          EditorView.lineWrapping,
          // Read-only rather than merely non-editable: an embed is a view of
          // another file, and a keystroke landing here would edit a buffer that
          // is never saved anywhere.
          EditorState.readOnly.of(true),
          EditorView.editable.of(false),
          this.preview(stack),
        ],
      }),
    });
    return () => view.destroy();
  }

  /**
   * Replaces the buffer without reporting a change, used when loading a note or
   * accepting an update that arrived from elsewhere. Reporting it would schedule
   * a save of content we just received, which is a write loop.
   */
  setContent(content: string) {
    this.silencing = true;
    this.view.dispatch({
      changes: { from: 0, to: this.view.state.doc.length, insert: content },
      selection: { anchor: 0 },
      scrollIntoView: true,
    });
    this.silencing = false;
  }

  content(): string {
    return this.view.state.doc.toString();
  }

  /** Toggles between rendered and raw markdown. The document is untouched. */
  setMode(mode: Mode) {
    if (mode === this.mode) return;
    this.mode = mode;
    this.view.dispatch({
      effects: this.modeCompartment.reconfigure(mode === "preview" ? this.preview() : []),
    });
  }

  currentMode(): Mode {
    return this.mode;
  }

  focus() {
    this.view.focus();
  }

  /** Inserts text at the cursor, used by the attachment drop handler. */
  insertAtCursor(text: string) {
    const { from, to } = this.view.state.selection.main;
    this.view.dispatch({
      changes: { from, to, insert: text },
      selection: { anchor: from + text.length },
    });
    this.view.focus();
  }

  /** The cursor's position in the document. */
  cursor(): number {
    return this.view.state.selection.main.head;
  }

  /** Moves the cursor, clamping to the document and scrolling it into view. */
  setCursor(pos: number) {
    const clamped = Math.max(0, Math.min(pos, this.view.state.doc.length));
    this.view.dispatch({
      selection: EditorSelection.cursor(clamped),
      scrollIntoView: true,
    });
  }

  /**
   * Moves the cursor to a heading by its text, reporting whether it was found.
   *
   * This is what a [[Note#Heading]] link needs once the note has loaded, and it
   * matches headings the way the server's anchors do: case-insensitively, on the
   * text rather than the hashes.
   */
  goToHeading(heading: string): boolean {
    const want = heading.trim().replace(/^#+\s*/, "").toLowerCase();
    if (!want) return false;

    const doc = this.view.state.doc;
    for (let i = 1; i <= doc.lines; i++) {
      const line = doc.line(i);
      const m = /^(#{1,6})\s+(.*)$/.exec(line.text);
      if (m && m[2].trim().toLowerCase() === want) {
        this.setCursor(line.from);
        this.view.focus();
        return true;
      }
    }
    return false;
  }

  destroy() {
    this.view.destroy();
  }
}
