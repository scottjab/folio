# Development

How to build and test folio, how the packages fit together, and why some of it
is shaped the way it is. For running folio, see the [README](../README.md).

## Commands

```sh
nix develop
make test           # Go and frontend suites
make race           # Go suite under the race detector
make check          # what CI runs
make icons          # re-render the app icons from their SVG sources
nix flake check     # builds everything, runs the Go tests
```

`make icons` is the only one that is not part of a build. The PNG icons are
committed rather than generated, because they change about once a year and
rendering SVG needs a toolchain neither the Go nor the node build otherwise
wants. Edit `web/icons/icon.svg` or `web/icons/icon-maskable.svg`, run it, and
commit the PNGs alongside.

## Layout

```
cmd/folio/        CLI: serve, dev, tui, mcp, index, doctor, version
internal/
  vaultpath/        every rule about what a vault path may look like
  markdown/         parse frontmatter, headings, wikilinks, tags, plaintext
  vault/            file operations, confined by os.Root, atomic writes
  store/            SQLite, migrations, generic query methods
  index/            indexer, FTS5 query translator, search, backlinks
  notes/            the operations layer both front ends share
  identity/         tailnet WhoIs to a folio user
  prefs/            per-user settings both front ends obey
  share/            who may read or write what
  watch/            debouncer and fsnotify wiring for external edits
  events/           typed in-process pub/sub
  httpapi/          JSON API, SSE, CSRF, the app shell
  mcpsrv/           MCP tools, resources, prompts
  client/           Go client for the JSON API, including the event stream
  tui/              the terminal client: model, key table, markdown renderer
  tsserve/          tsnet bootstrap; the only package that imports tailscale
web/
  manifest.webmanifest  the web app manifest: name, icons, shortcuts
  icons/                SVG sources and the PNGs rendered from them
  shell.mjs             which files the worker precaches, and this build's id
  build.mjs             two esbuild entry points: the app, then the worker
  src/                  CodeMirror 6 editor
    markdown-ext.ts       Lezer parsers for [[wikilinks]] and #tags
    livepreview.ts        the decorations that render markdown in place
    editor.ts             CodeMirror setup and the preview/source compartment
    app.ts                sidebar, routing, autosave, search palette
    pwa.ts                registration, the update offer, the install hint
    sw.ts                 the service worker
    sw-policy.ts          what the worker does with a request, as a function
```

## Why the code looks like this

The frontend tests run under jsdom and mount a real `EditorView`, because the
failure mode that matters there is a blank page rather than a wrong value: a
module that throws while loading takes the whole bundle with it, and only
actually starting the app catches that.

`style.test.ts` is static rather than computed-style, and deliberately so.
CodeMirror injects a base theme whose rules sit at a specificity of 0,2,0, so any
bare `.cm-*` selector of ours loses silently: that is how the caret ended up as
CodeMirror's 1.2px black hairline. jsdom does not implement cascade specificity,
so it cannot tell a working stylesheet from a broken one; the invariant is
checked against the source instead.

The service worker is split in two for the same reason. `sw.ts` has to be
compiled against the WebWorker library rather than the DOM one, so it gets its
own `tsconfig.sw.json` and cannot be imported by a test that touches the DOM.
The decision it makes on every request, though, depends on nothing but a method,
a mode and a URL, so that lives in `sw-policy.ts` where a test can reach it. The
cases that matter are not subtle: a cached `POST` loses a note, a cached `/api`
response shows somebody another user's vault, and an intercepted `/api/events`
hangs the worker on a stream that never ends.

The worker's cache is named after a hash of everything in `dist/`, which is why
there is no cache-busting policy anywhere: a build that differs by a byte reads
from a cache that is empty, and the old one is deleted on activate. `shell.mjs`
computes both that id and the precache list, and is tested, because the one
thing it gets to decide is a trap. The shell is precached as `/`, not
`/index.html`: Go's file server redirects the latter to the former, a fetch
follows the redirect, and browsers refuse to answer a navigation with a response
flagged as redirected, so precaching the redirecting URL would break the offline
launch the worker exists for.

Where an attachment goes, what it is called, and what link to write for it are
all decided in `internal/notes`, not in either editor. They look like client
concerns and are not: a drop in the browser and an `A` in the terminal that
disagree produce two different files on disk, and the only way two clients agree
forever is to not have two implementations. The terminal client gets this for
free twice over, because it is Go and can call `markdown.ResolveWikilink`
directly; the browser has a second copy of the resolution rule in
`vault-index.ts`, which is why the tie-break there is a byte comparison rather
than `localeCompare`. ICU collation folds case and Go's does not, so the two
disagreed about `Daily/` versus `attachments/` until they were made to match.

The web API and the MCP server both go through `internal/notes`, so permissions,
conflict handling, and link rewriting cannot drift between them. The terminal
client sits a layer further out, on the JSON API itself, for the same reason:
there is one implementation of what a note is, and three front ends that ask it.

All three are clients, and none of them is a reader. A feature that lands in the
browser lands in the terminal and in MCP, or there is a written reason why it
cannot: an agent has no cursor to drop a file onto, so `attach_file` takes
base64, and a terminal cannot draw a picture, so an embedded image is named
rather than rendered. Anything else is the browser quietly becoming the real
client and the other two decaying into viewers.

`internal/tui` is tested against a real server, not a mocked client: the test
starts the actual SQLite store, vault, and HTTP handlers, then drives the model
with keypresses and checks what reached the disk. The bugs a UI like this
actually has all live at that seam, and a mock is exactly where they hide.

Screen geometry lives in one place, `layout` in `internal/tui/mouse.go`, because
drawing and hit-testing computing it separately is how a click ends up opening
the note above the one you pointed at. The mouse tests click literal columns and
rows counted off the drawn screen rather than asking the layout where things
are: a hit test that agrees with the code it came from proves nothing.

## Go 1.27

The parts of the release this leans on, and where:

| Feature | Used for |
|---|---|
| Generic methods | `(*store.DB).All[T]` and `(*Tx).All[T]` share one name; `(*events.Bus).On[E]`; `(*httpapi.API).JSON[T]` |
| `encoding/json/v2` | `MarshalWrite` straight to the socket; `RejectUnknownMembers` turns a typo'd field into a 400 that names it |
| `jsontext` | encoding SSE frames incrementally |
| stdlib `uuid` | `NewV7()` for note ids, event ids, and atomic-write staging names |
| `strings.CutLast` | splitting extensions and anchors |
| `testing/synctest` | the debouncer and the identity cache TTL, tested with a fake clock |
| `httptest.NewTestServer` | API tests on an in-memory network |
| `os.Root` | vault confinement enforced by the kernel, not by string checks |
| `hash/maphash` | striped per-path write locks |
