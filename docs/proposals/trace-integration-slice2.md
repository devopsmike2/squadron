# Trace integration slice 2 — recommendation kinds

**Status:** design doc, locked for slice 2 implementation. Builds
directly on slice 1 (v0.89.73 through v0.89.78), which shipped
the traceindex package + receiver wiring + Discovery dashboard
panel + per-Inventory-row `last_seen_at` annotation. Slice 1
gave operators VISIBILITY into the gap between "primitive
enabled" and "spans flowing." Slice 2 turns that visibility into
proposer-drafted recommendations.

**See also:**
[Trace integration slice 1](./trace-integration-slice1.md),
[Database tier slice 2](./database-tier-slice2.md),
[Kubernetes tier slice 2](./kubernetes-tier-slice2.md).

## 1. Problem

After slice 1 lands an operator can see the discovery dashboard
showing "67% trace coverage across all providers" and an
inventory row showing `i-0abc | Container Insights enabled |
never`. The signal is honest, the gap is visible — but the
remediation is operator-manual. The operator has to:

1. Open the Compute Inventory page for that provider.
2. Notice the "never" indicator.
3. Decide whether the gap is SDK-not-deployed, exporter-
   misconfigured, or attribute-mismatch.
4. Author whatever fix applies.

For an operator with five resources in the gap, this is
tractable. For an operator with five hundred (the common
multi-cloud production case), this is a backlog item nobody
will work. The signal becomes a yellow indicator everyone
agrees to ignore.

Slice 2 closes the loop the same way every other Squadron arc
has: the proposer reads the gap and drafts a recommendation
against the IaC repo. The operator reviews the PR. If they merge
it, the SDK gets deployed via Terraform. If they decline, the
decline becomes signal in the verdict learning loop (#531 slice
2).

The recommendation surface gets twelve new kinds — one per
provider per tier:

```
trace-emission-aws-compute     trace-emission-gcp-compute
trace-emission-aws-db          trace-emission-gcp-db
trace-emission-aws-k8s         trace-emission-gcp-k8s

trace-emission-azure-compute   trace-emission-oci-compute
trace-emission-azure-db        trace-emission-oci-db
trace-emission-azure-k8s       trace-emission-oci-k8s
```

Each kind targets a specific IaC pattern that deploys the OTel
SDK or auto-instrumentation for that provider's tier. The
proposer's reasoning explains which pattern was picked and why.

## 2. Non-goals (slice 2)

- **Squadron deploys the SDK itself.** Squadron remains a
  recommender, not an actor. The recommendation drafts a
  Terraform PR; the operator's CI applies it.
- **Per-language SDK selection.** Slice 2 ships Terraform
  patterns for the cloud-native auto-instrumentation paths
  (AWS Distro for OpenTelemetry, GCP OpenTelemetry Operator,
  Azure App Service auto-instrumentation, OCI APM Java agent
  for JVM workloads). Per-language deep customization (Python
  asyncio, Go net/http middleware, etc.) is slice 3.
- **Service mesh sidecar injection.** Istio / Linkerd / Cilium
  ambient have their own trace propagation paths. Slice 3+.
- **Auto-instrumentation deployment via Helm chart.** Slice 2
  ships Terraform-only patterns to stay in the existing IaC PR
  surface. Helm chart deployment via the kubernetes Terraform
  provider is a slice 2 candidate but the simpler pure-IaC
  paths cover most cases.
- **Span quality recommendations.** Bad context propagation,
  missing resource attributes, etc. are slice 3 (span quality
  analysis arc).
- **Cross-cloud trace correlation.** A span chain that crosses
  AWS Lambda then SQS then GCP Cloud Function carries context
  across cloud boundaries. Slice 4+.
- **Trace volume cost analysis.** Slice 1's cost-spike detector
  watches collector self-metrics, not app trace volume. Slice 3
  candidate.

## 3. Detection rule (per kind)

The detection rule for each `trace-emission-*` recommendation:

```
inventory_row.primitive_enabled == true
  AND inventory_row.last_seen_at is null OR > 24h ago
  AND inventory_row.last_excluded_at is null (chunk 4 of #531 slice 2)
```

The 24h staleness threshold is intentionally permissive. Slice 3
may tune per workload type (a daily batch job won't emit
continuously; a web service that's silent for an hour probably
has a real issue).

For each tier:

- **Compute**: `primitive_enabled` = "has otel* tag" (the slice
  1 detection rule). A compute instance with the otel-collector
  tag but no recent emission gets a `trace-emission-{provider}-compute`
  recommendation.
- **Database**: `primitive_enabled` = the per-cloud database
  axis (PerformanceInsightsEnabled for AWS RDS,
  QueryInsightsEnabled for Cloud SQL, etc.). A database row
  with the primitive on but no trace from the workload that
  connects to it gets a `trace-emission-{provider}-db`
  recommendation.
- **Kubernetes**: `primitive_enabled` = the per-cloud
  observability addon (ADOT for EKS, Managed Prometheus for
  GKE, Azure Monitor for AKS, Ops Insights for OKE). A cluster
  with the addon active but no spans from any workload gets a
  `trace-emission-{provider}-k8s` recommendation.

### 3.1 Why not just check the resource directly?

A compute instance with `host.name=db-prod` and no spans MIGHT
be: (a) SDK not running, (b) SDK running but pointed at
wrong endpoint, (c) SDK running fine but emitting with a
different `host.name` value (the OTel host detector picked
something else). Slice 2 cannot distinguish from outside the
host. The recommendation's reasoning lays out all three cases;
the operator (or their on-call) walks through them.

The Terraform patch slice 2 drafts ALWAYS targets case (a) —
SDK deployment via auto-instrumentation. If the operator was
actually in case (b) or (c), they'll catch it on PR review,
decline the PR, and the verdict learning loop (#531 slice 2)
records why for the next proposal cycle.

## 4. Per-cloud recommendation patterns

### 4.1 AWS — trace-emission-aws-compute (EC2)

Terraform pattern: add the CloudWatch Agent with ADOT collector
sidecar as a Systems Manager Run Document, applied to the
target EC2 instance via tag-based association.

```hcl
resource "aws_ssm_association" "otel_collector_install" {
  name = "AWS-ConfigureAWSPackage"
  
  targets {
    key    = "tag:otel-collector"
    values = ["v1"]
  }
  
  parameters = {
    action = "Install"
    name   = "AmazonCloudWatchAgent"
  }
}
```

The recommendation reasoning explains that this installs the
CWAgent which includes the ADOT collector binary. The operator
still has to configure ADOT to export traces; slice 2 ships
the collector deployment, slice 3 ships the config.

### 4.2 AWS — trace-emission-aws-db (RDS)

Terraform pattern: enable Performance Insights long-term
retention. RDS Performance Insights itself emits no spans (it's
not OTel-native), but enabling LTR ensures the existing
metric stream is durable and the operator's workload (which
needs to emit DB-correlated spans separately) has a target to
correlate against.

```hcl
resource "aws_db_instance" "<name>" {
  # ... existing fields ...
  performance_insights_enabled          = true
  performance_insights_retention_period = 731  # 2 years (LTR tier)
}
```

The recommendation reasoning explains that the actual span
emission has to come from the application client; Squadron flags
the DB but the fix happens on the app side. The Terraform PR
is a half-step.

### 4.3 AWS — trace-emission-aws-k8s (EKS)

Terraform pattern: install the ADOT operator via the EKS addon
mechanism.

```hcl
resource "aws_eks_addon" "adot" {
  cluster_name             = aws_eks_cluster.<name>.name
  addon_name               = "adot"
  service_account_role_arn = aws_iam_role.adot.arn
}
```

The recommendation reasoning explains that ADOT provides the
operator + auto-instrumentation paths for JVM, Python, Node.js
workloads on the cluster. The operator still has to label
their Deployments to enable per-workload auto-instrumentation;
slice 2 ships the operator install.

### 4.4 GCP — trace-emission-gcp-compute (GCE)

Terraform pattern: add the Ops Agent metadata and labels to the
target instance via `google_compute_instance` metadata block.

```hcl
resource "google_compute_instance" "<name>" {
  # ... existing fields ...
  metadata = {
    enable-osconfig = "TRUE"
    google-logging-enabled = "true"
    google-monitoring-enabled = "true"
  }
}
```

### 4.5 GCP — trace-emission-gcp-db (Cloud SQL)

Pattern: enable Query Insights' enhanced fields.

```hcl
resource "google_sql_database_instance" "<name>" {
  settings {
    insights_config {
      query_insights_enabled  = true
      record_application_tags = true
      record_client_address   = true
    }
  }
}
```

### 4.6 GCP — trace-emission-gcp-k8s (GKE)

Pattern: deploy the OpenTelemetry Operator via the
`google_gke_hub_feature` Cloud Service Mesh integration, OR via
a kubernetes_manifest resource pointing at the operator's
upstream installation manifest.

```hcl
resource "google_gke_hub_feature" "service_mesh" {
  name     = "servicemesh"
  location = "global"
}
```

(Note: this is a soft-recommendation pattern; the cleaner path
for many operators is the Helm-based install which is slice 2's
non-goal. The Terraform pattern here is the IaC-pure version.)

### 4.7 Azure — trace-emission-azure-compute (VM)

Pattern: enable the Azure Monitor Agent VM extension.

```hcl
resource "azurerm_virtual_machine_extension" "azure_monitor_agent" {
  name                 = "AzureMonitorLinuxAgent"  # or Windows variant
  virtual_machine_id   = azurerm_linux_virtual_machine.<name>.id
  publisher            = "Microsoft.Azure.Monitor"
  type                 = "AzureMonitorLinuxAgent"
  type_handler_version = "1.0"
}
```

### 4.8 Azure — trace-emission-azure-db (Azure SQL)

Pattern: enable both Diagnostic Settings AND auto-tuning
recommendations on the SQL Database.

```hcl
resource "azurerm_mssql_database_extended_auditing_policy" "<name>" {
  database_id            = azurerm_mssql_database.<name>.id
  log_monitoring_enabled = true
}
```

### 4.9 Azure — trace-emission-azure-k8s (AKS)

Pattern: enable the AKS Application Insights add-on, which
includes auto-instrumentation for JVM, .NET, Node.js workloads.

```hcl
resource "azurerm_kubernetes_cluster" "<name>" {
  # ... existing fields ...
  monitor_metrics {
    annotations_allowed = ["service.name", "service.instance.id"]
  }
}
```

### 4.10 OCI — trace-emission-oci-compute (Instance)

Pattern: add the OCI APM Java agent (or Python agent) via
cloud-init script in the instance launch template.

Slice 2 ships the cloud-init pattern via
`oci_core_instance.metadata.user_data` with a base64-encoded
script. The recommendation flags this as an upgrade-during-
maintenance change since cloud-init only runs on first boot.

### 4.11 OCI — trace-emission-oci-db (Autonomous Database)

Pattern: enable Database Management's full Operations Insights
+ Performance Hub features, which include trace correlation.

```hcl
resource "oci_database_management_managed_database_group" "<name>" {
  compartment_id = var.compartment_ocid
  name           = "squadron-managed"
}
```

### 4.12 OCI — trace-emission-oci-k8s (OKE)

Pattern: install the OCI Service Operator on the OKE cluster
via kubernetes_manifest, which provides Operations Insights
integration for workloads.

## 5. Selection between primary and alternative patterns

Per recommendation, the proposer picks ONE Terraform pattern.
For tiers with multiple valid paths (e.g. AKS could enable the
oms-agent OR the newer azureMonitorProfile.metrics OR the newer
containerInsights), the proposer picks based on what's already
in the operator's IaC repo:

1. Read the existing `azurerm_kubernetes_cluster.<name>` block.
2. If it already has `oms_agent`, the recommendation extends
   that block.
3. If it already has `azure_monitor_profile.metrics`, the
   recommendation extends that.
4. If neither exists, the recommendation picks the newer
   `monitor_metrics` block as the default.

This logic lives in a new `internal/proposer/iacpicker` package
that the proposer consults before drafting. Slice 2 ships the
picker for the common tier pairs; slice 3 may add per-language
sub-picks.

## 6. Audit + verdict learning

Each new recommendation kind:

- Fires the existing `recommendation.pr_opened` audit event when
  the operator clicks Open PR (no new event types).
- The payload's `recommendation_kind` carries the new
  `trace-emission-*` value; SIEM consumers can filter on the
  prefix.
- The webhook receiver's kind-prefix detection switch extends:
  ```go
  case strings.HasPrefix(kind, "trace-emission-"):
      provider = providerFromTraceEmissionKind(kind)
  ```
  Where `providerFromTraceEmissionKind("trace-emission-aws-compute")` returns "aws".
- The verdict learning loop (#531 slice 2) records merges and
  declines on these kinds identically to existing recommendation
  kinds. A decline on `trace-emission-aws-k8s` becomes negative
  signal for the next K8s trace-emission scan in that scope.

## 7. UI updates

The Recommendations tab on each per-provider page already
renders any recommendation kind generically. Slice 2 does NOT
require new UI surface for the recommendations themselves —
they appear alongside the existing tag-, db-, and k8s-
recommendation kinds.

The Discovery dashboard's TRACE COVERAGE panel gains a sub-
indicator: "X resources with the primitive enabled but no recent
emission — see Recommendations tab on each provider for the
drafts." Click-through links to the per-provider Recommendations
page filtered to `trace-emission-*` kinds.

## 8. Slice 2 contract

**In:**

1. 12 new recommendation kind constants in the proposer prompt:
   `trace-emission-{aws,gcp,azure,oci}-{compute,db,k8s}`.
2. Per-kind Terraform pattern in the proposer's prompt context
   (the patterns listed in §4).
3. New `internal/proposer/iacpicker` package selecting which
   Terraform path to extend based on existing repo content.
4. Detection logic in the discovery proposer: for each
   inventory row that has `primitive_enabled=true` AND
   `last_seen_at == null OR > 24h`, emit the
   corresponding `trace-emission-*` recommendation.
5. Webhook handler kind-prefix detection extension.
6. Discovery dashboard TRACE COVERAGE panel sub-indicator.
7. Per-provider per-tier acceptance tests covering each new
   kind from detection through prompt emission.

**Out:**

- Per-language SDK customization (slice 3).
- Service mesh sidecar injection.
- Helm chart deployment paths (kubernetes Terraform provider).
- Span quality recommendations.
- Cross-cloud trace correlation.
- Trace volume cost analysis.
- Squadron auto-executes the SDK deployment.

## 9. Implementation chunks

- **Chunk 1: Proposer prompt + iacpicker package.** ~700-900
  lines. New `internal/proposer/iacpicker` package; system
  prompt extension listing all 12 kinds with Terraform patterns
  verbatim; the detection branch on the discovery proposer.
  v0.89.80.
- **Chunk 2: Webhook routing + audit verification.** ~300-500
  lines. Kind-prefix detection extends to recognize
  `trace-emission-*`. The webhook handler routes correctly per
  the provider hint in the kind. Tests pin the routing.
  v0.89.81.
- **Chunk 3: UI dashboard sub-indicator + Recommendations tab
  filter.** ~500-700 lines. Trace coverage panel gains the
  "N resources need investigation" line. Per-provider
  Recommendations tab gains a "Show only trace-emission-*"
  filter chip. v0.89.82.
- **Chunk 4: Operator runbook update.** ~250-400 lines. Extends
  the trace-coverage-operator-guide.md with the slice 2
  recommendation workflow + the per-cloud Terraform pattern
  examples + how to decline a recommendation when slice 2
  picked the wrong case (b/c failure mode). v0.89.83.

Total: 4 release tags. No parallelization across chunks; the
prompt extension in chunk 1 is the prerequisite for chunks 2-4.

## 10. Acceptance tests

1. **EC2 with otel tag and no recent emission → recommendation.**
   Seed an EC2 inventory row with `HasOTel=true` and
   `LastSeenAt = 25h ago`. Run discovery proposer. Assert: a
   `trace-emission-aws-compute` recommendation emitted with
   the §4.1 Terraform pattern.
2. **EC2 with otel tag and recent emission → NO recommendation.**
   Same setup but `LastSeenAt = 1h ago`. Assert: no
   trace-emission recommendation; existing instrumentation
   recommendations still emit normally.
3. **GKE with managed Prometheus and no recent emission →
   recommendation.** Cluster with `ManagedPrometheusEnabled=true`
   and zero spans. Assert: `trace-emission-gcp-k8s` emitted.
4. **AKS with monitor_metrics already set → picker extends
   existing block.** Existing IaC has
   `azurerm_kubernetes_cluster.monitor_metrics`. Proposer
   recommends extending that block, not introducing
   `oms_agent`.
5. **AKS with no monitor block → picker defaults to
   monitor_metrics.** Existing IaC has neither. Proposer picks
   `monitor_metrics` as the default per §5.
6. **Trace-emission excluded on a row → recommendation
   suppressed.** Operator clicked Don't propose this again on
   a previous trace-emission recommendation (chunk 4 of #531
   slice 2). Assert: no recommendation emitted.
7. **Webhook routes trace-emission-gcp-k8s to provider=gcp.**
8. **Webhook routes trace-emission-azure-db to provider=azure.**
9. **Dashboard sub-indicator surfaces non-zero count.** Index
   has 5 inventory rows with primitive enabled and no
   emission. Dashboard renders "5 resources with primitive
   enabled but no recent emission."
10. **Dashboard sub-indicator hidden when zero.** All inventory
    rows are emitting. Indicator does NOT render.
11. **Cold-start parity preserved.** All 4 providers compute-
    only AND compute+database AND compute+database+k8s
    cold-start prompts byte-identical to v0.89.78.
12. **Verdict learning incorporates trace-emission verdicts.**
    Operator declines a `trace-emission-oci-compute`
    recommendation with a note. Next proposer call on the
    same scope shows the decline as a citation in the prompt.

## 11. Threat model

Slice 2 introduces no new external surface (proposer + webhook
already exist) and no new credentials. The new threat surface
is recommendation-content quality:

**Wrong failure mode targeting.** Slice 2 ALWAYS drafts for
case (a) — SDK not deployed — even when the actual root cause
is case (b) — exporter misconfigured. An operator who blindly
merges the PR adds redundant SDK install on top of an existing
deployment. Mitigation: PR body explicitly lists all three
cases and asks the reviewer to confirm. The operator's review
catches the wrong-case bug.

**Trust in the IaC picker.** The picker reads the operator's
existing IaC repo to decide which pattern to extend. A
malformed repo (mismatched braces, unparseable HCL) could lead
the picker to mis-classify. Slice 2 falls back to the
"default" pattern when parsing fails and surfaces an audit
event noting the parse failure. The operator sees the audit
and can debug.

**Volume on trace-emission recommendations.** A fresh Squadron
deployment with 500 inventoried resources may surface 500
trace-emission recommendations on the first scan. Mitigation:
the existing per-rollout/per-recommendation Don't propose this
again affordance (chunk 4 of #531 slice 2) works on these
kinds the same way it works on existing kinds. Operators in
deployment mode triage in bulk via the exclusion.

## 12. Slice 3 candidates

- Per-language SDK customization (Python, Go, Node.js, JVM,
  .NET) — currently slice 2 ships cloud-native generic patterns.
- Service mesh sidecar injection patterns.
- Helm chart deployment via kubernetes Terraform provider.
- Span quality analysis (broken context propagation, missing
  required resource attributes).
- Cross-cloud trace correlation.
- Trace volume cost analysis.
- Workload-aware staleness thresholds (batch jobs, web
  services, cron jobs each get different 24h tolerance).
- Helm-based GKE OpenTelemetry Operator install path (slice 2
  ships the IaC-pure path; slice 3 adds the Helm path).

---

**Strategic frame:**

Slice 1 of trace integration shipped reconciliation visibility.
Slice 2 shipped reconciliation ACTION. The proposer now drafts
fixes for the gap slice 1 surfaced, the operator reviews + merges
or declines, and the verdict learning loop teaches the proposer
across the cycle.

The universal claim now reads:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, AND KUBERNETES for observability gaps,
> verifies telemetry is actually flowing, AND drafts the IaC
> PRs that close the gaps it finds.

Three verbs. One control plane. Squadron has gone from
discovery + recommendation (the v0.85 era) to discovery +
recommendation + reconciliation (today) to a feedback loop that
learns from operator decisions over time. The Tuesday LinkedIn
post's narrative ("make the postmortem about the proposal the
operator turned down") is now operator-visible at every layer.
