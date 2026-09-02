# Development

How to build and test folio, how the packages fit together, and why some of it
is shaped the way it is. For running folio, see the [README](../README.md).

## Commands

```sh
nix develop
make test           # Go and frontend suites
make race           # Go suite under the race detector
make check          # what CI runs
nix flake check     # builds everything, runs the Go tests
```

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
  share/            who may read or write what
  watch/            debouncer and fsnotify wiring for external edits
  events/           typed in-process pub/sub
  httpapi/          JSON API, SSE, CSRF, the app shell
  mcpsrv/           MCP tools, resources, prompts
  client/           Go client for the JSON API, including the event stream
  tui/              the terminal client: model, key table, markdown renderer
  tsserve/          tsnet bootstrap; the only package that imports tailscale
web/src/            CodeMirror 6 editor
  markdown-ext.ts     Lezer parsers for [[wikilinks]] and #tags
  livepreview.ts      the decorations that render markdown in place
  editor.ts           CodeMirror setup and the preview/source compartment
  app.ts              sidebar, routing, autosave, search palette
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

The web API and the MCP server both go through `internal/notes`, so permissions,
conflict handling, and link rewriting cannot drift between them. The terminal
client sits a layer further out, on the JSON API itself, for the same reason:
there is one implementation of what a note is, and three front ends that ask it.

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
