# Helm chart

`promptshipper/` deploys the two servers to Kubernetes. Each component
(`stt`, `clip`) is toggled with `<component>.enabled`, so you can run both in
one release or one per release (for example one per namespace).

```sh
helm install ps oci://ghcr.io/zacxdev/charts/promptshipper --version 0.1.0 \
  --namespace promptshipper --create-namespace \
  --set stt.auth.existingSecret=promptshipper-tokens \
  --set clip.auth.existingSecret=promptshipper-tokens
```

The chart is published to GHCR from every `v*` tag by
`.github/workflows/release.yml`, with the same version as the images, and only
after every image built and pushed.

🔴 **Neither server has TLS of its own.** Keep the Services cluster-internal or
on a private network. If you enable an Ingress, terminate TLS on it.

## Tokens

Rendering **fails** unless each enabled component has a token source — the
chart never deploys an open service.

| | values | Secret key (default) |
|---|---|---|
| stt bearer token (≥ 24 bytes inline) | `stt.auth.existingSecret` | `stt.auth.tokenKey` = `STT_API_TOKEN` |
| stt named extra tokens (optional key) | same Secret | `stt.auth.tokensKey` = `STT_API_TOKENS` |
| clip bearer token (≥ 24 bytes) | `clip.auth.existingSecret` | `clip.auth.tokenKey` = `CLIPSTORE_TOKEN` |

```sh
kubectl -n promptshipper create secret generic promptshipper-tokens \
  --from-literal=STT_API_TOKEN="$(openssl rand -hex 32)" \
  --from-literal=CLIPSTORE_TOKEN="$(openssl rand -hex 32)"
```

🔴 **The `tokenKey` must exist in the Secret.** It is mounted as a required
key, so a missing key leaves the pod in `CreateContainerConfigError` rather than
starting without a token. Only `stt.auth.tokensKey` is optional.

With `stt.enabled`, the clip pod also reads the STT Secret's `stt.auth.tokenKey`
(as `CLIPSTORE_REFUSE_TOKENS`, `optional: true`) so it refuses to store a clip
containing the STT token — see `server/clip/README.md`. It is a `secretKeyRef`
only; the value never appears outside the Secret. Changing the STT token
therefore means restarting **both** components. Because the chart owns that
variable, a render with `clip.extraEnv` naming `CLIPSTORE_REFUSE_TOKENS` while
`stt.enabled` **fails** rather than letting a duplicate silently replace it.

`<component>.auth.token` (at least 24 bytes) puts an inline token into a
chart-managed Secret instead. That writes it into your values and Helm release
history, so prefer `existingSecret`.

**Changing a token does not restart the pod by itself.** The chart deliberately
puts no checksum of the token in the pod template, because anyone who can
`get pods` could read it. After changing a token, either inline or in your own
Secret, restart the component:
`kubectl rollout restart deploy/<fullname>-<stt|clip>` (`<fullname>` is `<release>-promptshipper` unless overridden).

## Values that matter

| value | default | |
|---|---|---|
| `stt.backend` | `faster-whisper` | `faster-whisper` \| `openai` \| `fake` \| `cohere` (see `server/stt/README.md`) |
| `stt.model` | backend default (`small`) | `base`/`tiny` for small nodes |
| `stt.env` | `{}` | any other `STT_*` variable, name → value |
| `stt.persistence.*` | 10Gi RWO PVC | the model cache (`HF_HOME` = `<mountPath>/hf`). `existingClaim` to reuse one |
| `stt.snippets.existingConfigMap` / `existingSecret` / `content` | none | the snippet catalog, mounted as a directory at `/etc/snip`; key `snippets.json` |
| `stt.gpu.enabled` | `false` | sets `runtimeClassName` (`stt.gpu.runtimeClassName`, default `nvidia`) and `nvidia.com/gpu: <count>` as request and limit |
| `clip.persistence.*` | 1Gi RWO PVC, `helm.sh/resource-policy: keep` | the SQLite clipboard. `existingClaim` to reuse one |
| `clip.dbFile` | `clipstore.db` | file name inside the volume (`/data/<dbFile>`) |
| `<component>.service.name` / `.port` | `<fullname>-<component>`, 8000 / 8139 | set to keep an existing in-cluster DNS name |
| `<component>.ingress.*` | off | a plain `networking.k8s.io/v1` Ingress |
| `<component>.nodeSelector`, `.affinity`, `.tolerations`, `.resources`, `.priorityClassName` | — | as usual |
| `networkPolicy.enabled` | `false` | one ingress policy per component, limited to its port; `networkPolicy.<component>.from` lists the allowed peers and must be non-empty for every enabled component (rendering fails otherwise: an empty list would isolate nothing). With an Ingress enabled, include the ingress controller's pods/namespace in `from`, or the Ingress cannot reach the Service |

`clip` is always **one replica with the `Recreate` strategy**: it is a single
SQLite writer and two processes must never open one database file. There is no
value to change that.

GPU: the published `promptshipper-stt` image is CPU-only. To run on a GPU,
build `server/stt/Dockerfile.cohere` (not published yet), point
`stt.image.repository` at it, and set `stt.backend=cohere` and
`stt.gpu.enabled=true`.

## Adopting an existing install without losing data

If you already run the servers from your own manifests, the chart can take
over **without copying any data**: install it into the namespace that holds the
volume and Secret, and point it at them. Run one release per namespace if the
two servers live in different namespaces.

```yaml
# values-existing-clip.yaml — release in the namespace that holds the clipboard
stt:
  enabled: false
clip:
  image:
    tag: "0.1.0"              # or pin: digest: sha256:...
  auth:
    existingSecret: clip-auth # your Secret
    tokenKey: CLIPSTORE_TOKEN # the key inside it
  persistence:
    existingClaim: clip-data  # your PVC — the chart creates none
  dbFile: clipstore.db        # the file already in that volume
  service:
    name: clip                # keep the DNS name your clients use
    port: 8139
  nodeSelector: {}            # where the volume is, for node-local storage
```

```yaml
# values-existing-stt.yaml — release in the namespace that holds the model cache
clip:
  enabled: false
stt:
  auth:
    existingSecret: stt-auth
    tokenKey: token           # if your Secret uses other key names
    tokensKey: tokens
  persistence:
    existingClaim: stt-models # keeps the downloaded model: no re-download
  snippets:
    existingConfigMap: snippet-catalog
  service:
    name: stt
    port: 8000
```

Order of operations, so that **two clip processes never open the database at
the same time**:

1. Back the database up (`sqlite3 clipstore.db ".backup …"` from a pod that
   mounts the volume, or your existing backup job). Make sure the PV's
   `reclaimPolicy` is `Retain`.
2. Make sure nothing will delete the PVC or Secret when you stop managing the
   old Deployment — e.g. a GitOps tool that prunes objects removed from its
   source. Keep the PVC and Secret where they are; only the Deployment and
   Service move to the chart.
3. Delete the old Deployment and Service, and wait until the old pod is gone
   (`kubectl get pods` shows none). The chart's Service would otherwise collide
   with the old one, and Helm refuses to take over an object it did not create.
4. `helm install` with the values above.
5. Verify: `/healthz` answers, an authenticated `GET /v1/clips?limit=1` returns
   your existing newest clip, and the old row count is unchanged.

Rollback: `helm uninstall` (the PVC is yours, so it stays), then re-apply your
old manifests.

## Tests

```sh
python -m pytest -q deploy/helm/tests     # needs helm and PyYAML
```

CI (`.github/workflows/chart.yml`, on pull requests and main) runs those, `helm lint --strict`, and
`kubeconform -strict` over several renders on every pull request.
