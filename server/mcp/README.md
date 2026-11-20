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
| `recent_clips` | Newest-first list of what the user captured on their devices, optional `limit` (1..500, **default 20**) and `query` (every word must appear as a case-insensitive substring — the server's FTS5 trigram index). | `GET /v1/clips?limit=&q=` |
| `push_clip` | Send `text` to the user's devices as the newest clip, always with `device` = `mcp`. | `POST /v1/clips` |

Why these and no more:

* **Default limit 20, not the server's 100**: every row lands in the agent's
  context window, and "what did I just highlight" is answered by the newest few.
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
e.g. `clip server answered HTTP 400 invalid_limit: …` — never as an empty list.

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

Build the binary (Go ≥ 1.26; the repository is the source until releases ship):

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
After `./setup --rotate`, re-run the line above.)

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

## The token never leaves the process except in the `Authorization` header

Both output streams — stdout (every MCP frame) and stderr (every log line,
including the SDK's) — go through one redacting writer (`redact.go`) that
replaces the token with `[REDACTED]`. That covers paths no handler writes on
purpose: a clip whose body *is* the token (the user copied it on their phone),
a misconfigured URL pointing at something that echoes request headers into an
error page, an HTTP-stack error string.

It is a byte replace, and that is complete only because the token alphabet is
restricted to `[A-Za-z0-9._~-]` at startup (pairing v1's own rule): a JSON
encoder never escapes those characters, so the token appears in a frame as its
exact bytes. Configuration errors are printed before the writer exists and are
tested separately never to quote the token. A non-JSON error body is redacted
**before** it is truncated, so a cut cannot leave an unrecognisable prefix.

Consequence worth knowing: a clip that contains the token shows to the agent
with `[REDACTED]` in its place. That is intended.

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
* `secret_test.go` — the token-secrecy guards. Each asserts a positive control
  (the `[REDACTED]` marker is present, so the token provably reached that
  stream) and was watched fail under a mutation:

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
