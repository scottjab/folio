// Bundles the editor into a single script and stylesheet, which the Go binary
// embeds. No CDN, no code splitting: the app is served from a tailnet node and
// must work with a strict Content-Security-Policy of 'self'.
//
// There are two entry points. src/main.ts becomes app.js, the app itself.
// src/sw.ts becomes sw.js, the service worker, and has to be built second,
// because what it caches and the cache it caches into are both derived from the
// output of the first build.
import * as esbuild from "esbuild";
import { cp, mkdir, readdir, readFile, rm } from "node:fs/promises";
import { join } from "node:path";

import { buildID, shellPaths } from "./shell.mjs";

const watch = process.argv.includes("--watch");

await rm("dist", { recursive: true, force: true });
await mkdir("dist", { recursive: true });
await cp("index.html", "dist/index.html");
await cp("manifest.webmanifest", "dist/manifest.webmanifest");
await cp("icons", "dist/icons", {
  recursive: true,
  // icon-maskable.svg is the artwork the maskable and apple-touch PNGs are
  // rendered from, not a file anything loads. Shipping it would only put a
  // second copy of the same picture in the precache.
  filter: (src) => !src.endsWith("icon-maskable.svg"),
});

/** @type {import("esbuild").BuildOptions} */
const appOptions = {
  entryPoints: ["src/main.ts"],
  bundle: true,
  format: "esm",
  target: ["es2022"],
  outdir: "dist",
  entryNames: "app",
  assetNames: "assets/[name]-[hash]",
  minify: !watch,
  sourcemap: watch ? "inline" : false,
  logLevel: "info",
  // The page is served with script-src 'self', so everything has to be in the
  // bundle. A missed external import would fail at runtime rather than here.
  external: [],
};

/**
 * Builds the service worker against whatever is in dist/ at the time.
 *
 * The two injected values are what make the worker's caching correct without any
 * cache-busting policy: __PRECACHE__ is the shell as it actually was built, and
 * __BUILD_ID__ changes if and only if one of those files does, so a new build
 * cannot read an old build's cache.
 */
async function buildServiceWorker() {
  const shell = watch ? [] : shellPaths(await distFiles());
  /** @type {import("esbuild").BuildOptions} */
  const options = {
    entryPoints: ["src/sw.ts"],
    bundle: true,
    // A classic script rather than a module worker: module service workers are
    // recent enough that a browser could have service workers and still refuse
    // this one, and the worker has no imports to gain anything by it.
    format: "iife",
    target: ["es2022"],
    outfile: "dist/sw.js",
    minify: !watch,
    sourcemap: watch ? "inline" : false,
    logLevel: "info",
    define: {
      __BUILD_ID__: JSON.stringify(watch ? "dev" : buildID(await readShell(shell))),
      __PRECACHE__: JSON.stringify(shell),
      __DEV__: JSON.stringify(watch),
    },
  };
  await esbuild.build(options);
}

/**
 * Lists every file in dist/, relative to it.
 *
 * Called after the app build and before the worker build, so sw.js is not in it,
 * which is right. A worker that precached itself would be caching the file whose
 * job is to decide what is cached.
 */
async function distFiles() {
  const out = [];
  for (const entry of await readdir("dist", { recursive: true, withFileTypes: true })) {
    if (entry.isFile()) out.push(join(entry.parentPath, entry.name).slice("dist/".length));
  }
  return out;
}

/** Pairs each shell path with its bytes, for hashing. */
async function readShell(shell) {
  return Promise.all(
    shell.map(async (path) => [
      path,
      // "/" is index.html by another name; see shellPaths.
      await readFile(join("dist", path === "/" ? "index.html" : path.slice(1))),
    ]),
  );
}

if (watch) {
  const ctx = await esbuild.context(appOptions);
  await ctx.watch();
  // Built once rather than watched: in watch mode the worker is a pass-through
  // that caches nothing, so rebuilding it on every keystroke would achieve
  // nothing but noise.
  await buildServiceWorker();
  console.log("watching for changes");
} else {
  await esbuild.build(appOptions);
  await buildServiceWorker();
}
