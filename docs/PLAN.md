# promptshipper — open-source extraction plan

Status: **public since 2026-11-19** (operator decision: free CI, build in public). Pre-release until launch (phase 7).

## What it is

A private, self-hosted, two-way channel between you and your AI agents:

- **Android app** — a floating bubble over any app: tap, talk, and the transcript is
  on your clipboard or shared; history with re-transcribe; a snippet picker for
  canned prompts; clipboard capture to the shared clipboard; screen capture.
- **STT server** — OpenAI-compatible `POST /v1/audio/transcriptions`, plus
  `GET /v1/snippets` (the snippet catalog). Pluggable backend.
- **clip server** — the cross-device clipboard (Go + SQLite FTS5, bearer auth).
- **desktop** — `snip`, hotkey → fuzzy snippet picker → paste.
- **MCP server** — lets an agent read what you just highlighted/dictated on your
  phone and push text back to your phone's clipboard.

Key marketing angle: **higher-bandwidth interaction with your AI agents.**

## Decisions (operator, 2026-11-19)

| Decision | Value |
|---|---|
| Name | `promptshipper` (GitHub: 0 repos at check time; `.dev`/`.app` unregistered; `.com` held by a third party) |
| Repo | NEW repo `ZacxDev/promptshipper`, full stack, fresh history. Server code is copied out of the PRIVATE `homelab-infra` repo **without its history** |
| Scope | Android app, STT server, clip server, snip, **MCP server in v1** |
| License | MIT (matches snip) |
| Signing | NEW release key + NEW app ID; public APK is **not debuggable** |
| Onboarding | **Agent-first**: `AGENTS.md` + one deterministic `./setup` |
| Dogfood | homelab consumes this repo as its upstream (Helm chart for k8s, Nix flake for desktop). **No second copy** — the in-tree copies in `homelab-infra` are deleted after cutover |
| Posting | X, LinkedIn, HN/Reddit |

Defaults chosen by the planner (change by editing this table):

| Default | Value | Why |
|---|---|---|
| App ID | `dev.zacx.promptshipper` | reverse-DNS of a domain the operator owns |
| Quickstart STT backend | CPU faster-whisper (e.g. speaches) | runs on any box; GPU is an opt-in profile |
| Cohere Transcribe | optional backend **only if its license permits** — check before shipping | redistribution |
| Private network | docs recommend Tailscale; Nebula as advanced | easiest path for strangers |
| Registry | GHCR for images and the OCI Helm chart | public, free |

## Target layout

```
android/        the app (from ZacxDev/dicta)
server/stt/     transcription + snippets (from homelab-infra containers/cohere-stt)
server/clip/    clipstore (from homelab-infra containers/clipstore)
server/mcp/     MCP server over clip + history
desktop/snip/   from ZacxDev/snip (MIT; history may be kept via subtree)
deploy/compose/ docker-compose quickstart (+ gpu profile)
deploy/helm/    chart: existingSecret / existingClaim supported
flake.nix       packages + NixOS/home-manager modules
AGENTS.md       agent-first onboarding entrypoint
docs/           install, pairing, architecture, journal/
```

## Phases

1. **Skeleton** — LICENSE, README stub, this plan, CI scaffold. *(done in the first commit)*
2. **Server extraction** — `server/stt` with a pluggable backend, `server/clip`,
   `deploy/compose`, `./setup` (prereqs → tokens/.env → compose up → health check →
   pairing QR), CI building images to GHCR on tags.
3. **History export/import in `ZacxDev/dicta` FIRST**, shipped by OTA, so the
   operator's on-device history survives the move to a new app ID.
4. **Android port** — move the app into `android/`, new app ID and package, first-run
   pairing (scan the QR: URL + token), no hardcoded endpoints, cleartext only for
   private ranges, release-key signing, non-debuggable public build, import path.
5. **MCP server**, **snip** folded in, **Helm chart**, **flake**.
6. **Dogfood cutover** — homelab Flux HelmRelease from the chart, devrc flake input,
   delete `containers/cohere-stt` and `containers/clipstore` from homelab-infra,
   archive `ZacxDev/dicta` and `ZacxDev/snip` with pointers here, verify on the handset.
7. **Launch** — user-facing README, 30 s demo video, architecture diagram, flip public,
   posting series.

## 🔴 Hard constraints for every contributor (human or agent)

- **Nothing from the private homelab may land here**: no mesh IPs (`10.42.*`),
  no `*.homelab.lan` / `zacx.dev` hostnames, no secrets, no sops files, no
  homelab-infra git history. Grep for them before every commit.
- **No personal data**: no dictation audio, transcripts, corpus files or snippet
  catalog contents from the operator's real setup. Use synthetic fixtures.
- The dicta handoffs/claudedocs mix project lessons with **client-confidential**
  material from other work. Anything quoted into `docs/journal/` is vetted first.
- Tests carry over and stay green; a new behaviour gets a test shown to fail
  without it.
