# Kubernetes tier slice 2 — GCP GKE + Azure AKS + OCI OKE

**Status:** design doc, locked for slice 2 implementation across
three clouds in a fan-out arc, mirroring the database tier slice
2 pattern (v0.89.63 through v0.89.67). AWS EKS already shipped
at slice 1 in v0.89.0 with a composite two-axis rule (control
plane logging on api+audit AND an active ADOT or
CloudWatch-observability addon). This arc closes the parallel
gap on the remaining three clouds.

After this arc closes, the universal claim becomes "Squadron
scans AWS, GCP, Azure, AND Oracle Cloud for COMPUTE, DATABASE,
AND KUBERNETES observability gaps across the full fleet." Three
tiers across four clouds. Twelve scanner surfaces. Same
recommendation pipeline.

**See also:**
[AWS EKS slice 1 (#599)](./558-discovery-design.md),
[Database tier slice 2](./database-tier-slice2.md),
[GCP discovery slice 1](./gcp-discovery-slice1.md),
[Azure discovery slice 1](./azure-discovery-slice1.md),
[OCI discovery slice 1](./oci-discovery-slice1.md).

## 1. Problem

Operators with multi-cloud Kubernetes fleets see Squadron flag
AWS EKS clusters without ADOT but get no signal on their GCP
GKE, Azure AKS, or OCI OKE clusters. The Kubernetes
recommendation surface is structurally asymmetric across
providers: AWS detected, the rest invisible.

For an enterprise running cross-cloud Kubernetes workloads, the
asymmetry has the same shape as the database tier gap before
slice 2 closed it. The four-cloud breadth claim is materially
weakened when Squadron only sees one cloud's Kubernetes
inventory.

Slice 2 of the Kubernetes tier closes this asymmetry with
parallel scanners for GKE / AKS / OKE, each adapted to that
cloud's native managed observability primitive at the
cluster level.

## 2. Non-goals (slice 2)

- **Workload-level detection.** Whether individual pods,
  deployments, or daemonsets have OTel sidecars or auto-
  instrumentation is slice 3. Slice 2 detects ONLY at the
  cluster level (the managed observability addon / primitive).
- **AWS EKS expansion.** EKS slice 1's two-axis rule is
  sufficient for this slice. Adding per-nodegroup detection,
  Fargate-profile-level recommendations, or non-managed addon
  detection (operator-installed OTel collector) is slice 3.
- **GKE Autopilot vs Standard differentiation.** Slice 2 treats
  both as "GKE clusters" for the detection rule. Slice 3 may
  add Autopilot-specific signals.
- **AKS Virtual Nodes.** ACI-backed virtual nodes have their
  own observability surface. Slice 3.
- **OKE Virtual Nodes / Self-managed nodes.** OKE supports
  multiple node provisioning models; slice 2 detects at the
  cluster level regardless.
- **Cross-cluster mesh / service mesh observability.** Istio
  / Linkerd / Cilium detection is slice 4+ (requires
  Kubernetes API access which slice 2 does not have).
- **Pod-level OTel sidecar detection.** Slice 3+. Requires
  the cluster's Kubernetes API, not just the cloud control
  plane.

## 3. Architectural decision: per-cloud detection rules

Each cloud exposes a different managed observability primitive
for Kubernetes. We map each to a single canonical "instrumented
vs uninstrumented" axis for slice 2 (multi-axis composite
rules are slice 3, parallel to the AWS EKS two-axis pattern).

### 3.1 GCP GKE

**Primitive: Google Cloud Managed Service for Prometheus +
Cloud Logging integration.**

GKE exposes managed observability via two cluster-level
settings:
- `monitoringConfig.componentConfig.enableComponents` includes
  `SYSTEM_COMPONENTS` (control plane) and optionally
  `WORKLOADS` (managed Prometheus on workload metrics).
- `loggingConfig.componentConfig.enableComponents` includes
  `SYSTEM_COMPONENTS` and `WORKLOADS`.

**Detection rule:** cluster is INSTRUMENTED if
`monitoringConfig.managedPrometheusConfig.enabled == true`.
Operators without managed Prometheus can still run a
self-managed OTel collector, but slice 2 only detects the
managed primitive (consistent with the AWS EKS managed-addon
detection rule).

**Recommendation kind:** `gke-mp-enable` (Managed Prometheus
enable).

**Terraform target:**
`google_container_cluster.monitoring_config[0].managed_prometheus[0].enabled = true`.

### 3.2 Azure AKS

**Primitive: Azure Monitor Container Insights OR Managed
Prometheus.**

AKS exposes managed observability via two related addons:
- `addonProfiles.omsagent.enabled` (legacy Container Insights).
- `azureMonitorProfile.metrics.enabled` (newer Managed
  Prometheus).
- `azureMonitorProfile.containerInsights.enabled` (newer
  Container Insights, replacing omsagent).

**Detection rule:** cluster is INSTRUMENTED if EITHER
`addonProfiles.omsagent.enabled == true` OR
`azureMonitorProfile.metrics.enabled == true` OR
`azureMonitorProfile.containerInsights.enabled == true`. Mirrors
EKS's "ADOT OR CloudWatch observability" disjunction — operators
on the older addon get credit; operators on the newer Managed
Prometheus also get credit.

**Recommendation kind:** `aks-monitor-enable`.

**Terraform target:**
`azurerm_kubernetes_cluster.monitor_metrics` block or
`azurerm_kubernetes_cluster.oms_agent` block.

### 3.3 OCI OKE

**Primitive: Operations Insights + Container Monitoring
enrollment.**

OKE clusters can be enrolled in OCI's Operations Insights
service for monitoring, similar to the OCI Database arc's
Database Management primitive. Detection lives on the
cluster's `options.openIdConnectDiscovery` field for the
managed observability indicator OR on a tag-based check
(OCI does not expose a top-level "managed observability"
boolean as cleanly as GCP/Azure).

**Detection rule:** cluster is INSTRUMENTED if the cluster has
a tag key matching `operations-insights-enabled` (case-
insensitive) with value `true`. Slice 2 uses this convention
because OCI's Operations Insights API does not return a single
"cluster enrolled" boolean — operators tag the cluster when
they enroll. Slice 3 may move to a direct Operations Insights
API call.

**Recommendation kind:** `oke-ops-insights-enable`.

**Terraform target:** add the `operations-insights-enabled` tag
on `oci_containerengine_cluster.freeform_tags`.

### 3.4 Why no cross-cloud unified rule?

Same reasoning as the database tier (see
[database-tier-slice2.md](./database-tier-slice2.md) §3.4).
Kubernetes managed observability is about turning on a cloud-
native feature, not deploying a Squadron-controlled artifact.
Each cloud exposes a different primitive; slice 2 treats them
parallel-but-different.

## 4. Storage and snapshot type

The existing `scanner.ClusterSnapshot` type (defined in
`internal/discovery/scanner/scanner.go` per the AWS EKS slice
in v0.89.0) is generic enough at the field level
(ResourceID / Name / KubernetesVersion / Status /
ControlPlaneLogging / Addons / NodegroupCount /
FargateProfileCount / Region / Tags). Slice 2 extends with
three new optional axes plus a Provider discriminator:

```go
type ClusterSnapshot struct {
    // ... existing fields ...
    
    // GCP GKE: monitoringConfig.managedPrometheusConfig.enabled
    ManagedPrometheusEnabled bool `json:"managed_prometheus_enabled,omitempty"`
    
    // Azure AKS: addonProfiles.omsagent.enabled OR
    // azureMonitorProfile.metrics.enabled OR
    // azureMonitorProfile.containerInsights.enabled
    AzureMonitorEnabled bool `json:"azure_monitor_enabled,omitempty"`
    
    // OCI OKE: presence of operations-insights-enabled=true
    // tag (slice 2 convention; slice 3 moves to a direct API).
    OperationsInsightsEnabled bool `json:"operations_insights_enabled,omitempty"`
    
    // Provider discriminator. Empty defaults to "aws" for
    // backward compatibility with v0.89.0 audit rows. The
    // proposer reads Provider to decide which detection axis
    // to evaluate and which recommendation kind to emit.
    Provider string `json:"provider,omitempty"`
}
```

The existing AWS EKS axes (`ControlPlaneLogging`, `Addons`,
`NodegroupCount`, `FargateProfileCount`) remain populated only
for Provider="aws" callers. Non-AWS scanners leave them empty
slices. The AWS proposer logic reads them unchanged.

### 4.1 Result.FailedServices identifiers

- GCP GKE scanner: `gke`
- Azure AKS scanner: `aks`
- OCI OKE scanner: `oke`

## 5. Per-cloud scanner extensions

Each cloud's existing scanner package gets a new walk for its
managed Kubernetes service inside the existing `Scan` call
(mirroring the database tier pattern).

### 5.1 GCP scanner extension

Adds `container.googleapis.com` API calls:
- `GET /v1beta1/projects/{project}/locations/-/clusters`
- For each cluster, extract `monitoringConfig.managedPrometheusConfig.enabled`.

IAM scope: existing `roles/compute.viewer` doesn't cover GKE.
Operators need to add `roles/container.viewer` for slice 2.
Runbook update documents this.

### 5.2 Azure scanner extension

Adds ARM API call:
- `GET /subscriptions/{sub}/providers/Microsoft.ContainerService/managedClusters?api-version=2024-09-01`

Extract `properties.addonProfiles.omsagent.enabled`,
`properties.azureMonitorProfile.metrics.enabled`,
`properties.azureMonitorProfile.containerInsights.enabled`.

Service Principal Reader role at subscription scope already
covers Microsoft.ContainerService reads.

### 5.3 OCI scanner extension

Adds OCI API call:
- `GET https://containerengine.<region>.oraclecloud.com/20180222/clusters?compartmentId=<comp>`

For each cluster, extract `freeformTags` map and check for
`operations-insights-enabled` key with `true` value.

IAM policy: existing
`Allow group SquadronDiscovery to read instance-family in tenancy`
doesn't cover OKE. Add:
- `Allow group SquadronDiscovery to read cluster-family in tenancy`

## 6. Proposer integration

The proposer's recommendation kind enumeration extends:

```text
For GCP (Kubernetes tier slice 2):
- gke-mp-enable: Enable Managed Prometheus on a GKE cluster
  where monitoringConfig.managedPrometheusConfig.enabled is
  false. Terraform: google_container_cluster.monitoring_config[0].managed_prometheus[0].enabled = true.

For Azure (Kubernetes tier slice 2):
- aks-monitor-enable: Enable Azure Monitor (Container Insights
  or Managed Prometheus) on an AKS cluster where none of the
  three observability profile flags are true. Terraform:
  azurerm_kubernetes_cluster.monitor_metrics block.

For OCI (Kubernetes tier slice 2):
- oke-ops-insights-enable: Add the operations-insights-enabled=true
  freeform tag to an OKE cluster where it is missing. Terraform:
  oci_containerengine_cluster.freeform_tags.
```

Branch encoding extends:
- `gke-*` prefix → provider="gcp"
- `aks-*` prefix → provider="azure"
- `oke-*` prefix → provider="oci"

Mirrors the database tier prefix-routing pattern. The webhook
handler kind-prefix detection switch extends:

```go
switch {
case strings.HasPrefix(kind, "gce-") || strings.HasPrefix(kind, "cloudsql-") || strings.HasPrefix(kind, "gke-"):
    provider = "gcp"
case strings.HasPrefix(kind, "vm-") || strings.HasPrefix(kind, "azsql-") || strings.HasPrefix(kind, "aks-"):
    provider = "azure"
case strings.HasPrefix(kind, "compute-") || strings.HasPrefix(kind, "ocidb-") || strings.HasPrefix(kind, "oke-"):
    provider = "oci"
default:
    provider = "aws"
}
```

## 7. Audit events

Reuses the existing `discovery.<provider>.scan_completed` audit
event types. Payload gains `cluster_count`,
`instrumented_cluster_count`, `uninstrumented_cluster_count`
fields per scan (same shape as the database tier extension —
one extra category counted per scan).

No new event types — keeps the audit timeline coherent.

## 8. UI updates

Per-provider Inventory tabs (DiscoveryGCP, DiscoveryAzure,
DiscoveryOCI) gain a Kubernetes sub-tab inside Inventory:

- Existing Compute tab stays.
- Database sub-tab (from database tier slice 2) stays.
- NEW Kubernetes sub-tab shows projected ClusterSnapshot rows.

Columns: Cluster Name, Kubernetes Version, Region, Status,
Provider-axis boolean (instrumented?), Tags.

The Recommendations tab automatically surfaces the new
recommendation kinds (rendering is generic over kind).

The unified Discovery dashboard (v0.89.62) automatically sums
compute + database + kubernetes counts into per-provider totals
via the existing scan_completed aggregation — no dashboard code
changes.

## 9. Slice 2 contract

**In:**

1. Extended ClusterSnapshot with 4 new optional fields
   (ManagedPrometheusEnabled, AzureMonitorEnabled,
   OperationsInsightsEnabled, Provider).
2. GCP scanner extension: GKE cluster walker + managed
   Prometheus detection rule.
3. Azure scanner extension: AKS managed clusters walker +
   three-way disjunction Azure Monitor detection.
4. OCI scanner extension: OKE cluster walker + tag-based
   Operations Insights detection.
5. Proposer prompt extension: three new recommendation kinds.
6. Webhook handler kind-prefix detection extended for
   gke-/aks-/oke- prefixes.
7. Per-provider UI Inventory tab gains Kubernetes sub-tab.
8. Per-provider runbook updates documenting new IAM
   permission asks.
9. Tests covering each new scanner extension + proposer kinds
   + webhook routing.

**Out:**

- Workload-level detection (slice 3).
- AWS EKS expansion (slice 3).
- GKE Autopilot vs Standard differentiation.
- AKS Virtual Nodes / OKE Virtual Nodes.
- Service mesh observability.

## 10. Implementation chunks

Tighter than slice 1 cloud arcs and parallel to the database
tier slice 2 pattern. Three implementations run in parallel:

- **Chunk 1: ClusterSnapshot extension + audit payload schema.**
  ~150-250 lines. Shared across all three implementations.
  v0.89.69.
- **Chunk 2: GCP GKE scanner extension.** ~600-800 lines.
  v0.89.70 (parallel).
- **Chunk 3: Azure AKS scanner extension.** ~700-900 lines.
  v0.89.70 (parallel).
- **Chunk 4: OCI OKE scanner extension.** ~600-800 lines.
  v0.89.70 (parallel).
- **Chunk 5: Proposer + webhook routing + UI Kubernetes
  sub-tab.** ~700-900 lines. v0.89.71 (sequential after the
  three scanners merge).
- **Chunk 6: Three runbook updates (one per provider).**
  ~250-350 lines. v0.89.72 (final).

Total: 4-5 release tags. Chunks 2 + 3 + 4 fan-out parallel via
worktrees.

## 11. Acceptance tests

1. GKE cluster with managedPrometheusConfig.enabled=true →
   ManagedPrometheusEnabled=true, no recommendation.
2. GKE cluster with managedPrometheusConfig.enabled=false →
   gke-mp-enable recommendation.
3. AKS cluster with omsagent.enabled=true →
   AzureMonitorEnabled=true, no recommendation.
4. AKS cluster with azureMonitorProfile.metrics.enabled=true
   AND omsagent.enabled=false → AzureMonitorEnabled=true.
5. AKS cluster with all three observability flags false →
   aks-monitor-enable recommendation.
6. OKE cluster with operations-insights-enabled=true tag →
   OperationsInsightsEnabled=true.
7. OKE cluster with case-variant
   Operations-Insights-Enabled=TRUE tag → still detected
   (case-insensitive key + value).
8. OKE cluster without tag → oke-ops-insights-enable
   recommendation.
9. Cold-start parity preserved across all 4 providers (compute-
   only paths byte-identical).
10. Webhook routing: gke-mp-enable → gcp, aks-monitor-enable →
    azure, oke-ops-insights-enable → oci.
11. UI Kubernetes sub-tab renders for all three providers.
12. AWS EKS proposer path unchanged (Provider="aws"
    interpretation reads existing ControlPlaneLogging + Addons
    fields, not the new slice 2 fields).

## 12. Threat model

Inherits per-cloud threat models from slice 1. New surface
area:

- **GKE listing requires `roles/container.viewer`.** Operators
  must add this scope to the existing SA. Runbook makes it
  explicit; permission_denied error_kind specific to GKE
  surfaces on missing scope.
- **AKS listing.** Existing Reader role at subscription scope
  already covers Microsoft.ContainerService reads. No new SP
  scope.
- **OKE listing requires the new policy statement.** Runbook
  documents:
  `Allow group SquadronDiscovery to read cluster-family in tenancy`.

No new credential domains.

---

**Strategic frame:**

Kubernetes tier slice 2 is the second depth arc after
breadth claim closed. Combined with the database tier slice 2
that just shipped, Squadron's universal observability surface
now spans:

| Tier | AWS | GCP | Azure | OCI |
|---|---|---|---|---|
| Compute | ✓ slice 1 | ✓ slice 1 | ✓ slice 1 | ✓ slice 1 |
| Database | ✓ slice 1 (v0.87.0) | ✓ slice 2 | ✓ slice 2 | ✓ slice 2 |
| Kubernetes | ✓ slice 1 (v0.89.0) | this arc | this arc | this arc |

Three tiers across four clouds = twelve scanner surfaces. The
universal claim becomes "Squadron scans AWS, GCP, Azure, AND
Oracle Cloud across COMPUTE, DATABASE, AND KUBERNETES for
observability gaps." That's the strongest two-slice version of
the universal claim a single OSS control plane can defensibly
support.

Slice 3 candidates: workload-level observability detection
(OTel collector sidecar / auto-instrumentation labels across
all 4 cloud Kubernetes surfaces), trace integration (start
consuming OTel traces so Squadron can spot missing-span gaps),
or the fifth cloud (Alibaba / Tencent / IBM / DigitalOcean).
