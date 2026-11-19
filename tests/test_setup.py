"""./setup — idempotence, token handling, the pairing payload, and the start path.

Every test runs a COPY of setup in a throwaway tree, with a PATH that contains
only the tools it needs plus fakes (docker, tailscale, qrencode) — so neither a
real docker daemon nor this host's network decides the outcome. The start path
is exercised against stub HTTP servers standing in for the two services.
"""

import http.server
import json
import os
import pathlib
import re
import shutil
import socket
import stat
import subprocess
import threading
import struct
import zlib

import pytest

ROOT = pathlib.Path(__file__).resolve().parent.parent
TOOLS = ["bash", "env", "cat", "mkdir", "mktemp", "chmod", "mv", "cp", "sed", "head", "tr",
         "awk", "od", "dirname", "grep", "sleep", "curl", "rm", "printf"]
HEX64 = re.compile(r"^[0-9a-f]{64}$")


def write_exe(path, body):
    path.write_text("#!/usr/bin/env bash\n" + body)
    path.chmod(path.stat().st_mode | stat.S_IEXEC)


@pytest.fixture
def tree(tmp_path):
    t = tmp_path / "repo"
    (t / "deploy" / "compose").mkdir(parents=True)
    (t / "server" / "stt" / "examples").mkdir(parents=True)
    shutil.copy2(ROOT / "setup", t / "setup")
    for f in ("docker-compose.yml", ".env.example"):
        shutil.copy2(ROOT / "deploy" / "compose" / f, t / "deploy" / "compose" / f)
    shutil.copy2(ROOT / "server" / "stt" / "examples" / "snippets.json", t / "server" / "stt" / "examples")
    bin_ = tmp_path / "bin"
    bin_.mkdir()
    for tool in TOOLS:
        real = shutil.which(tool)
        assert real, f"test host lacks {tool}"
        (bin_ / tool).symlink_to(real)
    # Like the real one: a name with no args, and NOT an address for -I.
    write_exe(bin_ / "hostname", '[ $# -eq 0 ] && echo test-box || echo "hostname: invalid option" >&2\n')
    log = tmp_path / "docker.log"
    write_exe(bin_ / "docker", f'echo "$*" >> {log}\nexit 0\n')
    return t, bin_, log


def run(tree_, *args, extra_env=None):
    t, bin_, _ = tree_
    env = {"PATH": str(bin_), "HOME": str(t)}
    env.update(extra_env or {})
    return subprocess.run([str(t / "setup"), *args], capture_output=True, text=True, env=env, timeout=60)


def envfile(tree_):
    p = tree_[0] / "deploy" / "compose" / ".env"
    return p, dict(l.split("=", 1) for l in p.read_text().splitlines() if l and not l.startswith("#"))


def payload_of(out):
    line = next(l for l in out.splitlines() if l.startswith("PAIRING_PAYLOAD="))
    return json.loads(line.split("=", 1)[1])


# ------------------------------------------------------------------ .env + tokens


def test_first_run_writes_a_private_env_with_two_distinct_strong_tokens(tree):
    r = run(tree, "--no-start", "--host", "192.168.1.20", "--no-qr")
    assert r.returncode == 0, r.stderr
    p, cfg = envfile(tree)
    assert stat.S_IMODE(p.stat().st_mode) == 0o600
    assert HEX64.match(cfg["STT_API_TOKEN"]) and HEX64.match(cfg["CLIPSTORE_TOKEN"])
    assert cfg["STT_API_TOKEN"] != cfg["CLIPSTORE_TOKEN"]
    assert cfg["COMPOSE_PROFILES"] == "cpu"
    assert (cfg["PROMPTSHIPPER_HOST"], cfg["BIND"]) == ("192.168.1.20", "192.168.1.20")
    assert (cfg["STT_PORT"], cfg["CLIP_PORT"]) == ("8118", "8139")


def test_rerunning_keeps_the_tokens_and_the_file_byte_for_byte(tree):
    assert run(tree, "--no-start", "--host", "192.168.1.20", "--no-qr").returncode == 0
    p, first = envfile(tree)
    before = p.read_bytes()
    r = run(tree, "--no-start", "--no-qr")
    assert r.returncode == 0, r.stderr
    assert envfile(tree)[1]["STT_API_TOKEN"] == first["STT_API_TOKEN"]
    assert envfile(tree)[1]["CLIPSTORE_TOKEN"] == first["CLIPSTORE_TOKEN"]
    assert p.read_bytes() == before


def test_rotate_replaces_both_tokens_and_nothing_else(tree):
    run(tree, "--no-start", "--host", "192.168.1.20", "--stt-port", "9001", "--no-qr")
    _, a = envfile(tree)
    assert run(tree, "--no-start", "--rotate", "--no-qr").returncode == 0
    _, b = envfile(tree)
    assert a["STT_API_TOKEN"] != b["STT_API_TOKEN"] and a["CLIPSTORE_TOKEN"] != b["CLIPSTORE_TOKEN"]
    assert {k: v for k, v in a.items() if "TOKEN" not in k} == {k: v for k, v in b.items() if "TOKEN" not in k}


def test_settings_and_your_own_keys_survive_a_rerun(tree):
    run(tree, "--no-start", "--host", "192.168.1.20", "--clip-port", "9139", "--model", "base", "--no-qr")
    p, _ = envfile(tree)
    p.write_text(p.read_text() + "STT_NO_SPEECH_MAX=0.3\n")
    assert run(tree, "--no-start", "--no-qr").returncode == 0
    _, cfg = envfile(tree)
    assert (cfg["CLIP_PORT"], cfg["STT_MODEL"], cfg["STT_NO_SPEECH_MAX"]) == ("9139", "base", "0.3")


def test_a_short_existing_token_is_refused_not_used(tree):
    run(tree, "--no-start", "--host", "192.168.1.20", "--no-qr")
    p, _ = envfile(tree)
    p.write_text(re.sub(r"CLIPSTORE_TOKEN=.*", "CLIPSTORE_TOKEN=short", p.read_text()))
    r = run(tree, "--no-start", "--no-qr")
    assert r.returncode == 1 and "CLIPSTORE_TOKEN" in r.stderr


def test_the_snippet_catalog_is_seeded_once_and_your_edits_are_kept(tree):
    run(tree, "--no-start", "--host", "192.168.1.20", "--no-qr")
    snip = tree[0] / "deploy" / "compose" / "snippets" / "snippets.json"
    assert json.loads(snip.read_text())
    snip.write_text('[{"label":"mine","body":"b","search_terms":[],"category":"c"}]')
    run(tree, "--no-start", "--no-qr")
    assert "mine" in snip.read_text()


# ------------------------------------------------------------------ address detection


def test_the_tailscale_address_is_preferred_and_binds_the_tailnet_only(tree):
    write_exe(tree[1] / "tailscale", 'echo 100.64.1.2\n')
    assert run(tree, "--no-start", "--no-qr").returncode == 0
    _, cfg = envfile(tree)
    assert (cfg["PROMPTSHIPPER_HOST"], cfg["BIND"]) == ("100.64.1.2", "100.64.1.2")


def test_a_dns_name_binds_all_interfaces(tree):
    assert run(tree, "--no-start", "--host", "box.example.ts.net", "--no-qr").returncode == 0
    assert envfile(tree)[1]["BIND"] == "0.0.0.0"


def test_no_detectable_address_is_an_error_naming_the_flag(tree):
    r = run(tree, "--no-start", "--no-qr")
    assert r.returncode == 1 and "--host" in r.stderr


def test_a_detector_that_prints_a_non_address_is_not_believed(tree):
    write_exe(tree[1] / "hostname", '[ "$1" = -I ] && echo "usage: hostname [-fs]" || echo test-box\n')
    r = run(tree, "--no-start", "--no-qr")
    assert r.returncode == 1 and "--host" in r.stderr


# ------------------------------------------------------------------ pairing payload


def test_the_pairing_payload_is_exactly_the_v1_shape(tree):
    r = run(tree, "--no-start", "--host", "100.64.1.2", "--no-qr")
    p = payload_of(r.stdout)
    _, cfg = envfile(tree)
    assert p == {
        "v": 1,
        "type": "promptshipper-pairing",
        "name": "test-box",
        "stt": {"url": "http://100.64.1.2:8118", "token": cfg["STT_API_TOKEN"]},
        "clip": {"url": "http://100.64.1.2:8139", "token": cfg["CLIPSTORE_TOKEN"]},
    }


def test_an_ipv6_host_is_bracketed(tree):
    r = run(tree, "--no-start", "--host", "fd7a:115c:a1e0::1", "--no-qr")
    assert payload_of(r.stdout)["stt"]["url"] == "http://[fd7a:115c:a1e0::1]:8118"


def test_pairing_only_reprints_without_touching_anything(tree):
    run(tree, "--no-start", "--host", "100.64.1.2", "--no-qr")
    p, _ = envfile(tree)
    before = p.read_bytes()
    write_exe(tree[1] / "qrencode", 'cat >/dev/null; echo QR-RENDERED\n')
    r = run(tree, "--pairing")
    assert r.returncode == 0 and "QR-RENDERED" in r.stdout
    assert payload_of(r.stdout)["v"] == 1
    assert p.read_bytes() == before


def test_pairing_only_without_a_config_fails(tree):
    r = run(tree, "--pairing")
    assert r.returncode == 1 and "run ./setup first" in r.stderr


# ------------------------------------------------------------------ start path


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Stub:
    """Stands in for one service: /healthz, plus one authenticated route that
    checks the bearer against the token ./setup wrote to .env."""

    def __init__(self, env_path, key, auth_path, healthy=True):
        stub = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                if self.path == "/healthz":
                    self.send_response(200 if stub.healthy else 503)
                elif self.path == auth_path:
                    want = dict(l.split("=", 1) for l in env_path.read_text().splitlines()
                                if l and not l.startswith("#"))[key]
                    ok = self.headers.get("Authorization") == f"Bearer {want}"
                    self.send_response(200 if ok and not stub.reject else 401)
                else:
                    self.send_response(404)
                self.end_headers()

        self.healthy, self.reject = healthy, False
        self.port = free_port()
        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", self.port), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()


@pytest.fixture
def stubs(tree):
    env_path = tree[0] / "deploy" / "compose" / ".env"
    stt = Stub(env_path, "STT_API_TOKEN", "/v1/snippets")
    clip = Stub(env_path, "CLIPSTORE_TOKEN", "/v1/clips?limit=1")
    yield stt, clip
    stt.close()
    clip.close()


def start(tree, stubs, *extra):
    stt, clip = stubs
    return run(tree, "--host", "127.0.0.1", "--stt-port", str(stt.port), "--clip-port", str(clip.port),
               "--timeout", "6", "--no-qr", *extra)


def test_the_full_start_path_brings_up_verifies_and_prints_the_pairing(tree, stubs):
    r = start(tree, stubs)
    assert r.returncode == 0, r.stdout + r.stderr
    assert "both services accept their tokens" in r.stdout
    log = tree[2].read_text().splitlines()
    assert any(l.endswith("up -d --build clip stt") for l in log), log
    assert any("rm --stop --force stt-gpu" in l for l in log), log
    assert payload_of(r.stdout)["clip"]["url"] == f"http://127.0.0.1:{stubs[1].port}"


def test_the_gpu_profile_starts_stt_gpu_and_stops_the_cpu_variant(tree, stubs):
    r = start(tree, stubs, "--gpu")
    assert r.returncode == 0, r.stdout + r.stderr
    log = tree[2].read_text().splitlines()
    assert any(l.endswith("up -d --build clip stt-gpu") for l in log), log
    assert any(l.endswith("rm --stop --force stt") for l in log), log
    assert envfile(tree)[1]["COMPOSE_PROFILES"] == "gpu"


def test_a_service_that_never_gets_healthy_is_exit_3(tree, stubs):
    stubs[0].healthy = False
    r = start(tree, stubs)
    assert r.returncode == 3, r.stdout + r.stderr
    assert "stt did not become healthy" in r.stderr


def test_a_service_that_rejects_its_own_token_is_exit_4(tree, stubs):
    stubs[1].reject = True
    r = start(tree, stubs)
    assert r.returncode == 4, r.stdout + r.stderr
    assert "clip is up but answered 401" in r.stderr


def test_missing_docker_is_exit_2(tree):
    (tree[1] / "docker").unlink()
    r = run(tree, "--host", "127.0.0.1", "--no-qr")
    assert r.returncode == 2 and "docker is not installed" in r.stderr


def test_a_dead_docker_daemon_is_exit_2(tree):
    write_exe(tree[1] / "docker", 'case "$*" in info*) exit 1;; esac; exit 0\n')
    r = run(tree, "--host", "127.0.0.1", "--no-qr")
    assert r.returncode == 2 and "daemon is not reachable" in r.stderr


# ------------------------------------------------------------------ --qr-png


def link_real(tree_, tool):
    real = shutil.which(tool)
    assert real, f"{tool} is required for this test (CI installs it)"
    (tree_[1] / tool).symlink_to(real)


def assert_valid_png(data):
    """Signature, chunk CRCs, IHDR first, IEND last, IDAT inflates to the size IHDR implies."""
    assert data[:8] == b"\x89PNG\r\n\x1a\n"
    pos, chunks = 8, []
    while pos < len(data):
        (n,) = struct.unpack(">I", data[pos:pos + 4])
        kind, body = data[pos + 4:pos + 8], data[pos + 8:pos + 8 + n]
        (crc,) = struct.unpack(">I", data[pos + 8 + n:pos + 12 + n])
        assert zlib.crc32(kind + body) == crc, kind
        chunks.append((kind, body))
        pos += 12 + n
    assert chunks[0][0] == b"IHDR" and chunks[-1][0] == b"IEND"
    w, h, depth, ctype = struct.unpack(">IIBB", chunks[0][1][:10])
    assert w > 20 and h > 20
    raw = zlib.decompress(b"".join(b for k, b in chunks if k == b"IDAT"))
    channels = {0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[ctype]
    assert len(raw) == h * (1 + (w * channels * depth + 7) // 8)


def test_qr_png_writes_a_private_valid_png_and_prints_only_the_path(tree, tmp_path):
    link_real(tree, "qrencode")
    out = tmp_path / "pair.png"
    r = run(tree, "--no-start", "--host", "100.64.1.2", "--qr-png", str(out))
    assert r.returncode == 0, r.stderr
    _, cfg = envfile(tree)
    streams = r.stdout + r.stderr
    for key in ("STT_API_TOKEN", "CLIPSTORE_TOKEN"):
        assert cfg[key] not in streams, f"{key} leaked to stdout/stderr"
    assert "PAIRING_PAYLOAD" not in streams
    assert f"Pairing QR code written to: {out}" in r.stdout
    assert stat.S_IMODE(out.stat().st_mode) == 0o600
    assert_valid_png(out.read_bytes())


def test_qr_png_decodes_to_the_exact_pairing_payload(tree, tmp_path):
    link_real(tree, "qrencode")
    zbar = shutil.which("zbarimg")
    assert zbar, "zbarimg (zbar-tools) is required for this test (CI installs it)"
    out = tmp_path / "pair.png"
    assert run(tree, "--no-start", "--host", "100.64.1.2", "--qr-png", str(out)).returncode == 0
    decoded = subprocess.run([zbar, "--raw", "-q", str(out)], capture_output=True, text=True, check=True).stdout.strip()
    _, cfg = envfile(tree)
    p = json.loads(decoded)
    assert p["v"] == 1 and p["stt"]["token"] == cfg["STT_API_TOKEN"] and p["clip"]["token"] == cfg["CLIPSTORE_TOKEN"]


def test_qr_png_with_pairing_reprints_to_the_file_and_never_the_tokens(tree, tmp_path):
    run(tree, "--no-start", "--host", "100.64.1.2", "--no-qr")
    link_real(tree, "qrencode")
    out = tmp_path / "again.png"
    r = run(tree, "--pairing", "--qr-png", str(out))
    assert r.returncode == 0, r.stderr
    _, cfg = envfile(tree)
    assert cfg["STT_API_TOKEN"] not in r.stdout + r.stderr
    assert cfg["CLIPSTORE_TOKEN"] not in r.stdout + r.stderr
    assert_valid_png(out.read_bytes())


def test_qr_png_without_a_renderer_fails_and_leaves_no_file(tree, tmp_path):
    (tree[1] / "docker").unlink()
    out = tmp_path / "pair.png"
    r = run(tree, "--no-start", "--host", "100.64.1.2", "--qr-png", str(out))
    assert r.returncode == 5 and "could not render" in r.stderr
    assert not out.exists() and not list(tmp_path.glob("pair.png.*"))
    _, cfg = envfile(tree)
    assert cfg["STT_API_TOKEN"] not in r.stdout + r.stderr
