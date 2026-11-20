# Architecture

promptshipper is two small HTTP services you run yourself, plus the clients that
talk to them. Nothing in the stack calls a hosted service of ours; there is
none.

## Components

```mermaid
flowchart LR
    subgraph phone["Android phone"]
        app["promptshipper app<br/>dictation bubble · clip capture<br/>snippet picker · history"]
    end

    subgraph desktop["Desktop"]
        snip["snip<br/>hotkey picker (Bubble Tea)"]
        agent["AI agent<br/>(Claude Code, Claude Desktop, any MCP client)"]
        mcp["promptshipper-mcp<br/>stdio MCP server"]
        agent <-->|"stdio: recent_clips / push_clip"| mcp
    end

    subgraph server["Your server (Docker Compose or Helm)"]
        stt["stt :8118<br/>FastAPI<br/>faster-whisper (CPU, default)<br/>· cohere (GPU, opt-in)<br/>· openai (passthrough)"]
        clip["clip :8139<br/>Go + SQLite FTS5<br/>long-poll change feed"]
        catalog[("snippets.json")]
        db[("clipstore.db")]
        stt --- catalog
        clip --- db
    end

    app -->|"POST /v1/audio/transcriptions<br/>GET /v1/snippets"| stt
    app <-->|"POST /v1/clips (captures)<br/>GET /v1/clips?after=&wait= (feed)"| clip
    snip <-->|"GET /v1/clips · DELETE /v1/clips/{id}"| clip
    mcp <-->|"GET /v1/clips · POST /v1/clips (device=mcp)"| clip
```

Every arrow to `stt` or `clip` is plain HTTP with a bearer token, over a
private network you control (a Tailscale tailnet, your LAN, a VPN). Each server
has its own token; `GET /healthz` is the only unauthenticated route on either.

| component | code | role |
|---|---|---|
| STT server | [`server/stt`](../server/stt/README.md) | OpenAI-compatible `POST /v1/audio/transcriptions` with a pluggable backend, and `GET /v1/snippets`, the canned-prompt catalog. Keeps no audio and no transcript history |
| clip server | [`server/clip`](../server/clip/README.md) | the cross-device clipboard: one SQLite file with a full-text index, newest-first list, search, delete, and a long-poll change feed |
| MCP server | [`server/mcp`](../server/mcp/README.md) | a stdio MCP server; two tools, `recent_clips` (read) and `push_clip` (write, always as device `mcp`). Stores nothing |
| snip | [`desktop/snip`](../desktop/snip/README.md) | hotkey → fuzzy snippet picker → paste; `ctrl+g` lists the shared clipboard |
| Android app | [`android`](../android/README.md) | floating dictation bubble, text capture to the clip server (share sheet and the text-selection menu), snippet picker, dictation history, QR pairing, auto-copy of agent pushes |

## Dictation: phone → STT

```mermaid
sequenceDiagram
    autonumber
    participant U as You
    participant A as Android app
    participant S as stt
    participant C as clip
    U->>A: tap the bubble, talk, tap again
    A->>S: POST /v1/audio/transcriptions (audio, Bearer stt token)
    S-->>A: {"text": "...", "unreliable": ..., ...}
    A-->>U: editable card: copy or send
    opt "Send each dictation to my clip feed" (off by default)
        A->>C: POST /v1/clips {"device": "phone-dictation"}
    end
```

The server writes the upload to a temporary file, decodes and transcribes it,
and deletes it. If the phone is not paired, or the server cannot be reached,
the app falls back to the phone's own speech recognizer.

## Agent → phone: the long-poll push

```mermaid
sequenceDiagram
    autonumber
    participant G as AI agent
    participant M as promptshipper-mcp
    participant C as clip
    participant A as Android app (bubble running)
    A->>C: GET /v1/clips?after=SEQ&wait=50
    Note over C: request held open until a clip<br/>is posted or the wait expires
    G->>M: push_clip(text)
    M->>C: POST /v1/clips {"body": text, "device": "mcp"}
    C-->>M: 201 (or 200 "deduped": true)
    C-->>A: {"clips": [{..., "device": "mcp", "seq": N}], "head": N}
    A->>A: device == "mcp" → copy to clipboard, show "From your agent"
    A->>C: GET /v1/clips?after=N&wait=50
```

- Only clips whose `device` is exactly `mcp` are copied automatically. Text
  captured on your other devices appears in the clip list but is never copied
  unasked.
- The app polls only while its bubble is running, and stops five minutes after
  the screen goes off; it holds no wake lock. A push made while it is not
  polling waits at the top of the clip list, and when polling resumes only the
  newest `mcp` clip of the backlog is copied.
- Long-poll rather than SSE or WebSockets: one ordinary request per poll
  survives flaky mobile networks and a suspended foreground service with no
  reconnection protocol. The full contract (`seq`, `head`, resync, limits) is in
  [`server/clip/README.md`](../server/clip/README.md#change-feed-long-poll).

## Agent ← phone/desktop: reading clips

An agent calls `recent_clips` to see what you just highlighted, shared or
copied. The MCP server fetches the newest clips, redacts the clip server's own
token from every body, and filters by your query **locally** — the query is
never sent to the clip server (see
[`server/mcp/README.md`](../server/mcp/README.md#what-is-and-is-not-protected)
for why).

## Pairing

`./setup` generates both tokens and prints a QR code holding a small JSON
payload: both server URLs and both tokens. The phone scans it (through any
installed QR scanner app, or by pasting the text). Desktop tools get a
clip-only code from `./setup --pairing --for desktop`, so the STT token stays
on the phone. Format: [`docs/pairing.md`](pairing.md).
