# stt — transcription server + snippet catalog

An OpenAI-compatible `POST /v1/audio/transcriptions` endpoint with a pluggable
model backend, plus `GET /v1/snippets`, the canned-prompt catalog the clients
pick from. FastAPI, one process. Deployed by `deploy/compose/` (see the repo's
`AGENTS.md`).

## Routes

| | auth | |
|---|---|---|
| `GET /healthz` | none | `200 {"ok":true,"backend":…,"device":…}` once the model is loaded; `503` before (first start downloads the model) |
| `GET /` | none | service, backend and model name |
| `GET /v1/snippets` | bearer | the mounted catalog's bytes, `ETag` + `If-None-Match` → `304`; a named `503` when it is missing, malformed or empty — never an empty `200` |
| `POST /v1/audio/transcriptions` | bearer | multipart: `file` (any container ffmpeg reads — ogg/opus, aac/m4a, webm, wav, mp3), `language` (bare code, default `en`), `punctuation` (bool, default true) |

No TLS: run it on a private network (see `AGENTS.md`, *Networking*).

## Backends

`STT_BACKEND` picks one; an unknown name refuses to boot.

| `STT_BACKEND` | image | runs on | notes |
|---|---|---|---|
| `faster-whisper` (default) | `Dockerfile` | CPU (or CUDA with `STT_DEVICE=cuda`) | `STT_MODEL` default `small` (~0.5 GB, downloaded at first start); `base`/`tiny` for weak machines |
| `cohere` | `Dockerfile.cohere` | NVIDIA GPU | **opt-in; license not yet reviewed — see below.** Gated model: needs `HF_TOKEN` |
| `openai` | `Dockerfile` | anywhere | passthrough to any OpenAI-compatible upstream: `STT_UPSTREAM_URL` (+ `STT_UPSTREAM_TOKEN`, `STT_UPSTREAM_MODEL`, `STT_UPSTREAM_FORMAT`) |
| `fake` | `Dockerfile` | anywhere | no model; answers `STT_FAKE_TEXT`. For tests and smoke-testing a deploy only |

Each backend's module docstring (`backends/*.py`) lists its own variables.

> 🔴 **Cohere Transcribe licensing is an open question.** The `cohere` backend
> loads `CohereLabs/cohere-transcribe-03-2026` from the Hugging Face Hub, where
> it is gated behind accepting its terms. This repository ships only the loader
> code, never weights. Whether we may advertise or recommend it has **not been
> decided**; until it is, it is not mentioned in user-facing docs beyond this note.

## Common configuration

| | default | |
|---|---|---|
| `STT_API_TOKEN` | — | the bearer token (`./setup` generates it). With no token configured every authenticated route answers `503 server has no STT_API_TOKEN configured`, and the app treats that 503 as permanent |
| `STT_API_TOKENS` | — | extra named tokens, `name:token,name2:token2`, each revocable on its own. Validate with `python3 check_tokens.py` before a restart: a malformed list refuses boot |
| `STT_NO_SPEECH_MAX` | per backend | `unreliable` fires when `no_speech_prob` is above it; `off` disables the rule |
| `STT_AVG_LOGPROB_MIN` | off | `unreliable` fires when `avg_logprob` is below it |
| `STT_MAX_AUDIO_SECONDS` | `3600` | longer uploads are `413` |
| `HF_HOME` | `/models/hf` | model cache; compose keeps it on the `stt-models` volume |

The snippet catalog path is fixed at **`/etc/snip/snippets.json`** (deliberately
not configurable). Mount a *directory* there so edits are picked up without a
restart.

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
