# mcp — promptshipper for AI agents

An [MCP](https://modelcontextprotocol.io) server that gives an AI agent (Claude
Code, Claude Desktop, any MCP client) a live, two-way channel to the user's
devices through the promptshipper [clip server](../clip/README.md):

* **read** what the user just highlighted, shared or copied on their phone or
  desktop — no more retyping or pasting it into the chat. Their **dictations**
  are included **only when the user enables "send dictations to my clip feed"
  in the app**; otherwise only highlighted/shared/copied text reaches the clip
  server;
* **push** text back — a command, a reply draft, a link — onto their phone's
  clipboard (see *Push to the phone* for the condition).

It is a thin client of the clip server's HTTP API and stores nothing itself.

## Tools

Two, one per direction.

| tool | purpose | clip API |
|---|---|---|
| `recent_clips` | Newest-first list of what the user captured on their devices, optional `limit` (1..500, **default 20**) and `query` (every word must appear as a case-insensitive substring). **A query is matched locally, over the newest max(`limit`, 100) clips only** (see below). | `GET /v1/clips?limit=…` — never `q=` |
| `push_clip` | Send `text` to the user's devices as the newest clip, always with `device` = `mcp`. Not idempotent: re-pushing identical text bumps it to the front and can re-trigger the phone's auto-copy. | `POST /v1/clips` |

Why these and no more:

* **Default limit 20, not the server's 100**: every row lands in the agent's
  context window, and "what did I just highlight" is answered by the newest few.
  For the same reason each returned body is capped at **16 KiB**; a capped row
  carries `truncated: true` and its full size in `body_bytes`.
* 🔴 **A query searches a bounded window, and never leaves this process.**
  Without a query, `recent_clips` requests exactly `limit` rows. With one, it
  requests the newest **max(`limit`, 100) rows, never above 500** (the clip
  server's cap), with no query, redacts them, filters them locally, then
  applies `limit`. So the default call searches the newest 100 clips; pass
  `limit: 500` to search deepest. Older clips are not findable here. The
  result's `searched` field is how many clips were matched against — the
  search's whole scope (fewer when the store holds fewer, or when the response
  exceeded the 16 MiB cap and the window was halved until it fit). Why not use
  the clip server's own search: see *What is and is not protected*. Why not
  always fetch 500: the clip server builds the whole response in memory on one
  SQLite connection, and 500 rows of 256 KiB clips took its peak RSS from
  24 MB to 800 MB.
* **No get-by-id tool.** The clip server has no `GET /v1/clips/{id}` route yet;
  a tool faking it by scanning the newest 500 would be wrong about older clips.
  Re-add it on that route once it exists.
* **No delete tool.** `DELETE /v1/clips/{id}` exists, but an agent erasing the
  user's clipboard history is destructive and serves nothing in the read/push
  loop. Deletion stays in the user's own hands (the app, snip).
* **No STT tool.** The STT server keeps no history; dictation reaches agents
  only through the clip feed, by the app opt-in above.

Every tool result carries structured content (`created_at` is RFC 3339 UTC).
Clip-server errors are returned as tool errors with the server's stable code,
e.g. `clip server answered HTTP 503 store_unreadable: the clip store could not
be read` — never as an empty list. (An out-of-range `limit` is refused by this
server before any request: `limit must be an integer in 1..500`.)

## Push to the phone

`push_clip` stores a clip with `device` = `mcp`. That value is **hardcoded, not
configurable**: the app copies clips whose device is exactly `mcp` to the
phone's clipboard automatically, so an override would be a way to switch that
off silently.

🔴 **The automatic copy is conditional**: it happens only **while the
promptshipper app's bubble is running and the installed app version supports the
clip feed** (the long-poll `GET /v1/clips?after=&wait=` being added to the clip
server, with the app's auto-copy, alongside this server). Otherwise the clip
waits at the top of the app's clip list (and snip) for the user to copy by hand.
Each push result carries a `note` saying exactly this, so an agent does not
promise more than was delivered. This server cannot observe whether the phone
copied it.

## Install

With Nix (x86_64-linux): the root flake exposes `packages.<system>.mcp`
(`server/mcp/package.nix`), whose binary is `promptshipper-mcp`:

```sh
nix profile install github:ZacxDev/promptshipper#mcp   # or: nix run github:ZacxDev/promptshipper#mcp -- -version
```

Its `-version` prints `0-unstable-<commit>` (the source has no release
version yet). The examples below use `~/.local/bin/promptshipper-mcp`; with
Nix, use `$(command -v promptshipper-mcp)` instead.

Or build the binary yourself (Go ≥ 1.26):

```sh
cd server/mcp
CGO_ENABLED=0 go build -trimpath -o ~/.local/bin/promptshipper-mcp .
```

### Configure: a pairing file (recommended) or env

The server needs the clip server's URL and token. Give it **one** of:

| | |
|---|---|
| `-pairing PATH` or `$PROMPTSHIPPER_PAIRING_FILE` | a [pairing payload](../../docs/pairing.md) JSON file, format v1, **with or without** the `stt` object (the desktop form omits it). Only `clip.url` and `clip.token` are read; `stt` is never required and unknown fields are ignored. |
| `$PROMPTSHIPPER_CLIP_URL` + `$PROMPTSHIPPER_CLIP_TOKEN` | the same two values directly |

Setting both a pairing file and the env pair is refused (exit 2) rather than
resolved by precedence: a stale one silently winning is a 401 with an invisible
cause. The token has no command-line flag, because argv is visible to every user
on the machine.

**Prefer the pairing file**: `claude mcp add -e` and Claude Desktop's `env`
both store the token in plaintext in the client's config file. On the machine
that runs `./setup`, write the file without the token ever reaching the
terminal:

```sh
mkdir -p ~/.config/promptshipper
(umask 077; ./setup --pairing --no-qr | sed -n 's/^PAIRING_PAYLOAD=//p' > ~/.config/promptshipper/pairing.json)
```

(The phone form also contains the STT token, which this server does not use.
After `./setup --rotate`, re-run the line above. `stt` being optional is the
format change `docs/pairing.md` is getting in the clip-server PR, #7; until that
lands, that document still describes `stt` as required.)

### Claude Code

```sh
claude mcp add --scope user promptshipper -- \
  ~/.local/bin/promptshipper-mcp -pairing ~/.config/promptshipper/pairing.json
```

or, with env instead of a file:

```sh
claude mcp add --scope user promptshipper \
  -e PROMPTSHIPPER_CLIP_URL=http://100.101.102.103:8139 \
  -e PROMPTSHIPPER_CLIP_TOKEN=<clip token> \
  -- ~/.local/bin/promptshipper-mcp
```

Check with `claude mcp list` (it should show `promptshipper … ✓ Connected`).

### Claude Desktop

Add to `claude_desktop_config.json` (macOS:
`~/Library/Application Support/Claude/`, Windows: `%APPDATA%\Claude\`) and
restart Claude Desktop. Use absolute paths — Desktop does not expand `~`:

```json
{
  "mcpServers": {
    "promptshipper": {
      "command": "/home/you/.local/bin/promptshipper-mcp",
      "args": ["-pairing", "/home/you/.config/promptshipper/pairing.json"]
    }
  }
}
```

or with env:

```json
{
  "mcpServers": {
    "promptshipper": {
      "command": "/home/you/.local/bin/promptshipper-mcp",
      "env": {
        "PROMPTSHIPPER_CLIP_URL": "http://100.101.102.103:8139",
        "PROMPTSHIPPER_CLIP_TOKEN": "<clip token>"
      }
    }
  }
}
```

### Any other MCP client

It is a stdio server: launch `promptshipper-mcp` with the configuration above.
Exit codes: `0` clean shutdown, `1` the session failed, `2` configuration
refused (the reason is on stderr). Logs go to stderr; stdout is the protocol.

## Transport: stdio only

No streamable-HTTP listener. It would be a second network surface exposing the
user's clipboard and would need an authentication scheme of its own, while every
client this targets launches stdio servers. Add it only together with that auth.

## What is and is not protected

The clip token is sent in one place: the `Authorization` header to the clip
server. Beyond that, two separate mechanisms apply, and neither is a guarantee
about anything else.

**1. Output redaction — the token's bytes.** Both output streams — stdout
(every MCP frame) and stderr (every log line, including the SDK's) — go
through one redacting writer (`redact.go`) that replaces the token, matched
ASCII-case-insensitively, with `[REDACTED]`. Clip bodies and device names are
also redacted at the source, before the local search and the 16 KiB cap see
them. That covers a clip containing the token (the user copied it on their
phone), a misconfigured URL pointing at something that echoes request headers
into an error page, and an HTTP-stack error string. It is a byte replace, and
that is complete only because the token alphabet is restricted to
`[A-Za-z0-9._~-]` at startup (pairing v1's own rule): a JSON encoder never
escapes those characters. Configuration errors are printed before the writer
exists and are tested separately never to quote the token. Error bodies and
clip bodies are redacted **before** they are truncated, so a cut cannot leave
an unrecognisable prefix.

**2. No search oracle — information about the token.** Output redaction alone
did **not** protect the token, and this README used to say it did. The agent's
`query` used to be passed to the clip server, whose substring match answers
"does any clip contain X?" — so with a clip like `my clip token <token> keep
safe`, extending a query one character at a time recovered a 64-hex token
exactly (575 calls, round-1 audit of PR #3) while every result correctly showed
`[REDACTED]`. Now the query never leaves this process and is matched only
against already-redacted bodies, so no result depends on the token's value.
`oracle_test.go` runs every prefix and 1/2/3/8-character window of a token
against two stores that differ only in that token and requires identical
results, and asserts at the HTTP `RoundTripper` that no request carries the
query.

**Not protected:**

* **A stored copy of the token is still in the clip store.** Redaction hides it
  from the agent; it does not remove it. Anyone who can read the clip store's
  database, or holds the token, can read it. Delete such a clip in the app.
* **Variants that are not the exact token.** A token split by whitespace or
  line breaks, encoded (base64, URL-escaped), or partially quoted is not
  recognised, and its pieces are shown and searchable.
* **Other secrets.** Only the clip token configured here is redacted. Passwords,
  the STT token or API keys the user copied are shown to the agent as text —
  that is what a clipboard reader does.
* **Exact-body probing through `push_clip`.** Pushing a body identical to a
  stored clip returns `deduped: true`. That answers "is there a clip whose
  ENTIRE body is exactly this?" — it confirms a fully guessed body and gives no
  partial-match signal, so it cannot be extended character by character, but it
  is a yes/no answer about stored text.
* **Any future tool or parameter that makes the clip server compute over raw
  bodies** (a server-side search, filter or count) reopens the oracle, whatever
  the output redaction does.

## Tests

```sh
cd server/mcp
CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test -count=1 ./...
```

* `harness_test.go` — an in-process fake of the clip API, and a driver that runs
  the **real** `run()` (flags, config, redacting writers, stdio transport)
  against an MCP client over pipes, capturing the raw bytes of both streams.
* `realclip_test.go` — the **seam** test: the same calls against a real
  clipstore binary, so the fake cannot drift from `server/clip`. It needs
  `CLIPSTORE_BIN=/path/to/clipstore` and skips without it; CI (the `mcp` job in
  `.github/workflows/ci.yml`) sets `REQUIRE_REAL_CLIP=1`, which turns that
  skip into a failure.

  ```sh
  (cd ../clip && CGO_ENABLED=0 go build -o /tmp/clipstore .)
  CLIPSTORE_BIN=/tmp/clipstore REQUIRE_REAL_CLIP=1 CGO_ENABLED=0 go test -count=1 ./...
  ```
* `oracle_test.go` — the search-oracle guards (section above). Both were run
  **red against `3e5ef8d`**, the code that sent `q=` to the clip server: 451 of
  514 token-derived queries gave an observable difference, and every request
  carried `q=`.
* `secret_test.go` — the token-secrecy guards. Each asserts a positive control
  (the `[REDACTED]` marker is present, so the token provably reached that
  stream; for stdout it comes from an error detail that is NOT redacted at the
  source, so only the stream writer can have produced it) and was watched fail
  under a mutation:

  | mutation | guard that goes red |
  |---|---|
  | `redactBytes` returns its input | `TestTheTokenNeverAppearsInAnyOutput` (stdout frames, decoded results, stderr) |
  | `run()` hands raw stdout to the transport | same test, stdout + decoded results |
  | logger built on raw stderr | same test, stderr |
  | error body truncated before redaction | `TestTruncatedErrorBodyDoesNotLeakATokenPrefix` |
  | redact each `Write` instead of each line | `TestRedactingWriterCatchesATokenSplitAcrossWrites` |
  | a config error quotes the token | `TestConfigErrorsNeverEchoTheToken` |
  | `pushDevice` changed from `mcp` | `TestPushClipStoresAsDeviceMCPAndReportsDedup` and the seam test |
  | a pairing file without `stt` refused | `TestConfigFromAPairingV1File` |
  | search filters RAW bodies, then redacts | `TestSearchIsNotATokenOracle` |
  | server applies `limit` before the local filter | `TestLimitIsAppliedAfterTheLocalFilter` |
  | body capped before redaction | `TestBodiesAreCappedAfterRedaction` |
  | case-sensitive redaction | `TestAnUpperCasedTokenIsRedactedToo` |
  | no window halving on a too-large response | `TestAnOversizedResponseShrinksTheWindowInsteadOfFailing` |
  | `push_clip` claims `idempotentHint` | `TestTheToolLedgerIsExactlyTheseTools` |
  | local `limit` bound removed | `TestClipServerErrorsAreToolErrorsWithTheirCode` |
  | no-query path fetches the query window (or 500) | `TestTheFetchedWindowIsLimitWithoutAQueryAndBoundedWithOne` |
  | query window not raised to `limit` | same test |
  | device not redacted at the source | `TestRecentClipsRedactsDevicesWithoutTheStreamWriter` |
