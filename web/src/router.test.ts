import { describe, expect, it } from "vitest";
import { parseRoute, routeToPath, Route } from "./router";

describe("parseRoute", () => {
  it("parses the routes the app uses", () => {
    expect(parseRoute("/")).toEqual({ kind: "home" });
    expect(parseRoute("")).toEqual({ kind: "home" });
    expect(parseRoute("/n/alice-github/Daily/2026-08-30.md")).toEqual({
      kind: "note",
      vault: "alice-github",
      path: "Daily/2026-08-30.md",
    });
    expect(parseRoute("/t/go")).toEqual({ kind: "tag", tag: "go" });
    expect(parseRoute("/t/work/urgent")).toEqual({ kind: "tag", tag: "work/urgent" });
    expect(parseRoute("/s/quick%20brown")).toEqual({ kind: "search", query: "quick brown" });
  });

  it("falls back to home rather than throwing", () => {
    // A hand-edited or truncated URL should land somewhere usable.
    for (const p of ["/nope", "/n", "/n/onlyvault", "/t", "/s", "//"]) {
      expect(parseRoute(p).kind).toBe("home");
    }
  });

  it("decodes paths with spaces and unicode", () => {
    expect(parseRoute("/n/me/My%20Notes/Caf%C3%A9.md")).toEqual({
      kind: "note",
      vault: "me",
      path: "My Notes/Café.md",
    });
  });
});

describe("routeToPath", () => {
  it("round-trips every route", () => {
    const routes: Route[] = [
      { kind: "home" },
      { kind: "note", vault: "alice-github", path: "Daily/2026-08-30.md" },
      { kind: "note", vault: "me", path: "My Notes/Café.md" },
      { kind: "note", vault: "me", path: "a#b.md" },
      { kind: "tag", tag: "work/urgent" },
      { kind: "search", query: "quick brown fox" },
    ];
    for (const r of routes) {
      expect(parseRoute(routeToPath(r))).toEqual(r);
    }
  });

  it("escapes characters that would break the URL", () => {
    const path = routeToPath({ kind: "note", vault: "me", path: "a?b#c.md" });
    expect(path).not.toContain("?");
    expect(path).not.toContain("#");
  });
});
