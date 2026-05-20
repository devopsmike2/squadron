# Deploying Squadron on OpenShift

Squadron ships first-class support for OpenShift 4.x. The image
is built to be compatible with the `restricted-v2` SCC (random
UID, non-root, no privileged ports), and a Kustomize manifest
set lives at `deploy/openshift/`.

This page walks through the deployment end-to-end on a typical
enterprise OpenShift cluster.

## Prerequisites

- OpenShift 4.x (tested on 4.21+).
- Project-edit permissions on the target project. Cluster-admin
  is not required.
- A StorageClass that supports `ReadWriteOnce` PVCs.
- Outbound HTTPS to `api.anthropic.com` if you want AI features
  on. Skipping AI is fine; everything else works without it.

## Step 1 — Mirror the image into your cluster's registry

Most enterprise OpenShift clusters can't pull from public
registries directly. Mirror the Squadron image into the
cluster's internal registry:

```bash
oc registry login
docker pull ghcr.io/devopsmike2/squadron:v0.30.0
docker tag ghcr.io/devopsmike2/squadron:v0.30.0 \
  image-registry.openshift-image-registry.svc:5000/<your-project>/squadron:v0.30.0
docker push \
  image-registry.openshift-image-registry.svc:5000/<your-project>/squadron:v0.30.0
```

Alternative: use an OpenShift BuildConfig to build directly from
the GitHub repo. `oc new-build` with the source URL works.

## Step 2 — Copy the example overlay

```bash
cp -r deploy/openshift/overlays/example deploy/openshift/overlays/<your-env>
```

Edit `deploy/openshift/overlays/<your-env>/kustomization.yaml`
and change three things:

1. `namespace:` — your project name.
2. `images:` — `newName` to point at your mirrored image.
3. The two Route hostname patches and the `BACKEND_URL` env var
   patch — set to a hostname under your cluster's apps domain.

To find your cluster's apps domain, run `oc get route` in any
project you have access to and look at the hostname suffix.
It'll look like `*.apps.<cluster-name>.<organization>`.

## Step 3 — Apply

```bash
oc apply -k deploy/openshift/overlays/<your-env>
```

This creates:

- `Deployment/squadron` — the application pod.
- `Service/squadron` — internal ClusterIP for in-cluster
  collectors and the Route to find the pod.
- `Route/squadron` — external HTTPS access to the UI and API.
- `PersistentVolumeClaim/squadron-data` — 10 GiB of SQLite +
  DuckDB state.
- `ConfigMap/squadron-config` — the `squadron.yaml`.
- `Secret/squadron-secrets` — empty by default; populate via
  `oc set data secret/squadron-secrets ANTHROPIC_API_KEY=...`.

Watch the rollout:

```bash
oc rollout status deployment/squadron
oc logs deployment/squadron -f
```

If the pod stays in `ContainerCreating` for more than a minute,
check the PVC:

```bash
oc get pvc squadron-data
oc describe pvc squadron-data
```

If the pod CrashLoopBackoffs, that's almost always a UID/volume
permission issue — see the troubleshooting section below.

## Step 4 — Get the bootstrap token

On first start Squadron logs an auto-generated admin token. Pull
it from the pod logs:

```bash
oc logs deployment/squadron | grep -i bootstrap
```

You'll see something like:

```
"bootstrap_token":"sq_a1b2c3d4..."
```

Use this token in the `Authorization: Bearer <token>` header
when calling the API, or paste it into the UI login screen.

To skip the auto-generation and use your own token, populate the
`BOOTSTRAP_TOKEN` field in the Secret before first start:

```bash
oc set data secret/squadron-secrets BOOTSTRAP_TOKEN=sq_your_token_here
oc rollout restart deployment/squadron
```

## Step 5 — Point collectors at Squadron

Open the Squadron UI at your Route hostname. The Quickstart
wizard at `/quickstart` walks through both fresh-install
(generates a complete collector config) and adoption (gives you
the snippet to paste into existing configs).

The OpAMP endpoint your collectors need:

- **Same-cluster collectors**:
  `ws://squadron.<your-project>.svc.cluster.local:4320/v1/opamp`
- **Cross-cluster / external collectors**: see the
  `external-collectors` overlay (separate Route for the OpAMP
  port with WebSocket-friendly annotations).

## Troubleshooting

### Pod CrashLoopBackoff with "permission denied"

This is the most common failure on OpenShift. The image's
`restricted-v2` compatibility relies on group 0 ownership of
`/app/`. If you're using an older base image or built Squadron
locally without the v0.30 Dockerfile changes, the fix is to
rebuild from `main` (the Dockerfile now does `chown -R 1001:0`
and `chmod -R g=u`).

Verify the image you're running has the fix:

```bash
oc exec deployment/squadron -- ls -la /app | head -5
```

You should see ownership `1001 root` (or `1001 0`) with group
read+write+exec on directories.

### Pod OOMKilled during startup

DuckDB needs ~256 MiB of headroom for initialization. The
default deployment requests 512 MiB / limits 2 GiB. If your
cluster has tighter quota that's overriding the limit, bump it
in your overlay:

```yaml
patches:
  - target:
      kind: Deployment
      name: squadron
    patch: |
      - op: replace
        path: /spec/template/spec/containers/0/resources/limits/memory
        value: 4Gi
```

### NFS PVC won't bind or has permission errors

NFS-backed PVCs and OpenShift's random-UID model interact
poorly. Two paths:

1. Switch to a block-storage StorageClass (the example overlay
   uses `powerstore-xfs-retain`). This is the preferred fix.
2. If you must use NFS, set `supplementalGroups` to match your
   NFS export's group ID — check with your storage team.

   ```yaml
   - op: add
     path: /spec/template/spec/securityContext/supplementalGroups
     value: [65534]   # whatever your NFS export uses
   ```

### Route hostname conflicts with existing Lawrence deployment

If you're running both Lawrence and Squadron side-by-side in the
same project (for migration), name the Squadron Route hostname
differently than the Lawrence one. Service names are namespaced
so they don't conflict; Routes are too, but cluster-wide
hostname uniqueness is enforced.

## Migrating from Lawrence

Lawrence is the OSS project Squadron was forked from. Squadron
added: drift detection, alerts, audit log, staged rollouts, AI
assist, cost insights, recommendations, savings dashboard, and
cost-spike alerting. The application store schema is
backwards-compatible for the tables Lawrence had; Squadron
auto-creates the new tables on first start.

**Side-by-side first, cut over once stable.** Deploy Squadron
alongside Lawrence in the same project under different Service /
Route / PVC names. Point a few test collectors at Squadron, verify
behavior, then update the rest of the fleet's OpAMP server URL
config and decommission Lawrence. Don't migrate data — fresh
fleet inventory builds up quickly from collector check-ins.

## See also

- `docs/getting-started.md` — single-node Docker quickstart.
- `docs/auth.md` — bootstrap token + scopes + token expiry.
- `docs/scale-testing.md` — what to expect performance-wise.
- `docs/cost-spikes.md` — v0.29 cost-spike alerting (extra config
  needed in your `squadron.yaml`).
