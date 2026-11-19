"""The response contract the Android client depends on, for every backend path.

What the client reads (measured from its source, SttHttp.kt / SttRetryPolicy.kt):
  * 2xx: top-level `text` (a string — a 2xx without it is treated as failure),
    `truncated` and `unreliable` as three-valued booleans (null = no signal).
  * non-2xx: top-level `detail`; a 503 whose detail contains `STT_API_TOKEN`
    is treated as PERMANENT (no retry), every other 503 is retried.
  * status codes: 400/401/413/422/500 fail fast, 503/502/504 retry.
Everything else in the body is diagnostics for humans and logs.
"""

import pytest

from backends import BackendRequestError, BackendUnavailable, Transcript
from backends.fake import FakeBackend
from conftest import TOKEN as TOKEN_DEFAULT, auth, silence, tone, upload

# 🔴 A LITERAL, not derived from app.DIAGNOSTIC_KEYS: a key set asserted against
# itself is not a contract.
RESPONSE_KEYS = {
    "text", "duration_s", "elapsed_s",
    "unreliable", "unreliable_reasons", "finish_reason", "truncated",
    "avg_logprob", "compression_ratio", "no_speech_prob",
}


def assert_client_readable(body):
    assert isinstance(body["text"], str)
    assert body["truncated"] in (True, False, None)
    assert body["unreliable"] in (True, False, None)
    assert isinstance(body["unreliable_reasons"], list)


def test_a_normal_transcription_has_exactly_the_contract_keys(make_client):
    c, _, be = make_client()
    r = upload(c, tone())
    assert r.status_code == 200, r.text
    body = r.json()
    assert set(body) == RESPONSE_KEYS
    assert body["text"] == "fake transcript"
    assert_client_readable(body)
    assert be.calls and be.calls[0]["language"] == "en" and be.calls[0]["punctuation"] is True


def test_a_dead_capture_is_200_empty_and_flagged_with_the_SAME_keys(make_client):
    c, _, be = make_client()
    r = upload(c, silence(2.0))
    assert r.status_code == 200, r.text
    body = r.json()
    assert set(body) == RESPONSE_KEYS
    assert body["text"] == ""
    assert body["unreliable"] is True
    assert body["unreliable_reasons"] == ["constant_signal"]
    assert body["truncated"] is None and body["finish_reason"] == "unknown"
    assert body["avg_logprob"] is None and body["no_speech_prob"] is None
    assert be.calls == [], "the backend must never see a constant signal"


@pytest.mark.parametrize(
    "finish,truncated", [("stop", False), ("length", True), ("unknown", None)]
)
def test_truncated_is_three_valued_from_finish_reason(make_client, finish, truncated):
    c, _, _ = make_client(FakeBackend(Transcript(text="x", finish_reason=finish)))
    body = upload(c, tone()).json()
    assert body["finish_reason"] == finish
    assert body["truncated"] is truncated


def test_the_verdict_fires_above_the_threshold_and_not_below(make_client):
    c, mod, _ = make_client(FakeBackend(Transcript(text="x", no_speech_prob=0.9)))
    assert mod.NO_SPEECH_MAX == 0.5  # the fake backend's default
    body = upload(c, tone()).json()
    assert body["unreliable"] is True and body["unreliable_reasons"] == ["no_speech_prob"]

    c2, _, _ = make_client(FakeBackend(Transcript(text="x", no_speech_prob=0.1)))
    body = upload(c2, tone()).json()
    assert body["unreliable"] is False and body["unreliable_reasons"] == []


def test_no_signal_is_null_not_false(make_client):
    c, _, _ = make_client(FakeBackend(Transcript(text="x")))
    body = upload(c, tone()).json()
    assert body["unreliable"] is None
    assert body["avg_logprob"] is None and body["no_speech_prob"] is None


def test_a_rule_turned_off_by_env_is_null_not_false(make_client):
    c, mod, _ = make_client(FakeBackend(Transcript(text="x", no_speech_prob=0.9)), STT_NO_SPEECH_MAX="off")
    assert mod.NO_SPEECH_MAX is None
    assert upload(c, tone()).json()["unreliable"] is None


def test_a_blank_threshold_means_the_backend_default_not_off(make_client):
    c, mod, _ = make_client(FakeBackend(Transcript(text="x", no_speech_prob=0.9)), STT_NO_SPEECH_MAX="")
    assert mod.NO_SPEECH_MAX == 0.5
    assert upload(c, tone()).json()["unreliable"] is True


def test_a_blank_env_var_means_the_backend_default(monkeypatch):
    from backends import faster_whisper

    monkeypatch.setenv("STT_MODEL", "")
    monkeypatch.setenv("STT_BEAM_SIZE", "  ")
    be = faster_whisper.FasterWhisperBackend()
    assert (be.model, be.beam_size) == ("small", 5)


def test_env_overrides_the_backend_default_threshold(make_client):
    c, mod, _ = make_client(FakeBackend(Transcript(text="x", no_speech_prob=0.3)), STT_NO_SPEECH_MAX="0.2")
    assert mod.NO_SPEECH_MAX == 0.2
    assert upload(c, tone()).json()["unreliable"] is True


def test_a_tiny_probability_survives_rounding(make_client):
    c, _, _ = make_client(FakeBackend(Transcript(text="x", no_speech_prob=2.345678912e-9)))
    assert upload(c, tone()).json()["no_speech_prob"] == pytest.approx(2.34568e-9)


def test_a_structural_flag_forces_unreliable(make_client):
    c, _, _ = make_client(FakeBackend(Transcript(text="", flags=("vad_no_speech",))))
    body = upload(c, tone()).json()
    assert body["unreliable"] is True and body["unreliable_reasons"] == ["vad_no_speech"]


def test_a_bcp47_tag_is_a_400_naming_the_bare_code(make_client):
    c, _, be = make_client()
    r = upload(c, tone(), language="en-US")
    assert r.status_code == 400
    assert "send 'en'" in r.json()["detail"]
    assert be.calls == []


def test_a_backend_without_a_language_list_lets_the_backend_decide(make_client):
    be = FakeBackend()
    c, _, _ = make_client(be)
    be.languages = None
    assert upload(c, tone(), language="xx").status_code == 200


def test_undecodable_bytes_are_a_400(make_client):
    c, _, _ = make_client()
    r = upload(c, b"this is not audio at all" * 10)
    assert r.status_code == 400
    assert r.json()["detail"].startswith("could not decode the uploaded audio")


def test_an_empty_container_is_a_400(make_client):
    from conftest import wav_bytes
    import numpy as np

    c, _, _ = make_client()
    r = upload(c, wav_bytes(np.zeros(0)))
    assert r.status_code == 400


def test_a_missing_ffmpeg_is_a_retryable_503_not_a_400(make_client, monkeypatch):
    c, mod, _ = make_client()
    real_run = mod.subprocess.run

    def run(cmd, *a, **k):
        return real_run(["/nonexistent/ffmpeg", *cmd[1:]], *a, **k)

    monkeypatch.setattr(mod.subprocess, "run", run)
    r = upload(c, tone())
    assert r.status_code == 503
    assert "ffmpeg could not be started" in r.json()["detail"]


def test_too_long_is_a_413(make_client, monkeypatch):
    c, mod, _ = make_client()
    monkeypatch.setattr(mod, "MAX_AUDIO_SECONDS", 1.0)
    r = upload(c, tone(3.0))
    assert r.status_code == 413
    assert "more than 1s" in r.json()["detail"]


def test_backend_request_error_is_400_and_unavailable_is_a_RETRYABLE_503(make_client):
    class Refusing(FakeBackend):
        def __init__(self, exc):
            super().__init__()
            self.exc = exc

        def transcribe(self, audio, language, punctuation):
            raise self.exc

    c, _, _ = make_client(Refusing(BackendRequestError("nope")))
    r = upload(c, tone())
    assert r.status_code == 400 and r.json()["detail"] == "nope"

    c, _, _ = make_client(Refusing(BackendUnavailable("upstream down")))
    r = upload(c, tone())
    assert r.status_code == 503
    # The client stops retrying ONLY when detail carries this marker.
    assert "STT_API_TOKEN" not in r.json()["detail"]


def test_the_unauthenticated_routes_are_exactly_healthz_and_root(make_client):
    c, _, _ = make_client()
    assert c.get("/healthz").status_code == 200
    assert c.get("/").status_code == 200
    assert c.get("/v1/snippets").status_code == 401
    assert upload(c, tone(), headers={}).status_code == 401
    assert upload(c, tone(), headers=auth("wrong-token-wrong-token-wrong")).status_code == 401


def test_healthz_is_503_until_the_backend_is_loaded(make_client):
    c, _, be = make_client()
    be.ready = False
    r = c.get("/healthz")
    assert r.status_code == 503
    assert upload(c, tone()).status_code == 503


def test_no_token_configured_is_the_PERMANENT_503(make_client):
    c, _, _ = make_client(STT_API_TOKEN=None)
    r = upload(c, tone(), headers=auth())
    assert r.status_code == 503
    assert "STT_API_TOKEN" in r.json()["detail"]
    assert c.get("/healthz").status_code == 200


def test_named_tokens_each_authenticate_and_no_header_changes_that(make_client):
    c, _, _ = make_client(STT_API_TOKENS="phone:phone-token-abcdefghijkl,laptop:laptop-token-abcdefghijk")
    for tok in (TOKEN_DEFAULT, "phone-token-abcdefghijkl", "laptop-token-abcdefghijk"):
        assert upload(c, tone(), headers=auth(tok)).status_code == 200
    # The former "public entry" header is gone: it neither grants nor narrows.
    assert upload(c, tone(), headers={**auth("phone-token-abcdefghijkl"), "X-STT-Entry": "public"}).status_code == 200
    assert upload(c, tone(), headers={"X-STT-Entry": "public"}).status_code == 401


def test_an_unknown_backend_refuses_boot(load_app):
    import backends

    with pytest.raises(backends.UnknownBackend, match="faster-whisper"):
        load_app(STT_BACKEND="whisperx")


def test_a_malformed_token_list_refuses_boot(load_app):
    with pytest.raises(Exception, match="entry 1"):
        load_app(STT_API_TOKENS="no-colon-here")
