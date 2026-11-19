"""Test harness for the STT server. No model, no network: STT_BACKEND=fake.

ffmpeg IS required (app.decode_audio shells out to it); CI installs it.
"""

import importlib
import io
import os
import shutil
import sys
import wave

import numpy as np
import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
STT_DIR = os.path.dirname(HERE)
sys.path.insert(0, STT_DIR)

TOKEN = "test-token-0123456789abcdef0123456789"

# Set BEFORE app is first imported: it reads these at import time.
os.environ["STT_BACKEND"] = "fake"
os.environ["STT_API_TOKEN"] = TOKEN
os.environ.pop("STT_API_TOKENS", None)
os.environ.pop("STT_NO_SPEECH_MAX", None)
os.environ.pop("STT_AVG_LOGPROB_MIN", None)

if shutil.which("ffmpeg") is None:  # fail loudly, never skip: decode is the core path
    raise RuntimeError("ffmpeg is not on PATH; the STT tests need it (apt-get install ffmpeg)")


def wav_bytes(samples: np.ndarray, rate: int = 16000) -> bytes:
    pcm = (np.clip(samples, -1, 1) * 32767).astype("<i2").tobytes()
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(pcm)
    return buf.getvalue()


def tone(seconds: float = 1.0, rate: int = 16000) -> bytes:
    t = np.arange(int(seconds * rate)) / rate
    rng = np.random.default_rng(7)
    return wav_bytes(0.3 * np.sin(2 * np.pi * 220 * t) + 0.01 * rng.standard_normal(t.size), rate)


def silence(seconds: float = 1.0, rate: int = 16000) -> bytes:
    return wav_bytes(np.zeros(int(seconds * rate)), rate)


@pytest.fixture
def load_app(monkeypatch):
    """Re-import app.py under a given environment. Returns the module."""

    def _load(**env):
        for k, v in env.items():
            if v is None:
                monkeypatch.delenv(k, raising=False)
            else:
                monkeypatch.setenv(k, v)
        import app

        return importlib.reload(app)

    yield _load
    # Leave a clean default module for the next test.
    monkeypatch.undo()
    import app

    importlib.reload(app)


@pytest.fixture
def make_client(load_app, monkeypatch):
    """A TestClient over a freshly loaded app whose backend is `backend`
    (default: a FakeBackend). Returns (client, app_module, backend)."""
    from fastapi.testclient import TestClient

    from backends.fake import FakeBackend

    clients = []

    def _make(backend=None, **env):
        mod = load_app(**env)
        be = backend or FakeBackend()
        monkeypatch.setattr(mod, "BACKEND", be)
        c = TestClient(mod.app)
        c.__enter__()  # runs lifespan -> be.load()
        clients.append(c)
        return c, mod, be

    yield _make
    for c in clients:
        c.__exit__(None, None, None)


def auth(token: str = TOKEN) -> dict:
    return {"Authorization": f"Bearer {token}"}


def upload(client, data: bytes, headers=None, **form):
    return client.post(
        "/v1/audio/transcriptions",
        headers=auth() if headers is None else headers,
        files={"file": ("dictation.ogg", data, "audio/ogg")},
        data={"language": "en", "punctuation": "true", **form},
    )
