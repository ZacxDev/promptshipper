"""GET /v1/snippets: the mounted catalog, its named 503s, ETag/304 and the cache."""

import json
import os
import pathlib

import pytest

from conftest import auth

EXAMPLE = pathlib.Path(__file__).resolve().parent.parent / "examples" / "snippets.json"
SNIPPET_KEYS = {"label", "body", "search_terms", "category"}


@pytest.fixture
def catalog(make_client, monkeypatch, tmp_path):
    c, mod, _ = make_client()
    path = tmp_path / "snippets.json"
    monkeypatch.setattr(mod, "SNIP_CATALOG_PATH", str(path))
    mod._snip_cache.update(key=None, body=None, etag=None, last_modified=None, rows=0)
    return c, mod, path


def test_missing_catalog_is_a_named_503(catalog):
    c, mod, _ = catalog
    r = c.get("/v1/snippets", headers=auth())
    assert r.status_code == 503
    assert r.json()["detail"].startswith(mod.SNIP_CATALOG_MISSING)


@pytest.mark.parametrize("raw,reason", [
    (b"{not json", "SNIP_CATALOG_MALFORMED"),
    (b'{"label": "x"}', "SNIP_CATALOG_MALFORMED"),
    (b"[1, 2]", "SNIP_CATALOG_MALFORMED"),
    (b"[]", "SNIP_CATALOG_EMPTY"),
])
def test_bad_catalogs_are_named_503s_never_an_empty_200(catalog, raw, reason):
    c, mod, path = catalog
    path.write_bytes(raw)
    r = c.get("/v1/snippets", headers=auth())
    assert r.status_code == 503
    assert r.json()["detail"].startswith(getattr(mod, reason))


def test_a_good_catalog_is_served_byte_for_byte_with_an_etag_and_304(catalog):
    c, _, path = catalog
    raw = EXAMPLE.read_bytes()
    path.write_bytes(raw)
    r = c.get("/v1/snippets", headers=auth())
    assert r.status_code == 200
    assert r.content == raw
    assert r.headers["content-type"].startswith("application/json")
    etag = r.headers["etag"]
    assert etag.startswith('"') and etag.endswith('"')
    r2 = c.get("/v1/snippets", headers={**auth(), "If-None-Match": etag})
    assert r2.status_code == 304


def test_the_file_is_read_once_until_it_changes(catalog, monkeypatch):
    c, mod, path = catalog
    path.write_bytes(EXAMPLE.read_bytes())
    reads = []
    real = mod._snip_read_file
    monkeypatch.setattr(mod, "_snip_read_file", lambda p: reads.append(p) or real(p))
    for _ in range(3):
        assert c.get("/v1/snippets", headers=auth()).status_code == 200
    assert len(reads) == 1
    path.write_bytes(b'[{"label":"changed","body":"b","search_terms":[],"category":"c"}]')
    os.utime(path, ns=(1, 1))  # a different (mtime, size) key
    r = c.get("/v1/snippets", headers=auth())
    assert r.json()[0]["label"] == "changed"
    assert len(reads) == 2


def test_the_shipped_example_catalog_is_valid_for_the_client():
    rows = json.loads(EXAMPLE.read_text())
    assert isinstance(rows, list) and rows
    for row in rows:
        assert set(row) == SNIPPET_KEYS
        assert isinstance(row["label"], str) and row["label"]
        assert isinstance(row["body"], str) and row["body"]
        assert isinstance(row["category"], str)
        assert isinstance(row["search_terms"], list)
        assert all(isinstance(t, str) for t in row["search_terms"])
