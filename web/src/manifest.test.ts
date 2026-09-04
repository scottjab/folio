import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "./app";

/**
 * The manifest and index.html are the two files nothing else imports, so nothing
 * else checks them. A renamed icon, a start_url that no longer matches the
 * scope, or a shortcut pointing at a query the app stopped handling all fail the
 * same way: silently, on somebody's phone, weeks later.
 */

const root = process.cwd();
const manifest = JSON.parse(readFileSync(join(root, "manifest.webmanifest"), "utf8"));
const html = readFileSync(join(root, "index.html"), "utf8");
const css = readFileSync(join(root, "src", "style.css"), "utf8");

/** Resolves a path the browser would ask for back to the file build.mjs ships. */
function shipped(url: string): string {
  return join(root, url.replace(/^\//, ""));
}

describe("the web app manifest", () => {
  it("describes an app that opens on its own rather than in a tab", () => {
    expect(manifest.display).toBe("standalone");
    expect(manifest.name).toBe("folio");
    expect(manifest.short_name).toBe("folio");
  });

  it("scopes itself to the whole origin, so no route escapes the installed app", () => {
    // A link out of scope opens in the browser instead, which on iOS means the
    // installed app blinks and hands you off to Safari mid-note.
    expect(manifest.scope).toBe("/");
    expect(manifest.start_url.startsWith("/")).toBe(true);
    expect(manifest.id).toBe("/");
  });

  it("ships every icon it names", () => {
    expect(manifest.icons.length).toBeGreaterThan(0);
    for (const icon of manifest.icons) {
      expect(existsSync(shipped(icon.src)), `${icon.src} is missing`).toBe(true);
    }
  });

  it("has the two raster sizes an installer actually looks for", () => {
    const sizes = manifest.icons
      .filter((i: { type: string }) => i.type === "image/png")
      .map((i: { sizes: string }) => i.sizes);
    expect(sizes).toContain("192x192");
    expect(sizes).toContain("512x512");
  });

  it("has a maskable icon, so Android does not paste ours onto a white tile", () => {
    const maskable = manifest.icons.filter((i: { purpose?: string }) =>
      i.purpose?.split(" ").includes("maskable"),
    );
    expect(maskable.length).toBeGreaterThan(0);
  });

  it("matches the page's own background, so the splash does not flash", () => {
    // background_color is what a launching app paints before the bundle runs.
    // Anything but the stylesheet's own --bg is a white flash on a dark phone.
    const light = /:root\s*\{[\s\S]*?--bg:\s*(#[0-9a-f]+)/i.exec(css);
    expect(light).not.toBeNull();
    expect(manifest.background_color).toBe(light![1]);
  });
});

describe("the page's head", () => {
  it("links the manifest, which is what makes any of this happen", () => {
    expect(html).toMatch(/<link rel="manifest" href="\/manifest\.webmanifest">/);
  });

  it("paints under the notch and the home indicator", () => {
    // Without viewport-fit=cover an installed iOS app is letterboxed by bars in
    // the browser's colour, and env(safe-area-inset-*) all report zero.
    expect(html).toMatch(/name="viewport"[^>]*viewport-fit=cover/);
    expect(css).toContain("env(safe-area-inset-top");
    expect(css).toContain("env(safe-area-inset-bottom");
  });

  it("leaves pinch-zoom alone", () => {
    // Disabling zoom is the usual way to stop iOS zooming a small input, and it
    // takes an accessibility feature with it. The stylesheet sizes the fields
    // instead.
    expect(html).not.toMatch(/user-scalable\s*=\s*no/);
    expect(html).not.toMatch(/maximum-scale/);
  });

  it("tells Safari it can run standalone, which the manifest alone does not", () => {
    expect(html).toMatch(/name="apple-mobile-web-app-capable" content="yes"/);
    expect(html).toMatch(/name="apple-mobile-web-app-title" content="folio"/);
  });

  it("ships the icon iOS uses for the home screen", () => {
    // iOS takes this one and ignores the manifest's, so a manifest that
    // validates perfectly can still install with a screenshot for an icon.
    const link = /<link rel="apple-touch-icon" href="([^"]+)">/.exec(html);
    expect(link, "no apple-touch-icon").not.toBeNull();
    expect(existsSync(shipped(link![1]))).toBe(true);
  });

  it("colours the status bar for both themes", () => {
    const light = /<meta name="theme-color" content="(#[0-9a-f]+)" media="\(prefers-color-scheme: light\)">/i.exec(html);
    const dark = /<meta name="theme-color" content="(#[0-9a-f]+)" media="\(prefers-color-scheme: dark\)">/i.exec(html);
    expect(light, "no light theme-color").not.toBeNull();
    expect(dark, "no dark theme-color").not.toBeNull();
    expect(light![1]).not.toBe(dark![1]);

    // And each should be the background the stylesheet actually paints.
    expect(css).toMatch(new RegExp(`:root\\s*\\{[\\s\\S]*?--bg:\\s*${light![1]}`, "i"));
    expect(css).toMatch(new RegExp(`prefers-color-scheme: dark[\\s\\S]*?--bg:\\s*${dark![1]}`, "i"));
  });
});

// --- the shortcuts, exercised against the real app ---

const me = { login: "me@x.com", displayName: "Me", vault: "me-x.com", isAgent: false };
const note = {
  vault: "me-x.com", ownerLogin: "me@x.com", path: "Daily/2026-09-04.md",
  title: "Friday", tags: [], sha256: "abc", updatedAt: "2026-09-04T00:00:00Z",
  content: "# Friday\n", size: 10, modTime: "2026-09-04T00:00:00Z",
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
    const key = Object.keys(routes)
      .sort((a, b) => b.length - a.length)
      .find((k) => url.pathname === k || url.pathname.startsWith(k + "/"));
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

async function startAt(url: string) {
  history.replaceState(null, "", url);
  const app = new App();
  await app.start();
  await new Promise((r) => setTimeout(r, 0));
  return app;
}

describe("home screen shortcuts", () => {
  beforeEach(() => {
    document.body.innerHTML = '<div id="app" class="app is-loading"></div>';
    vi.stubGlobal("fetch", fakeFetch());
    vi.stubGlobal("EventSource", FakeEventSource);
    localStorage.clear();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = "";
    history.replaceState(null, "", "/");
  });

  it("only offers shortcuts the app knows how to open", async () => {
    // The manifest is the only place these strings appear outside app.ts, so
    // this is what stops a rename in one from going unnoticed in the other.
    for (const shortcut of manifest.shortcuts) {
      const open = new URL(shortcut.url, "http://localhost").searchParams.get("open");
      expect(["new", "search"], `${shortcut.url} is not a shortcut the app handles`)
        .toContain(open);
    }
  });

  it("opens the search palette when launched from the search shortcut", async () => {
    await startAt("/?open=search");
    expect(document.querySelector(".palette")).not.toBeNull();
  });

  it("starts a new note when launched from the new-note shortcut", async () => {
    const prompt = vi.fn().mockReturnValue("");
    vi.stubGlobal("prompt", prompt);
    await startAt("/?open=new");
    expect(prompt).toHaveBeenCalled();
  });

  it("opens today's note on a plain launch, which is what start_url asks for", async () => {
    // No shortcut for this one: start_url is "/", and "/" is the daily note.
    expect(manifest.start_url).toBe("/");
    await startAt("/");
    expect(document.querySelector(".note-title")!.textContent).toBe(note.title);
  });

  it("strips the query, so a refresh does not do it a second time", async () => {
    await startAt("/?open=search");
    expect(window.location.search).toBe("");
  });

  it("does nothing special on an ordinary launch", async () => {
    await startAt("/");
    expect(document.querySelector(".palette")).toBeNull();
  });
});
