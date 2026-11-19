"""The transcription upload is authenticated and size-capped BEFORE its body is read.

FastAPI parses a multipart body (and spools it to /tmp) before any `Depends`
runs, so a check that lives only in a dependency lets an unauthenticated caller
write an upload of any size to the container's disk. The instrument here is the
ASGI `receive` callable itself: it counts how many body bytes the application
actually PULLED. Pulling zero bytes is the only proof the body was never parsed.
"""

import asyncio
import json

import pytest

from conftest import TOKEN, tone

PATH = "/v1/audio/transcriptions"
BOUNDARY = "gateboundary"


def multipart(audio: bytes) -> bytes:
    return (
        f"--{BOUNDARY}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"d.ogg\"\r\n"
        f"Content-Type: audio/ogg\r\n\r\n".encode() + audio
        + f"\r\n--{BOUNDARY}\r\nContent-Disposition: form-data; name=\"language\"\r\n\r\nen\r\n"
        f"--{BOUNDARY}--\r\n".encode()
    )


def call(asgi_app, body: bytes, headers: dict, chunk: int = 1 << 16, declare_length: bool = True):
    """Drive the ASGI app directly. Returns (status, json_body, bytes_pulled)."""
    hdrs = [(k.lower().encode(), v.encode()) for k, v in headers.items()]
    hdrs.append((b"content-type", f"multipart/form-data; boundary={BOUNDARY}".encode()))
    if declare_length:
        hdrs.append((b"content-length", str(len(body)).encode()))
    scope = {"type": "http", "asgi": {"version": "3.0"}, "http_version": "1.1", "method": "POST",
             "scheme": "http", "path": PATH, "raw_path": PATH.encode(), "query_string": b"",
             "root_path": "", "headers": hdrs, "client": ("127.0.0.1", 1), "server": ("t", 80)}
    pieces = [body[i:i + chunk] for i in range(0, len(body), chunk)] or [b""]
    state = {"pulled": 0, "i": 0}
    sent = []

    async def receive():
        i = state["i"]
        if i >= len(pieces):
            await asyncio.sleep(3600)  # a client that never sends more
        state["i"] += 1
        state["pulled"] += len(pieces[i])
        return {"type": "http.request", "body": pieces[i], "more_body": i < len(pieces) - 1}

    async def send(msg):
        sent.append(msg)

    asyncio.run(asyncio.wait_for(asgi_app(scope, receive, send), 30))
    start = next(m for m in sent if m["type"] == "http.response.start")
    raw = b"".join(m.get("body", b"") for m in sent if m["type"] == "http.response.body")
    return start["status"], (json.loads(raw) if raw else None), state["pulled"]


BIG = multipart(b"\0" * (4 << 20))  # 4 MiB upload


@pytest.fixture
def mod(make_client):
    _, mod, _ = make_client(STT_API_TOKENS="phone:public:phone-public-token-abcdefgh")
    return mod


def test_an_unauthenticated_upload_is_refused_without_reading_the_body(mod):
    status, body, pulled = call(mod.app, BIG, {})
    assert status == 401 and body == {"detail": "invalid or missing bearer token"}
    assert pulled == 0, f"{pulled} body bytes were read before the token was checked"


def test_a_wrong_token_is_refused_without_reading_the_body(mod):
    status, _, pulled = call(mod.app, BIG, {"authorization": "Bearer not-the-token-xxxxxxxx"})
    assert (status, pulled) == (401, 0)


def test_an_internal_token_through_the_public_door_is_refused_without_reading_the_body(mod):
    status, body, pulled = call(mod.app, BIG, {"authorization": f"Bearer {TOKEN}", "x-stt-entry": "public"})
    assert status == 403 and body["detail"] == "this token is not permitted through the public entry"
    assert pulled == 0


def test_no_configured_token_is_the_same_permanent_503_without_reading_the_body(make_client):
    _, mod, _ = make_client(STT_API_TOKEN=None)
    status, body, pulled = call(mod.app, BIG, {"authorization": "Bearer anything-at-all-xxxxxxxx"})
    assert status == 503 and "STT_API_TOKEN" in body["detail"] and pulled == 0


def test_an_oversize_declared_body_is_413_without_reading_it(mod, monkeypatch):
    monkeypatch.setattr(mod, "MAX_UPLOAD_BYTES", 1 << 20)
    status, body, pulled = call(mod.app, BIG, {"authorization": f"Bearer {TOKEN}"})
    assert status == 413 and "upload too large" in body["detail"]
    assert pulled == 0


def test_an_oversize_chunked_body_is_413_and_reading_stops_at_the_cap(mod, monkeypatch):
    cap = 1 << 20
    monkeypatch.setattr(mod, "MAX_UPLOAD_BYTES", cap)
    status, body, pulled = call(mod.app, BIG, {"authorization": f"Bearer {TOKEN}"}, declare_length=False)
    assert status == 413 and "upload too large" in body["detail"]
    assert pulled <= cap + (1 << 16), f"read {pulled} bytes past a {cap}-byte cap"


def test_an_authorised_upload_under_the_cap_still_transcribes(mod):
    status, body, _ = call(mod.app, multipart(tone()), {"authorization": f"Bearer {TOKEN}"})
    assert status == 200 and body["text"] == "fake transcript"
    status, body, _ = call(mod.app, multipart(tone()), {"authorization": f"Bearer {TOKEN}"}, declare_length=False)
    assert status == 200 and body["text"] == "fake transcript"


def test_a_public_token_through_the_public_door_still_uploads(mod):
    status, _, _ = call(mod.app, multipart(tone()),
                        {"authorization": "Bearer phone-public-token-abcdefgh", "x-stt-entry": "public"})
    assert status == 200


def test_the_default_cap_is_derived_from_max_audio_seconds(load_app):
    m = load_app(STT_MAX_AUDIO_SECONDS="100", STT_MAX_UPLOAD_BYTES=None)
    assert m.MAX_UPLOAD_BYTES == 100 * 32000 + (1 << 20)
    m = load_app(STT_MAX_UPLOAD_BYTES="5000000")
    assert m.MAX_UPLOAD_BYTES == 5000000


def test_blank_settings_mean_defaults_not_a_crash(load_app):
    # compose passes every documented setting, so an unset one arrives blank.
    m = load_app(STT_MAX_AUDIO_SECONDS="", STT_MAX_UPLOAD_BYTES="", STT_BACKEND="")
    assert m.MAX_AUDIO_SECONDS == 3600.0 and m.BACKEND.name == "faster-whisper"


def test_root_requires_a_token(make_client):
    c, _, _ = make_client()
    assert c.get("/").status_code == 401
    r = c.get("/", headers={"Authorization": f"Bearer {TOKEN}"})
    assert r.status_code == 200 and r.json()["backend"] == "fake"
