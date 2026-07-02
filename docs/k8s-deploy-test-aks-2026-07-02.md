# Squadron OSS — real Kubernetes deployment test (AKS)

**Date:** 2026-07-02 · **Platform:** Azure Kubernetes Service, k8s 1.35.5, 2× Standard_D2s_v7, region eastus
**Image:** `ghcr.io/devopsmike2/squadron:v0.89.380` · **Chart:** `deploy/helm/squadron` (0.1.0 → **0.1.2** after fix)
**Result:** PASS (after one chart fix). Deploy → PVC → health → LoadBalancer → OTel collector → Fleet all verified end-to-end.

## Why a real cluster

Local `kind`/CRC validate the chart's templating but not the parts that only a real
managed cluster exercises: a real CSI-provisioned PVC (ownership/permissions), a
cloud LoadBalancer, image pull from ghcr over the internet, and multi-node
scheduling. This test used AKS to hit all of those.

## What was tested

1. **Provision** — `az aks create` (2 nodes, managed-disk default StorageClass).
2. **Deploy** — `helm install squadron ./deploy/helm/squadron --set image.tag=v0.89.380 --set service.type=LoadBalancer`.
3. **Persistence** — 5Gi RWO PVC dynamically provisioned by `disk.csi.azure.com`, bound to the pod.
4. **Reachability** — `/health` → 200 (`duckdb: healthy, sqlite: healthy`) and the UI served through a real Azure public IP.
5. **End-to-end** — an in-cluster OTel collector (hostmetrics → OTLP) shipped telemetry to the in-cluster Squadron service; the agent registered in **Fleet** (`aks-e2e-collector`, online) and telemetry landed durably (150 metric items, **0 dropped**).

## Bug found and fixed (chart)

**Symptom:** with `persistence.enabled=true` the pod crash-looped:
`failed to enable WAL mode: unable to open database file: no such file or directory`.

**Root cause:** the image runs as non-root **UID 1001**, but a freshly-provisioned
PVC (Azure Disk / EBS / GCE PD) mounts `/app/data` as **root:root 0755** — no group
write — so UID 1001 can't create `./data/app.db`. The chart shipped an empty
`podSecurityContext` (no `fsGroup`), so nothing made the volume writable. Confirmed
with a debug pod: `ls -ld /app/data` → `drwxr-xr-x root root`.

**Fix (commit efd7668, chart 0.1.2):** default `podSecurityContext.fsGroup: 1001`
+ `fsGroupChangePolicy: OnRootMismatch`, plus a hardened container `securityContext`
(runAsNonRoot, runAsUser 1001, runAsGroup 0, drop ALL). The kubelet then chowns the
volume so 1001 can write. Re-verified from **chart defaults alone** (no `--set`
overrides): pod healthy, `/health` 200.

**OpenShift note:** the separate `deploy/openshift/` kustomize base is unaffected.
Helm-on-OpenShift users should clear `podSecurityContext` and let the restricted-v2
SCC assign fsGroup/UID from the namespace range (a hardcoded fsGroup can be rejected
by the SCC). This is why local docker-compose never caught it — bind mounts inherit
host perms; only a real CSI PVC exposes the ownership gap.

## Notes / not-yet-tested

- AI features off (no `ANTHROPIC_API_KEY`) and API auth off — both expected defaults; the NOTES.txt and startup logs call this out.
- Exposing all four service ports (8080/4320/4317/4318) via one LoadBalancer is fine for a test; in production split UI (ingress) from OpAMP/OTLP (LB/gRPC), as the chart NOTES already advise.
- OpenShift path (kustomize base) not exercised in this pass — recommend a follow-up on OpenShift Local/ROSA if that's a target.
