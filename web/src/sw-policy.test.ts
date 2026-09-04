import { describe, expect, it } from "vitest";

import { isLive, planFor } from "./sw-policy";

/**
 * The service worker sits in front of every request the app makes, so the ways
 * it can go wrong are not subtle: a cached POST loses a note, a cached /api
 * response shows somebody the wrong vault, and an intercepted /api/events hangs
 * the worker on a stream that never closes. This is the function that decides,
 * and these are the cases it must never get wrong.
 */

const ORIGIN = "https://folio.tailnet.ts.net";
const SHELL = ["/", "/app.js", "/app.css", "/manifest.webmanifest", "/icons/icon-192.png"];

function plan(url: string, over: { method?: string; mode?: string } = {}) {
  return planFor(
    { method: over.method ?? "GET", mode: over.mode ?? "cors", url },
    ORIGIN,
    SHELL,
  );
}

describe("what the worker touches", () => {
  it("serves the app shell from the build's cache", () => {
    for (const path of SHELL) {
      expect(plan(ORIGIN + path), path).toBe("cache");
    }
  });

  it("treats a page load as a navigation, so a client-side route survives offline", () => {
    expect(plan(ORIGIN + "/n/me/Daily/2026-09-04.md", { mode: "navigate" })).toBe("shell");
    expect(plan(ORIGIN + "/", { mode: "navigate" })).toBe("shell");
  });

  it("leaves anything it was not built with alone", () => {
    // The shell list comes from the build. A file that is not in it is one this
    // worker knows nothing about, and guessing is how a stale asset survives a
    // deploy.
    expect(plan(ORIGIN + "/app.v2.js")).toBe("network");
  });
});

describe("what the worker must not touch", () => {
  it("never caches the API", () => {
    // Notes are per-user and permission-checked on every read. A copy in a
    // device cache is outside the only place that knows who may see it.
    expect(plan(ORIGIN + "/api/me")).toBe("network");
    expect(plan(ORIGIN + "/api/vaults/me/notes/Daily.md")).toBe("network");
    expect(plan(ORIGIN + "/api")).toBe("network");
    expect(plan(ORIGIN + "/mcp")).toBe("network");
    expect(plan(ORIGIN + "/mcp/tools")).toBe("network");
  });

  it("stays out of the event stream", () => {
    // /api/events is a response that never ends. A worker holding it open in a
    // respondWith would never settle that fetch handler.
    expect(plan(ORIGIN + "/api/events")).toBe("network");
    expect(isLive("/api/events")).toBe(true);
  });

  it("is not fooled by a path that merely starts like the API", () => {
    expect(isLive("/apifake")).toBe(false);
    expect(isLive("/mcpx")).toBe(false);
  });

  it("never handles a write", () => {
    // Replaying a PUT out of a cache would resurrect an old version of a note.
    for (const method of ["POST", "PUT", "DELETE", "PATCH"]) {
      expect(plan(ORIGIN + "/app.js", { method }), method).toBe("network");
      expect(plan(ORIGIN + "/", { method, mode: "navigate" }), method).toBe("network");
    }
  });

  it("ignores other origins", () => {
    expect(plan("https://example.com/app.js")).toBe("network");
    expect(plan("https://example.com/", { mode: "navigate" })).toBe("network");
  });

  it("falls back to the network on a URL it cannot parse", () => {
    expect(plan("not a url at all")).toBe("network");
  });
});
