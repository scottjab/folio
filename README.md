# folio

Self-hosted markdown notes on your tailnet, with an Obsidian-style editor and a
full MCP server so your agents can work on the same notes you do.

Your notes are a directory of plain markdown files. Everything else, the search
index included, is derived from them and can be thrown away and rebuilt.

## What it does

- **Obsidian-style live preview.** One CodeMirror buffer holding raw markdown.
  Formatting renders in place, and the line your cursor is on shows its real
  syntax and is tinted so you can see where you are. Switching to markdown view
  flips one extension off; nothing is serialized, so your files are never
  rewritten behind your back.
- **A page, not a column.** The writing area fills the window by default, with
  margins that grow with the viewport rather than staying a fixed number of
  pixels. The width control in the header cycles Full (the default, no cap),
  Wide (grows with the window, stopping once lines get too long to track across),
  and Narrow (a classic reading measure). The choice is remembered. Below 860px
  the sidebar becomes a drawer, so it works on a phone.
- **Real full-text search.** SQLite FTS5 with BM25 ranking and highlighted
  snippets. `tag:go`, `path:Daily`, `"exact phrase"`, `prefix*`, `-exclusions`.
- **Tailnet identity.** No passwords, no sessions, no cookies. folio asks
  tailscaled who is on the other end of the connection. Everyone gets their own
  vault.
- **Sharing.** Grant another tailnet user read or write on a note or a folder.
  Shared notes show up in their search too.
- **Obsidian on the desktop still works.** Point Obsidian at a vault directory.
  Edits there are noticed and reindexed within a second, and open browser tabs
  update live.
- **MCP.** 19 tools, note resources, and three prompts, over Streamable HTTP at
  `/mcp` or through a stdio bridge. An agent acts as a specific tailnet user and
  sees exactly what that person sees.
- **One static binary.** Pure Go, `CGO_ENABLED=0`, web app embedded. Copy it to
  a machine and run it.

## Building

The browser app is a bundle that has to be built before it can be embedded, so
use one of the commands that does both:

```sh
nix build .#     # hermetic, into ./result/bin/folio
make build       # bundles the app, then compiles, into ./bin/folio
```

Plain `go build ./cmd/folio` compiles without the bundle. That binary still
serves the API and MCP perfectly well, but it has no browser UI, so it refuses
to start unless you pass `--allow-stub-ui` and tells you which command to run
instead. If you want to use `go build` directly, run `go generate ./...` first
to produce the bundle.

## Try it without a tailnet

```sh
nix develop          # Go 1.27, Node 24, esbuild
make dev             # http://127.0.0.1:8080
```

Or, from a built binary, with no flags at all:

```sh
folio dev
```

`dev` runs the real API, the real MCP server, the real editor, and writes real
markdown files. The only thing it replaces is the identity step: every request
is treated as `$USER@localhost`, because that is the one part that genuinely
needs a tailnet. It refuses to listen anywhere but loopback, and keeps its state
in `./dev-state` so experimenting cannot touch notes you care about.

## Run it for real

```sh
nix build .#
export TS_AUTHKEY=tskey-auth-...          # first run only
./result/bin/folio serve --hostname folio
```

Ctrl-C or `SIGTERM` shuts down gracefully: open event streams are closed,
in-flight requests get up to 15 seconds to finish, and a second Ctrl-C exits
immediately.

Then open `https://folio.<your-tailnet>.ts.net`. The tailnet needs MagicDNS
and HTTPS certificates enabled, both under DNS in the admin console.

On NixOS, using the flake's module:

```nix
{
  inputs.folio.url = "github:scottjab/folio";

  # in your configuration
  imports = [ inputs.folio.nixosModules.default ];

  services.folio = {
    enable = true;
    hostname = "notes";
    authKeyFile = "/run/secrets/folio-authkey";   # first run only

    # Let an agent on a tagged node act as you over MCP.
    agents = [{ tag = "tag:notes-agent"; actAs = "you@github"; }];
  };
}
```

The module writes folio's JSON config file and passes it as the single argument
to `folio serve`, so there is one place a setting comes from and no chance of
flags and file disagreeing. `services.folio.settings` is the freeform escape
hatch for anything without an option yet; folio rejects unknown keys, so a typo
stops the service at startup naming the key rather than being ignored.

Notes live in `/var/lib/folio/vaults` by default, owned by the `folio` user and
group. `services.folio.stateDir` moves that anywhere absolute; the directory and
its parents are created on start with the right ownership, and it is the only
path the unit is allowed to write to. A location under `/home` works too, but
relaxes `ProtectHome` for the service, which the module warns about. The
service runs as a static user rather than under `DynamicUser` on purpose: the
whole point is that your notes are ordinary files you can back up or point a
desktop Obsidian at, and a rotating uid under `/var/lib/private` makes that
needlessly awkward. Add your own account to the `folio` group to read them
directly.

The unit is sandboxed hard: no capabilities at all (tsnet does its networking in
userspace, with no TUN device), `ProtectSystem=strict` with the state directory
as the only writable path, and a seccomp filter. `nix flake check` boots a VM and
starts the service to prove none of that stops folio running, which is the
failure that would otherwise only turn up on a real deploy.

## On disk

```
$STATE_DIR/
  folio.db                     SQLite: the index, plus users and shares
  tsnet/                         tailnet node state
  vaults/
    you-github/                  one directory per tailnet user
      Daily/2026-08-30.md
      Projects/folio.md
      attachments/diagram.png
      .obsidian/                 preserved, never indexed
      .folio/{tmp,trash}/      atomic-write staging and deleted notes
```

A note is ordinary markdown:

```markdown
---
id: 019bd0f4-8c31-7a2e-9f10-3b6c7d8e9f01
tags: [daily, go]
---

# Thursday

Shipped the [[Projects/folio]] indexer. Still owe #go127 a writeup.
```

The `id` is written once, when the note is created, and never rewritten. It is
what lets a note keep its identity across a rename.

**The markdown is the source of truth.** The database is a cache. If it ever
disagrees with the files, the files win:

```sh
folio doctor                # what does the index think, and does it match?
folio index sync            # reconcile the difference
folio index rebuild         # reconstruct the index from the files entirely
```

None of these touch users, vaults, or shares, which is why `index rebuild` is
the supported recovery path rather than deleting `folio.db`.

## Connecting an agent

Most MCP clients can point straight at the HTTP endpoint:

```json
{ "mcpServers": { "folio": { "url": "https://folio.your-tailnet.ts.net/mcp" } } }
```

For a client that only speaks stdio:

```json
{
  "mcpServers": {
    "folio": {
      "command": "folio",
      "args": ["mcp", "--server", "https://folio.your-tailnet.ts.net"]
    }
  }
}
```

Either way the agent is identified by the tailnet, as whoever owns the machine
it runs on. To run one on a tagged server, map the tag to a user:

```json
{ "agents": [{ "tag": "tag:notes-agent", "actAs": "you@github" }] }
```

A tagged node can only ever borrow an identity that already exists; it cannot
create one.

**Tools:** `search_notes`, `list_notes`, `read_note`, `create_note`,
`update_note`, `edit_note`, `append_note`, `delete_note`, `move_note`,
`get_backlinks`, `list_tags`, `list_folders`, `get_daily_note`, `list_vaults`,
`vault_stats`, `list_shares`, `share_note`, `unshare_note`, `read_attachment`.

`edit_note` and `append_note` exist so an agent can change one line without
rewriting the file. `edit_note` refuses an ambiguous match rather than guessing,
and applies all of its edits or none.

## Keyboard

| | |
|---|---|
| `⌘K` / `Ctrl-K` | search |
| `⌘/` / `Ctrl-/` | toggle live preview and raw markdown |
| `⌘S` / `Ctrl-S` | save now (it autosaves anyway) |
| `⌘F` / `Ctrl-F` | find within the note |
| `Esc` | close the search palette, or the sidebar drawer |
| `[[` | link to a note, with completion |
| `#` | tag, with completion |

## Concurrent edits

Saves are compare-and-swap against a content hash. If a note changed underneath
you, the save is refused, your version is written to
`Note (conflict 2026-08-30T12-00-00Z).md` beside the original, and the editor
tells you where it went. Nothing silently picks a winner, and nothing is lost.

## Development

```sh
nix develop
make test           # Go and frontend suites
make race           # Go suite under the race detector
make check          # what CI runs
nix flake check     # builds everything, runs the Go tests
```

Layout:

```
cmd/folio/        CLI: serve, dev, mcp, index, doctor, version
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
  tsserve/          tsnet bootstrap; the only package that imports tailscale
web/src/            CodeMirror 6 editor
  markdown-ext.ts     Lezer parsers for [[wikilinks]] and #tags
  livepreview.ts      the decorations that render markdown in place
  editor.ts           CodeMirror setup and the preview/source compartment
  app.ts              sidebar, routing, autosave, search palette
```

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
conflict handling, and link rewriting cannot drift between them.

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
