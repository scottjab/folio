import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "./app";

/**
 * Mounts the whole app against a fake API.
 *
 * The bug that prompted these tests showed up as a blank page and one console
 * error: a module threw while loading, so nothing rendered. No amount of testing
 * pure helpers can see that. Starting the real app can.
 */

const me = {
  login: "me@example.com",
  displayName: "Me",
  vault: "me-example.com",
  isAgent: false,
};

const note = {
  vault: "me-example.com",
  ownerLogin: "me@example.com",
  path: "Daily/2026-08-31.md",
  title: "Monday",
  tags: ["daily"],
  sha256: "abc123",
  updatedAt: "2026-08-31T00:00:00Z",
  content: "---\ntags: [daily]\n---\n# Monday\n\nSee [[Projects/tsnotes]] and #go127.\n",
  size: 70,
  modTime: "2026-08-31T00:00:00Z",
  perm: "write" as const,
  backlinks: [{ path: "Projects/tsnotes.md", title: "tsnotes", kind: "wiki" }],
};

/** Routes a request path to a canned response. */
function fakeFetch(overrides: Record<string, unknown> = {}) {
  const routes: Record<string, unknown> = {
    "/api/me": me,
    "/api/vaults/me-example.com/daily": note,
    "/api/vaults/me-example.com/notes": { notes: [note] },
    "/api/tags": { tags: [{ tag: "daily", count: 1 }, { tag: "go127", count: 1 }] },
    ...overrides,
  };

  return vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input), "http://localhost");
    const key = Object.keys(routes).find(
      (k) => url.pathname === k || url.pathname.startsWith(k + "/"),
    );
    const body = key ? routes[key] : null;

    if (body === null) {
      return new Response(JSON.stringify({ error: "not found" }), {
        status: 404,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (body instanceof Response) return body;
    return new Response(JSON.stringify(body), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  });
}

/** jsdom has no EventSource, and the app opens one on startup. */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  listeners: Record<string, Array<(e: MessageEvent) => void>> = {};

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }
  addEventListener(kind: string, fn: (e: MessageEvent) => void) {
    (this.listeners[kind] ??= []).push(fn);
  }
  close() {}
  emit(kind: string, data: unknown) {
    for (const fn of this.listeners[kind] ?? []) {
      fn(new MessageEvent(kind, { data: JSON.stringify(data) }));
    }
  }
}

async function startApp(overrides?: Record<string, unknown>) {
  const app = new App();
  await app.start();
  // Let the sidebar refresh and the initial route settle.
  await new Promise((r) => setTimeout(r, 0));
  void overrides;
  return app;
}

describe("App", () => {
  beforeEach(() => {
    document.body.innerHTML = '<div id="app" class="app is-loading"></div>';
    vi.stubGlobal("fetch", fakeFetch());
    vi.stubGlobal("EventSource", FakeEventSource);
    FakeEventSource.instances = [];
    history.replaceState(null, "", "/");
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = "";
  });

  it("renders the shell rather than a blank page", async () => {
    await startApp();
    const root = document.getElementById("app")!;

    expect(root.classList.contains("is-loading")).toBe(false);
    expect(root.querySelector(".sidebar")).not.toBeNull();
    expect(root.querySelector(".main")).not.toBeNull();
    expect(root.querySelector(".cm-editor")).not.toBeNull();
    expect(root.textContent).toContain("tsnotes");
  });

  it("shows who you are", async () => {
    await startApp();
    expect(document.querySelector(".sidebar-foot")?.textContent).toContain("Me");
  });

  it("lists notes and tags in the sidebar", async () => {
    await startApp();
    expect(document.querySelector(".note-item-title")?.textContent).toBe("Monday");
    const tags = [...document.querySelectorAll(".tag-item")].map((e) => e.textContent);
    expect(tags.join(" ")).toContain("#daily");
  });

  it("loads the daily note and renders its markdown", async () => {
    await startApp();
    const content = document.querySelector(".cm-content")!.textContent ?? "";
    expect(content).toContain("Monday");
    // The wikilink renders as a widget rather than raw brackets.
    expect(document.querySelector(".cm-tsn-wikilink")).not.toBeNull();
    expect(document.querySelector(".cm-tsn-tag")).not.toBeNull();
  });

  it("shows backlinks", async () => {
    await startApp();
    expect(document.querySelector(".backlinks")?.textContent).toContain("tsnotes");
  });

  it("subscribes to the event stream", async () => {
    await startApp();
    expect(FakeEventSource.instances.map((e) => e.url)).toEqual(["/api/events"]);
  });

  it("toggles between preview and markdown", async () => {
    await startApp();
    const toggle = document.querySelector<HTMLButtonElement>(".mode-toggle")!;
    expect(toggle.textContent).toBe("Markdown");
    expect(document.querySelector(".cm-tsn-wikilink")).not.toBeNull();

    toggle.click();
    expect(toggle.textContent).toBe("Preview");
    expect(document.querySelector(".cm-tsn-wikilink")).toBeNull();
    expect(document.querySelector(".cm-content")?.textContent).toContain("[[Projects/tsnotes]]");
  });

  it("opens the search palette", async () => {
    await startApp();
    document.querySelector<HTMLButtonElement>(".search-btn")!.click();
    expect(document.querySelector(".palette-input")).not.toBeNull();
  });

  it("reports a failure to identify instead of rendering nothing", async () => {
    // A 503 here means tailscaled is briefly unreachable. The user should see
    // that, not an empty page.
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(JSON.stringify({ error: "identity service unavailable" }), {
          status: 503,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    const app = new App();
    await app.start();

    const root = document.getElementById("app")!;
    expect(root.classList.contains("app-error")).toBe(true);
    expect(root.textContent).toContain("could not start");
    expect(root.textContent).toContain("tailscaled");
  });
});

describe("wikilink anchors", () => {
  beforeEach(() => {
    document.body.innerHTML = '<div id="app" class="app is-loading"></div>';
    vi.stubGlobal("EventSource", FakeEventSource);
    FakeEventSource.instances = [];
    history.replaceState(null, "", "/");
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = "";
  });

  it("jumps to the heading a [[Note#Heading]] link names", async () => {
    const sectioned = {
      ...note,
      content: "# Top\n\nintro\n\n## Design\n\ndetail\n\nSee [[Daily/2026-08-31#Design]].\n",
      backlinks: [],
    };
    vi.stubGlobal("fetch", fakeFetch({ "/api/vaults/me-example.com/daily": sectioned }));

    await startApp();
    await new Promise((r) => setTimeout(r, 60));

    const link = document.querySelector<HTMLElement>(".cm-tsn-wikilink");
    expect(link).not.toBeNull();
    link!.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    await new Promise((r) => setTimeout(r, 60));

    expect(document.querySelector(".cm-activeLine")?.textContent).toBe("## Design");
  });

  it("says so when the heading does not exist", async () => {
    const sectioned = {
      ...note,
      content: "# Top\n\nSee [[Daily/2026-08-31#Missing]].\n",
      backlinks: [],
    };
    vi.stubGlobal("fetch", fakeFetch({ "/api/vaults/me-example.com/daily": sectioned }));

    await startApp();
    await new Promise((r) => setTimeout(r, 60));

    document.querySelector<HTMLElement>(".cm-tsn-wikilink")!.dispatchEvent(
      new MouseEvent("mousedown", { bubbles: true }),
    );
    await new Promise((r) => setTimeout(r, 60));

    const banner = document.querySelector(".banner");
    expect(banner?.className).toContain("banner-warn");
    expect(banner?.textContent).toContain("Missing");
  });
});
