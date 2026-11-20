"""The optional public entry: one locked-down token exposed through a reverse proxy.

A proxy that publishes this service beyond the private network sets the
`X-STT-Entry` header on every request it forwards. A request carrying that
header is accepted ONLY with a token whose entry is `name:public:token`; every
other (internal) token is refused there with 403. Requests without the header
are unaffected. Semantics match the deployment this server was extracted from.
"""

import app
from conftest import TOKEN, auth, tone, upload

PHONE = "phone-public-token-abcdefgh"
LAPTOP = "laptop-internal-token-abcdef"
NAMED = f"phone:public:{PHONE},laptop:{LAPTOP}"
DOOR = {"X-STT-Entry": "public"}


def test_a_public_entry_is_parsed_as_scope_public_and_the_bare_token():
    table = app.parse_tokens("", "phone:public:tok,lap:internal:a:b,old:plain")
    assert [(n, s) for n, s, _ in table] == [("phone", "public"), ("lap", "internal"), ("old", "internal")]
    # `public:` is the SCOPE, not part of the token.
    assert app.match_token("tok", table) == ("phone", "public")
    assert app.match_token("public:tok", table) is None
    assert app.match_token("a:b", table) == ("lap", "internal")


def test_a_public_token_through_the_public_door_is_accepted(make_client):
    c, _, _ = make_client(STT_API_TOKENS=NAMED)
    assert upload(c, tone(), headers={**auth(PHONE), **DOOR}).status_code == 200


def test_an_internal_token_through_the_public_door_is_refused(make_client):
    c, _, _ = make_client(STT_API_TOKENS=NAMED)
    for tok in (LAPTOP, TOKEN):  # a named internal token, and the default token
        r = upload(c, tone(), headers={**auth(tok), **DOOR})
        assert r.status_code == 403, tok
        assert r.json()["detail"] == "this token is not permitted through the public entry"
    # Any value, even empty, and any repetition, marks the request public.
    r = c.get("/v1/snippets", headers=[("Authorization", f"Bearer {LAPTOP}"), ("X-STT-Entry", ""), ("X-STT-Entry", "public")])
    assert r.status_code == 403


def test_without_the_header_every_token_works(make_client):
    c, _, _ = make_client(STT_API_TOKENS=NAMED)
    for tok in (PHONE, LAPTOP, TOKEN):
        assert upload(c, tone(), headers=auth(tok)).status_code == 200, tok


def test_an_unknown_scope_refuses_boot_without_echoing_it():
    import pytest

    with pytest.raises(app.TokenConfigError) as e:
        app.parse_tokens("", "a:secretish:x")
    assert "unknown scope" in str(e.value) and "secretish" not in str(e.value)
