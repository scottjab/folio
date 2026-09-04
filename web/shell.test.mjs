import { describe, expect, it } from "vitest";

import { buildID, shellPaths } from "./shell.mjs";

/**
 * What the service worker precaches, and what it calls the cache it puts it in.
 * Both are decided at build time and baked into sw.js, so a mistake here is one
 * nothing in the app can see: it shows up as an installed folio that will not
 * open offline, or one that never picks up a deploy.
 */

describe("the app shell", () => {
  it("asks for the document by the URL the browser navigates to", () => {
    // Not "/index.html". Go's file server 301s that to "/", fetch follows the
    // redirect, and the response comes back flagged as redirected; a browser
    // refuses to answer a navigation with one of those, so an offline launch
    // would fail with a network error rather than opening the app.
    expect(shellPaths(["index.html"])).toEqual(["/"]);
  });

  it("keeps every other file at its own path", () => {
    expect(shellPaths(["app.js", "app.css", "icons/icon-192.png"]))
      .toEqual(["/app.css", "/app.js", "/icons/icon-192.png"]);
  });

  it("leaves source maps out", () => {
    // Nothing loads a source map until a debugger is already open on the file.
    expect(shellPaths(["app.js", "app.js.map"])).toEqual(["/app.js"]);
  });

  it("is ordered, so the build id does not depend on the filesystem", () => {
    expect(shellPaths(["b.js", "a.js", "index.html"])).toEqual(["/", "/a.js", "/b.js"]);
  });

  it("normalises the separators a Windows readdir would give it", () => {
    expect(shellPaths(["icons\\icon.svg"])).toEqual(["/icons/icon.svg"]);
  });
});

describe("the build id", () => {
  it("changes when a file's contents change", () => {
    // This is the whole cache invalidation strategy: a new build cannot read an
    // old build's cache, because it does not know its name.
    const before = buildID([["/app.js", "one"]]);
    const after = buildID([["/app.js", "two"]]);
    expect(after).not.toBe(before);
  });

  it("changes when a file is added or renamed", () => {
    const base = buildID([["/app.js", "x"]]);
    expect(buildID([["/app.js", "x"], ["/app.css", "y"]])).not.toBe(base);
    expect(buildID([["/app.mjs", "x"]])).not.toBe(base);
  });

  it("does not change for the same build twice", () => {
    // Otherwise every deploy would discard a cache it could have kept, and
    // every reproducible build would produce a different binary.
    const files = [["/app.js", "x"], ["/app.css", "y"]];
    expect(buildID(files)).toBe(buildID(files));
    expect(buildID([...files].reverse())).toBe(buildID(files));
  });
});
