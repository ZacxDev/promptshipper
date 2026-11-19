"""Token parsing (app.parse_tokens) and the stdlib pre-flight (check_tokens.py)."""

import io

import pytest

import app
import check_tokens


def test_legacy_and_named_tokens_parse_to_digests_only():
    table = app.parse_tokens("legacy-token", "phone:pt1234,laptop:lt:12:34")
    assert [n for n, _ in table] == ["default", "phone", "laptop"]
    assert all(len(d) == 32 for _, d in table)
    # Everything after the FIRST colon is the token, colons included.
    assert app.match_token("lt:12:34", table) == "laptop"
    assert app.match_token("legacy-token", table) == "default"
    assert app.match_token("nope", table) is None


@pytest.mark.parametrize("named,fragment", [
    ("a:x,", "entry 2 is empty"),
    ("nocolon", "entry 1 is not"),
    ("Bad:x", "invalid name"),
    ("a:", "empty token"),
    ("a:x y", "whitespace"),
    ("a:x,a:y", "duplicate token name"),
    ("a:same,b:same", "same token"),
])
def test_malformed_entries_are_refused_and_never_echo_the_token(named, fragment):
    with pytest.raises(app.TokenConfigError) as e:
        app.parse_tokens("", named)
    assert fragment in str(e.value)
    # Messages name entries by position; a token never appears in one.
    for tok in ("same", "x y"):
        assert f":{tok}" not in str(e.value)


def test_check_tokens_runs_the_same_rules():
    out, err = io.StringIO(), io.StringIO()
    assert check_tokens.main({"STT_API_TOKEN": "t", "STT_API_TOKENS": "p:q"}, out, err) == 0
    assert "2 token(s)" in out.getvalue() and "  p\n" in out.getvalue()
    out, err = io.StringIO(), io.StringIO()
    assert check_tokens.main({"STT_API_TOKENS": "oops"}, out, err) == 2
    assert "REFUSED" in err.getvalue()
