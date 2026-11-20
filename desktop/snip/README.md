# snip

Hotkey → fuzzy snippet picker → paste. One static Go binary, **no daemon** —
every invocation is a one-shot process and the catalog is read at process
start, so a config change is picked up by the next fire.

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea). Inspired
by espanso; replaces its daemon + YAML with a picker TUI over a plain JSON
catalog.

## Features

- **Picker TUI** — opens in *browse* order (most recently used first, from a
  use-recency file); typing switches to the matcher's ranking. Rows are
  label-only: the label is the row's identity, the matcher ranks declared
  `search_terms` above label prose, and every whitespace-separated token of
  the query must match (multi-word queries work).
- **Detached paste** — the picker spawns a helper that waits for the picker
  window to die, refocuses the origin window, sends the paste key, restores
  the clipboard, and records a fire row. If the origin window is gone, it
  aborts rather than paste into "whatever is focused now".
- **ctrl+e edit** — edits the selected row's body in `$SNIP_EDITOR` (or
  `$VISUAL`/`$EDITOR`, default `nvim`), then lands the change through a
  landing script (see [The ctrl+e landing](#the-ctrle-landing)). An emptied
  body offers to **delete** the row after an explicit `[y/N]` confirm — the
  mode is a flag, never an inference from an empty file.
- **`{{clip}}`** — the one variable a body may carry, resolved at expand time
  from the X clipboard (no shell vars, no date vars, no forms).
- **`ctrl+g` shared clipboard** — an opt-in second keypress that lists a
  cross-device clip store as a *disjoint mode* with its own list, filter and
  keys, and pastes a clip body into the origin window. Inside it, **`ctrl+x`
  deletes the highlighted clip from the store**, behind a `y/N` confirm — on
  every device, with no undo and no trash. Unconfigured by default; see
  [The shared clipboard](#the-shared-clipboard-ctrlg).
- **Fire telemetry (opt-in, off by default)** — with `SNIP_ACTIVITY_SPOOL=1`,
  every open/cancel/select appends a row to a local spool, so an audit can
  tell used rows from dead ones. Unset, nothing is written; see
  [Environment](#environment).

## Install

```sh
go install github.com/ZacxDev/promptshipper/desktop/snip/cmd/snip@latest
```

or from a checkout of [promptshipper](https://github.com/ZacxDev/promptshipper),
where snip is the desktop client (`desktop/snip/`, its own Go module):

```sh
git clone https://github.com/ZacxDev/promptshipper
cd promptshipper/desktop/snip
go build -o snip ./cmd/snip
```

### Upgrading from ZacxDev/snip

snip used to live at `github.com/ZacxDev/snip` (0.2.0 and earlier). Two
behaviours changed in 0.3.0, and both can look like breakage:

- **Fire telemetry is off unless you turn it on.** Earlier versions always
  appended fire rows to the activity spool. Now nothing is written unless
  `SNIP_ACTIVITY_SPOOL=1` is set in snip's environment (your launcher or WM
  keybind wrapper). `ACTIVITY_SPOOL_DIR` alone no longer turns it on.
- **`shared.env` with a plain `http://` hostname is refused.** The shared
  clipboard only sends its token over `http://` to `localhost` or a private IP
  address (see [Configuring it](#configuring-it)); a hostname needs `https://`.
  The old README's own example (`http://your-store.example:8080`) is exactly
  the case. You see it at once, as a config error naming the file — change
  `SNIP_SHARED_URL` to the server's IP address (e.g. its Tailscale `100.x`
  address), or put the clip server behind https.

Requires Go 1.26+. Runtime prerequisites (Linux/X11): `xclip`, `xdotool`;
optionally `i3` (`i3-msg` is used to refocus when available, `xdotool`
otherwise) and `dunstify` for failure toasts.

## The catalog

`snip` reads a plain JSON array — `$SNIP_CONFIG`, else
`$XDG_CONFIG_HOME/snip/snippets.json`, else `~/.config/snip/snippets.json`:

```json
[
  {
    "label": "End-of-session ritual",
    "body": "reflect on work done and key learnings",
    "search_terms": ["end", "session", "ritual"],
    "category": "prompt"
  }
]
```

- `label` — the row's identity. Must be non-empty and unique; the loader
  refuses the whole file otherwise, loudly, naming the file and row.
- `body` — the expansion text; may carry `{{clip}}`.
- `search_terms` — declared tokens; a query reaching a row through them
  outranks one that only spells it in the label.
- `category` — picks the row icon and label colour (path/ssh/kubeconfig/
  prompt/clipboard are known; anything else renders the plain default).

Typically the file is *generated* from a declarative source (nix, home-manager,
a template) at deploy time — that is what the ctrl+e landing assumes.

## Commands

```
snip                 the picker TUI (default; also `snip picker`)
snip paste           the detached paste helper (spawned by the picker)
  --label L          the snippet to paste, by its label (required)
  --body-file F      paste F's contents instead of the artifact's body
  --ephemeral        paste F's contents WITHOUT loading the catalog
                     (the shared clipboard's path)
snip list            the catalog as a table (--category C to filter)
snip search QUERY    ranked matches (the same matcher the picker runs)
snip get LABEL       the raw body ({{clip}} unresolved)
snip expand LABEL    the resolved expansion ({{clip}} from the X clipboard)
```

Exit codes: `0` success/no-op (a picker cancel is 0), `1` a real failure
(config, X11, paste path), `2` usage.

Picker keys: `↑/↓` move · `⏎` paste · `ctrl+e` edit · `ctrl+g` shared
clipboard · `esc` cancel.

Shared-clipboard keys (`ctrl+g`, a disjoint table — nothing falls through to
the ones above): `↑/↓` move · `⏎` paste the clip · `ctrl+x` delete the clip
from the store, behind a `y/N` confirm · `esc` back to the catalog.

## The shared clipboard (`ctrl+g`)

A second keypress inside the picker switches to a **cross-device clipboard**:
a separate, chronological, unlabelled store served over your own network, so
text captured on another device can be pasted into the window you were just
in. It is **opt-in and unconfigured by default** — with no config file the
mode says so and the catalog picker is unaffected.

It is a **mode, not a merged list**, and deliberately so:

- a clip has no label and no declared `search_terms`, and its searchable text
  is the *body* — which the catalog matcher never reads. Two populations, two
  match functions; the catalog's `Rank` is untouched.
- the key table is **disjoint**: nothing falls through to the browse keys, so
  `ctrl+e` (edit-and-land, which has no meaning for a row from another
  device) is swallowed rather than acting on a catalog row.
- the list is the server's order, **verbatim** — chronological newest-first,
  with no recency re-sort. Typing filters it by substring over the body.
- the fetch is **lazy**: it happens on the keypress, never at picker open, so
  an open that never asks for a clip pays no network cost.
- a failed fetch is **not** an empty list. The three states are rendered
  distinctly — *fetching…*, *the store is empty*, and *unreachable, with the
  reason in words* — because "you have no clips" and "the store is down" are
  different facts.
- pasting carries the **body**, not an id, so the detached helper never
  re-looks-up a row that a delete from another device may have removed in the
  meantime. `{{clip}}` is **not** expanded in a clip body (it came from
  another machine and has no claim on this one's clipboard), and a clip paste
  is not recorded in the catalog's browse order.
- clip fire rows carry their own telemetry `source` (`snipclip`), so an audit
  of *catalog* demand never counts a clipboard paste as demand for a snippet.
  Those rows also carry **no query**: the shared filter matches on the clip
  *body*, so what you type here is a fragment of the captured text, and it is
  dropped before the row is written (the `method` flag — whether anything was
  typed at all — is kept). A catalog row still carries its query, which is
  label words.

### Configuring it

The store is promptshipper's **clip server** (`server/clip/`). There are two
ways to point snip at it; use exactly one.

**From the pairing payload (recommended).** The same payload the phone scans
([docs/pairing.md](../../docs/pairing.md), version 1). On the server machine,
run `./setup --pairing` in your own terminal and save its `PAIRING_PAYLOAD=…`
line — or just the JSON after the `=` — to `~/.config/snip/pairing.json`,
mode **0600**:

```sh
install -m 600 /dev/null ~/.config/snip/pairing.json
$EDITOR ~/.config/snip/pairing.json     # paste the PAIRING_PAYLOAD= line
```

snip uses the payload's `clip` object and **ignores `stt` entirely**: the
desktop form from `./setup --pairing --for desktop` has no `stt`, a phone
payload has one, and both work. Nothing about `stt` can cause a refusal. The v1
refusals on the rest are kept: a payload that is not
`"type":"promptshipper-pairing"`, has a `v` other than `1`, lacks `clip` (or
its `url`/`token`), has a clip base URL with a path, query or no port, or a
token under 24 characters or outside `[A-Za-z0-9._~-]` is refused with the
reason. Unknown fields are ignored. After `./setup --rotate`, save the new
payload over the old file.

**Or an env file.** `~/.config/snip/shared.env`, mode **0600**, with two keys:

```sh
SNIP_SHARED_URL=http://100.101.102.103:8139
SNIP_SHARED_TOKEN=your-bearer-token
```

`$SNIP_PAIRING` / `$SNIP_SHARED_ENV` override the two paths, and an override
wins over a default file. **Two sources at the same level are an error, not a
precedence**: both overrides set, or both default files present, and the shared
view tells you which two files to choose between — a stale `shared.env`
silently winning over a fresh pairing is the case this prevents.

**snip ships no default endpoint and no default token, on purpose.** This is
a public repository; an address compiled in here would be the *author's*
network, and a token distributed in a build artifact (a Nix store path is
world-readable) would be readable by every local user on the machine. A
launcher wrapper may override the *path* (`SNIP_PAIRING`, `SNIP_SHARED_ENV`);
never the value.

Missing, unreadable or short credentials are a **named failure in the picker**,
never a silent empty list — the file path(s) and the remedy are in the
message, and no message ever carries a token. In the env file the URL must be
`http(s)` and the token at least 8 characters; a pairing payload has the
stricter v1 rules above.

**Plain `http://` only to a private IP.** Whichever file the URL came from,
snip refuses to send a request — and so the token — over `http://` unless the
host is `localhost` or an IP literal in 10/8, 172.16/12, 192.168/16,
100.64/10 (Tailscale's CGNAT range), 127/8, `::1` or `fd00::/8`. A hostname
needs `https://`, even one that resolves privately at home: what it resolves
to depends on the network the laptop is on now. This is the same rule as the
Android app.

The service it talks to is four routes — `GET /v1/clips?limit=N&q=…`,
`POST /v1/clips`, `DELETE /v1/clips/{id}`, `GET /healthz`. snip **reads**
(`GET /v1/clips?limit=N`) and filters locally, and **deletes**
(`DELETE /v1/clips/{id}`) when the operator presses `ctrl+x` and answers the
confirm. It does **not** capture — nothing in snip writes a clip — and it does
not use the server's `q=` search, whose semantics are its own (a full-text
index's substring matching is not the same rule as this client's per-token body
substring test, and the two must not be assumed to agree).

### Deleting a clip (`ctrl+x`)

`ctrl+x` in the shared mode removes the highlighted clip **from the store**:
every device loses it, there is no trash and the service has no undo. So:

- it only **asks**. The chord raises a modal `[y/N]` naming the clip's id and a
  preview of its body, and nothing is called until `y`. The prompt is rendered
  from the armed state itself, so no other event can take the question off the
  screen while leaving the answer live.
- the list on screen must be the store's **current answer**. While a fetch or a
  delete is in flight — re-entering the mode, or the re-list after a deletion —
  `⏎` and `ctrl+x` are inert, because the rows are from the previous read.
- the list is **re-read from the store** after a delete lands rather than edited
  locally, so a refused delete can never show a deletion that did not happen.
- a 404 is **success**: the row is gone either way, which is what was asked for.

## Nix

`desktop/snip/package.nix` is the package; promptshipper's root flake exposes
it as `packages.<system>.snip` (`nix run` uses its `meta.mainProgram`), x86_64-linux only:

```sh
nix build github:ZacxDev/promptshipper#snip    # or: nix run github:ZacxDev/promptshipper#snip -- --version
# as an input, with the consumer's nixpkgs:
#   inputs.promptshipper.url = "github:ZacxDev/promptshipper";
#   inputs.promptshipper.inputs.nixpkgs.follows = "nixpkgs";
```

`package.nix` reads the version out of `cmd/snip/version.go` (never a hand-held
literal), pins the runtime tools the binary execs (`xclip`, `xdotool`,
`dunstify`, `bash`, `nvim`) by store path, and runs **no** Go checks at build
time — the tests run in CI; a red test must never be able to fail a machine
upgrade. It does not pin `i3` (the binary falls back to `xdotool` when
`i3-msg` is absent) and does not ship a landing script.

## The ctrl+e landing

Editing lands through an external script: `$SNIP_REPO/scripts/snip-edit.sh`,
receiving the label (and `--delete` for the delete mode). The script's stderr
is passed through verbatim into the TUI, so its refusals are the message.

`SNIP_REPO` unset means **no landing**: ctrl+e reports that the edit landing
is unavailable, and nothing is changed. snip deliberately ships no default
checkout path and no landing script — where your catalog's source lives is
your setup's business. If your catalog is a plain JSON file you manage by
hand, a landing script can be as small as "write the new body back into the
JSON array"; the contract is only: refuse loudly in one line when the landing
is not safe.

The delete mode is the landing script's explicit `--delete` flag — an emptied
body never deletes by inference.

## Environment

| Variable | Meaning |
| --- | --- |
| `SNIP_CONFIG` | catalog path override |
| `SNIP_RECENT` | use-recency file override (browse order) |
| `SNIP_EDITOR` / `VISUAL` / `EDITOR` | the ctrl+e editor (default `nvim`) |
| `SNIP_REPO` | checkout holding `scripts/snip-edit.sh` (unset = no landing) |
| `SNIP_RESTORE_DELAY_MS` | clipboard-restore delay (default 300) |
| `SNIP_ORIGIN_WINDOW` / `SNIP_ORIGIN_CLASS` | set by the WM keybind wrapper |
| `SNIP_QUERY` / `SNIP_OPEN_LATENCY_MS` | injected by the picker into the helper |
| `SNIP_ACTIVITY_SPOOL` | **`1` turns fire telemetry on; anything else (unset, `0`, `true`) leaves it off** and nothing is written |
| `ACTIVITY_SPOOL_DIR` | fire-row spool directory override (only where; it does not turn telemetry on) |
| `SNIP_PAIRING` | shared-clipboard pairing-payload **path** override (default `~/.config/snip/pairing.json`, mode 0600). The path only — never the payload |
| `SNIP_SHARED_ENV` | shared-clipboard env-file **path** override (default `~/.config/snip/shared.env`, mode 0600, holding `SNIP_SHARED_URL` + `SNIP_SHARED_TOKEN`). The path only — never the token value |

State paths: the recency file is `$XDG_STATE_HOME/snip/recent.json` (else
`~/.local/state/snip/recent.json`); fire rows, when `SNIP_ACTIVITY_SPOOL=1`,
go to `$XDG_STATE_HOME/activity/spool` (else `~/.local/state/activity/spool`).

## Wire it to a hotkey

Bind your WM key to the picker and let it capture the origin window:

```sh
snip-launch ... snip picker        # any wrapper that exports SNIP_ORIGIN_WINDOW/CLASS
```

A wrapper that records the focused window's id and class into
`SNIP_ORIGIN_WINDOW` / `SNIP_ORIGIN_CLASS` before exec'ing `snip picker` is
all snip needs to paste back into the window you fired from.

## Development

```sh
go vet ./...
go test ./...
```

Release builds stamp the version via `-X main.buildVersion=vX.Y.Z`; the
declaration's shape is pinned by `TestVersionDeclarationShape`.

## Cutting a release

`cmd/snip/version.go` is the single source of truth. `package.nix` reads that
one line with a regex and **throws at flake-eval time** if it does not match
exactly once, so a reformatted declaration breaks every consumer's evaluation
instead of quietly shipping a wrong version; `TestVersionDeclarationShape`
pins the same shape in CI, so you find out one step earlier.

snip is a nested Go module in a monorepo, so its tags carry the module's
directory as a prefix (`desktop/snip/vX.Y.Z` — the form `go install …@vX.Y.Z`
resolves). A bare `v*` tag is the server images' release, not snip's.

1. **Check whether `main` has already moved past the last tag.** Nothing
   enforces this, which is why it is the step that gets skipped:

   ```sh
   git log --oneline "$(git describe --tags --abbrev=0 --match 'desktop/snip/v*')"..main -- desktop/snip
   ```

   Non-empty output while `buildVersion` still holds the tagged value means
   merged work is sitting inside an already-published release number.

2. Bump `buildVersion` in `cmd/snip/version.go` — that line only, and do not
   reformat it.
3. Commit and push to `main`. Pushing the bump installs nothing; see step 5.
4. Tag it to match, and push the tag:

   ```sh
   git tag desktop/snip/v0.3.0 && git push origin desktop/snip/v0.3.0
   ```

5. **Update the consumer, or you have shipped nothing.** A desktop that pins
   promptshipper as a flake input changes nothing until someone runs
   `nix flake update promptshipper` there and switches. A release that
   stops at step 4 leaves the feature merged, tagged, and still absent from
   the installed binary.

### Why steps 1 and 5 are written down

They are the two halves of one measured failure. `v0.1.1` was cut on
2026-11-01. #2 — the whole `ctrl+g` shared clipboard — merged on 2026-11-16
with `buildVersion` left at `"0.1.1"`, while devrc's `flake.lock` still
pinned the commit *before* it, so the installed binary carried no
shared-clipboard code at all. Because the version had not moved, the build
**with** the feature and the build **without** it were both named
`snip-0.1.1`, differing only by store hash: `snip --version` could not
separate them, and neither could a derivation name in a diff of two system
closures. #3 bumped it to 0.2.0 on 2026-11-17.

So an unbumped version is not a cosmetic lapse — it removes the cheapest
signal anyone has that two builds differ, which is precisely the signal you
reach for when a merged feature appears to be missing. This is a checklist
rather than a CI gate on purpose: the natural gate (fail while commits sit
between the last tag and `HEAD` with the version still equal to the tag's) is
red on every feature PR until someone bumps, and a permanently-red check only
teaches everyone to click through it.

## Provenance

snip began inside the author's configuration repository
([innovation-upstream/devrc](https://github.com/innovation-upstream/devrc)) —
as the espanso replacement for a nix-managed desktop — and was extracted here
in 2026 with a fresh history as `ZacxDev/snip`, then folded into promptshipper
at `desktop/snip/` with that history kept (`git subtree add`). The devrc repo
still carries the author's landing script and desktop integration.

## License

[MIT](LICENSE)
