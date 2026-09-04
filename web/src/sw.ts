// The service worker: what makes folio installable and lets it open offline.
//
// Scope is deliberately small. folio's notes live on a server that authenticates
// by tailnet position, so caching them on the device would mean holding a copy
// of somebody's vault outside the one place that knows who may read it. This
// worker therefore caches the *app shell* only, and every /api request goes
// straight to the network. What you get from that is an installed app that
// launches to its own UI instead of the browser's offline page, and can then say
// plainly that it cannot reach the server.
//
// Cache invalidation is by construction rather than by policy. The cache is
// named after a build id derived from the contents of dist/, so a new build
// reads from a cache that is empty and fills it from the network, and the old
// one is deleted on activate. Nothing is ever revalidated, because nothing in a
// build-keyed cache can be stale.
//
// This file is built as its own esbuild entry point (a classic worker script, so
// it works in every browser that has service workers at all) and typechecked
// against tsconfig.sw.json, which swaps the DOM library for WebWorker. The
// routing decision lives in sw-policy.ts, where a test can reach it.

import { planFor } from "./sw-policy";

// `self` is a WorkerGlobalScope in lib.webworker; narrowing it is only possible
// because the import above makes this file a module, so the declaration below is
// local rather than a redeclaration of the global.
declare const self: ServiceWorkerGlobalScope;

/** Injected by build.mjs: a hash of everything in dist/. */
declare const __BUILD_ID__: string;
/** Injected by build.mjs: the absolute paths of the shell's files. */
declare const __PRECACHE__: readonly string[];
/**
 * True in `npm run dev`. The bundle is not content-hashed and is rewritten on
 * every keystroke there, so a build-keyed cache would serve the morning's app
 * all afternoon. In watch mode the worker registers and installs exactly as it
 * does in production, and then passes every request through untouched.
 */
declare const __DEV__: boolean;

const CACHE = `folio-${__BUILD_ID__}`;

/**
 * The document served for every client-side route, and so for an offline launch.
 *
 * "/" and not "/index.html": Go's file server redirects the latter to the
 * former, and a browser will not answer a navigation with a response that came
 * back flagged as redirected. Precaching the redirecting URL would leave the
 * offline launch failing with a network error, which is the one case this
 * worker exists for. shellPaths in shell.mjs makes the same swap.
 */
const SHELL = "/";

self.addEventListener("install", (event) => {
  if (__DEV__) return;
  event.waitUntil(
    (async () => {
      const cache = await caches.open(CACHE);
      // cache: "reload" bypasses the browser's own HTTP cache. Without it a
      // fresh install can precache the *previous* app.js, which is the one
      // mistake this worker would then make permanent.
      await cache.addAll(__PRECACHE__.map((p) => new Request(p, { cache: "reload" })));
    })(),
  );
  // No skipWaiting here on purpose. A new worker taking over mid-session would
  // reload the page under whoever was typing; instead it waits, the app notices
  // and offers a reload, and the message handler below is how that offer is
  // accepted.
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      await Promise.all(
        (await caches.keys())
          .filter((key) => key.startsWith("folio-") && key !== CACHE)
          .map((key) => caches.delete(key)),
      );
      // Claim the open tabs, so the load that follows an update is controlled
      // rather than running uncontrolled until the next navigation.
      await self.clients.claim();
    })(),
  );
});

self.addEventListener("fetch", (event) => {
  if (__DEV__) return;
  const req = event.request;
  switch (planFor({ method: req.method, mode: req.mode, url: req.url }, self.location.origin, __PRECACHE__)) {
    case "shell":
      event.respondWith(shell(req));
      return;
    case "cache":
      event.respondWith(fromCache(req));
      return;
    case "network":
      // Not calling respondWith is the way to say "not mine": the browser then
      // makes the request itself, with none of the worker in the path.
      return;
  }
});

self.addEventListener("message", (event) => {
  const data = event.data as { type?: string } | null;
  if (data?.type === "skip-waiting") void self.skipWaiting();
});

/**
 * Serves a navigation, network first.
 *
 * Network first rather than cache first because index.html carries the reference
 * to the bundle: serving a cached one while the server has moved on would pin
 * the app to an old build until the worker happened to update. The cached copy
 * is the offline fallback, and nothing more.
 */
async function shell(req: Request): Promise<Response> {
  const cache = await caches.open(CACHE);
  try {
    const fresh = await fetch(req);
    // The server answers every client-side route with index.html, so whatever
    // path this navigation was for, the body is the shell.
    if (fresh.ok) await cache.put(SHELL, fresh.clone());
    return fresh;
  } catch (err) {
    const cached = await cache.match(SHELL);
    if (cached) return cached;
    throw err;
  }
}

/** Serves a precached file from this build's cache, filling it on a miss. */
async function fromCache(req: Request): Promise<Response> {
  const cache = await caches.open(CACHE);
  const hit = await cache.match(req);
  if (hit) return hit;

  const fresh = await fetch(req);
  if (fresh.ok) await cache.put(req, fresh.clone());
  return fresh;
}
