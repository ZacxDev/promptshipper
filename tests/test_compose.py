"""deploy/compose/docker-compose.yml — every documented STT setting reaches the
container, and the published address is exactly BIND (IPv4 or IPv6).

The set of settings is READ from the code and the docs, not typed here, so a new
setting that is documented or read but not passed through fails this test.
Needs the docker compose CLI (no daemon): CI's runners have it.
"""

import json
import pathlib
import re
import shutil
import subprocess

import pytest

ROOT = pathlib.Path(__file__).resolve().parent.parent
COMPOSE = ROOT / "deploy" / "compose" / "docker-compose.yml"
EXAMPLE = ROOT / "deploy" / "compose" / ".env.example"
STT = ROOT / "server" / "stt"
SETTING = re.compile(r"\b(?:STT|HF)_[A-Z0-9_]+\b")
CODE_READ = re.compile(r'(?:\benv|os\.environ\.get|_optional_float)\("((?:STT|HF)_[A-Z0-9_]+)"')


def documented_and_read() -> set:
    names = set(SETTING.findall((STT / "README.md").read_text()))
    for py in [STT / "app.py", *sorted((STT / "backends").glob("*.py"))]:
        text = py.read_text()
        names |= set(CODE_READ.findall(text))
        names |= set(SETTING.findall(text.split('"""')[1] if text.count('"""') >= 2 else ""))
    return names


def config(env_file: pathlib.Path) -> dict:
    docker = shutil.which("docker")
    assert docker, "the docker CLI (with compose) is required for this test"
    out = subprocess.run(
        [docker, "compose", "-f", str(COMPOSE), "--env-file", str(env_file), "--profile", "cpu",
         "--profile", "gpu", "config", "--format", "json"],
        capture_output=True, text=True, check=True,
        env={"PATH": "/usr/bin:/bin:" + str(pathlib.Path(docker).parent), "HOME": "/tmp"},
    ).stdout
    return json.loads(out)["services"]


def test_the_setting_list_is_not_vacuous():
    names = documented_and_read()
    # Positive control on the instrument: known settings from three sources.
    assert {"STT_MAX_AUDIO_SECONDS", "STT_VAD", "STT_UPSTREAM_FORMAT", "STT_MAX_NEW_TOKENS", "HF_TOKEN"} <= names
    assert len(names) >= 20


@pytest.mark.parametrize("service", ["stt", "stt-gpu"])
def test_every_documented_setting_reaches_the_container(service):
    env = config(EXAMPLE)[service]["environment"]
    missing = sorted(documented_and_read() - set(env))
    assert not missing, f"{service} does not pass through: {missing}"
    # The image sets HF_HOME; a blank pass-through would erase it.
    assert env["HF_HOME"] == "/models/hf"


@pytest.mark.parametrize("bind", ["127.0.0.1", "100.70.1.9", "fd7a:115c:a1e0::9"])
def test_ports_are_published_on_exactly_bind(tmp_path, bind):
    env_file = tmp_path / ".env"
    env_file.write_text(EXAMPLE.read_text().replace("BIND=127.0.0.1", f"BIND={bind}"))
    svcs = config(env_file)
    for svc, port in (("stt", "8118"), ("stt-gpu", "8118"), ("clip", "8139")):
        (p,) = svcs[svc]["ports"]
        assert (p["host_ip"], str(p["published"])) == (bind, port), (svc, p)
