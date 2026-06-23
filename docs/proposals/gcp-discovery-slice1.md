# GCP discovery — slice 1 design

**Status:** design doc, locked for slice 1 implementation. This is
the first universal-observability arc beyond AWS. The AWS discovery
arc (#558, shipped across v0.85 through v0.89) established the
scanner interface, the credential substrate (credstore), the
recommendation model, and the wizard framework. GCP discovery
reuses those substrates and adds a GCP-specific scanner + connector
+ wizard content.

The strategic frame: Squadron's positioning claim is "the universal
observability control plane." The webhook and verdict-learning
arcs that closed in the last 48 hours added depth on AWS. This arc
adds breadth — Squadron now scans a second cloud provider. After
slice 1 ships, the operator-facing claim becomes "Squadron scans
your AWS AND GCP fleets for observability gaps and drafts the IaC
PRs that close them." That's the first concrete step toward
universal.

**See also:**
[AWS discovery design (#558)](./558-discovery-design.md),
[discovery-iac-first-time-setup.md](../discovery-iac-first-time-setup.md),
[ai-features.md](../ai-features.md),
[concepts.md](../concepts.md).

## 1. Problem

Squadron's AWS discovery slice 1 (v0.85.0) shipped a scanner that
walks an AWS account's EC2, RDS, S3, ALB, EKS, DynamoDB, and ECS
resources looking for observability gaps. The proposer drafts
recommendations. The operator clicks Open PR. The PR shows up in
their IaC repo. The loop closes.

That loop only works for AWS. Operators with multi-cloud fleets
have GCP resources that Squadron is blind to. The proposer cannot
recommend instrumentation for GCE instances, Cloud SQL databases,
or GKE clusters because there is no scanner that sees them. The
recommendation surface is exactly half of what it should be.

The universal observability claim cannot survive a multi-cloud
operator asking "does Squadron see my GCP project?" today. Slice
1 of this arc closes that gap with a single GCP service category
(Compute Engine, the GCE / virtual machine surface) and the
substrate to grow into the other categories in slices 2 through 5.

## 2. Non-goals (slice 1)

- **Cloud SQL.** The RDS analog. Slice 2 candidate. Mirror the AWS
  RDS scanner's PerformanceInsightsEnabled +
  EnhancedMonitoringEnabled two-axis rule but against Cloud SQL's
  Query Insights setting.
- **GKE.** The EKS analog. Slice 3 candidate. Mirror the AWS EKS
  scanner's ADOT addon detection but against GKE's Managed Service
  for Prometheus + Cloud Logging integration.
- **Cloud Storage.** The S3 analog. Slice 4 candidate.
- **Cloud Load Balancing.** The ALB analog. Slice 5 candidate.
- **Workload Identity Federation (WIF).** Per §3, slice 1 ships
  Service Account JSON key authentication only. WIF is the more
  secure path for production but adds wizard complexity that
  doesn't fit slice 1's scope. Slice 2 candidate.
- **Multi-project scans.** AWS shipped multi-account orchestration
  in v0.89.7a. The GCP analog is multi-project scanning. Slice 1
  ships single-project; slice 2 mirrors the orchestrator pattern.
- **Cross-organization scans.** GCP Organizations are a higher
  layer above projects. Slice 1 connects per-project; org-level
  discovery is slice 3+ work.
- **GCP-specific recommendation kinds beyond gce-otel-tag.**
  Slice 1 has one recommendation kind: tag-based OTel instrumentation
  detection for GCE instances. Cloud-native recommendations
  (Cloud Trace agent installation, OpenTelemetry Collector deployment
  via Cloud Run) are slice 2 candidates.
- **GCP Asset Inventory API.** A more powerful query interface
  than per-service list calls. Worth using in slice 2 for multi-
  service scans; slice 1 sticks with the direct Compute Engine
  API to keep the implementation parallel to the AWS scanner.
- **Wizard for OAuth-based credentials.** Some operators prefer
  OAuth user-context credentials over SA keys for development.
  Slice 1 ships SA key only.

## 3. Architectural decision: credential model

GCP authentication has three real options. Each materially shapes
slice 1 and beyond.

### Option A — Service Account JSON key, sealed via credstore

Operator creates a GCP Service Account in their project, downloads
a JSON key file, pastes the JSON into Squadron's wizard. Squadron
seals the JSON via credstore (same substrate as v0.85's AWS PAT
and v0.89.31's per-connection webhook secrets). The scanner
unseals at scan time, instantiates a google-cloud-go client with
the credentials.

**Picked for slice 1.** Mirrors the AWS PAT path that operators
already know. Credstore substrate exists; no new sealing logic.
The wizard step is "paste this JSON" which is a single field.

### Option B — Workload Identity Federation (WIF) with token exchange

Operator configures a WIF pool in GCP pointing at Squadron's
deployment identity (e.g. a Kubernetes service account, an AWS
IAM role, an OIDC token issued by Squadron itself). Squadron
exchanges a federated identity token for a short-lived GCP access
token at scan time. No long-lived credential lives in Squadron.

**Deferred to slice 2.** The security posture is strictly better
than SA keys — no key material to compromise, automatic short
lifetime, native GCP audit trail. But the wizard flow is materially
more complex (operator configures WIF in GCP console, copies the
provider name back to Squadron, Squadron's deployment identity
needs to be a known shape), and the implementation requires
Squadron to know its own identity (its OIDC issuer URL, its
service account if running on GKE, etc.). Slice 1 operators with
sandbox projects don't have a real need for WIF; slice 2 ships it
for production operators.

### Option C — Application Default Credentials (ADC) inherited from the host

If Squadron runs on GCE / GKE / Cloud Run, GCP's ADC client picks
up the host's service account automatically. No wizard step at all.

**Rejected for slice 1.** Couples Squadron to GCP for its own
deployment. Squadron's value is that it runs anywhere — a $5 VPS,
a Kubernetes cluster on AWS, a developer's laptop. Requiring
operators to deploy Squadron on GCP just to scan GCP is bad
posture. ADC is a slice 2 fallback for operators who DO deploy
Squadron on GCP and want zero-config credentials, but it should
never be the only path.

## 4. Architectural decision: scanner interface

The AWS scanner (`internal/discovery/aws`) implements the
`scanner.Scanner` interface defined in `internal/discovery/scanner`.
The interface is provider-agnostic by design: it returns a
`scanner.Result` carrying typed snapshot slices
(ComputeInstanceSnapshot, DatabaseInstanceSnapshot, etc.) plus
partial-failure metadata.

GCP discovery reuses the SAME interface. A new
`internal/discovery/gcp` package implements `Scanner` with a
`GCPScanner` struct that owns a google-cloud-go compute client +
project ID. The Scan method walks the project's GCE instances and
returns a Result with `ComputeInstanceSnapshot` entries.

The provider-agnostic snapshot types (ComputeInstanceSnapshot,
DatabaseInstanceSnapshot) already accommodate GCP naming —
`InstanceType` is "raw string" (n2-standard-4 fits next to
m5.large), `OSFamily` is normalized to linux/windows/unknown,
Tags map[string]string accepts GCP labels directly. No type
changes needed.

The Result.FailedServices identifier vocabulary extends to include
GCP service names: "gce" for Compute Engine, "cloud_sql" for slice
2's database surface, etc. The AWS scanner already uses bare
service names ("ec2", "rds"); GCP uses "gce", "cloud_sql" —
unprefixed because the connection model (§5) carries the provider
discriminator separately.

## 5. Storage

A new `GCPConnection` row type parallel to the existing
`AWSConnection` (renamed `CloudConnection` in v0.85's
generalization refactor). Look at `internal/discovery/clouds/types.go`
or the equivalent to see the current CloudConnection shape — slice
1 extends it with a `Provider` field if not present and adds
GCP-specific fields.

```go
type GCPConnection struct {
    ID         string
    DisplayName string
    ProjectID  string
    SealedSA   []byte  // credstore-sealed Service Account JSON
    CreatedAt  time.Time
    UpdatedAt  time.Time
    
    // Slice 1 ships single-region; slice 2 adds region selection
    // (multi-region scans are slice 2).
    Region string  // e.g. "us-central1"; default "" means "scan all regions"
    
    // Reuses the v0.89.27/v0.89.28 LearnFromAcceptedRecommendations
    // flag once the GCP side feeds the discovery proposer's verdict
    // learning loop. Not load-bearing in slice 1 because no PRs
    // have merged on the GCP side yet, but the column exists for
    // consistency with the IaC-connected shape.
    LearnFromAcceptedRecommendations bool
}
```

A new `gcpconnstore` package mirroring `internal/discovery/awsconnstore`
or `iacconnstore`. SQLite schema migration v9 → v10 adds:

```sql
CREATE TABLE gcp_connections (
    id                                  TEXT PRIMARY KEY,
    display_name                        TEXT NOT NULL,
    project_id                          TEXT NOT NULL,
    sealed_sa                           BLOB NOT NULL,
    region                              TEXT,
    learn_from_accepted_recommendations INTEGER NOT NULL DEFAULT 1,
    created_at                          TIMESTAMP NOT NULL,
    updated_at                          TIMESTAMP NOT NULL
);
```

The `iac_recommendation_verdicts` table from #531 slice 2 chunk 4
already carries `connection_id` as a generic TEXT field (not
foreign-keyed to any specific connection table), so GCP-scoped
exclusions slot in without schema changes.

### 5.1 Why GCPConnection is its own type

Slice 0 of "universal observability" might have argued for a
single polymorphic CloudConnection type with provider discriminator.
We reject that for slice 1:

- The credential shapes are different (AWS: role ARN + ExternalID;
  GCP: SA JSON). Forcing them into the same row with nullable
  fields is worse than two clear types.
- The scan shapes are different (AWS: regions list + assume-role
  per region; GCP: project ID + optional region filter). Same
  argument.
- Each provider's wizard reads its own connection type and
  knows its own field set. No client-side polymorphism needed.

Slice 2's multi-cloud reporting surface (a unified "all your
fleets" inventory view) can derive the common projection at query
time without forcing the storage layer to flatten now.

## 6. API endpoints

Mirror the AWS surface at `/api/v1/discovery/gcp/*`:

- `POST   /api/v1/discovery/gcp/connections` — create connection
- `GET    /api/v1/discovery/gcp/connections` — list connections
- `GET    /api/v1/discovery/gcp/connections/:id` — get connection
- `PATCH  /api/v1/discovery/gcp/connections/:id` — update connection
- `DELETE /api/v1/discovery/gcp/connections/:id` — delete connection
- `POST   /api/v1/discovery/gcp/connections/:id/validate` — dry-run scan: list 1 instance from each region the SA can see, confirm credentials work
- `POST   /api/v1/discovery/gcp/connections/:id/scan` — synchronous full scan
- `POST   /api/v1/discovery/gcp/connections/:id/recommendations` — run the proposer against a recent scan and return recommendations

Body shapes mirror AWS counterparts. The POST /connections body
accepts `{display_name, project_id, sealed_sa: <base64>, region}`.
SA JSON is base64-encoded over the wire to avoid JSON-in-JSON
escape pain; the server base64-decodes then credstore-seals before
storage.

### 6.1 Validate flow

The validate endpoint is the operator's confidence check that
their SA has the right scopes. It:

1. Unseals the stored SA JSON.
2. Instantiates a google-cloud-go compute client.
3. Calls `instances.list` on the first available zone in the
   configured region (or any zone if Region is empty).
4. Returns `{ok: true, instance_count: N}` on success.
5. On failure: returns `{ok: false, error_kind: "..."}` with kind
   in {permission_denied, project_not_found, network, credentials_invalid}.

The validate endpoint catches the common operator mistake of "I
created a SA but forgot to grant it compute.viewer." Operator gets
an immediate, actionable error instead of a silent half-empty
scan.

## 7. UI

A new `/discovery/gcp` page paralleling `/discovery/aws`. Same tab
structure: Wizard / Inventory / Recommendations. The page is a
near-clone of `DiscoveryAWS.tsx`; slice 1 extracts the shared
parts into a `DiscoveryConnectionPage` component or simply
duplicates with documented dedup as a slice 2 candidate.

The wizard differs in step content but not structure:

**Step 1: Connect a GCP project**
- Project ID input field with validation against GCP project naming
  rules ([a-z][-a-z0-9]{4,28}[a-z0-9])
- "Why am I doing this?" explainer link

**Step 2: Create a Service Account**
- Instructions for the operator: `gcloud iam service-accounts create
  squadron-discovery --display-name "Squadron Discovery"`
- Grant role: `gcloud projects add-iam-policy-binding <project> \
  --member="serviceAccount:squadron-discovery@<project>.iam.gserviceaccount.com" \
  --role="roles/compute.viewer"`
- (Slice 2 adds Cloud SQL viewer and Container viewer for those
  scanners.)

**Step 3: Download key and paste into Squadron**
- Instructions: `gcloud iam service-accounts keys create key.json \
  --iam-account=squadron-discovery@<project>.iam.gserviceaccount.com`
- Paste-textarea for the JSON contents.
- Warning: "This key grants read access to your GCE inventory.
  Squadron seals it at rest with AES-GCM; the bytes never appear
  in audit payloads or logs."

**Step 4: Validate**
- Click button → calls validate endpoint → shows result.
- On success: "Connected ✓ — N instances visible."
- On failure: humanized error with specific remediation by
  error_kind.

**Step 5: Scan**
- Click button → calls scan endpoint → renders inventory + offers
  to draft recommendations.

The wizard component framework from v0.85 already handles step
progression, validation, deep-linking. The wizard step data lives
in `ui/src/data/gcpDiscoveryWizard.ts` mirroring
`iacGithubWizard.ts`.

## 8. Slice 1 service category: Compute Engine

For slice 1, the scanner walks `compute.instances.list` across the
project. The instrumentation rule is the same single-axis tag
heuristic the AWS EC2 scanner uses in slice 1: a tag (GCP "label")
key matching `otel*` case-insensitive marks the instance as
instrumented.

Why match the AWS slice 1 rule? Symmetry across providers makes
the recommendation kinds parallel. Operators with mixed fleets see
the same recommendation shape ("instance lacks OTel label") on
both sides. Slice 2 adds richer signals (CloudWatch Container
Insights analog → Cloud Monitoring agent detection) once the
breadth foundation is solid.

The recommendation kind for GCP Compute Engine slice 1:
`gce-otel-label` (mirroring AWS's `ec2-otel-tag` — the rename
captures GCP's "label" terminology).

The proposer's recommendation reasoning template is shared with
AWS via a small templating helper; only the resource description
substring changes.

## 9. Proposer integration

The discovery proposer at
`internal/ai/proposer_discovery.go::ProposeFromDiscoveryScan`
takes a `DiscoveryScanContext` and emits recommendations. Slice 1
extends the context to carry a `Provider string` discriminator
("aws" or "gcp") so the system prompt's instruction set can adapt:

- "Recommendations of kind=`gce-otel-label` apply to GCP Compute
  Engine instances. The PR should add a label to the
  google_compute_instance Terraform resource."
- "Recommendations of kind=`ec2-otel-tag` apply to AWS EC2
  instances. The PR should add a tag to the aws_instance
  Terraform resource."

The system prompt for slice 1 carries both kinds; the user
message describes which scope was scanned (provider + project_id
+ region for GCP, provider + account_id + region for AWS). The
existing `verdict_examples_used_by_state` scope filter (chunk 6 of
#531 slice 2) already buckets by `(connection_id, account_id,
region)` for AWS — slice 1 extends this to scope on `(connection_id,
project_id, region)` for GCP. The bridge layer abstracts the
"scope tuple" so the proposer and the verdict learning loop don't
need to know which provider authored the scan.

### 9.1 Branch name encoding for GCP

The v0.89.28 branch encoding for Squadron-opened PRs is
`squadron/rec/<kind>/<account_id>/<region>/<short_id>`. For GCP,
account_id maps to project_id directly. Branch shape:
`squadron/rec/<kind>/<project_id>/<region>/<short_id>`.

The webhook receiver's `parseRecommendationScopeFromBranch` helper
returns `(kind, scope_id, region, ok)` where scope_id is provider-
agnostic. Audit payloads use `scope_id` (not `account_id`) going
forward. Existing AWS payloads carrying `account_id` continue to
work via backward-compat read path.

The branch-name encoding change is a soft break in the audit
schema: GCP `recommendation.pr_merged` events carry both
`account_id` (empty for GCP) and `project_id` (set for GCP).
SIEM consumers reading historical events get unchanged shape;
new consumers read `project_id` when `provider="gcp"`.

## 10. Slice 1 contract

**In:**

1. `internal/discovery/gcp/scanner.go`: GCPScanner with Scan
   method walking Compute Engine instances, returning
   scanner.Result.
2. `internal/discovery/gcp/types.go`: any GCP-specific helpers.
3. `internal/discovery/gcpconnstore/`: new package paralleling
   awsconnstore / iacconnstore. SQLite + memory impls.
4. Schema migration v9 → v10: gcp_connections table.
5. SA JSON sealing helper at `internal/discovery/credstore/gcp_sa.go`
   paralleling `webhook_secret.go` (v0.89.31) with a domain-tagged
   AAD `"squadron.gcp_sa.v1"`.
6. Audit event constants:
   - `AuditEventDiscoveryGCPConnectionCreated`
   - `AuditEventDiscoveryGCPConnectionDeleted`
   - `AuditEventDiscoveryGCPScanStarted`
   - `AuditEventDiscoveryGCPScanCompleted`
   - `AuditEventDiscoveryGCPScanFailed`
   - `AuditEventDiscoveryGCPRecommendationsGenerated`
7. HTTP handlers in `internal/api/handlers/discovery_gcp.go`
   implementing the §6 endpoint surface.
8. UI page at `ui/src/pages/DiscoveryGCP.tsx` paralleling
   DiscoveryAWS.tsx, with the wizard / inventory / recommendations
   tab structure.
9. Wizard step data at `ui/src/data/gcpDiscoveryWizard.ts`.
10. Proposer extension: `Provider` field on DiscoveryScanContext,
    new `gce-otel-label` recommendation kind, system prompt
    addition documenting the GCP path.
11. Branch encoding helper extension:
    `parseRecommendationScopeFromBranch` returns provider-agnostic
    scope_id (currently account_id, now also accepts project_id).
12. Operator runbook at `docs/discovery-gcp-first-time-setup.md`
    mirroring the AWS counterpart.
13. End-to-end acceptance test: bring up a GCP sandbox project,
    create a connection via wizard, validate, scan, draft
    recommendations, open PR through the IaC integration. Manual
    for slice 1; automated against a fake GCP API in slice 2.

**Out:**

- Cloud SQL, GKE, Cloud Storage, Cloud Load Balancing scanners.
- Workload Identity Federation auth path.
- Application Default Credentials inheritance.
- Multi-project scans.
- Cross-organization discovery.
- GCP Asset Inventory API consumption.
- Wizard for OAuth-based credentials.
- Per-region orchestration (slice 1 ships single-region with
  empty-region "scan all").
- GCP-specific recommendation kinds beyond gce-otel-label.

## 11. Threat model

The AWS arc's threat model covers credential-at-rest (credstore
seal), credential-in-transit (HTTPS only), and recommendation
provenance (audit emit before any external side effect). The GCP
arc inherits all three with adjustments.

### 11.1 SA JSON at rest

The SA JSON key carries the full content of a GCP-issued private
key. If extracted from Squadron's storage, the key authenticates
to GCP as the operator's service account until the operator
explicitly revokes it from the GCP console.

Mitigation:
- Sealing via credstore with domain-tagged AAD
  `"squadron.gcp_sa.v1"` (parallel to v0.89.31's webhook secret
  domain separator).
- Never log the unsealed bytes. Never embed in audit payloads.
  Never include in error responses (parallel to v0.89.31's posture
  for the webhook secret).
- The wizard explainer tells operators to set the SA to expire
  after slice 1 verification, and to rotate annually as
  hygiene. The runbook documents the rotation procedure.

### 11.2 GCP IAM scope

Slice 1's wizard documents granting `roles/compute.viewer` (the
predefined role for read-only Compute Engine access). This role
permits listing instances, fetching their metadata, and reading
labels — exactly what the scanner needs. It does NOT permit any
write operations on the project.

Operators with stricter posture preferences can create a custom
role with only `compute.instances.list` and `compute.instances.get`
permissions. Runbook documents this alternative.

### 11.3 Project-not-found and silent half-empty scans

If the operator's SA was created in project A but the connection
configures project B, GCP's compute API returns an empty list
without error. The scanner sees "0 instances" and emits a
scan_completed event with the partial flag unset — which is
wrong.

Mitigation: the validate endpoint cross-checks the SA's project
binding against the configured project_id. On mismatch: validate
returns `{ok: false, error_kind: "project_mismatch"}` with a
humanized message. The scan endpoint also performs this check
on first run.

### 11.4 Cross-tenant SA leak

Squadron's deployment is single-tenant. An operator who pastes a
GCP SA JSON for project A and another for project B in two
different connections does NOT see them cross. The SA JSON
sealed bytes are stored per-connection with the connection_id as
part of the row; the scanner reads the SA bound to the specific
connection being scanned.

Multi-tenant Squadron deployments (not in scope for slice 1) would
need additional isolation. Document as a slice-3+ consideration.

## 12. Open questions

1. **Service Account email validation.** The wizard could parse
   the SA JSON's `client_email` field and validate it ends in
   `.iam.gserviceaccount.com` to catch operators who paste the
   wrong file. Add to slice 1 — it's a small validation but
   catches a real-world failure mode.

2. **Region selection UI.** Slice 1 ships single-region with
   empty = "scan all regions." Operators with very large GCE
   fleets across many regions may hit rate limits during scan.
   Worth surfacing region selection in the wizard? Slice 1 says
   no — keep the wizard simple; slice 2 adds region selection.

3. **GCE preemptible instances.** Some GCE instances are
   short-lived (preemptible). Slice 1 includes them in the
   recommendation pool; the resulting PR's labels survive the
   instance lifetime since labels apply to the Terraform resource
   that recreates the instance. No special handling needed.

4. **Project ID vs project number.** GCP exposes both. Slice 1
   uses project ID exclusively because it's operator-readable
   and stable. The compute client accepts either.

5. **OS family detection.** AWS EC2 returns OS metadata in the
   describe response. GCE returns it as instance metadata under
   `licenseCodes` (with a separate lookup). Slice 1 sets
   `OSFamily="unknown"` for GCP instances and defers proper
   detection to slice 2 once the cost/benefit of the extra API
   call is clear.

6. **OAuth scope vs IAM role.** The SA JSON itself doesn't carry
   the IAM bindings; those live on the GCP project. If an
   operator grants `roles/owner` (not `roles/compute.viewer`),
   the validate endpoint still succeeds — Squadron can't see the
   role assignment. Surface this as a runbook note: "If validate
   succeeds but the SA has broader scope than compute.viewer,
   tighten the binding."

7. **Connection rename in audit payloads.** The existing AWS
   audit shape uses `account_id`. The GCP shape uses
   `project_id`. Should the audit field be renamed to a
   provider-agnostic `scope_id`? Pick: keep both fields, populate
   the relevant one for each scan, document the shape in the
   runbook. SIEM consumers can read either. Soft schema change
   in slice 1.

## 13. Acceptance tests

1. **Create + read GCP connection round-trip.** POST /connections
   with valid body. Assert: 201, returned ID stable. GET /connections
   returns the row. GET /connections/:id returns the full row with
   `sealed_sa` absent from the response (security: never echo
   sealed credential bytes).

2. **Seal + unseal SA JSON round-trip.** Unit test on
   `internal/discovery/credstore/gcp_sa.go::SealGCPServiceAccount`
   and `UnsealGCPServiceAccount`. Assert: marshalled bytes
   decrypt to original JSON, version byte intact, domain tag
   distinguished from PAT seal and webhook secret seal.

3. **Validate endpoint with mocked compute API.** Stand up a
   httptest server emulating the compute.instances.list response.
   Configure a connection pointing at it. Call validate. Assert:
   `{ok: true, instance_count: <seeded count>}`.

4. **Validate endpoint with permission_denied.** Mock GCP returns
   403. Call validate. Assert: `{ok: false, error_kind:
   "permission_denied"}`.

5. **Scan endpoint produces ComputeInstanceSnapshot entries.**
   Mock GCP returns 3 instances with various label sets. Call
   scan. Assert: response carries 3 snapshot entries with
   `HasOTel` true for instances with otel* labels and false
   otherwise. PartialReason empty, FailedServices empty.

6. **Scan endpoint partial failure on rate limit.** Mock GCP
   returns 429 partway through a multi-page response. Call scan.
   Assert: partial=true, partial_reason mentions GCP rate limit,
   failed_services contains "gce".

7. **Recommendations endpoint emits gce-otel-label kind.** Seed
   a scan result with 2 GCE instances missing OTel labels. Call
   recommendations. Assert: proposer returns at least 1
   recommendation with kind="gce-otel-label".

8. **Audit events fire correctly across the wizard flow.** Walk
   the wizard end-to-end (create connection → validate → scan →
   recommendations). Assert the audit timeline emits, in order:
   discovery.gcp.connection_created, discovery.gcp.scan_started,
   discovery.gcp.scan_completed, discovery.gcp.recommendations_generated.

9. **Project mismatch detection.** SA JSON's project_id field
   differs from the connection's configured project_id. Call
   validate. Assert: `{ok: false, error_kind: "project_mismatch"}`.

10. **UI smoke test: wizard step progression.** Render the
    DiscoveryGCP page. Assert: the 5 wizard steps render, the
    project_id field validates against GCP project naming rules,
    the SA JSON paste textarea accepts JSON content, the validate
    button is enabled only after all required fields are filled.

11. **Cold-start parity on proposer side.** Scan a fresh
    connection with zero prior accepted recommendations. Assert:
    discovery proposer prompt does NOT include the verdict
    examples block (chunk 1 of #531 slice 2 cold-start parity
    preserved when the connection's scope tuple matches no audit
    rows).

12. **Branch encoding round-trips through webhook.** Open a PR
    via the IaC integration for a gce-otel-label recommendation.
    Branch name encodes provider-agnostic
    `squadron/rec/gce-otel-label/<project_id>/<region>/<id>`.
    Webhook receiver on merge produces audit event with
    `scope_id=<project_id>` and the existing connection_id
    lookup succeeds against the GCP connection.

## 14. Implementation chunks

Slice 1 implementation breaks into 7 chunks. Estimated 6 to 9
sessions across them, sized roughly:

- **Chunk 1: Foundation** — storage type + gcpconnstore + SA
  sealing + audit constants. ~700-900 lines. Backend only, no
  wire-up to scanner yet. v0.89.46.
- **Chunk 2: Scanner** — internal/discovery/gcp package +
  Compute Engine scanner + scan.Scanner interface implementation
  + unit tests with mocked compute API. ~600-800 lines. v0.89.47.
- **Chunk 3: API handlers** — HTTP endpoints for the §6 surface +
  validate / scan / recommendations + tests. ~700-900 lines.
  Connects chunks 1 and 2 to the network surface. v0.89.48.
- **Chunk 4: UI page + wizard** — DiscoveryGCP page + wizard
  step data + tests. ~800-1100 lines (UI typically denser). v0.89.49.
- **Chunk 5: Proposer integration** — Provider field on
  DiscoveryScanContext + gce-otel-label kind + system prompt
  extension + branch encoding refresh. ~500-700 lines. v0.89.50.
- **Chunk 6: Runbook + visual assets** — Operator runbook
  mirroring discovery-iac-first-time-setup.md but for GCP +
  README index entry + (optional) docs/README.md entry.
  ~400-600 lines. v0.89.51.
- **Chunk 7: End-to-end smoke test** — Spin up a real GCP
  sandbox project, walk through the wizard, scan, draft, PR.
  Capture screenshots for LinkedIn / demo. Manual test.
  No code release — closes the arc with proven end-to-end loop.

Parallelism opportunities: chunks 2 and 3 can run in parallel
(scanner is independent of the API handlers; the handlers wire
to a Scanner interface that the chunk 2 worktree implements).
Chunks 4 and 5 can run in parallel (UI is independent of
proposer extensions). Chunks 1 and 6 are sequential bookends.

## 15. Slice 2+ candidates

Listed in §2 plus:

- Region multi-select UI (parallel to AWS multi-region in
  v0.89.7a / v0.89.7b)
- Multi-project orchestrator paralleling v0.89.7a's multi-account
- Cloud SQL scanner (slice 2 primary deliverable)
- GKE scanner (slice 3 primary deliverable)
- Cloud Storage scanner
- Cloud Load Balancing scanner
- Workload Identity Federation auth path
- Application Default Credentials inheritance
- Asset Inventory API for higher-throughput multi-service scans
- OAuth user-context credentials for development
- Cross-organization discovery
- GCP-specific richer recommendation kinds: Cloud Trace agent
  installation detection, OpenTelemetry Collector via Cloud Run,
  Managed Service for Prometheus instrumentation
- Multi-tenant Squadron isolation review for shared deployments
- Validate endpoint's SA project_id cross-check enforced in
  the scan path too (slice 1 only does it on validate)
- BigQuery / Pub/Sub / Cloud Functions discovery as the
  surface broadens

---

**Strategic notes for the implementer:**

This is the first non-AWS discovery arc. The substrate work in
chunks 1-3 is the load-bearing part: once the connector + scanner
+ API are in place, the UI / proposer / runbook work is mostly
pattern-matching against the AWS counterparts. Plan to invest
review attention in chunks 1 and 2; chunks 4-6 should flow more
mechanically.

After slice 1 ships, Squadron's public positioning becomes "the
universal observability control plane that scans your AWS AND
GCP fleets." That claim becomes defensible. The Tuesday LinkedIn
post Michael's been thinking about can mention "GCP coming next"
as the build-up; the post after that lands when GCP slice 1 ships.

The next provider after GCP (Azure) needs a separate design doc
but reuses the entire substrate. Slice 1 of Azure should be
shippable in 4-5 chunks (smaller than GCP slice 1 because the
patterns are now established). Universal observability as a real
operator-facing claim lands when AWS + GCP + Azure all have
slice 1 surfaces shipped.
