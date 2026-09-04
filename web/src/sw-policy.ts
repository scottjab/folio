// What the service worker does with a request, as a plain function.
//
// This is separated from sw.ts so it can be tested. sw.ts has to be compiled
// against the WebWorker library and cannot be imported from a test that also
// touches the DOM; the decision it makes, though, is about nothing but a method,
// a mode and a URL, so it lives here where a test can reach it.

/**
 * What to do with a request.
 *
 *   network  hand it to the browser untouched
 *   shell    a navigation: network first, cached index.html if that fails
 *   cache    part of the app shell: serve the build's cached copy
 */
export type Plan = "network" | "shell" | "cache";

/** The parts of a Request the decision actually depends on. */
export interface RequestFacts {
  method: string;
  /** Request.mode: "navigate" for a page load, something else otherwise. */
  mode: string;
  url: string;
}

/**
 * Decides how to serve a request.
 *
 * The bias is towards "network": anything this function is not certain about is
 * something the worker should keep its hands off. That is what keeps a caching
 * bug from turning into a note that will not save.
 */
export function planFor(
  req: RequestFacts,
  origin: string,
  precache: readonly string[],
): Plan {
  // A write must never be served from, or recorded in, a cache.
  if (req.method !== "GET") return "network";

  // Request.url is always absolute, so no base is passed: anything that does
  // not parse on its own is not something this worker should be guessing about.
  let url: URL;
  try {
    url = new URL(req.url);
  } catch {
    return "network";
  }
  if (url.origin !== origin) return "network";
  if (isLive(url.pathname)) return "network";

  if (req.mode === "navigate") return "shell";
  return precache.includes(url.pathname) ? "cache" : "network";
}

/**
 * Reports whether a path must always go to the network.
 *
 * The API is live data behind a per-user permission check, so a cached copy
 * would be both stale and, on a shared machine, somebody else's. /api/events is
 * worse than stale: it is a stream that never ends, and a worker that tried to
 * cache it would sit holding the response body open forever. /mcp is the same
 * API by another door.
 */
export function isLive(pathname: string): boolean {
  return pathname === "/api" || pathname.startsWith("/api/") ||
    pathname === "/mcp" || pathname.startsWith("/mcp/");
}
