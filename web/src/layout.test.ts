import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "./app";

/**
 * The editor is the whole point of the app, so it should behave like a page you
 * write on rather than a narrow column stranded in whitespace, and it has to
 * work on a screen too small to show the sidebar and the page at once.
 */

const css = readFileSync(join(process.cwd(), "src", "style.css"), "utf8");
const rules = css.replace(/\/\*[\s\S]*?\*\//g, "");

const me = { login: "me@x.com", displayName: "Me", vault: "me-x.com", isAgent: false };
const note = {
  vault: "me-x.com", ownerLogin: "me@x.com", path: "Daily/2026-08-31.md",
  title: "Monday", tags: [], sha256: "abc", updatedAt: "2026-08-31T00:00:00Z",
  content: "# Monday\n\ntext\n", size: 16, modTime: "2026-08-31T00:00:00Z",
  perm: "write" as const, backlinks: [],
};

function fakeFetch() {
  const routes: Record<string, unknown> = {
    "/api/me": me,
    "/api/vaults/me-x.com/daily": note,
    "/api/vaults/me-x.com/notes": { notes: [note] },
    "/api/tags": { tags: [] },
  };
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input), "http://localhost");
    const key = Object.keys(routes).find(
      (k) => url.pathname === k || url.pathname.startsWith(k + "/"),
    );
    return new Response(JSON.stringify(key ? routes[key] : { error: "nope" }), {
      status: key ? 200 : 404,
      headers: { "Content-Type": "application/json" },
    });
  });
}

class FakeEventSource {
  addEventListener() {}
  close() {}
}

async function startApp() {
  const app = new App();
  await app.start();
  await new Promise((r) => setTimeout(r, 0));
  return app;
}

describe("the writing surface", () => {
  it("is not pinned to a narrow fixed column", () => {
    // 46rem in the middle of a wide screen is a column, not a notebook.
    const rule = /\.cm-editor \.cm-content\s*\{([^}]*)\}/.exec(rules);
    expect(rule).not.toBeNull();
    const maxWidth = /max-width:\s*([^;]+);/.exec(rule![1]);
    expect(maxWidth, "the measure should come from a token, not a literal").not.toBeNull();
    expect(maxWidth![1]).toContain("var(--measure-box)");
  });

  it("adds the padding on top of the measure rather than inside it", () => {
    // CodeMirror sets box-sizing: border-box on the content, so capping at the
    // measure directly would have the padding eat into the text and widen the
    // margins at the same time. That is most of why it read as a column.
    expect(rules).toMatch(/--measure-box:\s*calc\(var\(--measure\)\s*\+\s*2\s*\*\s*var\(--page-pad\)\)/);
  });

  it("lets the wide measure grow with the window", () => {
    // A fixed cap leaves a bigger and bigger margin as the screen grows. This
    // one tracks the viewport until lines would get too long to read.
    const wide = /--measure-wide:\s*clamp\(([^)]*)\)/.exec(rules);
    expect(wide, "the wide measure should be a clamp, not a fixed width").not.toBeNull();
    expect(wide![1]).toMatch(/vw/);
  });

  it("has a narrower option and an uncapped one", () => {
    expect(rules).toMatch(/--measure-comfortable:\s*[\d.]+rem/);
    expect(rules).toMatch(/\[data-width="full"\][^}]*--measure-box:\s*none/);
  });

  it("falls back to uncapped if the width attribute never gets set", () => {
    // If the script fails before applyWidth runs, the page should still fill
    // the window rather than collapsing to whatever the last cap happened to be.
    expect(rules).toMatch(/:root\s*\{[^}]*--measure-box:\s*none/s);
  });

  it("scales its margins with the viewport rather than fixing them", () => {
    // A notebook has margins proportional to the page, not 20px whatever the
    // screen.
    expect(rules).toMatch(/--page-pad:\s*clamp\([^)]*vw[^)]*\)/);
  });

  it("sizes the sidebar against the viewport too", () => {
    expect(rules).toMatch(/--sidebar-w:\s*clamp\(/);
  });

  it("uses dynamic viewport height, so mobile browser chrome does not crop it", () => {
    expect(rules).toMatch(/height:\s*100dvh/);
  });
});

describe("narrow screens", () => {
  beforeEach(() => {
    document.body.innerHTML = '<div id="app" class="app is-loading"></div>';
    vi.stubGlobal("fetch", fakeFetch());
    vi.stubGlobal("EventSource", FakeEventSource);
    localStorage.clear();
    history.replaceState(null, "", "/");
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = "";
  });

  it("offers a way to open the sidebar", async () => {
    // The old stylesheet hid the sidebar below 820px and offered no way to get
    // it back, which made the app unusable on a phone.
    await startApp();
    expect(document.querySelector(".menu-btn")).not.toBeNull();
  });

  it("toggles the sidebar open and shut", async () => {
    await startApp();
    const root = document.getElementById("app")!;
    const menu = document.querySelector<HTMLButtonElement>(".menu-btn")!;

    expect(root.classList.contains("sidebar-open")).toBe(false);
    menu.click();
    expect(root.classList.contains("sidebar-open")).toBe(true);
    menu.click();
    expect(root.classList.contains("sidebar-open")).toBe(false);
  });

  it("closes when the backdrop is tapped", async () => {
    await startApp();
    const root = document.getElementById("app")!;
    document.querySelector<HTMLButtonElement>(".menu-btn")!.click();

    document.querySelector<HTMLElement>(".scrim")!.click();
    expect(root.classList.contains("sidebar-open")).toBe(false);
  });

  it("closes on Escape", async () => {
    await startApp();
    const root = document.getElementById("app")!;
    document.querySelector<HTMLButtonElement>(".menu-btn")!.click();

    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" }));
    expect(root.classList.contains("sidebar-open")).toBe(false);
  });

  it("closes after you pick a note, so the page is not left covered", async () => {
    await startApp();
    const root = document.getElementById("app")!;
    document.querySelector<HTMLButtonElement>(".menu-btn")!.click();

    document.querySelector<HTMLButtonElement>(".note-item")!.click();
    await new Promise((r) => setTimeout(r, 0));
    expect(root.classList.contains("sidebar-open")).toBe(false);
  });

  it("reports the sidebar's state to assistive technology", async () => {
    await startApp();
    const menu = document.querySelector<HTMLButtonElement>(".menu-btn")!;
    expect(menu.getAttribute("aria-expanded")).toBe("false");
    menu.click();
    expect(menu.getAttribute("aria-expanded")).toBe("true");
  });

  it("collapses to one column and turns the sidebar into a drawer", () => {
    const narrow = rules.slice(rules.indexOf("@media (max-width:"));
    expect(narrow).toMatch(/grid-template-columns:\s*minmax\(0, ?1fr\)/);
    expect(narrow).toMatch(/\.sidebar\s*\{[^}]*position:\s*fixed/);
    expect(narrow).toMatch(/sidebar-open[^}]*transform:\s*none/);
  });
});

describe("the width control", () => {
  beforeEach(() => {
    document.body.innerHTML = '<div id="app" class="app is-loading"></div>';
    vi.stubGlobal("fetch", fakeFetch());
    vi.stubGlobal("EventSource", FakeEventSource);
    localStorage.clear();
    history.replaceState(null, "", "/");
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = "";
  });

  it("fills the window by default", async () => {
    // The page should use the space it has unless you ask it not to.
    await startApp();
    expect(document.getElementById("app")!.dataset.width).toBe("full");
  });

  it("cycles through every width and back", async () => {
    await startApp();
    const root = document.getElementById("app")!;
    const btn = document.querySelector<HTMLButtonElement>(".width-toggle")!;

    const seen = [root.dataset.width];
    for (let i = 0; i < 3; i++) {
      btn.click();
      seen.push(root.dataset.width);
    }
    expect(seen).toEqual(["full", "wide", "comfortable", "full"]);
  });

  it("says which width is in effect", async () => {
    await startApp();
    const btn = document.querySelector<HTMLButtonElement>(".width-toggle")!;
    expect(btn.textContent).toBe("Full");
    btn.click();
    expect(btn.textContent).toBe("Wide");
    btn.click();
    expect(btn.textContent).toBe("Narrow");
  });

  it("ignores a stored value it does not recognise", async () => {
    localStorage.setItem("tsnotes.width", "enormous");
    await startApp();
    expect(document.getElementById("app")!.dataset.width).toBe("full");
  });

  it("remembers the choice across a reload", async () => {
    await startApp();
    const btn = document.querySelector<HTMLButtonElement>(".width-toggle")!;
    btn.click(); // full -> wide
    btn.click(); // wide -> comfortable
    expect(localStorage.getItem("tsnotes.width")).toBe("comfortable");

    document.body.innerHTML = '<div id="app" class="app is-loading"></div>';
    await startApp();
    expect(document.getElementById("app")!.dataset.width).toBe("comfortable");
  });

  it("survives localStorage being unavailable", async () => {
    // Private windows and locked-down browsers throw on access rather than
    // returning null, and losing a width preference must not take the app down.
    const boom = () => {
      throw new Error("denied");
    };
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(boom);
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(boom);

    await startApp();
    const root = document.getElementById("app")!;
    expect(root.dataset.width).toBe("full");
    expect(() => document.querySelector<HTMLButtonElement>(".width-toggle")!.click()).not.toThrow();
    expect(root.dataset.width).toBe("wide");
    vi.restoreAllMocks();
  });

  it("drives the measure from the chosen mode", () => {
    expect(rules).toMatch(/\[data-width="wide"\][^}]*--measure:\s*var\(--measure-wide\)/);
    expect(rules).toMatch(/\[data-width="comfortable"\][^}]*--measure:\s*var\(--measure-comfortable\)/);
  });
});
