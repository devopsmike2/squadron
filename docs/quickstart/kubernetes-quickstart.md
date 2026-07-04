# Quickstart: Squadron OSS on Kubernetes

Deploy Squadron OSS to a Kubernetes cluster with Helm in about 30 minutes. This
is the production path for teams already running Kubernetes.

Squadron OSS is **single-instance**: it uses an embedded store (SQLite +
DuckDB) on a PersistentVolume, so `replicaCount` is fixed at 1. Multi-replica
HA needs Postgres, which is a commercial-tier concern — don't try to scale the
Deployment horizontally on OSS.

## What the chart deploys

The chart at `deploy/helm/squadron` creates:

- A single-replica Deployment (`ghcr.io/devopsmike2/squadron`).
- A PersistentVolumeClaim for `/app/data` (SQLite stores + the auto-generated
  `secrets.key`).
- A Service exposing UI/API (**8080**), OpAMP (**4320**), and OTLP
  (**4317/4318**).
- Optional Ingress for the UI/API, and an optional Secret for sensitive env.

## Before you start

- A Kubernetes cluster and `kubectl` context, plus Helm 3.
- A default StorageClass (or set `persistence.storageClass`).
- Optional: an `ANTHROPIC_API_KEY` for AI features.

## 1. Install with Helm

From a checkout of the repo:

```bash
helm install squadron ./deploy/helm/squadron \
  --namespace squadron --create-namespace
```

That's enough to get a running control plane. To pin a version, enable AI, and
expose the UI via Ingress:

```bash
helm install squadron ./deploy/helm/squadron \
  --namespace squadron --create-namespace \
  --set image.tag=v0.89.292 \
  --set secrets.anthropicApiKey=sk-ant-... \
  --set ingress.enabled=true \
  --set ingress.hosts[0].host=squadron.example.com
```

### Manage secrets yourself (recommended)

Rather than passing keys on the command line, create a Secret and reference it
with `secrets.existingSecret`. The keys the chart expects:

| Secret key                     | Purpose                              |
|--------------------------------|--------------------------------------|
| `ANTHROPIC_API_KEY`            | Enables AI features                  |
| `SQUADRON_SECRETS_KEY`         | Encryption key (auto-generated if unset) |
| `SQUADRON_GITHUB_WEBHOOK_SECRET` | GitHub webhook verification        |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` | Cloud discovery creds |

```bash
kubectl -n squadron create secret generic squadron-secrets \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-...

helm install squadron ./deploy/helm/squadron \
  --namespace squadron --create-namespace \
  --set secrets.existingSecret=squadron-secrets
```

## 2. Key values to know

Defaults from the chart's `values.yaml`:

- `persistence.enabled: true`, `persistence.size: 5Gi` — the data dir. Set
  `persistence.storageClass` if your cluster has no default.
- `service.type: ClusterIP` — the UI/API stays in-cluster unless you add
  Ingress or change the type.
- `resources.requests: 250m CPU / 512Mi`, `limits: 2Gi` memory — comfortable
  for hundreds of collectors.
- Health probes hit `/health` on the HTTP port.
- Keep `replicaCount` at its default of **1**.

## 3. Exposing OpAMP and OTLP to out-of-cluster collectors

The Ingress in the chart covers the **UI/API (8080) only**. If your collectors
live outside the cluster, they need to reach OpAMP (4320) and OTLP (4317/4318)
too. Two things matter:

1. **Expose those ports separately** — a `LoadBalancer` Service or a
   gRPC-capable Ingress — since the chart's HTTP Ingress won't route them.
2. **Give OpAMP a long timeout.** OpAMP is a WebSocket the collector holds open
   indefinitely. If it goes through an nginx Ingress, raise the timeouts to the
   hour range so collectors don't thrash reconnecting every 60 seconds:

   ```yaml
   nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
   nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
   ```

## 4. Verify and connect a collector

```bash
kubectl -n squadron get pods
kubectl -n squadron port-forward svc/squadron 8080:8080
# then open http://localhost:8080
```

Point an OpenTelemetry collector at the OpAMP endpoint
(`ws://<squadron-host>:4320/v1/opamp`) with OTLP export to `:4317`. Within a few
seconds it shows up as **online** on the Agents page. The minimal collector
config is in `docs/getting-started.md`.

## Before you call it "production"

- Auth on (`auth.require_token: true`) and the bootstrap token rotated.
- TLS on the Ingress for the UI, and on OpAMP/OTLP if they cross untrusted
  networks.
- Tokens scoped narrowly — read-only for dashboards, write only for humans/CI.
- Back up the PersistentVolume (snapshot the SQLite data dir); restore-drill once.
- A notification channel wired for silent-agent and rollout alerts.

## Where to go next

- `docs/deployment.md` — full deployment reference, raw manifests, and the
  production checklist.
- `docs/getting-started.md` — collector config and config concepts.
- `docs/oss-vs-enterprise.md` — what's OSS vs commercial-tier (HA, SSO,
  compliance retention, etc.).
- `docs/operating.md` — upgrades, backup, and restore.
