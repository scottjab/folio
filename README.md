# folio

Self-hosted markdown notes on your tailnet, with an Obsidian-style editor and a
full MCP server so your agents can work on the same notes you do.

Your notes are a directory of plain markdown files. Everything else, the search
index included, is derived from them and can be thrown away and rebuilt.

![The folio editor in a browser, showing a note in live preview with a rendered
table, a tinted cursor line, and the backlinks bar](docs/web-editor.png)

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
- **Obsidian's links, and its conventions.** `[[Note]]`, `[[Note#Heading]]`,
  `[[Note|alias]]`, and `![[Note]]` to pull another note in where you are
  standing. `[[` completes as you type and writes the shortest form that still
  resolves, so links read as prose rather than as paths. A link to a note you
  have not written yet is drawn as an invitation; clicking it creates the note.
- **Attachments by drag, drop, or paste.** Drop a file on the editor or paste a
  screenshot and it uploads, lands where your settings say, and links itself.
  Sizing works too: `![[shot.png|300]]`. Where new attachments go is the same
  setting Obsidian has, and it lives on the server, so the browser and the
  terminal file things in the same place.
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
- **A terminal client, at parity.** `folio tui` is a full client, not a viewer:
  search, write, rename, delete, daily notes, backlinks, sharing, `[[`
  completion, attaching a file, and transclusion, over the same JSON API the
  browser uses. It edits in place or hands the note to `$EDITOR`, saves as a
  compare-and-swap like everything else, and updates live when a note changes
  underneath it. Anything the browser learns, it learns.
- **MCP, at parity too.** 24 tools, note resources, and three prompts, over
  Streamable HTTP at `/mcp` or through a stdio bridge. An agent acts as a
  specific tailnet user, sees exactly what that person sees, and can do what
  they can do: attach a file, resolve a link, read the settings. It is a client,
  not a reader.
- **One static binary.** Pure Go, `CGO_ENABLED=0`, web app embedded. Copy it to
  a machine and run it.

Search is the same index from either client, `⌘K` in the browser and `/` in the
terminal:

![The search palette over the folio editor, matching four notes with the hit
highlighted in each snippet](docs/web-search.png)

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

`attachments/` is where uploads land by default. The other three of Obsidian's
options are there too, in the browser's Settings and behind `,` in the terminal:
the vault root, the note's own folder, or a named subfolder of it. The choice is
stored per user on the server rather than in each client, because a drop in a
browser tab and an `A` in a terminal have to agree about where a file goes.

**The markdown is the source of truth.** The database is a cache. If it ever
disagrees with the files, the files win:

```sh
folio doctor                # what does the index think, and does it match?
folio index sync            # reconcile the difference
folio index rebuild         # reconstruct the index from the files entirely
```

None of these touch users, vaults, or shares, which is why `index rebuild` is
the supported recovery path rather than deleting `folio.db`.

## In the terminal

![folio tui showing the note list on the left and a note rendered on the right,
with the table drawn in box characters](docs/tui.png)

```sh
folio tui                                   # the folio node on your tailnet
folio tui Projects/folio.md                 # open straight into a note
folio tui --server http://127.0.0.1:8080    # a local `folio dev`
```

With no `--server` it goes to the node named `folio` on this machine's tailnet,
which is where `folio serve` puts it. It asks the local tailscaled for the
tailnet's MagicDNS suffix and uses the full name rather than the short one:
MagicDNS would resolve `folio` through the search domain, but the certificate is
issued for `folio.<tailnet>.ts.net`, so `https://folio` fails to verify.
`FOLIO_SERVER` overrides the default, and `--server` overrides that.

There is nothing to log in to: the TUI runs on your machine, so its requests
arrive at the server from your tailnet address and WhoIs says who you are,
exactly as the browser and the MCP bridge do.

It is a client of a running folio rather than another way to open the state
directory, so permissions, conflict handling, and link rewriting stay the
server's business and there is no second implementation to keep in step. Every
endpoint the browser uses has a method in `internal/client`.

The screen is a note list, the note, and one line at the bottom that is either a
message, a question, or the keys worth knowing right now. Below 62 columns the
list and the note take turns, the way the web sidebar becomes a drawer on a
phone.

| | |
|---|---|
| `?` | every key, generated from the same table that dispatches them |
| `/` or `Ctrl-K` | search every vault you can read |
| `Tab` | move between the list and the note |
| `Enter` | open the selected note, or start editing the open one |
| `i` / `e` | edit here / hand the note to `$EDITOR` and save on exit |
| `Ctrl-S` | save |
| `p` | rendered markdown or raw |
| `n` `m` `x` | new, rename, delete |
| `a` | append a line, without opening the editor |
| `A` | attach a file, which uploads it and links it |
| `[[` | while editing: complete a link from the whole vault |
| `,` | settings: where new attachments go |
| `D` | today's daily note |
| `t` `f` `v` | filter by tag, by folder, switch vault |
| `B` `L` | what links here, what this links to |
| `s` `S` | shares in both directions, share this note |
| `o` | open this note in a browser, which is how you see an image |
| `Esc` | back: close an overlay, clear a filter, stop editing |
| `M` | mouse on or off |

`/` opens search over every vault you can read, with the same ranking and
snippets the browser shows:

![The folio tui search overlay, listing four matches with the query highlighted
in each snippet](docs/tui-search.png)

The mouse works too. Click a note in the list to open it, click a `[[link]]` in
a note to follow it, and use the wheel to move through the list or scroll the
note; clicking away from an overlay closes it. Clicking an embedded `![[image]]`
opens it in a browser, since a terminal cannot draw it. A terminal that is
reporting mouse events cannot also be used to select text, so `M` hands the
pointer back (most terminals also let you hold Shift to select regardless).

Markdown is rendered in place: headings, emphasis, task boxes, tables, fenced
code, wikilinks, and tags, wrapped to the pane. `p` switches to the raw text,
which is also what the editor shows.

`![[Note]]` on a line of its own is expanded where it stands, drawn against a
left rule so it reads as another note's text rather than as yours, and
`![[Note#Heading]]` pulls in just that section. An embed of a picture is named
rather than drawn, because a terminal cannot draw one; `o` opens the note in a
browser that can.

There is no autosave, unlike the browser. Every write is a keypress you made,
and every path that would lose a buffer, quitting or opening another note, stops
and offers to save it first. A save is still a compare-and-swap: if the note
changed underneath you, folio writes your version to a conflict file beside the
original and the UI tells you where it went and offers to reload or overwrite.

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
`vault_stats`, `list_shares`, `share_note`, `unshare_note`, `read_attachment`,
`list_attachments`, `attach_file`, `resolve_link`, `get_settings`,
`set_settings`.

`edit_note` and `append_note` exist so an agent can change one line without
rewriting the file. `edit_note` refuses an ambiguous match rather than guessing,
and applies all of its edits or none.

`resolve_link` is the one worth knowing about. A link is not a path: `[[folio]]`
resolves against the whole vault and prefers the linking note's own folder, so
the same bare name means different notes in different places. An agent that
guessed would be reimplementing the resolution rule, which is exactly what this
codebase is arranged to prevent, so it asks instead and gets the same answer the
editor and the indexer get, `#Heading` sections included.

`attach_file` takes base64 and hands back the link to write. It does not take a
path, because where an attachment goes is the user's setting rather than the
caller's choice, and an agent filing things somewhere the browser would not is
the whole failure this is built to avoid.

## Keyboard, in the browser

The terminal client has its own keys, above.

| | |
|---|---|
| `⌘K` / `Ctrl-K` | search |
| `⌘/` / `Ctrl-/` | toggle live preview and raw markdown |
| `⌘S` / `Ctrl-S` | save now (it autosaves anyway) |
| `⌘F` / `Ctrl-F` | find within the note |
| `Esc` | close the search palette, or the sidebar drawer |
| `[[` | link to a note, with completion |
| `#` | tag, with completion |
| drop / paste | upload a file or a screenshot and link it |

## Links and attachments

Links are Obsidian's, including the resolution rule: an exact path first, then
the same path with `.md`, then a unique basename, preferring one in the linking
note's own folder and breaking a remaining tie by depth and then by name. That
rule lives in `internal/markdown` and every client asks it rather than
reimplementing it, which is what stops a link rendering as broken in the editor
and then resolving in the index.

Writing a link is the same rule run backwards. Completion inserts the shortest
target that still finds the note, so you get `[[folio]]` and not
`[[Projects/folio]]` unless the short form would land somewhere else.

Dropping a file, pasting a screenshot, pressing `A` in the terminal, or an agent
calling `attach_file` all reach one endpoint that applies your attachment setting, refuses to overwrite a name
already in use (`IMG_0001.jpg` becomes `IMG_0001 1.jpg`), and hands back the link
to write. A pasted image with no filename is named from the clock, the way
Obsidian names one. Images embed with `![[...]]`; everything else links with
`[[...]]`, because a PDF rendered into the middle of a paragraph helps nobody.

`![[Note]]` renders the note in place, `![[Note#Heading]]` renders one section of
it, and both stop rather than recursing when a note embeds itself. The browser
renders an embed with the same editor the note itself uses, so a table inside an
embed looks like a table.

## Concurrent edits

Saves are compare-and-swap against a content hash. If a note changed underneath
you, the save is refused, your version is written to
`Note (conflict 2026-08-30T12-00-00Z).md` beside the original, and the editor
tells you where it went. Nothing silently picks a winner, and nothing is lost.

An open browser tab, or an open `folio tui`, follows the note it is showing. A
change from anywhere, another tab, your phone, Obsidian, the terminal client, or
an agent over MCP, arrives on the event stream and is loaded straight away when
there is nothing local to lose. When there is, the editor says so and offers the
new version rather than choosing for you.

Whether a change is worth loading is decided by the content hash, not by who
made it. The login on an event is your own whenever the other writer is you
somewhere else, so treating "by me" as "already know about it" is how an edit
made in one tab fails to show up in another.

## Development

Commands, package layout, why the tests are shaped the way they are, and the
Go 1.27 features this leans on: [docs/development.md](docs/development.md).

## License

AGPL-3.0. See [LICENSE](LICENSE).

The short version: use it, change it, run it for yourself or your company, but
if you run a modified folio as a service for other people, you have to publish
your changes. If you want to use it commercially under different terms, ask me.
