"""Backend registry and the pure parts of each backend — no model libraries."""

import io
import json
import wave
from types import SimpleNamespace

import httpx
import numpy as np
import pytest

import backends
from backends import BackendRequestError, BackendUnavailable
from backends import cohere, faster_whisper, openai_compat


def test_the_backend_ledger_is_exactly_these():
    assert set(backends.REGISTRY) == {"faster-whisper", "cohere", "openai", "fake"}
    assert backends.DEFAULT_BACKEND == "faster-whisper"


@pytest.mark.parametrize("name", ["faster-whisper", "cohere", "openai", "fake"])
def test_every_backend_instantiates_without_its_model_library(name):
    be = backends.create(name)
    assert be.name == name
    assert be.ready is False


@pytest.mark.parametrize("name,nsp", [("faster-whisper", "0.6"), ("openai", "0.6"), ("cohere", "1e-8"), ("fake", "0.5")])
def test_threshold_defaults_are_per_backend(name, nsp):
    assert backends.create(name).default_no_speech_max == nsp


# --- faster-whisper -------------------------------------------------------------


def seg(text, avg, nsp, n_tokens):
    return SimpleNamespace(text=text, avg_logprob=avg, no_speech_prob=nsp, tokens=list(range(n_tokens)))


def test_summarize_weights_logprob_by_tokens_and_takes_the_max_no_speech():
    t = faster_whisper.summarize([seg(" Hello", -0.2, 0.05, 3), seg(" world.", -1.0, 0.4, 1)])
    assert t.text == "Hello world."
    assert t.avg_logprob == pytest.approx((-0.2 * 3 + -1.0 * 1) / 4)
    assert t.no_speech_prob == 0.4
    assert t.finish_reason == "unknown"


def test_summarize_of_no_segments_reports_no_signal():
    t = faster_whisper.summarize([])
    assert (t.text, t.avg_logprob, t.no_speech_prob) == ("", None, None)


def test_an_empty_vad_result_is_flagged(monkeypatch):
    be = faster_whisper.FasterWhisperBackend()
    be._model = SimpleNamespace(transcribe=lambda *a, **k: (iter([]), None))
    t = be.transcribe(np.ones(16000, dtype=np.float32), "en", True)
    assert t.flags == ("vad_no_speech",)


def test_a_model_valueerror_is_a_request_error():
    def boom(*a, **k):
        raise ValueError("bad language")

    be = faster_whisper.FasterWhisperBackend()
    be._model = SimpleNamespace(transcribe=boom)
    with pytest.raises(BackendRequestError):
        be.transcribe(np.ones(10, dtype=np.float32), "xx", True)


# --- cohere helpers -------------------------------------------------------------


def test_cohere_finish_reason_reads_eos_presence_not_position():
    eos = cohere.eos_id_set([3])
    assert cohere.finish_reason_of([[5, 3, 0, 0], [5, 6, 3]], eos) == "stop"  # padded row
    assert cohere.finish_reason_of([[5, 6, 7]], eos) == "length"
    assert cohere.finish_reason_of([[5, 6, 7]], frozenset()) == "unknown"
    assert cohere.eos_id_set(None) == frozenset() and cohere.eos_id_set(2) == {2}


def test_cohere_all_zero_no_speech_is_no_signal_not_zero():
    assert cohere.no_speech_prob_of([0.0, 0.0]) is None
    assert cohere.no_speech_prob_of([0.0, 2e-9]) == 2e-9
    assert cohere.mean_logprob([float("-inf"), -1.0, -3.0]) == -2.0
    assert cohere.mean_logprob([float("-inf")]) is None


def test_cohere_finds_the_no_speech_token_in_added_vocab_only():
    assert cohere.find_no_speech_token({"<unk>": 0, "<|nospeech|>": 1, "<pad>": 2}) == 1
    assert cohere.find_no_speech_token({"<unk>": 0}) is None


# --- openai-compatible passthrough ----------------------------------------------


def make_openai(monkeypatch, handler, **env):
    monkeypatch.setenv("STT_UPSTREAM_URL", "http://upstream.test:9000/")
    for k, v in env.items():
        monkeypatch.setenv(k, v)
    be = openai_compat.OpenAICompatBackend(transport=httpx.MockTransport(handler))
    be.load()
    return be


def test_openai_sends_wav_and_reads_verbose_json_signals(monkeypatch):
    seen = {}

    def handler(request):
        seen["url"] = str(request.url)
        seen["auth"] = request.headers.get("authorization")
        seen["body"] = request.content
        return httpx.Response(200, json={
            "text": " Hello there. ",
            "segments": [
                {"text": " Hello", "avg_logprob": -0.5, "no_speech_prob": 0.1, "tokens": [1, 2]},
                {"text": " there.", "avg_logprob": -0.1, "no_speech_prob": 0.7, "tokens": [3, 4]},
            ],
        })

    be = make_openai(monkeypatch, handler, STT_UPSTREAM_TOKEN="up-secret")
    t = be.transcribe(np.full(1600, 0.1, dtype=np.float32), "en", True)
    assert seen["url"] == "http://upstream.test:9000/v1/audio/transcriptions"
    assert seen["auth"] == "Bearer up-secret"
    assert b"RIFF" in seen["body"] and b"verbose_json" in seen["body"]
    assert t.text == "Hello there."
    assert t.avg_logprob == pytest.approx(-0.3)
    assert t.no_speech_prob == 0.7


def test_openai_plain_json_means_no_signals(monkeypatch):
    be = make_openai(monkeypatch, lambda r: httpx.Response(200, json={"text": "hi"}), STT_UPSTREAM_FORMAT="json")
    t = be.transcribe(np.ones(10, dtype=np.float32), "en", True)
    assert (t.text, t.avg_logprob, t.no_speech_prob, t.finish_reason) == ("hi", None, None, "unknown")


@pytest.mark.parametrize("status,exc", [(400, BackendRequestError), (422, BackendRequestError),
                                         (401, BackendUnavailable), (500, BackendUnavailable)])
def test_openai_status_mapping(monkeypatch, status, exc):
    be = make_openai(monkeypatch, lambda r: httpx.Response(status, json={"detail": "x"}))
    with pytest.raises(exc):
        be.transcribe(np.ones(10, dtype=np.float32), "en", True)


def test_openai_unreachable_and_garbage_are_unavailable(monkeypatch):
    def refuse(request):
        raise httpx.ConnectError("refused")

    be = make_openai(monkeypatch, refuse)
    with pytest.raises(BackendUnavailable):
        be.transcribe(np.ones(10, dtype=np.float32), "en", True)
    be = make_openai(monkeypatch, lambda r: httpx.Response(200, content=b"<html>"))
    with pytest.raises(BackendUnavailable):
        be.transcribe(np.ones(10, dtype=np.float32), "en", True)


def test_openai_refuses_boot_without_an_upstream(monkeypatch):
    monkeypatch.delenv("STT_UPSTREAM_URL", raising=False)
    with pytest.raises(SystemExit):
        openai_compat.OpenAICompatBackend().load()


def test_to_wav_is_16k_mono_16bit():
    with wave.open(io.BytesIO(openai_compat.to_wav(np.zeros(160, dtype=np.float32)))) as w:
        assert (w.getframerate(), w.getnchannels(), w.getsampwidth(), w.getnframes()) == (16000, 1, 2, 160)
