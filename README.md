# promptshipper

A private, self-hosted, two-way channel between you and your AI agents: dictate
from anywhere on Android, share a clipboard across devices, fire canned prompts,
and let your agent read from and write back to your phone over MCP.

Pre-release. See [docs/PLAN.md](docs/PLAN.md).

## Run the servers

```sh
git clone https://github.com/ZacxDev/promptshipper && cd promptshipper
./setup
```

`./setup` needs Docker with the compose plugin. It generates tokens, starts the
transcription server and the clipboard server, checks both, and prints a QR
code for the phone app. Re-running it is safe. Using an AI agent to install it?
Point it at [AGENTS.md](AGENTS.md).

| | |
|---|---|
| [`server/stt`](server/stt/README.md) | speech-to-text (OpenAI-compatible) + the snippet catalog; pluggable backend, CPU by default |
| [`server/clip`](server/clip/README.md) | the cross-device clipboard |
| [`deploy/compose`](deploy/compose/docker-compose.yml) | the compose stack `./setup` drives |
| [`docs/pairing.md`](docs/pairing.md) | the QR pairing payload format |

Keep the servers on a private network (Tailscale is the easy way); they speak
plain HTTP. See *Networking* in [AGENTS.md](AGENTS.md).

MIT licensed.
