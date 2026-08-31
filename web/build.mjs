// Bundles the editor into a single script and stylesheet, which the Go binary
// embeds. No CDN, no code splitting: the app is served from a tailnet node and
// must work with a strict Content-Security-Policy of 'self'.
import * as esbuild from "esbuild";
import { cp, mkdir, rm } from "node:fs/promises";

const watch = process.argv.includes("--watch");

await rm("dist", { recursive: true, force: true });
await mkdir("dist", { recursive: true });
await cp("index.html", "dist/index.html");

/** @type {import("esbuild").BuildOptions} */
const options = {
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

if (watch) {
  const ctx = await esbuild.context(options);
  await ctx.watch();
  console.log("watching for changes");
} else {
  await esbuild.build(options);
}
