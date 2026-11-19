"""`helm template` assertions for deploy/helm/promptshipper.

Run from anywhere:  python -m pytest -q deploy/helm/tests
Needs `helm` on PATH and PyYAML. CI also pipes every render through kubeconform
(.github/workflows/chart.yml); these tests pin the chart's CONTRACT — what an
existing install relies on to keep its data — not schema validity.

Fixture values are pairwise distinct and distinct from every chart default, so
a template that hardcodes a default (or swaps two components' values) cannot
pass: e.g. the GPU count is 3, never the default 1.
"""

from __future__ import annotations

import os
import shutil
import subprocess

import pytest
import yaml

CHART = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "promptshipper")
TOKEN_STT = "stt-inline-token-aaaaaaaaaaaaaaaa"
TOKEN_CLIP = "clip-inline-token-bbbbbbbbbbbbbbbb"

if shutil.which("helm") is None:  # fail loudly, never skip
    raise RuntimeError("helm is not on PATH; these tests render the chart with it")


def render(*sets: str, release: str = "rel", expect_fail: bool = False):
    cmd = ["helm", "template", release, CHART, "--namespace", "ns"]
    for s in sets:
        cmd += ["--set", s]
    p = subprocess.run(cmd, capture_output=True, text=True)
    if expect_fail:
        assert p.returncode != 0, f"render unexpectedly succeeded:\n{p.stdout}"
        return p.stderr
    assert p.returncode == 0, p.stderr
    return [d for d in yaml.safe_load_all(p.stdout) if d]


def inline_tokens():
    return (f"stt.auth.token={TOKEN_STT}", f"clip.auth.token={TOKEN_CLIP}")


def existing_secrets():
    return (
        "stt.auth.existingSecret=legacy-stt-secret",
        "stt.auth.tokenKey=token",
        "stt.auth.tokensKey=tokens",
        "clip.auth.existingSecret=legacy-clip-secret",
        "clip.auth.tokenKey=the-clip-key",
    )


def of_kind(docs, kind):
    return [d for d in docs if d["kind"] == kind]


def one(docs, kind, component):
    hits = [d for d in of_kind(docs, kind) if d["metadata"]["labels"]["app.kubernetes.io/component"] == component]
    assert len(hits) == 1, f"expected exactly one {component} {kind}, got {len(hits)}"
    return hits[0]


def pod(docs, component):
    return one(docs, "Deployment", component)["spec"]["template"]["spec"]


def container(docs, component):
    (c,) = pod(docs, component)["containers"]
    return c


def env(docs, component):
    return {e["name"]: e for e in container(docs, component)["env"]}


def volume(docs, component, name):
    (v,) = [v for v in pod(docs, component)["volumes"] if v["name"] == name]
    return v


# --- tokens -----------------------------------------------------------------


def test_no_token_refuses_to_render_for_either_component():
    err = render(expect_fail=True)
    assert "stt.auth.existingSecret" in err
    err = render("stt.enabled=false", expect_fail=True)
    assert "clip.auth.existingSecret" in err


def test_short_inline_clip_token_refuses_to_render():
    err = render("stt.enabled=false", "clip.auth.token=only-23-bytes-long-xxxx", expect_fail=True)
    assert "at least 24 bytes" in err


def test_inline_tokens_create_one_secret_per_component_and_reference_it():
    docs = render(*inline_tokens())
    stt_secret = one(docs, "Secret", "stt")
    clip_secret = one(docs, "Secret", "clip")
    assert stt_secret["stringData"] == {"STT_API_TOKEN": TOKEN_STT}
    assert clip_secret["stringData"] == {"CLIPSTORE_TOKEN": TOKEN_CLIP}
    ref = env(docs, "stt")["STT_API_TOKEN"]["valueFrom"]["secretKeyRef"]
    assert ref == {"name": stt_secret["metadata"]["name"], "key": "STT_API_TOKEN"}
    ref = env(docs, "clip")["CLIPSTORE_TOKEN"]["valueFrom"]["secretKeyRef"]
    assert ref == {"name": clip_secret["metadata"]["name"], "key": "CLIPSTORE_TOKEN"}


def test_existing_secret_renders_no_secret_and_uses_its_name_and_keys():
    docs = render(*existing_secrets())
    assert of_kind(docs, "Secret") == []
    e = env(docs, "stt")
    assert e["STT_API_TOKEN"]["valueFrom"]["secretKeyRef"] == {"name": "legacy-stt-secret", "key": "token"}
    assert e["STT_API_TOKENS"]["valueFrom"]["secretKeyRef"] == {
        "name": "legacy-stt-secret",
        "key": "tokens",
        "optional": True,
    }
    assert env(docs, "clip")["CLIPSTORE_TOKEN"]["valueFrom"]["secretKeyRef"] == {
        "name": "legacy-clip-secret",
        "key": "the-clip-key",
    }
    # No checksum of a secret the chart does not own.
    for comp in ("stt", "clip"):
        ann = one(docs, "Deployment", comp)["spec"]["template"]["metadata"].get("annotations") or {}
        assert "checksum/auth" not in ann


# --- persistence --------------------------------------------------------------


def test_default_creates_claims_and_the_clip_claim_survives_uninstall():
    docs = render(*inline_tokens())
    clip_pvc = one(docs, "PersistentVolumeClaim", "clip")
    stt_pvc = one(docs, "PersistentVolumeClaim", "stt")
    assert clip_pvc["metadata"]["annotations"]["helm.sh/resource-policy"] == "keep"
    assert volume(docs, "clip", "data")["persistentVolumeClaim"]["claimName"] == clip_pvc["metadata"]["name"]
    assert volume(docs, "stt", "models")["persistentVolumeClaim"]["claimName"] == stt_pvc["metadata"]["name"]


def test_existing_claim_renders_no_pvc_and_mounts_that_claim():
    docs = render(
        *inline_tokens(),
        "clip.persistence.existingClaim=old-clip-data",
        "stt.persistence.existingClaim=old-model-cache",
    )
    assert of_kind(docs, "PersistentVolumeClaim") == []
    assert volume(docs, "clip", "data") == {"name": "data", "persistentVolumeClaim": {"claimName": "old-clip-data"}}
    assert volume(docs, "stt", "models") == {
        "name": "models",
        "persistentVolumeClaim": {"claimName": "old-model-cache"},
    }
    mounts = {m["name"]: m["mountPath"] for m in container(docs, "clip")["volumeMounts"]}
    assert mounts["data"] == "/data"
    assert container(docs, "clip")["args"] == ["-addr", ":8139", "-db", "/data/clipstore.db"]


def test_existing_claim_on_one_component_leaves_the_other_chart_managed():
    docs = render(*inline_tokens(), "clip.persistence.existingClaim=old-clip-data")
    assert [d["metadata"]["labels"]["app.kubernetes.io/component"] for d in of_kind(docs, "PersistentVolumeClaim")] == [
        "stt"
    ]


def test_storage_class_and_size_reach_the_created_claim():
    docs = render(*inline_tokens(), "clip.persistence.storageClass=fast-ssd", "clip.persistence.size=7Gi")
    spec = one(docs, "PersistentVolumeClaim", "clip")["spec"]
    assert spec["storageClassName"] == "fast-ssd"
    assert spec["resources"]["requests"]["storage"] == "7Gi"


# --- GPU ------------------------------------------------------------------------


def test_gpu_off_by_default():
    docs = render(*inline_tokens())
    p = pod(docs, "stt")
    assert "runtimeClassName" not in p
    res = container(docs, "stt")["resources"]
    assert "nvidia.com/gpu" not in res.get("limits", {})
    assert "nvidia.com/gpu" not in res.get("requests", {})


def test_gpu_on_sets_runtime_class_and_gpu_request_and_limit():
    docs = render(*inline_tokens(), "stt.gpu.enabled=true", "stt.gpu.count=3", "stt.gpu.runtimeClassName=gpu-rc")
    assert pod(docs, "stt")["runtimeClassName"] == "gpu-rc"
    res = container(docs, "stt")["resources"]
    assert res["limits"]["nvidia.com/gpu"] == 3
    assert res["requests"]["nvidia.com/gpu"] == 3
    # The rest of the resources survive the merge.
    assert res["limits"]["memory"] == "4Gi"
    # GPU never leaks onto clip.
    assert "runtimeClassName" not in pod(docs, "clip")


# --- clip is a single SQLite writer ----------------------------------------------


def test_clip_is_one_replica_with_recreate_whatever_the_values_say():
    docs = render(*inline_tokens(), "clip.replicas=4", "clip.strategy.type=RollingUpdate")
    spec = one(docs, "Deployment", "clip")["spec"]
    assert spec["replicas"] == 1
    assert spec["strategy"] == {"type": "Recreate"}


# --- probes, images, services, toggles -------------------------------------------


@pytest.mark.parametrize("component", ["stt", "clip"])
def test_probes_hit_unauthenticated_healthz(component):
    c = container(render(*inline_tokens()), component)
    for probe in ("startupProbe", "livenessProbe", "readinessProbe"):
        assert c[probe]["httpGet"] == {"path": "/healthz", "port": "http"}


def test_image_tag_defaults_to_app_version_and_digest_wins():
    docs = render(*inline_tokens())
    assert container(docs, "stt")["image"] == "ghcr.io/zacxdev/promptshipper-stt:0.1.0"
    assert container(docs, "clip")["image"] == "ghcr.io/zacxdev/promptshipper-clip:0.1.0"
    docs = render(*inline_tokens(), "clip.image.tag=9.9.9", "stt.image.digest=sha256:" + "a" * 64)
    assert container(docs, "clip")["image"] == "ghcr.io/zacxdev/promptshipper-clip:9.9.9"
    assert container(docs, "stt")["image"] == "ghcr.io/zacxdev/promptshipper-stt@sha256:" + "a" * 64


def test_service_name_and_port_overrides_keep_an_existing_dns_name():
    docs = render(
        *inline_tokens(),
        "stt.service.name=legacy-stt",
        "stt.service.port=8001",
        "clip.service.name=legacy-clip",
        "clip.service.port=8140",
    )
    stt = one(docs, "Service", "stt")
    clip = one(docs, "Service", "clip")
    assert (stt["metadata"]["name"], stt["spec"]["ports"][0]["port"]) == ("legacy-stt", 8001)
    assert (clip["metadata"]["name"], clip["spec"]["ports"][0]["port"]) == ("legacy-clip", 8140)
    # The selector must still pick only its own component's pods.
    assert stt["spec"]["selector"]["app.kubernetes.io/component"] == "stt"
    assert clip["spec"]["selector"]["app.kubernetes.io/component"] == "clip"


@pytest.mark.parametrize("off,on", [("stt", "clip"), ("clip", "stt")])
def test_each_component_toggles_off_independently(off, on):
    tokens = {"stt": f"stt.auth.token={TOKEN_STT}", "clip": f"clip.auth.token={TOKEN_CLIP}"}
    docs = render(f"{off}.enabled=false", tokens[on])
    comps = {d["metadata"]["labels"]["app.kubernetes.io/component"] for d in docs}
    assert comps == {on}


def test_snippets_existing_configmap_is_mounted_at_etc_snip():
    docs = render(*inline_tokens(), "stt.snippets.existingConfigMap=my-catalog")
    assert volume(docs, "stt", "snippets")["configMap"]["name"] == "my-catalog"
    mounts = {m["name"]: m for m in container(docs, "stt")["volumeMounts"]}
    assert mounts["snippets"]["mountPath"] == "/etc/snip"
    assert of_kind(docs, "ConfigMap") == []


def test_two_snippet_sources_refuse_to_render():
    err = render(
        *inline_tokens(),
        "stt.snippets.existingConfigMap=a",
        "stt.snippets.existingSecret=b",
        expect_fail=True,
    )
    assert "at most one" in err


def test_network_policy_restricts_each_component_to_its_own_pods_and_port():
    docs = render(
        *inline_tokens(),
        "networkPolicy.enabled=true",
        "networkPolicy.clip.from[0].podSelector.matchLabels.role=reader",
    )
    clip = one(docs, "NetworkPolicy", "clip")
    stt = one(docs, "NetworkPolicy", "stt")
    assert clip["spec"]["podSelector"]["matchLabels"]["app.kubernetes.io/component"] == "clip"
    assert clip["spec"]["ingress"] == [
        {"ports": [{"port": "http", "protocol": "TCP"}], "from": [{"podSelector": {"matchLabels": {"role": "reader"}}}]}
    ]
    assert stt["spec"]["ingress"] == [{"ports": [{"port": "http", "protocol": "TCP"}]}]


def test_ingress_points_at_the_component_service():
    docs = render(
        *inline_tokens(),
        "clip.service.name=legacy-clip",
        "clip.ingress.enabled=true",
        "clip.ingress.hosts[0].host=clip.example.com",
    )
    (ing,) = of_kind(docs, "Ingress")
    backend = ing["spec"]["rules"][0]["http"]["paths"][0]["backend"]["service"]
    assert backend == {"name": "legacy-clip", "port": {"number": 8139}}
