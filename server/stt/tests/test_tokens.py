"""Token parsing (app.parse_tokens) and the stdlib pre-flight (check_tokens.py)."""

import io

import pytest

import app
import check_tokens


def test_legacy_and_named_tokens_parse_to_digests_only():
    table = app.parse_tokens("legacy-token", "phone:pt1234,laptop:public:lt1234")
    assert [(n, s) for n, s, _ in table] == [("default", "internal"), ("phone", "internal"), ("laptop", "public")]
    assert all(len(d) == 32 for _, _, d in table)
    assert app.match_token("lt1234", table) == ("laptop", "public")
    assert app.match_token("nope", table) is None


@pytest.mark.parametrize("named,fragment", [
    ("a:x,", "entry 2 is empty"),
    ("nocolon", "entry 1 is not"),
    ("Bad:x", "invalid name"),
    ("a:secret:x", "unknown scope"),
    ("a:", "empty token"),
    ("a:x y", "whitespace"),
    ("a:x,a:y", "duplicate token name"),
    ("a:same,b:same", "same token"),
])
def test_malformed_entries_are_refused_and_never_echo_the_token(named, fragment):
    with pytest.raises(app.TokenConfigError) as e:
        app.parse_tokens("", named)
    assert fragment in str(e.value)
    # The scope slot of `a:secret:x` may itself be a token: never echoed.
    assert "secret" not in str(e.value)


def test_check_tokens_runs_the_same_rules():
    out, err = io.StringIO(), io.StringIO()
    assert check_tokens.main({"STT_API_TOKEN": "t", "STT_API_TOKENS": "p:q"}, out, err) == 0
    assert "2 token(s)" in out.getvalue() and "p  internal" in out.getvalue()
    out, err = io.StringIO(), io.StringIO()
    assert check_tokens.main({"STT_API_TOKENS": "oops"}, out, err) == 2
    assert "REFUSED" in err.getvalue()
