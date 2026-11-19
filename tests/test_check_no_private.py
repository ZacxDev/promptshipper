"""scripts/check-no-private.sh — positive and negative controls.

Each test builds a throwaway git repo holding a COPY of the gate, so the real
repository's contents never decide the outcome. The forbidden strings are
assembled at runtime so this file does not itself trip the gate.
"""

import pathlib
import shutil
import subprocess

import pytest

ROOT = pathlib.Path(__file__).resolve().parent.parent
GATE = ROOT / "scripts" / "check-no-private.sh"

# One realistic sample per pattern, built from pieces.
FORBIDDEN = {
    "mesh ip": "base_url = http://10" + ".42.0.10:8118",
    "internal zone": "image: harbor." + "homelab" + ".lan/library/stt:1",
    "private host": "curl https://stt." + "zacx" + ".dev/healthz",
    "sops value": "token: ENC" + "[AES256_GCM,data:abc,iv:def]",
    "sops block": "sops" + ":\n  age: []",
    "case-folded": "HOST=STT." + "ZACX" + ".DEV",
}


def run(repo, *args):
    return subprocess.run(["bash", str(repo / "scripts" / "check-no-private.sh"), *args],
                          cwd=repo, capture_output=True, text=True)


@pytest.fixture
def repo(tmp_path):
    (tmp_path / "scripts").mkdir()
    shutil.copy2(GATE, tmp_path / "scripts" / "check-no-private.sh")
    (tmp_path / "README.md").write_text("a clean file\n")
    subprocess.run(["git", "init", "-q", str(tmp_path)], check=True)
    return tmp_path


def test_a_clean_tree_passes_and_says_how_many_files_it_scanned(repo):
    r = run(repo)
    assert r.returncode == 0, r.stdout + r.stderr
    assert "scanned 2 file(s), 0 forbidden, 0 stale" in r.stdout


@pytest.mark.parametrize("name", sorted(FORBIDDEN))
def test_every_pattern_fails_the_gate(repo, name):
    (repo / "leak.txt").write_text("ok line\n" + FORBIDDEN[name] + "\n")
    r = run(repo)
    assert r.returncode == 1, f"{name}: gate passed a forbidden string\n{r.stdout}{r.stderr}"
    assert "FORBIDDEN leak.txt:2:" in r.stdout


def test_the_full_line_is_never_printed(repo):
    (repo / "leak.txt").write_text("secret-value-42 http://10" + ".42.0.10:1\n")
    r = run(repo)
    assert r.returncode == 1
    assert "secret-value-42" not in r.stdout + r.stderr


def test_untracked_files_are_scanned_and_gitignored_ones_are_not(repo):
    (repo / ".gitignore").write_text(".env\n")
    (repo / ".env").write_text(FORBIDDEN["mesh ip"] + "\n")
    assert run(repo).returncode == 0
    (repo / "new-untracked.md").write_text(FORBIDDEN["mesh ip"] + "\n")
    assert run(repo).returncode == 1


def test_explicit_paths_are_scanned_even_outside_git(repo, tmp_path_factory):
    outside = tmp_path_factory.mktemp("outside") / "fixture.yaml"
    outside.write_text(FORBIDDEN["sops value"] + "\n")
    r = run(repo, str(outside))
    assert r.returncode == 1 and "FORBIDDEN" in r.stdout


def test_an_allowlisted_exact_line_passes_but_any_other_line_fails(repo):
    rule = "- no mesh IPs (`10" + ".42.*`) may land here"
    (repo / "PLAN.md").write_text(rule + "\n")
    (repo / "scripts" / "private-allowlist.txt").write_text("# comment\nPLAN.md\t" + rule + "\n")
    assert run(repo).returncode == 0
    (repo / "PLAN.md").write_text(rule + "\n" + FORBIDDEN["mesh ip"] + "\n")
    r = run(repo)
    assert r.returncode == 1 and "FORBIDDEN PLAN.md:2:" in r.stdout


def test_a_stale_allowlist_entry_fails(repo):
    (repo / "scripts" / "private-allowlist.txt").write_text("PLAN.md\tno longer present\n")
    r = run(repo)
    assert r.returncode == 1 and "STALE allowlist entry for PLAN.md" in r.stdout


def test_a_scan_of_nothing_is_an_error_not_a_pass(tmp_path):
    (tmp_path / "scripts").mkdir()
    shutil.copy2(GATE, tmp_path / "scripts" / "check-no-private.sh")
    subprocess.run(["git", "init", "-q", str(tmp_path)], check=True)
    (tmp_path / ".gitignore").write_text("*\n")
    r = run(tmp_path)
    assert r.returncode == 2, r.stdout + r.stderr


def test_the_gate_does_not_match_itself():
    r = subprocess.run(["bash", str(GATE), str(GATE)], capture_output=True, text=True)
    assert r.returncode == 0, r.stdout


def test_this_repository_is_clean():
    r = subprocess.run(["bash", str(GATE)], capture_output=True, text=True)
    assert r.returncode == 0, r.stdout + r.stderr
