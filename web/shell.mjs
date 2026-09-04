// Which files make up the app shell, and what to call this build.
//
// Split out of build.mjs so it can be tested. Both functions are pure: they take
// a listing of dist/ and return what gets injected into the service worker.

import { createHash } from "node:crypto";

/**
 * Turns a listing of dist/ into the paths a browser would ask for.
 *
 * The one surprise is index.html, which comes back as "/" rather than
 * "/index.html". That is not tidiness. Go's file server redirects
 * /index.html to /, a fetch follows the redirect, and the response it hands
 * back is flagged as redirected; a browser refuses to answer a navigation with
 * a redirected response, so the offline launch this whole worker exists for
 * would fail with a network error. Asking for "/" in the first place is the
 * only version of this that works.
 *
 * @param {string[]} files paths relative to dist/, in any order
 * @returns {string[]} sorted absolute paths
 */
export function shellPaths(files) {
  const out = new Set();
  for (const file of files) {
    const path = file.split(/[\\/]/).join("/");
    // Source maps are for a debugger that has already loaded the file.
    if (path.endsWith(".map")) continue;
    out.add(path === "index.html" ? "/" : "/" + path);
  }
  return [...out].sort();
}

/**
 * Names this build after its contents.
 *
 * The worker uses it as its cache name, which is what makes cache invalidation a
 * non-problem: a build that differs by one byte reads from a cache that is
 * empty, and a build that differs by nothing reuses the one it already filled.
 *
 * @param {Array<[string, Buffer | string]>} contents path and bytes, per file
 */
export function buildID(contents) {
  const hash = createHash("sha256");
  for (const [path, bytes] of [...contents].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) {
    hash.update(path);
    hash.update(bytes);
  }
  return hash.digest("hex").slice(0, 16);
}
