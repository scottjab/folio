// The app shell: sidebar, editor, search palette, and the wiring between them.
//
// This module exports App and starts nothing. src/main.ts is the browser entry
// point that instantiates it. Keeping the two apart is what lets a test mount
// the whole app against a fake API, which is the only kind of test that catches
// a failure to render at all.

import {
  api,
  ApiError,
  ConflictError,
  Me,
  Note,
  NoteEvent,
  NoteSummary,
  Prefs,
  subscribe,
} from "./api";
import type { EmbedResult } from "./livepreview";
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
  // The note a save is in flight for. The server emits the change event as it
  // writes, so that event can arrive before our own response does, and it must
  // not be mistaken for somebody else editing underneath us.
  private savingKey: string | null = null;
  private sidebarOpen = false;
  private width: Width = "full";
  // Resolved embeds, keyed by vault, note, target and anchor. Cleared whenever
  // the vault changes, which is the only thing that can invalidate one.
  private embedCache = new Map<string, Promise<EmbedResult>>();

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
        ? "folio could not reach tailscaled to work out who you are. It is usually back within a few seconds."
        : err instanceof ApiError && err.status === 403
          ? "This connection has no tailnet user behind it. Tagged nodes need an agent mapping in the config."
          : "";
    this.el.root.innerHTML = "";
    this.el.root.className = "app app-error";
    const box = div("error-box");
    box.append(h("h1", {}, "folio could not start"), h("p", {}, msg));
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
    brand.append(h("span", { class: "brand-mark" }, "📝"), h("span", {}, "folio"));
    brand.addEventListener("click", () => this.router.go({ kind: "home" }));

    const searchBtn = h("button", { class: "search-btn", type: "button" }, "Search  ⌘K");
    searchBtn.addEventListener("click", () => this.openPalette());

    const newBtn = h("button", { class: "new-btn", type: "button" }, "New note");
    newBtn.addEventListener("click", () => this.createNote());

    const dailyBtn = h("button", { class: "ghost-btn", type: "button" }, "Today");
    dailyBtn.addEventListener("click", () => this.openDaily());

    const settingsBtn = h("button", { class: "settings-btn", type: "button" }, "Settings");
    settingsBtn.title = "Where new attachments go";
    settingsBtn.addEventListener("click", () => void this.openSettings());

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
      div("sidebar-foot", h("span", { class: "muted" }, this.me.displayName || this.me.login), settingsBtn),
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
    const saved = readStored("folio.width") as Width | null;
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
    writeStored("folio.width", next);
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
      completeNotes: () =>
        this.index.all().map((n) => ({
          path: n.path,
          title: n.title,
          // Obsidian's "shortest path when possible": the bare name unless the
          // vault holds two notes that would answer to it.
          insert: this.index.shortest(n.path, this.current?.path ?? ""),
        })),
      completeTags: () => this.index.tags(),
      openLink: (target, anchor) => this.followLink(target, anchor),
      openTag: (tag) => this.router.go({ kind: "tag", tag }),
      isResolved: (target) => this.index.has(target, this.current?.path ?? ""),
      resolveEmbed: (target) => {
        const resolved = this.index.resolve(target, this.current?.path ?? "") ?? target;
        if (!isImagePath(resolved)) return null;
        return api.attachmentURL(this.current?.vault ?? this.me.vault, resolved);
      },
      loadEmbed: (target, anchor) => this.loadEmbed(target, anchor),
    });
    this.setupDropAndPaste();
  }

  /**
   * Resolves an ![[embed]] through the server, memoized per note.
   *
   * The cache matters more than it looks: a widget is rebuilt whenever the
   * decoration set is, which is on every keystroke, so without it a note with
   * three embeds would issue three requests per character typed. It is dropped
   * whenever anything in the vault changes, which is the only event that can
   * make an answer stale.
   */
  private loadEmbed(target: string, anchor: string): Promise<EmbedResult> {
    const vault = this.current?.vault ?? this.me.vault;
    const from = this.current?.path ?? "";
    const key = [vault, from, target, anchor].join("\u0000");

    const hit = this.embedCache.get(key);
    if (hit) return hit;

    const full = anchor ? `${target}#${anchor}` : target;
    const pending = api
      .embed(vault, from, full)
      .then((e): EmbedResult => ({
        kind: e.kind,
        path: e.path,
        title: e.title,
        content: e.content,
        truncated: e.truncated,
        href: e.kind === "attachment" && e.path ? api.attachmentURL(vault, e.path) : undefined,
      }))
      .catch((err) => {
        // A failed lookup must not be remembered, or a blip leaves the embed
        // broken until the next vault change.
        this.embedCache.delete(key);
        throw err;
      });

    this.embedCache.set(key, pending);
    return pending;
  }

  /**
   * Drop a file on the editor, or paste one, and it is uploaded and linked.
   *
   * Both land on the same path deliberately: a screenshot pasted from the
   * clipboard and the same file dragged out of a folder should end up in the
   * same place with the same kind of link.
   */
  private setupDropAndPaste() {
    const host = this.el.editorHost;

    // Without preventing dragover the browser refuses the drop, and without the
    // class there is nothing telling you the editor will take it.
    host.addEventListener("dragover", (e) => {
      if (!e.dataTransfer?.types.includes("Files")) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = "copy";
      host.classList.add("is-dropping");
    });
    host.addEventListener("dragleave", (e) => {
      if (e.target === host) host.classList.remove("is-dropping");
    });
    host.addEventListener("drop", (e) => {
      const files = [...(e.dataTransfer?.files ?? [])];
      host.classList.remove("is-dropping");
      if (files.length === 0) return;
      e.preventDefault();
      void this.uploadFiles(files);
    });

    host.addEventListener("paste", (e) => {
      const items = [...(e.clipboardData?.items ?? [])].filter((i) => i.kind === "file");
      if (items.length === 0) return; // ordinary text; let CodeMirror have it
      const files = items.map((i) => i.getAsFile()).filter((f): f is File => f !== null);
      if (files.length === 0) return;
      e.preventDefault();
      // A clipboard image has no name worth keeping ("image.png" every time), so
      // the server names it from the clock the way Obsidian does. A file copied
      // in a file manager does have one, and keeps it.
      void this.uploadFiles(files, { keepNames: false });
    });
  }

  private async uploadFiles(files: File[], opts: { keepNames?: boolean } = {}) {
    const note = this.current;
    if (!note) {
      this.showBanner("Open a note before adding a file to it.", "warn");
      return;
    }
    if (note.perm === "read") {
      this.showBanner("This note is read only, so its vault will not take uploads.", "warn");
      return;
    }
    const keepNames = opts.keepNames ?? true;

    this.setStatus("uploading");
    try {
      for (const file of files) {
        const up = await api.upload(note.vault, note.path, keepNames ? file.name : "", file);
        // An image embeds; anything else is a link, because a rendered PDF in
        // the middle of a paragraph is not what a reader wants.
        const bang = isImagePath(up.path) ? "!" : "";
        this.editor.insertAtCursor(`${bang}[[${up.link}]]`);
      }
      // The new file has to be in the index before the embed we just wrote can
      // resolve, or it renders as missing until the next refresh.
      this.embedCache.clear();
      await this.refreshIndex();
      this.setStatus("unsaved");
      this.autosave.schedule();
    } catch (err) {
      this.setStatus("saved");
      this.showBanner(describe(err), "error");
    }
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
    this.savingKey = noteKey(note.vault, note.path);
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
    } finally {
      this.savingKey = null;
    }
  }

  // --- server events ---

  private onServerEvent(e: NoteEvent) {
    // A note changing is the only thing that can make a resolved embed stale,
    // whether it was edited here, in Obsidian, or by an agent.
    this.embedCache.clear();
    void this.refreshIndex();

    const note = this.current;
    if (!note || e.vault !== note.vault) return;
    // A move event names where the note went, so the note we have open is the
    // one it came from.
    if (e.path !== note.path && e.oldPath !== note.path) return;

    // The echo of a save we are still waiting on. Its response is the
    // authoritative answer, conflict included, so this event has nothing to add.
    if (this.savingKey === noteKey(note.vault, note.path)) return;
    // The echo of a save that has already landed.
    if (e.sha256 && e.sha256 === note.sha256) return;

    // The login is not a useful test of whose change this is: it is your own
    // whenever the other writer is you in another tab, on your phone, in the
    // terminal client, or an agent acting as you over MCP. Only the hash can
    // tell an echo from a change worth showing.

    if (e.kind === "note.deleted") {
      this.showBanner(
        "This note was deleted elsewhere. What is on screen is the last version you had.",
        "warn",
      );
      return;
    }

    if (this.autosave.pending || this.editor.content() !== note.content) {
      this.showBanner(
        "This note just changed elsewhere, and you have unsaved edits. Saving will create a conflict copy.",
        "warn",
        { label: "Load the new version", run: () => this.openNote(e.vault, e.path) },
      );
      return;
    }
    // Nothing local to lose, so take the update. This is what makes an edit in
    // Obsidian, in another tab, in the terminal client, or by an agent show up
    // here without a refresh. A move changes the path, so the address bar
    // follows it; a plain update leaves the history alone.
    void this.openNote(e.vault, e.path, e.path === note.path);
  }

  // --- sidebar ---

  private async refreshIndex() {
    try {
      const [notes, tags, files] = await Promise.all([
        api.listNotes(this.me.vault),
        api.tags(),
        // Attachments belong in the index because ![[diagram.png]] resolves by
        // the same rule [[Some Note]] does. Without them the short form of an
        // embed dangles and the editor asks for a file at the vault root.
        //
        // Both listings are of your own vault, so the index describes exactly
        // one. A shared note open from someone else's vault therefore resolves
        // its embeds through the server instead, which is the correct answer
        // rather than a fast one: an index holding half of two vaults would
        // point a link at a file that is not the one the indexer means.
        api.listAttachments(this.me.vault).catch(() => [] as Array<{ path: string }>),
      ]);
      this.notes = notes;
      this.tags = tags;
      this.index.replace(
        notes.map((n) => ({ vault: n.vault, path: n.path, title: n.title, tags: n.tags })),
        files.map((f) => f.path),
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

  // --- settings ---

  /**
   * The settings sheet. Only what both clients share lives here: the attachment
   * folder is a server-side preference because a drop in the browser and an
   * attach in the terminal have to file things in the same place, while the
   * text width stays in this browser's localStorage where it belongs.
   */
  private async openSettings() {
    let current: Prefs;
    try {
      current = await api.prefs();
    } catch (err) {
      this.showBanner(describe(err), "error");
      return;
    }

    const overlay = div("palette-overlay");
    const box = div("settings");
    const close = () => overlay.remove();
    overlay.addEventListener("mousedown", (e) => {
      if (e.target === overlay) close();
    });

    box.append(h("h2", {}, "Settings"));
    box.append(h("p", { class: "muted" }, "Where new attachments go, by Obsidian's four options."));

    const select = h("select", { class: "settings-select" }) as HTMLSelectElement;
    for (const [value, label] of MODES) {
      const opt = h("option", { value }, label) as HTMLOptionElement;
      opt.selected = current.attachmentMode === value;
      select.append(opt);
    }

    const folder = h("input", {
      class: "settings-input",
      type: "text",
      placeholder: "attachments",
    }) as HTMLInputElement;
    folder.value = current.attachmentFolder;

    const folderRow = div("settings-row", h("label", {}, "Folder"), folder);
    // Two of the four modes have no folder to name, so hiding the field is the
    // difference between a setting and a puzzle.
    const syncFolderRow = () => {
      folderRow.hidden = select.value === "vault" || select.value === "current";
    };
    select.addEventListener("change", syncFolderRow);
    syncFolderRow();

    const status = h("p", { class: "settings-status muted" }, "");
    const save = h("button", { class: "new-btn", type: "button" }, "Save");
    save.addEventListener("click", async () => {
      status.textContent = "";
      try {
        await api.setPrefs({
          attachmentMode: select.value,
          attachmentFolder: folder.value.trim(),
        });
        close();
      } catch (err) {
        // The server validates the folder, so this is where a path that would
        // escape the vault or never be served comes back.
        status.textContent = describe(err);
        status.className = "settings-status error-text";
      }
    });

    box.append(
      div("settings-row", h("label", {}, "New attachments"), select),
      folderRow,
      status,
      div("settings-actions", save),
    );
    overlay.append(box);
    document.body.append(overlay);
    select.focus();

    overlay.addEventListener("keydown", (e) => {
      if (e.key === "Escape") close();
    });
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

  private setStatus(
    state: "saved" | "saving" | "unsaved" | "error" | "conflict" | "read only" | "uploading",
  ) {
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

/** Names a note by the two things that identify one: its vault and its path. */
function noteKey(vault: string, path: string): string {
  return `${vault}\n${path}`;
}

/** A sensible default name for a new note: today's date under Notes/. */
function suggestName(): string {
  const now = new Date();
  const stamp = now.toISOString().slice(0, 10);
  return `Notes/${stamp}.md`;
}

/**
 * The attachment-folder modes, by the names Obsidian gives them in its own
 * settings, so a vault open in both can be made to agree.
 */
const MODES: Array<[string, string]> = [
  ["folder", "In one folder"],
  ["vault", "In the vault root"],
  ["current", "Beside the note"],
  ["subfolder", "In a subfolder beside the note"],
];
