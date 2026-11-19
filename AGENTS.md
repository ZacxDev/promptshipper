# AGENTS.md — installing promptshipper for a user

You are an AI agent asked to set up promptshipper on the user's machine. This
file is everything you need. Read it fully before running anything.

## What it is

promptshipper is a private, self-hosted channel between a person and their AI
agents. This repository's server half runs two small services in Docker:

| service | port | what it does |
|---|---|---|
| **stt** | 8118 | speech-to-text: an OpenAI-compatible `POST /v1/audio/transcriptions`, plus `GET /v1/snippets` (a catalog of canned prompts). Default model: Whisper `small` on the CPU via faster-whisper, downloaded on first start (~0.5 GB) |
| **clip** | 8139 | a cross-device clipboard (SQLite + full-text search) |

Both require a bearer token on every route except `GET /healthz`. The Android
app pairs with them by scanning a QR code that `./setup` prints
([docs/pairing.md](docs/pairing.md)).

## Install — the whole procedure

1. **Prerequisites.** Docker Engine with the compose plugin
   (`docker compose version` works), the daemon running, and the user able to
   run `docker` without sudo. `curl`. Optional: `qrencode` (for the QR code;
   otherwise `./setup` renders it in a throwaway container), `tailscale`.
   Do not install system packages without the user's agreement.

2. **Decide the network path** (see *Networking* below). Ask the user only if
   you cannot tell. Default: if Tailscale is installed and logged in, use it.

3. **Run setup from the repository root:**

   ```sh
   ./setup --qr-png ~/promptshipper-pairing.png                    # auto-detects: Tailscale IP, else the LAN IP
   ./setup --qr-png ~/promptshipper-pairing.png --host 100.x.y.z   # or say which address the phone uses
   ```

   🔴 **Always pass `--qr-png` when you (an agent) run it.** It writes the
   pairing QR code to that file (mode 0600) and prints only the path, so the
   tokens never appear in your output. Without it, setup prints the tokens to
   the terminal for a human to scan.

   Useful flags (`./setup --help` lists all):

   | flag | when |
   |---|---|
   | `--host ADDR` | the phone reaches this machine at ADDR (Tailscale IP / MagicDNS name, or LAN IP) |
   | `--model base` or `--model tiny` | a slow or small machine (Raspberry Pi class, old laptop) |
   | `--stt-port N`, `--clip-port N` | the default ports are taken |
   | `--backend openai --upstream-url URL` | the user already runs an OpenAI-compatible transcription server |
   | `--timeout SECONDS` | a slow connection: the first start downloads the model (default 1800) |

   It is **non-interactive** and **idempotent**: it never prompts, and running it
   again keeps the tokens and every setting not passed again. It exits non-zero
   on any failure (see *Troubleshooting*). The first run builds two images and
   downloads the model: expect several minutes.

4. **Hand over pairing.** Tell the user where the image is, e.g.:

   > Your pairing code is in `~/promptshipper-pairing.png`. Open it on this
   > computer and scan it with the promptshipper app ("Pair with a server"),
   > then delete the file — it contains your access tokens.

   🔴 **Do not open, describe, upload or copy the image's contents, and never
   print `deploy/compose/.env`** — the code holds both bearer tokens. If the
   file is lost, `./setup --pairing --qr-png <path>` writes it again and changes
   nothing else. If the user cannot open an image (remote shell), they can run
   `./setup --pairing` in *their own* terminal to see the code as text.

5. **Report** what you ran, the two server URLs (no tokens), and the result of
   the verification below.

## Verify

`./setup` already checks, and fails loudly otherwise, that both services answer
`/healthz` **and** accept their own token on an authenticated route. To re-check
later without printing secrets:

```sh
docker compose --project-directory deploy/compose ps          # both "running"; stt "healthy"
curl -s http://<host>:8118/healthz                            # {"ok":true,"backend":"faster-whisper",...}
curl -s http://<host>:8139/healthz                            # {"status":"ok"}
```

A real end-to-end check (records nothing; needs any short audio file):

```sh
set -a; . deploy/compose/.env; set +a
curl -s -H "Authorization: Bearer $STT_API_TOKEN" -F file=@speech.ogg -F language=en \
  "http://$PROMPTSHIPPER_HOST:$STT_PORT/v1/audio/transcriptions"
```

`"text"` holds the transcript. `"unreliable": true` with `"constant_signal"`
means the file was pure digital silence.

## Networking

The services speak **plain HTTP** and authenticate with bearer tokens. That is
fine on a private network and **not** fine on the internet.

* **Tailscale (recommended, easiest).** Install Tailscale on this machine and the
  phone, log both into the same tailnet, run `./setup`. It detects the Tailscale
  IP and publishes the ports **on the tailnet address only**, so nothing on the
  LAN or the internet can reach them. Traffic is encrypted by WireGuard.
* **Home LAN.** `./setup --host <LAN IP>` publishes on that LAN address. Anyone on
  the LAN can reach the ports (they still need a token) and traffic is
  unencrypted, so only do this on a network the user trusts. It does not work
  away from home.
* **Other VPNs / mesh overlays** (WireGuard, Nebula, ZeroTier): pass the overlay IP
  as `--host`.
* 🔴 **Never expose these ports to the public internet** — no router port
  forward, no `--bind 0.0.0.0` on a VPS — unless a TLS-terminating reverse proxy
  is in front of them and the user explicitly asks for it. Without TLS the
  tokens travel in cleartext.

`--bind` decides which host address Docker publishes on; `--host` is what goes
into the QR code. They default to the same IP.

## Troubleshooting

| exit / symptom | meaning | fix |
|---|---|---|
| exit 1, `could not detect this machine's address` | no Tailscale IP and no default route | pass `--host` |
| exit 2 | docker / compose / curl missing, or the daemon unreachable | install or start Docker; add the user to the `docker` group (they must log out and in) |
| exit 3, `stt did not become healthy` | usually still downloading the model, or out of memory | `docker compose --project-directory deploy/compose logs --tail 50 stt`; re-run with a larger `--timeout`, or a smaller `--model` |
| exit 3, `clip did not become healthy` | clip refused to start | `... logs clip`; a token problem prints `FATAL: $CLIPSTORE_TOKEN` — run `./setup --rotate` |
| exit 5, `could not render the pairing QR as a PNG` | no `qrencode`, and the docker fallback could not fetch it | install `qrencode`, then `./setup --pairing --qr-png <path>` |
| exit 4, `answered 401 to its own token` | `.env` was edited while the containers kept old values | re-run `./setup` (it recreates containers whose settings changed) |
| `port is already allocated` | something else holds 8118/8139 | `./setup --stt-port 18118 --clip-port 18139`, then re-pair |
| containers not running after a reboot, `cannot assign requested address` in `docker compose ... up` | **Known risk of the tailnet-only bind** (the default when Tailscale is present): Docker can start before Tailscale has its IP, and a port published on the tailnet address cannot bind until that address exists. Not yet observed in this project's testing | `docker compose --project-directory deploy/compose up -d`; to fix it for good, order the docker service after tailscaled (systemd: `After=tailscaled.service`) |
| warning `the phone's address ... is not reachable from this machine` | `--host` and `--bind` disagree | pass both explicitly, or only `--host` |
| the app says "unreliable" on real speech | the threshold does not suit this voice/mic | add `STT_NO_SPEECH_MAX=<higher>` (or `off`) to `deploy/compose/.env`, re-run `./setup` |
| snippets missing in the app | the catalog is empty or invalid JSON | edit `deploy/compose/snippets/snippets.json` (format: `server/stt/README.md`); changes are live |

Tokens leaked or a device lost: `./setup --rotate`, then re-pair every device.

## Things not to do

* Don't commit or print `deploy/compose/.env` — it holds the tokens.
* Don't run `./setup --rotate` unless the user wants every device re-paired.
* Don't enable the `--gpu` profile (the `cohere` backend) on your own initiative:
  it needs an NVIDIA GPU, a Hugging Face token for a gated model, and its
  license has not been reviewed by this project.
* Don't change the published ports to `0.0.0.0` on an internet-facing machine.

## Uninstall

```sh
docker compose --project-directory deploy/compose down        # stop, keep data
docker compose --project-directory deploy/compose down -v     # also delete the model cache and the clipboard
```
