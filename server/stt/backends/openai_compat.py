"""Passthrough backend: forward to any OpenAI-compatible transcription API.

Use it to put promptshipper's auth, snippet catalog and response contract in
front of a transcription server you already run (speaches, a llama.cpp/whisper
server, a hosted API). The upload is decoded and guarded HERE first, then sent
upstream as 16 kHz mono 16-bit WAV, so the upstream never sees a container it
might not understand.

Environment:

  STT_UPSTREAM_URL     base URL, e.g. `http://127.0.0.1:9000` (REQUIRED). The
                       request goes to `<base>/v1/audio/transcriptions`.
  STT_UPSTREAM_TOKEN   bearer token for the upstream, optional
  STT_UPSTREAM_MODEL   the `model` form field, default `whisper-1`
  STT_UPSTREAM_FORMAT  `verbose_json` (default; carries per-segment signals)
                       or `json` (text only -> every signal null)
  STT_UPSTREAM_TIMEOUT seconds, default 120

Signals: from `verbose_json` segments when present (same reduction as the
faster-whisper backend: token-weighted avg_logprob, MAX no_speech_prob), else
null. `finish_reason` is always "unknown".

Errors: an upstream 400/413/415/422 is the caller's audio -> 400. Anything else
(401/403 = OUR upstream credentials are wrong, 5xx, timeouts, refused
connections) -> 503, which clients retry.
"""

from __future__ import annotations

import io
import json
import wave
from types import SimpleNamespace

import numpy as np

from .base import Backend, BackendRequestError, BackendUnavailable, Transcript, env
from .faster_whisper import summarize

CALLER_FAULT = frozenset({400, 413, 415, 422})


def to_wav(audio: np.ndarray, rate: int = 16000) -> bytes:
    pcm = (np.clip(audio, -1.0, 1.0) * 32767.0).astype("<i2").tobytes()
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(pcm)
    return buf.getvalue()


def parse_upstream(body: bytes) -> Transcript:
    """Turn an upstream 2xx body into a Transcript, or raise BackendUnavailable."""
    try:
        doc = json.loads(body)
    except ValueError as e:
        raise BackendUnavailable("the upstream answered 2xx with a body that is not JSON") from e
    if not isinstance(doc, dict) or not isinstance(doc.get("text"), str):
        raise BackendUnavailable('the upstream answered 2xx with no "text" string')
    segs = doc.get("segments")
    usable = []
    if isinstance(segs, list):
        for s in segs:
            if isinstance(s, dict) and isinstance(s.get("avg_logprob"), (int, float)) and isinstance(
                s.get("no_speech_prob"), (int, float)
            ):
                usable.append(SimpleNamespace(
                    text=str(s.get("text", "")), avg_logprob=s["avg_logprob"],
                    no_speech_prob=s["no_speech_prob"], tokens=s.get("tokens") or (),
                ))
    if usable and len(usable) == len(segs):
        t = summarize(usable)
        # The upstream's own top-level text is the transcript; segments only
        # supply the signals.
        return Transcript(text=doc["text"].strip(), finish_reason="unknown",
                          avg_logprob=t.avg_logprob, no_speech_prob=t.no_speech_prob)
    return Transcript(text=doc["text"].strip(), finish_reason="unknown")


class OpenAICompatBackend(Backend):
    name = "openai"
    # Whisper-family upstreams report Whisper-scale no_speech_prob; see the
    # faster-whisper backend for why 0.6 is borrowed rather than measured.
    default_no_speech_max = "0.6"
    default_avg_logprob_min = ""

    def __init__(self, transport=None) -> None:
        super().__init__()
        self.base_url = env("STT_UPSTREAM_URL", "").strip().rstrip("/")
        self.token = env("STT_UPSTREAM_TOKEN", "")
        self.model = env("STT_UPSTREAM_MODEL", "whisper-1")
        self.format = env("STT_UPSTREAM_FORMAT", "verbose_json")
        self.timeout = float(env("STT_UPSTREAM_TIMEOUT", "120"))
        self.device = "upstream"
        self._transport = transport
        self._client = None

    def load(self) -> None:
        import httpx

        if not self.base_url.startswith(("http://", "https://")):
            raise SystemExit("STT_BACKEND=openai needs STT_UPSTREAM_URL=http(s)://host:port")
        if self.format not in {"verbose_json", "json"}:
            raise SystemExit("STT_UPSTREAM_FORMAT must be verbose_json or json")
        self._client = httpx.Client(timeout=self.timeout, transport=self._transport)
        self.ready = True

    def transcribe(self, audio: np.ndarray, language: str, punctuation: bool) -> Transcript:
        import httpx

        headers = {"Accept": "application/json"}
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"
        try:
            r = self._client.post(
                f"{self.base_url}/v1/audio/transcriptions",
                headers=headers,
                files={"file": ("audio.wav", to_wav(audio), "audio/wav")},
                data={"model": self.model, "language": language, "response_format": self.format},
            )
        except httpx.HTTPError as e:
            raise BackendUnavailable(f"the upstream could not be reached ({e.__class__.__name__})") from e
        if 200 <= r.status_code < 300:
            return parse_upstream(r.content)
        if r.status_code in CALLER_FAULT:
            raise BackendRequestError(f"the upstream refused the audio (HTTP {r.status_code})")
        raise BackendUnavailable(f"the upstream answered HTTP {r.status_code}")
