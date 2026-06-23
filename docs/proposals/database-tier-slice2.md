# Database tier slice 2 — GCP Cloud SQL + Azure SQL + OCI DB Systems

**Status:** design doc, locked for slice 2 implementation across
three clouds in a fan-out arc. This is the first **depth** arc
across the universal observability surface (the prior arcs were
breadth across clouds at slice 1). AWS already shipped database
scanning in v0.87.0 (RDS with the two-axis
PerformanceInsightsEnabled + EnhancedMonitoringEnabled rule —
#573). The remaining three clouds (GCP, Azure, OCI) each have a
slice-2 database scanner gap. This arc closes all three in one
coordinated implementation rather than three separate single-
cloud arcs.

The strategic frame: after the four-cloud breadth claim landed
in v0.89.62 with the unified dashboard, the next move is depth.
Telling operators "Squadron scans four clouds for compute
instrumentation gaps" is concrete. Telling them "Squadron scans
four clouds for compute AND database instrumentation gaps" is
materially stronger — because database observability gaps are
where the highest-value incidents land (slow queries, query
plans drifting, replica lag). The recommendation surface
doubles in dimension after this arc closes.

**See also:**
[AWS RDS scanner (#573)](./558-discovery-design.md),
[GCP discovery slice 1](./gcp-discovery-slice1.md),
[Azure discovery slice 1](./azure-discovery-slice1.md),
[OCI discovery slice 1](./oci-discovery-slice1.md),
[Unified Discovery dashboard slice 1](./unified-discovery-dashboard-slice1.md).

## 1. Problem

Operators with multi-cloud database fleets see Squadron
recommendations for their AWS RDS instances but nothing for their
GCP Cloud SQL, Azure SQL Database, or OCI DB Systems / Autonomous
Database fleets. The discovery proposer's recommendation surface
is structurally asymmetric across providers: compute is universal,
database is AWS-only.

For an enterprise running cross-cloud database workloads, the
asymmetry is a credibility problem. An evaluation conversation
that goes "Squadron handles AWS RDS Performance Insights gaps —
oh, but for Cloud SQL or Azure SQL, you'd need a different tool"
loses the universal claim. The four-cloud breadth becomes
"four-cloud compute breadth + one-cloud database depth," which is
a weaker positioning.

Slice 2 of the database tier closes this asymmetry with parallel
scanners across GCP / Azure / OCI, each adapted to that cloud's
native observability primitive.

## 2. Non-goals (slice 2)

- **AWS RDS expansion.** AWS RDS slice 1 (the two-axis PI + EM
  rule) is sufficient for this slice. Adding RDS Performance
  Insights long-term retention windows, RDS Proxy detection, or
  RDS Enhanced Monitoring granularity tuning is slice 3.
- **AWS Aurora-specific signals.** Aurora has its own observability
  surface (Aurora Performance Insights, Aurora Backtrack, etc.).
  Slice 3.
- **GCP AlloyDB.** Slice 3. AlloyDB is GCP's Aurora analog.
- **Azure Cosmos DB.** Slice 3+. Cosmos has its own multi-API
  surface that warrants its own arc.
- **OCI Autonomous Data Warehouse vs Autonomous Transaction
  Processing differentiation.** Slice 2 treats both as
  "Autonomous Database" for the OCI scanner. Slice 3 may
  differentiate.
- **Cross-database replication topology detection.** A multi-
  cloud read replica topology (e.g., primary in RDS, read
  replica in Cloud SQL via streaming replication) is out of
  scope. Slice 4+.
- **Query plan analysis.** Squadron recommends turning ON the
  observability primitive; it does not analyze the queries
  themselves. That stays a separate concern (PgHero / pganalyze /
  etc.).
- **Cross-cloud database recommendation prioritization.** Slice
  2 emits per-cloud recommendations independently; the dashboard
  surfaces them in the unified queue. Slice 3 may add
  cross-cloud prioritization heuristics.

## 3. Architectural decision: per-cloud detection rules

Each cloud exposes a different observability primitive for its
database services. We map each to a single canonical
"instrumented vs uninstrumented" axis for slice 2 (deeper
multi-axis rules are slice 3):

### 3.1 GCP Cloud SQL

**Primitive: Query Insights.**

Cloud SQL exposes Query Insights as a per-instance setting
(`settings.insightsConfig.queryInsightsEnabled`). When enabled,
Google captures normalized query strings, execution stats,
top-query lists, and end-to-end traces for slow queries.

**Detection rule:** instance is INSTRUMENTED if
`settings.insightsConfig.queryInsightsEnabled == true`. Otherwise
uninstrumented.

**Recommendation kind:** `cloudsql-pi-enable` (mnemonic: Cloud SQL
Performance Insights enable — parallels AWS's
`rds-pi-em` naming).

**Terraform target:** `google_sql_database_instance.settings[0].insights_config[0].query_insights_enabled = true`.

### 3.2 Azure SQL Database

**Primitive: Diagnostic Settings + Performance Insights.**

Azure SQL exposes observability through two related mechanisms:
- **Diagnostic Settings**: routes telemetry (SQLInsights category,
  AutomaticTuning category, QueryStoreRuntimeStatistics, etc.) to
  Log Analytics workspace, Storage, or Event Hub.
- **Query Performance Insight**: the in-portal performance
  dashboard, enabled per-database via the Azure SQL Analytics
  feature.

**Detection rule:** instance is INSTRUMENTED if it has at least
one Diagnostic Setting routing the `SQLInsights` log category to
ANY destination. We pick this over Query Performance Insight
because it's the operator-controllable rebuild axis (Query
Performance Insight is mostly always-on at the portal level but
the durable telemetry pipeline requires Diagnostic Settings).

**Recommendation kind:** `azsql-diag-enable`.

**Terraform target:**
`azurerm_monitor_diagnostic_setting` resource targeting the SQL
database with a `enabled_log { category = "SQLInsights" }` block.

### 3.3 OCI DB Systems + Autonomous Database

OCI has two database product families:
- **DB Systems** (VM / Bare Metal Oracle DB instances).
- **Autonomous Database** (fully-managed Oracle, transaction
  processing + data warehouse variants).

For slice 2, treat both uniformly: detection scans both lists and
applies the same rule.

**Primitive: Operations Insights enrollment + Performance Hub.**

OCI Operations Insights (formerly Database Management) provides
the observability surface. An instance is enrolled when the
`database.management` resource block exists and a Management
Agent is associated.

**Detection rule:** instance is INSTRUMENTED if its
`databaseManagementConfig.databaseManagementStatus == "ENABLED"`.

**Recommendation kind:** `ocidb-perfhub-enable`.

**Terraform target:**
`oci_database_db_systems_management` resource enabling the
management API for the DB system, or the equivalent block on
`oci_database_autonomous_database`.

### 3.4 Why no cross-cloud unified rule?

A unified rule like "instance has a tag `otel*`" (the slice 1
compute rule) doesn't apply here. Database observability is
about turning on a cloud-native feature, not about deploying an
OTel collector to the database instance. The four clouds each
have their own observability primitive; slice 2 treats them
parallel-but-different.

## 4. Storage and snapshot type

The existing `scanner.DatabaseInstanceSnapshot` (defined in
`internal/discovery/scanner/scanner.go` per the AWS RDS slice in
v0.87.0) accepts:

```go
type DatabaseInstanceSnapshot struct {
    ResourceID                  string
    Engine                      string  // "postgres" / "mysql" / "oracle" / "mssql"
    EngineVersion               string
    InstanceClass               string  // provider-specific size string
    HasOTel                     bool    // legacy from compute pattern; ignored here
    PerformanceInsightsEnabled  bool    // AWS RDS PI axis
    EnhancedMonitoringEnabled   bool    // AWS RDS EM axis
    Region                      string
    Tags                        map[string]string
}
```

Slice 2 extends this with three new optional axes (one per cloud's
new detection rule):

```go
type DatabaseInstanceSnapshot struct {
    // ... existing fields ...
    
    // GCP Cloud SQL: settings.insightsConfig.queryInsightsEnabled
    QueryInsightsEnabled bool `json:"query_insights_enabled,omitempty"`
    
    // Azure SQL: at least one Diagnostic Setting routing SQLInsights
    SQLInsightsDiagEnabled bool `json:"sql_insights_diag_enabled,omitempty"`
    
    // OCI: databaseManagementStatus == "ENABLED"
    DatabaseManagementEnabled bool `json:"database_management_enabled,omitempty"`
    
    // Provider discriminator (helps the proposer route to the right
    // recommendation kind). Empty defaults to AWS for backward compat
    // with v0.87.0 audit rows.
    Provider string `json:"provider,omitempty"`
}
```

The proposer reads `Provider` plus the appropriate boolean to
decide whether to emit a recommendation, and which kind. AWS RDS
slice 1 logic (PI + EM both-required) remains unchanged.

### 4.1 Result.FailedServices identifiers

- GCP Cloud SQL scanner: `cloudsql`
- Azure SQL scanner: `azuresql`
- OCI DB Systems / Autonomous Database scanner: `ocidb`

## 5. Per-cloud scanner extensions

Each cloud's existing scanner package gets a new method
`ScanDatabases(ctx) (scanner.Result, error)` that returns just the
database snapshot rows, OR the existing `Scan` method extends to
also walk the database surface (sharing the auth setup).

Picked: extend `Scan` to walk both compute and database surfaces
within a single call. Reduces the number of HTTP round-trips and
matches the AWS scanner's pattern (which already walks multiple
services in one Scan).

### 5.1 GCP scanner extension

Adds `sql.googleapis.com` API calls:
- `GET /sql/v1beta4/projects/{project}/instances`
- For each instance, extract
  `settings.insightsConfig.queryInsightsEnabled`.

IAM scope: existing `roles/compute.viewer` doesn't cover Cloud
SQL. Operators need to add `roles/cloudsql.viewer` for slice 2.
The runbook update documents this.

### 5.2 Azure scanner extension

Adds ARM API calls:
- `GET /subscriptions/{sub}/providers/Microsoft.Sql/servers` →
  list SQL servers.
- For each server: `GET /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Sql/servers/{server}/databases`.
- For each database:
  `GET /subscriptions/{sub}/.../databases/{db}/providers/microsoft.insights/diagnosticSettings`
  to check for SQLInsights routing.

Service Principal Reader role at subscription scope already covers
all three reads.

### 5.3 OCI scanner extension

Adds OCI API calls:
- DB Systems: `GET https://database.<region>.oraclecloud.com/20160918/dbSystems?compartmentId=<comp>`
- Autonomous Database: `GET https://database.<region>.oraclecloud.com/20160918/autonomousDatabases?compartmentId=<comp>`
- For each, extract `databaseManagementConfig.databaseManagementStatus`.

IAM policy: existing
`Allow group SquadronDiscovery to read instance-family in tenancy`
doesn't cover databases. Add:
- `Allow group SquadronDiscovery to read database-family in tenancy`

Runbook documents.

## 6. Proposer integration

The proposer's recommendation kind enumeration extends:

```text
For GCP (slice 2 additions):
- cloudsql-pi-enable: Enable Query Insights on a Cloud SQL
  instance where queryInsightsEnabled=false. Terraform:
  google_sql_database_instance.settings[0].insights_config[0].
  query_insights_enabled = true.

For Azure (slice 2 additions):
- azsql-diag-enable: Add a Diagnostic Setting routing SQLInsights
  to a Log Analytics workspace for an Azure SQL Database that
  lacks one. Terraform: azurerm_monitor_diagnostic_setting.

For OCI (slice 2 additions):
- ocidb-perfhub-enable: Enable Operations Insights / Database
  Management on a DB System or Autonomous Database where
  databaseManagementStatus != ENABLED. Terraform:
  oci_database_db_systems_management or the autonomous variant.
```

Branch encoding extends:
- `cloudsql-*` prefix → provider="gcp"
- `azsql-*` prefix → provider="azure"
- `ocidb-*` prefix → provider="oci"

These all route correctly through the existing
`parseRecommendationScopeFromBranch` helper since the encoding
shape (`squadron/rec/<kind>/<scope>/<region>/<id>`) is unchanged.
The kind prefix routing in the webhook handler extends:

```go
switch {
case strings.HasPrefix(kind, "gce-") || strings.HasPrefix(kind, "cloudsql-"):
    provider = "gcp"
case strings.HasPrefix(kind, "vm-") || strings.HasPrefix(kind, "azsql-"):
    provider = "azure"
case strings.HasPrefix(kind, "compute-") || strings.HasPrefix(kind, "ocidb-"):
    provider = "oci"
default:
    provider = "aws"
}
```

## 7. Audit events

Six new constants — one scan_completed extension per cloud (the
existing audit event keys are reused; the payload gains
`database_instance_count`, `database_instrumented_count`,
`database_uninstrumented_count` fields).

No new event types — keeps the audit timeline coherent (one
"scan_completed" per provider regardless of how many service
categories the scan walked).

## 8. UI updates

Per-provider Inventory tabs (DiscoveryGCP, DiscoveryAzure,
DiscoveryOCI) gain a Databases sub-tab inside Inventory:

- Existing Compute tab stays.
- New Databases tab shows the projected
  DatabaseInstanceSnapshot rows.

Columns: Resource ID, Engine, Engine Version, Size, Provider-axis
boolean (instrumented?), Region, Tags.

The Recommendations tab automatically surfaces the new
recommendation kinds without UI changes — the rendering is
generic over kind.

The unified Discovery dashboard (v0.89.62) sums compute +
database counts into the per-provider totals automatically since
ProviderSummary already aggregates from scan_completed events. No
dashboard code changes needed.

## 9. Slice 2 contract

**In:**

1. Extended DatabaseInstanceSnapshot with 4 new fields
   (QueryInsightsEnabled, SQLInsightsDiagEnabled,
   DatabaseManagementEnabled, Provider).
2. GCP scanner extension: Cloud SQL walker + Query Insights
   detection rule.
3. Azure scanner extension: SQL Server / Database walker +
   Diagnostic Settings probe.
4. OCI scanner extension: DB Systems + Autonomous Database
   walker + Operations Insights status check.
5. Proposer prompt extension: three new recommendation kinds
   documented.
6. Webhook handler kind-prefix detection extended for
   cloudsql-/azsql-/ocidb- prefixes.
7. UI per-provider Inventory tab gains Databases sub-tab.
8. Per-provider runbook updates documenting the new IAM
   permission asks (Cloud SQL viewer / database-family read).
9. Tests covering each new scanner extension + the proposer's
   new recommendation kinds + the webhook prefix routing.

**Out:**

- AWS RDS expansion.
- Aurora-specific signals (slice 3).
- AlloyDB / Cosmos / Autonomous Data Warehouse separation (slice 3).
- Cross-cloud replication topology.
- Query plan analysis.
- Cross-cloud recommendation prioritization.

## 10. Implementation chunks

Tighter than the slice 1 cloud arcs because the substrate is
established. Three implementations can run in parallel (one per
cloud), each ~600-800 lines including tests:

- **Chunk 1: DatabaseInstanceSnapshot extension + audit payload
  schema.** ~200-300 lines. Shared across all three cloud
  implementations. v0.89.64.
- **Chunk 2: GCP Cloud SQL scanner extension.** ~600-800 lines.
  v0.89.65 (parallel).
- **Chunk 3: Azure SQL scanner extension.** ~700-900 lines.
  v0.89.65 (parallel).
- **Chunk 4: OCI Database scanner extension.** ~700-900 lines.
  v0.89.65 (parallel).
- **Chunk 5: Proposer + webhook routing + UI Databases sub-tab.**
  ~600-800 lines. v0.89.66.
- **Chunk 6: Three runbook updates (one per provider).** ~300-400
  lines. v0.89.67.

Total: 4-5 release tags. Chunks 2 + 3 + 4 in one fan-out worktree
pass. Chunk 5 sequential (touches integration points). Chunk 6
finalizes documentation.

## 11. Acceptance tests

1. **GCP Cloud SQL detection:** seed a Cloud SQL instance with
   queryInsightsEnabled=true. Assert: snapshot
   QueryInsightsEnabled=true, no recommendation emitted.
2. **GCP Cloud SQL uninstrumented:** queryInsightsEnabled=false.
   Assert: cloudsql-pi-enable recommendation emitted.
3. **Azure SQL detection:** Diagnostic Setting routes SQLInsights
   → SQLInsightsDiagEnabled=true, no recommendation.
4. **Azure SQL uninstrumented:** no SQLInsights Diagnostic
   Setting. Assert: azsql-diag-enable recommendation emitted.
5. **OCI DB Systems detection:** databaseManagementStatus=ENABLED
   → DatabaseManagementEnabled=true, no recommendation.
6. **OCI Autonomous Database uninstrumented:** status != ENABLED.
   Assert: ocidb-perfhub-enable recommendation emitted.
7. **Per-cloud cold-start parity preserved.** Existing compute-
   only scan results still parse correctly in the audit timeline
   (the new database_* fields are optional / omitempty).
8. **Webhook routing for slice 2 kinds.** cloudsql-pi-enable
   branch parses to provider="gcp". Same for azsql-/ocidb-.
9. **Proposer prompt extension.** Three new kinds appear in the
   system prompt when corresponding scan data is present.
10. **UI Databases sub-tab renders.** Per-provider Inventory has
    Compute + Databases tabs visible.

## 12. Threat model

Inherits per-cloud threat models from slice 1. New surface area:

- **Cloud SQL listing requires `roles/cloudsql.viewer`.** Operators
  must add this scope to the existing SA. The runbook makes this
  explicit and surfaces a `permission_denied` error_kind specific
  to the Cloud SQL scope being missing.
- **Azure Diagnostic Settings query requires the Reader role at
  subscription scope.** Already granted; no new permission ask.
- **OCI database read permission requires the new policy
  statement.** Runbook documents.

No new credential domains — the slice 2 scanners reuse the slice
1 credential model (GCP SA JSON, Azure SP secret, OCI signing
key) sealed via credstore.

---

**Strategic frame:**

Slice 2 is the first **depth** arc after four-cloud breadth
closed. It doubles the dimensions of the recommendation surface
(compute instrumentation + database instrumentation) across three
clouds, bringing them to parity with AWS (which already has both
since v0.87.0).

After this arc closes, the universal claim becomes "Squadron
scans AWS, GCP, Azure, AND Oracle Cloud for COMPUTE and DATABASE
observability gaps across the full fleet." That's the strongest
slice-1+2 version of the claim a single OSS control plane can
make.

Slice 3 candidates: Kubernetes tier (EKS / GKE / AKS / OKE all
have slice 1 only — extend to slice 2/3 with workload-level
detection). Or trace integration (start consuming OTel traces
from the connectors so Squadron can spot missing-span gaps).
Or cost-spike + drift correlation across providers.

The depth pattern is now established: pick a tier, scan it
across all 4 clouds, ship in a single fan-out arc with parallel
worktrees. Same throughput pattern as breadth.
