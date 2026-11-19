"""promptshipper STT server — an OpenAI-compatible transcription endpoint plus the
snippet catalog, with a pluggable model backend.

Routes:
  GET  /healthz                    unauthenticated readiness (200 once the model is loaded)
  GET  /                           service/backend/model identity
  GET  /v1/snippets                the snippet catalog (bearer)
  POST /v1/audio/transcriptions    multipart `file` [+ `language`, `punctuation`] (bearer)

Everything that decides what the CLIENT sees lives in this file, once, for every
backend: auth, decoding, the dead-capture guard, the response shape and the
reliability verdict. A backend (see backends/) only turns PCM into a Transcript.
The response contract is documented in README.md; tests/test_contract.py pins it.
"""

import hashlib
import hmac
import json
import logging
import os
import re
import subprocess
import tempfile
import time
import zlib
from contextlib import asynccontextmanager
from email.utils import formatdate

import numpy as np
from fastapi import Depends, FastAPI, File, Form, HTTPException, Request, Response, UploadFile
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer

import backends
from backends import BackendRequestError, BackendUnavailable, Transcript
from backends.base import env

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("stt")

MAX_AUDIO_SECONDS = float(env("STT_MAX_AUDIO_SECONDS", "3600"))
# The largest upload body accepted, checked BEFORE the body is read (see
# UploadGate). Default: MAX_AUDIO_SECONDS of 16 kHz mono 16-bit PCM — the
# largest the format this server decodes to needs — plus 1 MiB for the
# multipart envelope. Compressed uploads are ~10x smaller than that.
MAX_UPLOAD_BYTES = int(env("STT_MAX_UPLOAD_BYTES", str(int(MAX_AUDIO_SECONDS * 32000) + (1 << 20))))
FFMPEG_TIMEOUT = 120.0
SAMPLE_RATE = 16000

# 🔴 AN UNKNOWN STT_BACKEND REFUSES BOOT, at import time, naming the valid set.
BACKEND = backends.create(env("STT_BACKEND", backends.DEFAULT_BACKEND))


def _optional_float(name: str, default: str):
    """A threshold, or None meaning "this rule is not configured".

    🔴 None IS A FIRST-CLASS VALUE. An unconfigured rule makes `reliability`
    answer null — "nothing was checked" — rather than false, which would claim
    "checked, and fine". `off`, `none` or `null` turns a rule off; UNSET or BLANK
    means "use the backend's default" (compose passes an omitted variable
    through as an empty string, which must not silently retire a rule).
    """
    raw = (os.environ.get(name, "").strip() or default).strip().lower()
    if raw in {"", "off", "none", "null"}:
        return None
    try:
        return float(raw)
    except ValueError:
        log.warning("%s=%r is not a number — the rule is OFF", name, raw)
        return None


# Defaults come from the BACKEND, because `no_speech_prob` has a different scale
# on every model family (Whisper: 0..1, Cohere: ~1e-10..1e-7). The env vars
# override. ⚠ Every default is a starting point, not a measurement of YOUR voice.
NO_SPEECH_MAX = _optional_float("STT_NO_SPEECH_MAX", BACKEND.default_no_speech_max)
AVG_LOGPROB_MIN = _optional_float("STT_AVG_LOGPROB_MIN", BACKEND.default_avg_logprob_min)


# ---------------------------------------------------------------------------
# the response contract
# ---------------------------------------------------------------------------


def compression_ratio(text: str) -> float:
    """utf-8 bytes / deflated bytes — Whisper's repetition heuristic.

    ⚠ Measured useless (and inverted) at dictation lengths: below ~100 bytes the
    deflate header dominates, so the ratio tracks LENGTH. Reported because it is
    free; do not threshold on it for short clips.
    """
    data = text.encode("utf-8")
    if not data:
        return 0.0
    return len(data) / len(zlib.compress(data, 9))


def reliability(no_speech_prob, avg_logprob, no_speech_max, avg_logprob_min):
    """(unreliable, reasons): True = a configured rule fired; False = every
    configured rule was EVALUATED and none fired; None = nothing could be
    checked (no rule configured, or the signal each rule needs is null).

    🔴 The threshold lives on the SERVER so it can be retuned without shipping
    an app; the client reads `unreliable` and never re-derives it.
    ⚠ It LABELS; it never refuses a request.
    """
    reasons = []
    checked = False
    if no_speech_max is not None and no_speech_prob is not None:
        checked = True
        if float(no_speech_prob) > float(no_speech_max):
            reasons.append("no_speech_prob")
    if avg_logprob_min is not None and avg_logprob is not None:
        checked = True
        if float(avg_logprob) < float(avg_logprob_min):
            reasons.append("avg_logprob")
    if not checked:
        return None, []
    return bool(reasons), reasons


# The diagnostics keys, in order. Every 200 from /v1/audio/transcriptions
# carries exactly these plus `text`, `duration_s`, `elapsed_s` — whatever the
# backend, whatever path produced it. Pinned by tests/test_contract.py.
DIAGNOSTIC_KEYS = (
    "unreliable",
    "unreliable_reasons",
    "finish_reason",
    "truncated",
    "avg_logprob",
    "compression_ratio",
    "no_speech_prob",
)


def diagnostics_of(t: Transcript) -> dict:
    """The fields that let a client distrust a transcript.

    🔴 The verdict is computed from the ROUNDED-FOR-DISPLAY values, so a client
    re-deriving it from the JSON gets the same answer the server did.
    🔴 `no_speech_prob` is rounded to significant figures (`%.6g`), never to
    decimal places: a probability whose whole purpose is to be small would round
    to a false 0.0.
    """
    shown_nsp = None if t.no_speech_prob is None else float(f"{float(t.no_speech_prob):.6g}")
    shown_avg = None if t.avg_logprob is None else round(float(t.avg_logprob), 4)
    unreliable, reasons = reliability(shown_nsp, shown_avg, NO_SPEECH_MAX, AVG_LOGPROB_MIN)
    if t.flags:
        # Structural verdicts (no threshold involved) force the label on.
        reasons = list(t.flags) + [r for r in reasons if r not in t.flags]
        unreliable = True
    return {
        "unreliable": unreliable,
        "unreliable_reasons": reasons,
        "finish_reason": t.finish_reason,
        # null, not false, when the backend cannot tell.
        "truncated": None if t.finish_reason == "unknown" else t.finish_reason == "length",
        "avg_logprob": shown_avg,
        "compression_ratio": round(compression_ratio(t.text), 3),
        "no_speech_prob": shown_nsp,
    }


CONSTANT_SIGNAL_REASON = "constant_signal"


def is_constant_signal(audio) -> bool:
    """True when every decoded sample holds the SAME value — a dead capture.

    🔴 NOT an RMS/energy silence gate. Measured: room tone that made a model
    hallucinate was LOUDER than quiet speech that transcribed correctly, so no
    amplitude cutoff can separate them. This reads CARDINALITY: a phone handing
    a recorder bit-exact silence produces exactly 1 distinct sample value, while
    any real microphone carries dither. So it cannot reject real speech.

    `min() == max()`, not `np.unique`: two O(n) scans instead of a sort+copy.
    An empty array is False (no samples is not evidence of a constant signal).
    """
    if audio.size == 0:
        return False
    return bool(audio.min() == audio.max())


# ---------------------------------------------------------------------------
# decoding
# ---------------------------------------------------------------------------


class UndecodableAudio(Exception):
    """ffmpeg could not read the upload as audio (the caller's fault -> 400)."""


class DecoderCrashed(Exception):
    """The decoder was KILLED (negative returncode, e.g. the OOM killer) rather
    than rejecting the input — a server fault, answered 503 so clients retry."""


def _scrub(stderr: bytes, path: str) -> str:
    """ffmpeg's message with the temp path removed and newlines collapsed (so a
    crafted upload cannot forge extra log records)."""
    flat = " ".join(stderr.decode("utf-8", "replace").replace(path, "<upload>").split())
    return flat[:500]


def decode_audio(path: str) -> np.ndarray:
    """Decode any container ffmpeg understands to 16 kHz mono float32.

    Dispatches on the BYTES, not the filename — browsers record webm/mp4,
    Android records ogg/opus or aac.

    🔴 `-t MAX+1` is an OUTPUT option, so the decoded PCM is hard-capped whatever
    arrives: one request cannot exhaust memory. The +1 lets the duration check
    still SEE an over-long upload and answer 413 instead of silently truncating.
    ⚠ Not bounded: concurrency, and the upload's own spool to /tmp. Put a body
    limit on any proxy in front of this.
    """
    cmd = [
        "ffmpeg", "-nostdin", "-loglevel", "error",
        "-i", path,
        "-t", str(MAX_AUDIO_SECONDS + 1),
        "-f", "f32le", "-acodec", "pcm_f32le",
        "-ac", "1", "-ar", str(SAMPLE_RATE),
        "-",
    ]
    try:
        proc = subprocess.run(cmd, capture_output=True, timeout=FFMPEG_TIMEOUT, stdin=subprocess.DEVNULL)
    except subprocess.TimeoutExpired:
        raise UndecodableAudio(f"ffmpeg did not finish within {FFMPEG_TIMEOUT:.0f}s")
    except OSError as e:
        # No ffmpeg at all is OUR fault, not the upload's: a 503, never the 400
        # that would tell the caller their audio can never work.
        raise DecoderCrashed(f"ffmpeg could not be started ({e.__class__.__name__})") from e
    if proc.returncode < 0:
        said = _scrub(proc.stderr, path)
        raise DecoderCrashed(
            f"the decoder was killed by signal {-proc.returncode}"
            + (f"; ffmpeg said: {said}" if said else "")
        )
    if proc.returncode != 0:
        raise UndecodableAudio(_scrub(proc.stderr, path))
    # A valid-but-empty container (e.g. a WAV with a zero-length data chunk)
    # exits 0 with no output.
    if not proc.stdout:
        raise UndecodableAudio("no decodable audio stream in the upload")
    return np.frombuffer(proc.stdout, dtype=np.float32)


# ---------------------------------------------------------------------------
# the snippet catalog
# ---------------------------------------------------------------------------
#
# 🔴 A PLAIN CONSTANT, deliberately not an env knob or a parameter: no route,
# query string or header can redirect the read. Mount a DIRECTORY at /etc/snip
# (never a single-file bind or a k8s subPath) so edits are picked up live.
SNIP_CATALOG_PATH = "/etc/snip/snippets.json"

# Named reasons: an unmounted catalog answering `[]` would be indistinguishable
# from "you have no snippets" and would wipe a client's working copy.
SNIP_CATALOG_MISSING = "no snip catalog is mounted"
SNIP_CATALOG_UNREADABLE = "the snip catalog is mounted but could not be read"
SNIP_CATALOG_MALFORMED = "the snip catalog is not a JSON array of snippet objects"
SNIP_CATALOG_EMPTY = "the snip catalog is mounted but holds zero snippets"


class SnipCatalogUnavailable(Exception):
    """The catalog cannot be served; the message IS the reason."""


_snip_cache = {"key": None, "body": None, "etag": None, "last_modified": None, "rows": 0}


def _snip_read_file(path: str) -> bytes:
    """The only blocking read in the snippets path (a seam tests count)."""
    with open(path, "rb") as fh:
        return fh.read()


def _snip_rows(raw: bytes) -> list:
    """Validate; the response body is still `raw`, byte for byte."""
    try:
        rows = json.loads(raw)
    except (ValueError, UnicodeDecodeError) as e:
        raise SnipCatalogUnavailable(f"{SNIP_CATALOG_MALFORMED} ({e.__class__.__name__})") from e
    if not isinstance(rows, list) or not all(isinstance(r, dict) for r in rows):
        raise SnipCatalogUnavailable(SNIP_CATALOG_MALFORMED)
    if not rows:
        raise SnipCatalogUnavailable(SNIP_CATALOG_EMPTY)
    return rows


def snip_catalog():
    """`(body, etag, last_modified)`, or raise SnipCatalogUnavailable.

    One `os.stat` per request; a re-read only when `(mtime_ns, size)` moves. A
    failed validation leaves the cache key alone, so a broken file is never
    served as current and the next request retries. The ETag is content-
    addressed, so a touch that changes nothing does not force a re-download.
    No lock: there is no `await` here, so it runs to completion on the loop.
    """
    try:
        st = os.stat(SNIP_CATALOG_PATH)
    except OSError as e:
        raise SnipCatalogUnavailable(
            f"{SNIP_CATALOG_MISSING} at {SNIP_CATALOG_PATH} ({e.__class__.__name__})"
        ) from e
    key = (st.st_mtime_ns, st.st_size)
    if _snip_cache["key"] != key:
        try:
            raw = _snip_read_file(SNIP_CATALOG_PATH)
        except OSError as e:
            raise SnipCatalogUnavailable(
                f"{SNIP_CATALOG_UNREADABLE} at {SNIP_CATALOG_PATH} ({e.__class__.__name__})"
            ) from e
        rows = _snip_rows(raw)
        _snip_cache.update(
            key=key,
            body=raw,
            etag='"' + hashlib.sha256(raw).hexdigest()[:32] + '"',
            last_modified=formatdate(st.st_mtime, usegmt=True),
            rows=len(rows),
        )
    return _snip_cache["body"], _snip_cache["etag"], _snip_cache["last_modified"]


# ---------------------------------------------------------------------------
# bearer tokens: one legacy token plus any number of NAMED, revocable ones
# ---------------------------------------------------------------------------
#
# STT_API_TOKEN   one token (what ./setup generates). Logged as caller "default".
# STT_API_TOKENS  comma-separated, accepted IN ADDITION:
#                   name:token          internal scope
#                   name:internal:token internal scope, spelled out
#                   name:public:token   also accepted through a "public entry"
#                 Revoking one caller = deleting its entry and restarting.
#
# THE PUBLIC ENTRY (advanced, off unless you build it): a reverse proxy that
# exposes this service more widely can SET the header `X-STT-Entry` on every
# request it forwards. A request carrying that header (any value, any number of
# times) is accepted only with a PUBLIC-scoped token. The header can only ever
# NARROW what a token may do, which is why the app may trust it at all.
#
# 🔴 A MALFORMED STT_API_TOKENS REFUSES BOOT; it is never partly applied.
# ⚠ ZERO TOKENS IS NOT A BOOT FAILURE: /healthz stays up and every
# authenticated route answers 503 naming STT_API_TOKEN — closed, not open.
LEGACY_TOKEN_NAME = "default"
TOKEN_NAME_RE = re.compile(r"[a-z0-9-]{1,32}")
SCOPE_INTERNAL = "internal"
SCOPE_PUBLIC = "public"
TOKEN_SCOPES = frozenset({SCOPE_INTERNAL, SCOPE_PUBLIC})
ENTRY_HEADER = "x-stt-entry"


class TokenConfigError(ValueError):
    """The token configuration cannot be used; the message names the entry by
    POSITION and never contains a token."""


def parse_tokens(legacy: str, named: str) -> tuple:
    """`((name, scope, sha256(token)), ...)`, or raise TokenConfigError.

    Only DIGESTS are kept, so comparisons are equal-length and no plaintext
    token lives in the table. With one colon an entry is `name:token`; with two
    or more the second segment must be a scope (`name:internal:a:b` -> `a:b`).
    """
    entries = []
    if legacy:
        entries.append((LEGACY_TOKEN_NAME, SCOPE_INTERNAL, legacy))
    if named.strip():
        for i, raw in enumerate(named.split(","), start=1):
            entry = raw.strip()
            if not entry:
                raise TokenConfigError(f"STT_API_TOKENS entry {i} is empty (stray comma?)")
            name, sep, rest = entry.partition(":")
            if not sep:
                raise TokenConfigError(f"STT_API_TOKENS entry {i} is not `name[:scope]:token`")
            if not TOKEN_NAME_RE.fullmatch(name):
                raise TokenConfigError(
                    f"STT_API_TOKENS entry {i} has an invalid name (want [a-z0-9-]{{1,32}})"
                )
            scope, sep, token = rest.partition(":")
            if not sep:
                scope, token = SCOPE_INTERNAL, rest
            elif scope not in TOKEN_SCOPES:
                raise TokenConfigError(
                    f"STT_API_TOKENS entry {i} ({name}) has an unknown scope "
                    f"(want one of {', '.join(sorted(TOKEN_SCOPES))})"
                )
            if not token:
                raise TokenConfigError(f"STT_API_TOKENS entry {i} ({name}) has an empty token")
            if any(c.isspace() for c in token):
                raise TokenConfigError(f"STT_API_TOKENS entry {i} ({name}) has whitespace in its token")
            entries.append((name, scope, token))
    names = [n for n, _, _ in entries]
    dup_names = sorted({n for n in names if names.count(n) > 1})
    if dup_names:
        raise TokenConfigError(f"duplicate token name(s): {', '.join(dup_names)}")
    by_token = {}
    for name, _, token in entries:
        by_token.setdefault(token, []).append(name)
    shared = sorted("+".join(ns) for ns in by_token.values() if len(ns) > 1)
    if shared:
        raise TokenConfigError(f"the same token is configured under several names: {', '.join(shared)}")
    return tuple((name, scope, hashlib.sha256(token.encode()).digest()) for name, scope, token in entries)


def match_token(presented: str, tokens: tuple):
    """`(name, scope)` of the matching entry, else None. No early exit: every
    digest is compared on every call."""
    digest = hashlib.sha256(presented.encode("utf-8", "surrogatepass")).digest()
    matched = None
    for name, scope, want in tokens:
        if hmac.compare_digest(digest, want):
            matched = (name, scope)
    return matched


TOKENS = parse_tokens(os.environ.get("STT_API_TOKEN", ""), os.environ.get("STT_API_TOKENS", ""))

bearer = HTTPBearer(auto_error=False)

# 🔴 THE CLIENT TREATS A 503 WHOSE `detail` CONTAINS "STT_API_TOKEN" AS
# PERMANENT (it stops retrying). Only the no-token-configured case may say it.
NO_TOKEN_DETAIL = "server has no STT_API_TOKEN configured"


def bearer_credential(authorization: str | None) -> str | None:
    """The credential of an `Authorization: Bearer <x>` header, else None —
    the same reading FastAPI's HTTPBearer makes, so the upload gate and the
    route dependency cannot disagree about who sent a token."""
    if not authorization:
        return None
    scheme, _, credential = authorization.partition(" ")
    if scheme.lower() != "bearer" or not credential:
        return None
    return credential


def authenticate(credential: str | None, public_entry: bool, method: str, path: str) -> str:
    """THE one auth rule. Return the caller's NAME, or raise HTTPException.

    Called by `require_token` (every authenticated route) AND by `UploadGate`
    (before an upload body is read), so the 401/403/503 semantics are one code
    path. The name is logged, the token never.
    """
    if not TOKENS:
        raise HTTPException(503, NO_TOKEN_DETAIL)
    hit = None if credential is None else match_token(credential, TOKENS)
    if hit is None:
        log.warning("auth refused (%s bearer) %s %s",
                    "missing" if credential is None else "invalid", method, path)
        raise HTTPException(401, "invalid or missing bearer token")
    name, scope = hit
    if public_entry and scope != SCOPE_PUBLIC:
        log.warning("auth refused (scope) caller=%s via public entry %s %s", name, method, path)
        raise HTTPException(403, "this token is not permitted through the public entry")
    log.info("auth caller=%s%s %s %s", name, " via public entry" if public_entry else "", method, path)
    return name


def require_token(request: Request, creds: HTTPAuthorizationCredentials = Depends(bearer)):
    # getlist, never get: `get` returns only the FIRST occurrence, so an empty
    # header ahead of the proxy's would read as internal.
    public = len(request.headers.getlist(ENTRY_HEADER)) > 0
    return authenticate(None if creds is None else creds.credentials, public,
                        request.method, request.url.path)


# ---------------------------------------------------------------------------
# the upload gate
# ---------------------------------------------------------------------------

UPLOAD_PATH = "/v1/audio/transcriptions"


class UploadGate:
    """ASGI middleware: authenticate and size-cap an upload BEFORE its body is read.

    🔴 WHY A MIDDLEWARE AND NOT THE DEPENDENCY. FastAPI parses a multipart body —
    spooling it to /tmp on the container's writable layer — before any `Depends`
    runs, so `require_token` alone let an UNAUTHENTICATED caller write an upload
    of any size to disk. This runs first and reads only the headers.

    * Auth: `authenticate()`, the same function the dependency calls, so the
      status codes and messages are identical. The dependency still runs after
      it, unchanged.
    * Declared size: a Content-Length above MAX_UPLOAD_BYTES is 413 at once.
    * Streamed size: for a chunked body, `receive` is counted and the request is
      cut at MAX_UPLOAD_BYTES — this answers 413 and tells the app the client
      disconnected; anything the app tries to send afterwards is dropped.
    """

    def __init__(self, inner):
        self.inner = inner

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http" or scope["method"] != "POST" or scope["path"] != UPLOAD_PATH:
            return await self.inner(scope, receive, send)
        headers = [(k.decode("latin-1").lower(), v.decode("latin-1")) for k, v in scope["headers"]]
        auth = next((v for k, v in headers if k == "authorization"), None)
        public = any(k == ENTRY_HEADER for k, _ in headers)
        try:
            authenticate(bearer_credential(auth), public, "POST", UPLOAD_PATH)
        except HTTPException as e:
            return await _reply(send, e.status_code, e.detail)
        declared = next((v for k, v in headers if k == "content-length"), None)
        if declared is not None and declared.strip().isdigit() and int(declared) > MAX_UPLOAD_BYTES:
            return await _reply(send, 413, _too_large())

        seen = {"bytes": 0, "cut": False}

        async def counted_receive():
            msg = await receive()
            if msg["type"] == "http.request":
                seen["bytes"] += len(msg.get("body", b""))
                if seen["bytes"] > MAX_UPLOAD_BYTES and not seen["cut"]:
                    seen["cut"] = True
                    await _reply(send, 413, _too_large())
                    return {"type": "http.disconnect"}
            return msg

        async def guarded_send(msg):
            if not seen["cut"]:
                await send(msg)

        try:
            await self.inner(scope, counted_receive, guarded_send)
        except Exception:
            if not seen["cut"]:
                raise


def _too_large() -> str:
    return (f"upload too large: more than {MAX_UPLOAD_BYTES} bytes "
            f"(STT_MAX_UPLOAD_BYTES; audio is capped at {MAX_AUDIO_SECONDS:.0f}s anyway)")


async def _reply(send, status: int, detail: str):
    body = json.dumps({"detail": detail}).encode()
    await send({"type": "http.response.start", "status": status,
                "headers": [(b"content-type", b"application/json"),
                            (b"content-length", str(len(body)).encode()),
                            (b"connection", b"close")]})
    await send({"type": "http.response.body", "body": body})


# ---------------------------------------------------------------------------
# the app
# ---------------------------------------------------------------------------


@asynccontextmanager
async def lifespan(app: FastAPI):
    BACKEND.load()
    yield


# No /docs, /redoc or /openapi.json: a machine API does not need a public map.
app = FastAPI(title="promptshipper-stt", lifespan=lifespan, docs_url=None, redoc_url=None, openapi_url=None)
app.add_middleware(UploadGate)


@app.get("/healthz")
async def healthz():
    """🔴 `async def` ON PURPOSE: a sync handler would queue in the same
    threadpool `transcribe` holds for a whole decode, so a retry storm could
    starve the probe and get a working server restarted. Reads state only.
    The ONE unauthenticated route."""
    if not BACKEND.ready:
        raise HTTPException(503, "model not loaded yet")
    return {"ok": True, "backend": BACKEND.name, "device": BACKEND.device}


@app.get("/")
async def root(_=Depends(require_token)):
    return {
        "service": "promptshipper-stt",
        "backend": BACKEND.name,
        "model": BACKEND.model,
        "usage": "POST /v1/audio/transcriptions (multipart: file)",
    }


@app.get("/v1/snippets")
async def snippets(request: Request, _=Depends(require_token)):
    """The snippet catalog, served as the mounted file's own bytes.

    `async def` for the same reason as healthz; `snip_catalog()` is what makes
    that honest (one stat per request). 503, not 404, when nothing is mounted:
    the route exists, a server-side file is missing, and clients retry a 503.
    """
    try:
        body, etag, last_modified = snip_catalog()
    except SnipCatalogUnavailable as e:
        raise HTTPException(503, str(e)) from e
    headers = {"ETag": etag, "Last-Modified": last_modified, "Cache-Control": "no-cache"}
    if request.headers.get("if-none-match") == etag:
        return Response(status_code=304, headers=headers)
    return Response(content=body, media_type="application/json", headers=headers)


def language_error(language: str) -> str | None:
    """None if the backend accepts `language`, else the 400 body. No silent
    coercion (`en-US` -> `en` would also turn `zh-TW` into `zh`)."""
    langs = BACKEND.languages
    if langs is None or language in langs:
        return None
    shown = (
        "the bare codes " + ", ".join(sorted(langs)) if len(langs) <= 20
        else f"{len(langs)} bare codes such as en, de, fr"
    )
    return (
        f"unsupported language {language!r}: the {BACKEND.name} backend accepts {shown}. "
        f"A BCP-47 tag such as 'en-US' is not one of them — send 'en'."
    )


@app.post("/v1/audio/transcriptions")
def transcribe(
    file: UploadFile = File(...),
    language: str = Form(default="en"),
    punctuation: bool = Form(default=True),
    _=Depends(require_token),
):
    if not BACKEND.ready:
        raise HTTPException(503, "model not loaded yet")
    # Before the handler's own copy of the upload: a request that can only be
    # refused should not also pay for the second spool.
    bad_language = language_error(language)
    if bad_language:
        raise HTTPException(400, bad_language)
    suffix = os.path.splitext(file.filename or "audio.wav")[1] or ".wav"
    tmp = tempfile.NamedTemporaryFile(suffix=suffix, delete=False)
    try:
        for chunk in iter(lambda: file.file.read(1 << 20), b""):
            tmp.write(chunk)
        tmp.close()
        t0 = time.time()
        try:
            audio = decode_audio(tmp.name)
        except DecoderCrashed as e:
            log.error("decoder crashed: %s", e)
            raise HTTPException(503, f"the audio decoder failed: {e}")
        except UndecodableAudio as e:
            raise HTTPException(400, f"could not decode the uploaded audio: {e}")
        duration = len(audio) / float(SAMPLE_RATE)
        if duration > MAX_AUDIO_SECONDS:
            # "more than", not a figure: decode is capped at MAX+1, so the
            # measured duration is the bound, not the upload's real length.
            raise HTTPException(413, f"audio too long: more than {MAX_AUDIO_SECONDS:.0f}s")
        if is_constant_signal(audio):
            # 🔴 200 WITH AN EMPTY TRANSCRIPT, NOT A 4xx: the client renders an
            # empty transcript plus its caveat, while a 4xx would be retried on a
            # recording that is byte-identically silent. The refusal is final.
            t = Transcript(text="", finish_reason="unknown", flags=(CONSTANT_SIGNAL_REASON,))
            log.info("rejected constant-signal audio: %.1fs, every sample == %r (lang=%s)",
                     duration, float(audio[0]), language)
        else:
            try:
                t = BACKEND.transcribe(audio, language, punctuation)
            except BackendRequestError as e:
                raise HTTPException(400, str(e))
            except BackendUnavailable as e:
                raise HTTPException(503, str(e))
        diagnostics = diagnostics_of(t)
        elapsed = time.time() - t0
        # The reasons only, never the TEXT: a fabrication is exactly the output
        # most worth keeping out of a log.
        log.info(
            "transcribed %.1fs audio in %.2fs (backend=%s, lang=%s, finish=%s, avg_logprob=%s, "
            "no_speech=%s, unreliable=%s%s)",
            duration, elapsed, BACKEND.name, language, diagnostics["finish_reason"],
            diagnostics["avg_logprob"], diagnostics["no_speech_prob"], diagnostics["unreliable"],
            (" " + ",".join(diagnostics["unreliable_reasons"]) if diagnostics["unreliable_reasons"] else ""),
        )
        return {
            "text": t.text,
            "duration_s": round(duration, 2),
            "elapsed_s": round(elapsed, 2),
            **diagnostics,
        }
    finally:
        os.unlink(tmp.name)
