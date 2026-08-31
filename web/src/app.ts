// The app shell: sidebar, editor, search palette, and the wiring between them.
//
// This module exports App and starts nothing. src/main.ts is the browser entry
// point that instantiates it. Keeping the two apart is what lets a test mount
// the whole app against a fake API, which is the only kind of test that catches
// a failure to render at all.

import { api, ApiError, ConflictError, Me, Note, NoteEvent, NoteSummary, subscribe } from "./api";
import { Autosave } from "./autosave";
import { Editor, Mode } from "./editor";
import { Route, Router } from "./router";
import { isImagePath, VaultIndex } from "./vault-index";

export class App {
  private me!: Me;
  private editor!: Editor;
  private router: Router;
  private index = new VaultIndex();
  private autosave: Autosave;

  private current: Note | null = null;
  // The heading a [[Note#Heading]] link asked for, applied once the note has
  // loaded. Without this the anchor half of a wikilink was silently dropped.
  private pendingAnchor = "";
  private notes: NoteSummary[] = [];
  private tags: Array<{ tag: string; count: number }> = [];
  private unsubscribe: (() => void) | null = null;
  private sidebarOpen = false;
  private width: Width = "full";

  private el = {
    root: document.getElementById("app") as HTMLElement,
    sidebar: null as unknown as HTMLElement,
    noteList: null as unknown as HTMLElement,
    tagList: null as unknown as HTMLElement,
    editorHost: null as unknown as HTMLElement,
    title: null as unknown as HTMLElement,
    status: null as unknown as HTMLElement,
    banner: null as unknown as HTMLElement,
    backlinks: null as unknown as HTMLElement,
    modeToggle: null as unknown as HTMLButtonElement,
    widthToggle: null as unknown as HTMLButtonElement,
    menuBtn: null as unknown as HTMLButtonElement,
    scrim: null as unknown as HTMLElement,
  };

  constructor() {
    this.router = new Router((r) => this.onRoute(r));
    this.autosave = new Autosave({ idleMs: 800, maxMs: 5000, save: () => this.save() });
  }

  async start() {
    try {
      this.me = await api.me();
    } catch (err) {
      this.fatal(err);
      return;
    }

    this.renderShell();
    this.setupEditor();
    this.setupKeys();

    await this.refreshIndex();
    this.unsubscribe = subscribe((e) => this.onServerEvent(e));

    // Save the pending draft before the tab goes away. This is best effort, and
    // the reason the autosave interval is short.
    window.addEventListener("beforeunload", () => this.autosave.flush());
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "hidden") this.autosave.flush();
    });

    this.router.start();
    this.el.root.classList.remove("is-loading");
  }

  private fatal(err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    const hint =
      err instanceof ApiError && err.status === 503
        ? "tsnotes could not reach tailscaled to work out who you are. It is usually back within a few seconds."
        : err instanceof ApiError && err.status === 403
          ? "This connection has no tailnet user behind it. Tagged nodes need an agent mapping in the config."
          : "";
    this.el.root.innerHTML = "";
    this.el.root.className = "app app-error";
    const box = div("error-box");
    box.append(h("h1", {}, "tsnotes could not start"), h("p", {}, msg));
    if (hint) box.append(h("p", { class: "muted" }, hint));
    this.el.root.append(box);
  }

  // --- shell ---

  private renderShell() {
    const root = this.el.root;
    root.innerHTML = "";
    root.className = "app";

    const sidebar = div("sidebar");
    const brand = div("brand");
    brand.append(h("span", { class: "brand-mark" }, "📝"), h("span", {}, "tsnotes"));
    brand.addEventListener("click", () => this.router.go({ kind: "home" }));

    const searchBtn = h("button", { class: "search-btn", type: "button" }, "Search  ⌘K");
    searchBtn.addEventListener("click", () => this.openPalette());

    const newBtn = h("button", { class: "new-btn", type: "button" }, "New note");
    newBtn.addEventListener("click", () => this.createNote());

    const dailyBtn = h("button", { class: "ghost-btn", type: "button" }, "Today");
    dailyBtn.addEventListener("click", () => this.openDaily());

    this.el.noteList = div("note-list");
    this.el.tagList = div("tag-list");

    sidebar.append(
      brand,
      searchBtn,
      div("sidebar-actions", newBtn, dailyBtn),
      h("h2", { class: "sidebar-heading" }, "Notes"),
      this.el.noteList,
      h("h2", { class: "sidebar-heading" }, "Tags"),
      this.el.tagList,
      div("sidebar-foot", h("span", { class: "muted" }, this.me.displayName || this.me.login)),
    );

    const main = div("main");
    this.el.banner = div("banner hidden");
    this.el.title = h("div", { class: "note-title" }, "");
    this.el.status = h("div", { class: "note-status" }, "");
    this.el.modeToggle = h("button", { class: "mode-toggle", type: "button" }, "Markdown") as HTMLButtonElement;
    this.el.modeToggle.title = "Toggle raw markdown (⌘/)";
    this.el.modeToggle.addEventListener("click", () => this.toggleMode());

    this.el.widthToggle = h("button", { class: "width-toggle", type: "button" }, "Wide") as HTMLButtonElement;
    this.el.widthToggle.title = "Toggle how wide the text runs";
    this.el.widthToggle.addEventListener("click", () => this.toggleWidth());

    // Only visible once the sidebar has become a drawer, which is the only
    // situation where it has anything to do.
    this.el.menuBtn = h("button", {
      class: "menu-btn", type: "button", "aria-label": "Notes and tags", "aria-expanded": "false",
    }, "☰") as HTMLButtonElement;
    this.el.menuBtn.addEventListener("click", () => this.setSidebar(!this.sidebarOpen));

    const header = div("note-header");
    header.append(
      div("note-header-inner",
        this.el.menuBtn, this.el.title, div("spacer"),
        this.el.status, this.el.widthToggle, this.el.modeToggle),
    );

    this.el.editorHost = div("editor-host");
    this.el.backlinks = div("backlinks");

    main.append(this.el.banner, header, this.el.editorHost, this.el.backlinks);

    this.el.scrim = div("scrim");
    this.el.scrim.addEventListener("click", () => this.setSidebar(false));

    root.append(sidebar, main, this.el.scrim);
    this.el.sidebar = sidebar;

    // Full by default: the page should fill the window unless you ask it not
    // to. The other two are there for long-form reading on a wide monitor.
    const saved = readStored("tsnotes.width") as Width | null;
    this.applyWidth(saved && WIDTHS.includes(saved) ? saved : "full");
  }

  // --- layout controls ---

  /**
   * Opens or closes the sidebar drawer.
   *
   * Above the breakpoint the sidebar is always visible and this class does
   * nothing, so the toggle is harmless to leave wired up at any size.
   */
  private setSidebar(open: boolean) {
    this.sidebarOpen = open;
    this.el.root.classList.toggle("sidebar-open", open);
    this.el.menuBtn.setAttribute("aria-expanded", String(open));
  }

  /** Cycles the measure and remembers the choice. */
  private toggleWidth() {
    const next = WIDTHS[(WIDTHS.indexOf(this.width) + 1) % WIDTHS.length];
    this.applyWidth(next);
    writeStored("tsnotes.width", next);
  }

  private applyWidth(width: Width) {
    this.width = width;
    this.el.root.dataset.width = width;
    this.el.widthToggle.textContent = WIDTH_LABELS[width];
    this.el.widthToggle.title = `Text width: ${WIDTH_LABELS[width]}. Click to change.`;
  }

  private setupEditor() {
    this.editor = new Editor({
      parent: this.el.editorHost,
      onChange: () => {
        if (this.current && this.current.perm !== "read") {
          this.setStatus("unsaved");
          this.autosave.schedule();
        }
      },
      onSaveRequest: () => this.autosave.flush(),
      completeNotes: () => this.index.all().map((n) => ({ path: n.path, title: n.title })),
      completeTags: () => this.index.tags(),
      openLink: (target, anchor) => this.followLink(target, anchor),
      openTag: (tag) => this.router.go({ kind: "tag", tag }),
      isResolved: (target) => this.index.has(target, this.current?.path ?? ""),
      resolveEmbed: (target) => {
        const resolved = this.index.resolve(target, this.current?.path ?? "") ?? target;
        if (!isImagePath(resolved)) return null;
        return api.attachmentURL(this.current?.vault ?? this.me.vault, resolved);
      },
    });
  }

  private setupKeys() {
    document.addEventListener("keydown", (e) => {
      const mod = e.metaKey || e.ctrlKey;
      if (mod && e.key.toLowerCase() === "k") {
        e.preventDefault();
        this.openPalette();
      } else if (mod && e.key === "/") {
        e.preventDefault();
        this.toggleMode();
      } else if (e.key === "Escape" && this.sidebarOpen) {
        this.setSidebar(false);
      }
    });
  }

  // --- routing ---

  private async onRoute(route: Route) {
    // Anything pending belongs to the note we are leaving, not the one we are
    // opening.
    this.autosave.flush();
    // On a narrow screen the drawer is covering the page you just asked for.
    this.setSidebar(false);

    switch (route.kind) {
      case "home":
        await this.openDaily(true);
        break;
      case "note":
        await this.openNote(route.vault, route.path, true);
        break;
      case "tag":
        this.renderNoteList(this.notes.filter((n) => n.tags.includes(route.tag)), `#${route.tag}`);
        break;
      case "search":
        await this.runSearch(route.query);
        break;
    }
  }

  private async openNote(vault: string, path: string, fromRoute = false) {
    try {
      const note = await api.readNote(vault, path);
      this.current = note;
      this.editor.setContent(note.content);
      this.el.title.textContent = note.title || path;
      this.el.title.title = `${note.vault}/${note.path}`;
      this.setStatus(note.perm === "read" ? "read only" : "saved");
      this.renderBacklinks(note);
      this.hideBanner();
      if (!fromRoute) this.router.go({ kind: "note", vault, path });
      this.markActive(path);
      this.applyPendingAnchor();
    } catch (err) {
      this.showBanner(describe(err), "error");
    }
  }

  private async openDaily(fromRoute = false) {
    try {
      const note = await api.dailyNote(this.me.vault);
      await this.refreshIndex();
      if (!fromRoute) {
        this.router.go({ kind: "note", vault: note.vault, path: note.path });
      } else {
        this.current = note;
        this.editor.setContent(note.content);
        this.el.title.textContent = note.title;
        this.setStatus("saved");
        this.renderBacklinks(note);
      }
    } catch (err) {
      this.showBanner(describe(err), "error");
    }
  }

  private followLink(target: string, anchor: string) {
    const vault = this.current?.vault ?? this.me.vault;

    // A bare [[#Heading]] is a jump within the note you are already reading.
    if (!target && anchor) {
      this.editor.goToHeading(anchor);
      return;
    }

    const resolved = this.index.resolve(target, this.current?.path ?? "");
    if (resolved) {
      this.pendingAnchor = anchor;
      if (resolved === this.current?.path && vault === this.current?.vault) {
        this.applyPendingAnchor(); // already open; just move the cursor
        return;
      }
      this.router.go({ kind: "note", vault, path: resolved });
      return;
    }
    // A dangling link is an invitation: clicking it creates the note.
    const path = target.endsWith(".md") ? target : target + ".md";
    void this.createNote(path);
  }

  /** Jumps to the heading a wikilink asked for, if it named one. */
  private applyPendingAnchor() {
    const anchor = this.pendingAnchor;
    this.pendingAnchor = "";
    if (!anchor) return;

    if (!this.editor.goToHeading(anchor)) {
      // Better to say the heading is gone than to leave the reader at the top
      // of a long note wondering whether the link worked.
      this.showBanner(`This note has no heading called "${anchor}".`, "warn");
    }
  }

  // --- notes ---

  private async createNote(path?: string) {
    const name =
      path ??
      window.prompt("New note path", suggestName())?.trim();
    if (!name) return;

    try {
      const created = await api.createNote(this.me.vault, name, "");
      await this.refreshIndex();
      this.router.go({ kind: "note", vault: created.vault, path: created.path });
      this.editor.focus();
    } catch (err) {
      this.showBanner(describe(err), "error");
    }
  }

  private async save() {
    const note = this.current;
    if (!note || note.perm === "read") return;

    const content = this.editor.content();
    if (content === note.content) {
      this.setStatus("saved");
      return;
    }

    this.setStatus("saving");
    try {
      const saved = await api.saveNote(note.vault, note.path, content, note.sha256);
      note.content = content;
      note.sha256 = saved.sha256;
      this.setStatus("saved");
      this.hideBanner();
    } catch (err) {
      if (err instanceof ConflictError) {
        // The note changed underneath us. The server kept our draft, so the
        // honest thing is to say where it went rather than pick a winner.
        this.setStatus("conflict");
        this.showBanner(
          `This note changed elsewhere while you were editing. Your version was saved as ${err.conflictPath}.`,
          "warn",
          { label: "Open my version", run: () => this.openNote(note.vault, err.conflictPath) },
          { label: "Reload theirs", run: () => this.openNote(note.vault, note.path) },
        );
        return;
      }
      this.setStatus("error");
      this.showBanner(describe(err), "error");
    }
  }

  // --- server events ---

  private onServerEvent(e: NoteEvent) {
    // Our own saves come back here; ignoring them keeps the editor from
    // reloading the content we just typed.
    if (e.byLogin === this.me.login) {
      void this.refreshIndex();
      return;
    }
    void this.refreshIndex();

    const note = this.current;
    if (!note || e.vault !== note.vault || e.path !== note.path) return;
    if (e.sha256 && e.sha256 === note.sha256) return;

    if (this.autosave.pending || this.editor.content() !== note.content) {
      this.showBanner(
        "This note just changed on disk, and you have unsaved edits. Saving will create a conflict copy.",
        "warn",
        { label: "Load the new version", run: () => this.openNote(note.vault, note.path) },
      );
      return;
    }
    // Nothing local to lose, so take the update silently. This is what makes an
    // edit in Obsidian appear in an open tab.
    void this.openNote(note.vault, note.path, true);
  }

  // --- sidebar ---

  private async refreshIndex() {
    try {
      const [notes, tags] = await Promise.all([api.listNotes(this.me.vault), api.tags()]);
      this.notes = notes;
      this.tags = tags;
      this.index.replace(
        notes.map((n) => ({ vault: n.vault, path: n.path, title: n.title, tags: n.tags })),
      );
      this.renderNoteList(notes, "Notes");
      this.renderTagList();
    } catch {
      // A failed refresh leaves the previous listing in place, which is better
      // than blanking the sidebar over a transient error.
    }
  }

  private renderNoteList(notes: NoteSummary[], heading: string) {
    const list = this.el.noteList;
    list.innerHTML = "";
    (list.previousElementSibling as HTMLElement).textContent = heading;

    if (notes.length === 0) {
      list.append(h("p", { class: "muted empty" }, "No notes yet."));
      return;
    }
    for (const n of notes) {
      const item = h("button", { class: "note-item", type: "button" });
      item.dataset.path = n.path;
      item.append(
        h("span", { class: "note-item-title" }, n.title || n.path),
        h("span", { class: "note-item-path" }, n.path),
      );
      item.addEventListener("click", () => this.router.go({ kind: "note", vault: n.vault, path: n.path }));
      list.append(item);
    }
    this.markActive(this.current?.path ?? "");
  }

  private renderTagList() {
    const list = this.el.tagList;
    list.innerHTML = "";
    for (const t of this.tags.slice(0, 40)) {
      const item = h("button", { class: "tag-item", type: "button" });
      item.append(h("span", {}, "#" + t.tag), h("span", { class: "tag-count" }, String(t.count)));
      item.addEventListener("click", () => this.router.go({ kind: "tag", tag: t.tag }));
      list.append(item);
    }
  }

  private markActive(path: string) {
    for (const el of this.el.noteList.querySelectorAll<HTMLElement>(".note-item")) {
      el.classList.toggle("is-active", el.dataset.path === path);
    }
  }

  private renderBacklinks(note: Note) {
    const box = this.el.backlinks;
    box.innerHTML = "";
    if (note.backlinks.length === 0) return;

    // The inner rail lines the backlinks up with the text above them.
    const inner = div("backlinks-inner");
    inner.append(h("h3", {}, `${note.backlinks.length} note${note.backlinks.length === 1 ? "" : "s"} link here`));
    const list = div("backlink-list");
    for (const b of note.backlinks) {
      const item = h("button", { class: "backlink", type: "button" }, b.title || b.path);
      item.addEventListener("click", () => this.router.go({ kind: "note", vault: note.vault, path: b.path }));
      list.append(item);
    }
    inner.append(list);
    box.append(inner);
  }

  // --- search palette ---

  private openPalette() {
    const overlay = div("palette-overlay");
    const box = div("palette");
    const input = h("input", {
      class: "palette-input",
      type: "text",
      placeholder: "Search notes. Try tag:go, path:Daily, \"a phrase\", -exclude",
      autocomplete: "off",
    }) as HTMLInputElement;
    const results = div("palette-results");
    box.append(input, results);
    overlay.append(box);
    document.body.append(overlay);
    input.focus();

    const close = () => overlay.remove();
    overlay.addEventListener("mousedown", (e) => {
      if (e.target === overlay) close();
    });

    let seq = 0;
    const run = async () => {
      const mine = ++seq;
      const q = input.value.trim();
      try {
        const res = await api.search(q, 30);
        if (mine !== seq) return; // a later keystroke already won
        results.innerHTML = "";
        if (res.hits.length === 0) {
          results.append(h("p", { class: "muted empty" }, q ? "Nothing matched." : "Type to search."));
          return;
        }
        for (const hit of res.hits) {
          const item = h("button", { class: "palette-hit", type: "button" });
          const title = h("span", { class: "palette-hit-title" }, hit.title || hit.path);
          if (hit.vault !== this.me.vault) {
            title.append(h("span", { class: "palette-hit-owner" }, hit.ownerLogin));
          }
          const snippet = h("span", { class: "palette-hit-snippet" });
          // The server marks matches with <mark> and escapes everything else,
          // so this is the one place innerHTML is safe and useful.
          snippet.innerHTML = hit.snippet;
          item.append(title, snippet, h("span", { class: "palette-hit-path" }, hit.path));
          item.addEventListener("click", () => {
            close();
            this.router.go({ kind: "note", vault: hit.vault, path: hit.path });
          });
          results.append(item);
        }
      } catch (err) {
        results.innerHTML = "";
        results.append(h("p", { class: "error-text" }, describe(err)));
      }
    };

    let timer: ReturnType<typeof setTimeout> | null = null;
    input.addEventListener("input", () => {
      if (timer) clearTimeout(timer);
      timer = setTimeout(run, 120);
    });
    input.addEventListener("keydown", (e) => {
      if (e.key === "Escape") close();
      if (e.key === "Enter") {
        const first = results.querySelector<HTMLElement>(".palette-hit");
        first?.click();
      }
    });
    void run();
  }

  private async runSearch(query: string) {
    try {
      const res = await api.search(query, 100);
      this.renderNoteList(res.hits, `Results for "${query}"`);
    } catch (err) {
      this.showBanner(describe(err), "error");
    }
  }

  // --- chrome ---

  private toggleMode() {
    const next: Mode = this.editor.currentMode() === "preview" ? "source" : "preview";
    this.editor.setMode(next);
    this.el.modeToggle.textContent = next === "preview" ? "Markdown" : "Preview";
    this.el.modeToggle.classList.toggle("is-source", next === "source");
  }

  private setStatus(state: "saved" | "saving" | "unsaved" | "error" | "conflict" | "read only") {
    this.el.status.textContent = state;
    this.el.status.className = `note-status status-${state.replace(" ", "-")}`;
  }

  private showBanner(
    message: string,
    kind: "warn" | "error",
    ...actions: Array<{ label: string; run: () => void }>
  ) {
    const banner = this.el.banner;
    banner.innerHTML = "";
    banner.className = `banner banner-${kind}`;
    banner.append(h("span", {}, message));
    for (const a of actions) {
      const btn = h("button", { class: "banner-action", type: "button" }, a.label);
      btn.addEventListener("click", () => {
        this.hideBanner();
        a.run();
      });
      banner.append(btn);
    }
    const dismiss = h("button", { class: "banner-dismiss", type: "button" }, "×");
    dismiss.addEventListener("click", () => this.hideBanner());
    banner.append(dismiss);
  }

  private hideBanner() {
    this.el.banner.className = "banner hidden";
    this.el.banner.innerHTML = "";
  }

  stop() {
    this.unsubscribe?.();
    this.editor.destroy();
  }
}

// --- small DOM helpers, in place of a framework ---

function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs: Record<string, string> = {},
  ...children: Array<string | Node>
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  el.append(...children);
  return el;
}

function div(className: string, ...children: Array<string | Node>): HTMLDivElement {
  return h("div", { class: className }, ...children);
}

/** How wide the text runs. */
type Width = "comfortable" | "wide" | "full";

/** The order the width control cycles through, starting at the default. */
const WIDTHS: Width[] = ["full", "wide", "comfortable"];

const WIDTH_LABELS: Record<Width, string> = {
  wide: "Wide",
  comfortable: "Narrow",
  full: "Full",
};

/**
 * Reads a saved preference.
 *
 * localStorage throws rather than returning null in a private window or a
 * browser with site data blocked, and losing a layout preference is not worth
 * taking the app down for.
 */
function readStored(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeStored(key: string, value: string) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // Nothing to do: the preference simply will not survive a reload.
  }
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** A sensible default name for a new note: today's date under Notes/. */
function suggestName(): string {
  const now = new Date();
  const stamp = now.toISOString().slice(0, 10);
  return `Notes/${stamp}.md`;
}
