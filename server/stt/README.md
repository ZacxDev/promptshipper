# stt — transcription server + snippet catalog

An OpenAI-compatible `POST /v1/audio/transcriptions` endpoint with a pluggable
model backend, plus `GET /v1/snippets`, the canned-prompt catalog the clients
pick from. FastAPI, one process. Deployed by `deploy/compose/` (see the repo's
`AGENTS.md`).

## Routes

| | auth | |
|---|---|---|
| `GET /healthz` | none | `200 {"ok":true,"backend":…,"device":…}` once the model is loaded; `503` before (first start downloads the model) |
| `GET /` | bearer | service, backend and model name |
| `GET /v1/snippets` | bearer | the mounted catalog's bytes, `ETag` + `If-None-Match` → `304`; a named `503` when it is missing, malformed or empty — never an empty `200` |
| `POST /v1/audio/transcriptions` | bearer | multipart: `file` (any container ffmpeg reads — ogg/opus, aac/m4a, webm, wav, mp3), `language` (bare code, default `en`), `punctuation` (bool, default true) |

No TLS: run it on a private network (see `AGENTS.md`, *Networking*).

## Backends

`STT_BACKEND` picks one; an unknown name refuses to boot.

| `STT_BACKEND` | image | runs on | notes |
|---|---|---|---|
| `faster-whisper` (default) | `Dockerfile` | CPU (or CUDA with `STT_DEVICE=cuda`) | `STT_MODEL` default `small` (~0.5 GB, downloaded at first start); `base`/`tiny` for weak machines |
| `cohere` | `Dockerfile.cohere` | NVIDIA GPU | **opt-in** (`./setup --gpu`); gated model download — see below |
| `openai` | `Dockerfile` | anywhere | passthrough to any OpenAI-compatible upstream: `STT_UPSTREAM_URL` (+ `STT_UPSTREAM_TOKEN`, `STT_UPSTREAM_MODEL`, `STT_UPSTREAM_FORMAT`) |
| `fake` | `Dockerfile` | anywhere | no model; answers `STT_FAKE_TEXT`. For tests and smoke-testing a deploy only |

Each backend's module docstring (`backends/*.py`) lists its own variables.

### The `cohere` backend (opt-in)

It loads `CohereLabs/cohere-transcribe-03-2026` from the Hugging Face Hub. The
model is licensed **Apache-2.0** per its Hugging Face model card (`license:
apache-2.0` in the Hub metadata, read 2026-11-19). This repository ships only
the loader code, never the weights, and the GPU image is not published to GHCR —
compose builds it locally.

The download is **gated**. Before the first start:

1. Sign in to huggingface.co, open the model page and accept its terms.
2. Create an access token (Settings → Access Tokens, *read* scope).
3. Put it in `deploy/compose/.env` as `HF_TOKEN=...` (or export `HF_TOKEN`
   before running `./setup --gpu`).

It needs an NVIDIA GPU and the NVIDIA container toolkit.

## Common configuration

| | default | |
|---|---|---|
| `STT_API_TOKEN` | — | the bearer token (`./setup` generates it). With no token configured every authenticated route answers `503 server has no STT_API_TOKEN configured`, and the app treats that 503 as permanent |
| `STT_API_TOKENS` | — | extra named tokens, `name:token,name2:token2`, each revocable on its own; `name:public:token` marks one usable through the public entry (below). Validate with `python3 check_tokens.py` before a restart: a malformed list refuses boot |
| `STT_NO_SPEECH_MAX` | per backend | `unreliable` fires when `no_speech_prob` is above it; `off` disables the rule |
| `STT_AVG_LOGPROB_MIN` | off | `unreliable` fires when `avg_logprob` is below it |
| `STT_MAX_AUDIO_SECONDS` | `3600` | longer uploads are `413` |
| `STT_MAX_UPLOAD_BYTES` | `STT_MAX_AUDIO_SECONDS` × 32000 + 1 MiB | the largest upload body accepted (`413` above it), enforced **before** the body is read, for both a declared `Content-Length` and a chunked body. The token is also checked before the body is read, so an unauthenticated caller cannot make the server spool anything |
| `HF_HOME` | `/models/hf` | model cache; compose keeps it on the `stt-models` volume |

The snippet catalog path is fixed at **`/etc/snip/snippets.json`** (deliberately
not configurable). Mount a *directory* there so edits are picked up without a
restart.

### Optional: the public entry

For exposing **one locked-down token** through a reverse proxy (for instance a
TLS proxy reachable from outside your private network) while every other token
stays private-network-only:

* give that caller a public-scoped entry: `STT_API_TOKENS=phone:public:<token>`
  (internal entries are `name:token`, or spelled out `name:internal:token`);
* configure the proxy to **set (overwrite)** the header `X-STT-Entry: public` on
  every request it forwards.

A request carrying `X-STT-Entry` (any value, empty included, any number of
times) is accepted **only** with a public-scoped token; an internal token gets
`403 this token is not permitted through the public entry`. Requests without the
header are unaffected. The header can only narrow what a token may do, so a
client that forges it only restricts itself. Without such a proxy the feature
is inert, and `./setup` does not set one up. Pinned by `tests/test_public_entry.py`.

## Response contract

Every `200` carries exactly these keys, for every backend and for the
dead-capture path (pinned by `tests/test_contract.py`):

| field | type | what the Android app does with it | how backends fill it |
|---|---|---|---|
| `text` | string | **required**; a 2xx without it is treated as a failure. Put on the clipboard / shown | the transcript; `""` for a dead capture or when the VAD heard nothing |
| `unreliable` | bool \| null | `true` → shows "unreliable" caveat (wins over `truncated`); `null` → "no signal", never "looks fine" | server-side verdict from the thresholds; `null` when no configured rule had a signal to check |
| `truncated` | bool \| null | `true` → shows a "truncated" caveat | `cohere`: from EOS tokens. `faster-whisper`/`openai`/dead capture: **always `null`** (cannot tell) |
| `unreliable_reasons` | string[] | not read (logs/debugging) | `no_speech_prob`, `avg_logprob`, `constant_signal`, `vad_no_speech` |
| `finish_reason` | `"stop"`\|`"length"`\|`"unknown"` | not read | `truncated` is derived from it |
| `avg_logprob` | number \| null | not read (the verdict is server-side so it can be retuned without an app release) | mean token logprob; `null` when unavailable |
| `no_speech_prob` | number \| null | not read | backend-scaled; Whisper 0..1, Cohere ~1e-10..1e-7 — hence per-backend threshold defaults |
| `compression_ratio` | number | not read | computed by the server from `text` |
| `duration_s`, `elapsed_s` | number | not read | measured by the server |

Errors are FastAPI's `{"detail": "…"}`. The app fails fast on `400` (bad or
undecodable audio, unsupported language), `401`, `413`, `422` and `500`, and
retries `502`/`503`/`504` — except a `503` whose `detail` contains
`STT_API_TOKEN`, which it treats as permanent. Backends therefore map their own
failures to `400` (the request can never work) or `503` (try again).

A **dead capture** — every decoded sample identical, which is what a phone
hands a recorder that lost the microphone — is answered `200` with `text: ""`,
`unreliable: true`, `unreliable_reasons: ["constant_signal"]`, without calling
the model (models fabricate prose from silence). It is not an energy threshold
and cannot reject real speech, however quiet.

## Snippet catalog format

A JSON array of objects with four fields; `examples/snippets.json` is a
synthetic example:

```json
[{"label": "Review this diff", "body": "Review the diff below…",
  "search_terms": ["review", "pr"], "category": "coding"}]
```

## Tests

```sh
pip install -r requirements-dev.txt   # plus ffmpeg from your OS
pytest -q                             # uses the fake backend: no model, no network
```
