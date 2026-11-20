# promptshipper

**Higher-bandwidth interaction with your AI agents.** Talk instead of type, and
move text between your phone, your desktop and your agents without retyping it.
promptshipper is a small self-hosted stack: a speech-to-text server and a
cross-device clipboard you run on your own machine, an Android app with a
floating dictation bubble, a desktop snippet picker, and an MCP server that lets
your agent read what you just captured and push text straight onto your phone's
clipboard. No accounts and no hosted service: your audio and your clips stay on
hardware you control.

<!-- DEMO: replace this block with the demo GIF/video once it exists. -->
> 🎬 **Demo video: coming soon.**

## What you get

- **Dictate anywhere on Android.** A floating mic bubble over any app: tap,
  talk, tap, and the transcript is in an editable card ready to copy or send.
  Transcribed by your own server (Whisper on the CPU by default).
- **A shared clipboard across devices.** Share or select text on the phone and
  it lands on your clip server; paste it on the desktop with `snip`, or let an
  agent read it.
- **Your agent can talk back.** Over MCP, an agent pushes a command, a link or a
  reply draft, and it is copied onto your phone's clipboard while the bubble is
  running.
- **Canned prompts.** A snippet catalog served by your STT server and picked
  from the phone; on the desktop, `snip` is hotkey → fuzzy picker → paste.
- **One command to install.** `./setup` generates tokens, starts the servers in
  Docker, checks them, and prints a QR code the phone scans to pair.

How the pieces fit together: [docs/architecture.md](docs/architecture.md).

## Quickstart

You need a Linux machine with Docker and the compose plugin, and `curl`.
[Tailscale](https://tailscale.com) on that machine and your phone is the easy
way to connect them.

### With your coding agent (recommended)

Clone the repo and hand it to the agent you already use:

```sh
git clone https://github.com/ZacxDev/promptshipper && cd promptshipper
```

> Read AGENTS.md and install promptshipper for me.

[AGENTS.md](AGENTS.md) is written for that agent: prerequisites, picking the
network path, running `./setup` so your tokens never appear in its output,
verifying the install, and troubleshooting. It finishes by telling you where it
wrote your pairing QR code.

### By hand

```sh
git clone https://github.com/ZacxDev/promptshipper && cd promptshipper
./setup                            # auto-detects your Tailscale IP, else your LAN IP
./setup --host 100.101.102.103     # or say which address the phone should use
```

`./setup` writes `deploy/compose/.env` (mode 0600, holds the tokens), seeds the
snippet catalog, starts the `stt` and `clip` containers, waits until both are
healthy and accept their own tokens, and prints the pairing QR code (install
`qrencode` to see it as a QR code; otherwise you get the same payload as text).
The first run builds two images and downloads the Whisper `small` model
(~0.5 GB), so expect several minutes. Re-running it is safe: it keeps your
tokens and settings. `./setup --help` lists every flag; the useful ones:

| | |
|---|---|
| `--model base` / `--model tiny` | a slow or small machine |
| `--backend openai --upstream-url URL` | forward to an OpenAI-compatible transcription server you already run |
| `--gpu` | the opt-in Cohere backend on an NVIDIA GPU (gated model, see [server/stt](server/stt/README.md#the-cohere-backend-opt-in)) |
| `--stt-port N`, `--clip-port N` | the defaults (8118, 8139) are taken |
| `--pairing` | print the pairing code again; changes nothing |
| `--rotate` | new tokens; every device must re-pair |

**Pull the published images instead of building them.** Put these two lines in
`deploy/compose/.env` before running `./setup` (it keeps them):

```sh
PULL_POLICY=missing
PROMPTSHIPPER_VERSION=0.1.0
```

That uses `ghcr.io/zacxdev/promptshipper-stt:0.1.0` and
`ghcr.io/zacxdev/promptshipper-clip:0.1.0` (linux/amd64 and linux/arm64).

**Without `./setup`:** `deploy/compose/` is an ordinary Compose project. Copy
`.env.example` to `.env`, fill in the two tokens (`openssl rand -hex 32`) and
the bind address, and run `docker compose up -d` in that directory.

**On Kubernetes:** the Helm chart is published at
`oci://ghcr.io/zacxdev/charts/promptshipper` (version `0.1.0`); see
[deploy/helm/README.md](deploy/helm/README.md).

## Install the app on your phone

The Android app (Android 8.0 or later) is a signed APK on this repository's
[GitHub Releases](https://github.com/ZacxDev/promptshipper/releases), tagged
`android-v*`.

- **With [Obtainium](https://github.com/ImranR98/Obtainium)**, which also keeps
  it updated: add an app with the URL `https://github.com/ZacxDev/promptshipper`.
- **Or directly:** download the `.apk` from the
  [latest release](https://github.com/ZacxDev/promptshipper/releases/latest)
  and install it.

Open the app, grant the microphone and "display over other apps" permissions,
tap **Pair with a server → Scan QR code**, and scan the code `./setup` printed.
Scanning goes through any installed QR scanner app (Binary Eye, for example);
without one, copy the code's text with your camera app and tap **Paste**.
Details: [android/README.md](android/README.md).

The release signing certificate's SHA-256 fingerprint is
`90cdc77e35a42d8fc457cb6ca97d711f83bbd189135ca41ff2e49d75ee4e6780`. The release
workflow will not publish an APK signed by anything else, or a debuggable one.

## Connect your agent (MCP)

[`server/mcp`](server/mcp/README.md) is a stdio MCP server with two tools:
`recent_clips` (what you just highlighted, shared or copied on your devices)
and `push_clip` (send text to your devices; the phone copies it to its clipboard
while the bubble is running). It works with Claude Code, Claude Desktop, or any
MCP client. From the repository root, on the machine that ran `./setup`:

```sh
# build it (Go 1.26+) -- or, with Nix on x86_64-linux:
#   nix profile install github:ZacxDev/promptshipper#mcp
# (installs the same promptshipper-mcp binary; use $(command -v promptshipper-mcp)
#  in place of ~/.local/bin/promptshipper-mcp below)
(cd server/mcp && CGO_ENABLED=0 go build -trimpath -o ~/.local/bin/promptshipper-mcp .)

# a clip-only pairing file (no STT token), written without printing the token
mkdir -p ~/.config/promptshipper
(umask 077; ./setup --pairing --for desktop --no-qr | sed -n 's/^PAIRING_PAYLOAD=//p' > ~/.config/promptshipper/pairing.json)

# register it with Claude Code
claude mcp add --scope user promptshipper -- \
  ~/.local/bin/promptshipper-mcp -pairing ~/.config/promptshipper/pairing.json
```

Claude Desktop, other clients, and exactly what the server does and does not
protect: [server/mcp/README.md](server/mcp/README.md).

Your dictations reach the agent only if you turn on **Send each dictation to my
clip feed** (Settings → Shared clipboard) in the app. It is off by default.

## Components

| | |
|---|---|
| [`server/stt`](server/stt/README.md) | speech-to-text server (FastAPI, OpenAI-compatible API). Backends: faster-whisper on the CPU (default), Cohere on an NVIDIA GPU (opt-in), OpenAI-compatible passthrough. Also serves the snippet catalog |
| [`server/clip`](server/clip/README.md) | the cross-device clipboard (Go, SQLite FTS5) with a long-poll change feed |
| [`server/mcp`](server/mcp/README.md) | the MCP server for agents (stdio) |
| [`desktop/snip`](desktop/snip/README.md) | hotkey → fuzzy snippet picker → paste (Bubble Tea, Linux/X11); `ctrl+g` opens the shared clipboard |
| [`android`](android/README.md) | the Android app |
| [`deploy/compose`](deploy/compose/docker-compose.yml) | the Compose stack `./setup` drives |
| [`deploy/helm`](deploy/helm/README.md) | the Helm chart |
| [`docs/pairing.md`](docs/pairing.md) | the QR pairing payload format |
| [`AGENTS.md`](AGENTS.md) | install instructions for an AI agent |

`flake.nix` provides a development shell (`nix develop`) with the toolchains
for the servers and their tests, plus two desktop packages for x86_64-linux:
`snip` (`nix build github:ZacxDev/promptshipper#snip`) and `mcp`, whose binary
is `promptshipper-mcp`. The servers install with Compose or the Helm chart, not
the flake.

## Security model

- **Two servers, two tokens.** The STT server and the clip server each require
  their own bearer token on every route except `GET /healthz`; `./setup`
  generates each as 32 random bytes. Neither ever runs open: the clip server
  refuses to start without a token, and the STT server answers `503` on every
  authenticated route until it has one. Desktop tools and agents get a
  clip-only pairing code (`./setup --pairing --for desktop`), so the STT token
  stays on the phone.
- **Private network only: the servers speak plain HTTP.** `./setup` publishes
  the ports only on a private address (Tailscale/CGNAT, RFC 1918, IPv6 ULA,
  loopback), refuses a public one, and never binds `0.0.0.0` unless you pass
  `--bind` yourself. The Android app and `snip` refuse to send a token over
  `http://` to anything but a private address; everything else needs
  `https://`. Never port-forward the servers to the internet without a TLS proxy
  in front.
- **What leaves the box.** With the default backend, your audio is transcribed
  on your machine: the upload is written to a temporary file that is deleted
  after transcription, and the STT server keeps no transcript history. The
  model is downloaded once from Hugging Face on first start. With
  `--backend openai`, audio goes to the upstream you configure. Clips are kept
  in one SQLite file on your server with no expiry, until you delete them. An
  agent using the MCP server reads clips into its context, so the model behind
  that agent sees them. When the phone is not paired or cannot reach your
  server, the app falls back to the phone's own speech recognizer, which may be
  a cloud service depending on the phone.
- **A leaked token or a lost phone:** `./setup --rotate`, then re-pair every
  device. The pairing code contains the tokens; treat it like a password.

## Status

v0.1.0: the server images, the Helm chart and the Android APK are published.
The roadmap is in [docs/PLAN.md](docs/PLAN.md).

## License

[MIT](LICENSE)
