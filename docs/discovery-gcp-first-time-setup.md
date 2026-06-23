# Connect a GCP project to Squadron — first-time setup

**As of v0.89.62, the unified Discovery dashboard at `/discovery` shows aggregated counts across all four clouds. See it for the cross-cloud view.**

This is the operator runbook for the v0.89.45 through v0.89.49 GCP
discovery arc that closed GCP slice 1: Squadron now scans Google
Cloud Compute Engine fleets for observability gaps, drafts
recommendations against your Terraform repo, and learns from the
PRs you accept (just like the AWS side has done since v0.85).

If you've never set up an AWS discovery connection on Squadron,
read
[discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md)
first. This runbook assumes you understand the discovery →
recommendation → IaC PR loop in general; the GCP version differs
only in credentials and the cloud-specific wizard steps.

For a first test against a sandbox project, the walkthrough takes
about 15 minutes. For a production GCP project with multi-zone
fleets, budget 30 minutes plus whatever your team's
change-management process requires for the Service Account
creation.

## What we're building

The same loop as AWS, on a different cloud:

1. **A GCP Service Account** in the project you want to scan,
   with `roles/compute.viewer` granted at the project level.
2. **A Service Account JSON key** that Squadron seals at rest
   via credstore and uses to authenticate Compute Engine API
   calls during each scan.
3. **A GCPConnection row** in Squadron's `gcp_connections` table
   (separate from `iac_connections` and `aws_connections`)
   carrying the project ID, the sealed SA bytes, the optional
   region restriction, and the per-connection feedback loop
   flag.
4. **A scan-and-recommend flow**: Squadron walks Compute Engine
   instances in scope, projects them into the same
   `ComputeInstanceSnapshot` shape the AWS scanner uses, marks
   instances `HasOTel=true` when any `otel*` label is present
   (case-insensitive, mirroring AWS slice 1's tag rule), and the
   proposer drafts `gce-otel-label` recommendations for any
   instance that lacks instrumentation signal.
5. **The same IaC integration** as AWS: clicking Open PR drafts
   a PR against your connected GitHub repo adding the
   `otel-collector` label to the relevant
   `google_compute_instance` Terraform resource.

After this runbook you have a working end-to-end loop: scan →
draft → review → merge → audit → learn. The proposer feedback
loop (#531 slice 2) and the Checks API back-signal arc
(v0.89.39 through v0.89.44) both work against GCP recommendations
identically to AWS — same audit event types, same exclusion
affordance, same check run lifecycle.

## What this is good for

- A team running multi-cloud (AWS + GCP) and needing the
  observability gaps surfaced in both clouds from a single
  control plane.
- An auditor who wants `discovery.gcp.scan_completed` and
  `recommendation.pr_merged` events alongside the AWS counterparts
  in a single audit timeline for compliance review.
- A platform team building dashboards that show OTel
  instrumentation coverage across the full cloud footprint, not
  just one provider.

## Database tier slice 2 — SHIPPED in v0.89.65 through v0.89.67

As of v0.89.65 (chunk 2 of the database tier arc — design at
[proposals/database-tier-slice2.md](./proposals/database-tier-slice2.md)),
Squadron's GCP scanner ALSO walks Cloud SQL instances during the
same scan call. The Inventory tab gains a Databases sub-tab; the
proposer emits a new `cloudsql-pi-enable` recommendation kind for
Cloud SQL instances where Query Insights is disabled.

**Detection rule:** instance is INSTRUMENTED if
`settings.insightsConfig.queryInsightsEnabled == true`. Otherwise
uninstrumented.

**Recommendation kind:** `cloudsql-pi-enable`. Targets
`google_sql_database_instance.settings[0].insights_config[0].query_insights_enabled = true`
in your Terraform repo.

**IAM scope additions for slice 2:** the existing
`roles/compute.viewer` does NOT cover Cloud SQL. Add
`roles/cloudsql.viewer` to your Squadron Discovery SA:

```sh
gcloud projects add-iam-policy-binding <your-project-id> \
  --member="serviceAccount:squadron-discovery@<your-project-id>.iam.gserviceaccount.com" \
  --role="roles/cloudsql.viewer"
```

Without this role, Cloud SQL list calls return 403 and Squadron
records a partial failure with `failed_services=["cloudsql"]` in
the scan_completed audit event — compute results are still
emitted normally. Re-run the scan after adding the role.

**OAuth scope:** the SA JSON now authenticates with both
`compute.readonly` AND `sqlservice.admin` scopes (least-privilege
union, NOT the broader `cloud-platform` scope).

**Custom-role addition for stricter posture:** if you use a
custom role for Cloud SQL, the minimum permissions are
`cloudsql.instances.list` and `cloudsql.instances.get`.

## What this is NOT (slice 1)

Slice 1 ships intentionally narrow. The following are slice 2+
candidates, called out so you don't expect them yet:

- **~~No Cloud SQL scanning.~~** ✓ SHIPPED in v0.89.65 — see
  "Database tier slice 2" section above.
- **No GKE scanning.** The GKE equivalent of the AWS EKS scanner
  is slice 3 work.
- **No Cloud Storage / Cloud Load Balancing / Pub/Sub scanning.**
  Slices 4 and beyond.
- **No Workload Identity Federation.** Slice 1 ships SA JSON
  authentication. WIF (recommended for production) is slice 2.
- **No multi-project orchestration.** Slice 1 connects one
  project per connection row. If you have 5 projects, create 5
  connections. The multi-project orchestrator paralleling AWS
  v0.89.7a is slice 2 work.
- **No GCP organization-level scanning.** Project-only for now.
- **No cross-organization cross-tenant SA sharing.** Each
  connection owns its own SA JSON sealed in credstore.
- **No Application Default Credentials inheritance.** Squadron
  takes the SA explicitly — it does not rely on the host
  environment having GCP credentials available.

If any of these matter for your deployment, the
[GCP discovery slice 1 design doc](./proposals/gcp-discovery-slice1.md)
§15 lists the slice 2+ candidate items so you can track which
arc unblocks the use case you care about.

## Prerequisites

- A Squadron deployment on v0.89.49 or later.
- A Google Cloud project you have IAM permission to administer
  (specifically: ability to create service accounts and grant
  project-level IAM bindings). The `roles/iam.serviceAccountAdmin`
  + `roles/resourcemanager.projectIamAdmin` combination is
  sufficient.
- `gcloud` CLI installed locally (or comparable access via the
  GCP console). All the steps below show `gcloud` syntax;
  console clicks are equivalent.
- (Optional but recommended) An existing IaC GitHub connection
  ([discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md))
  so Open PR works end-to-end on the recommendations Squadron
  drafts. Without the IaC connection, the proposer still drafts
  recommendations; Open PR is just disabled.

## Step 1 — Connect a GCP project in Squadron

Open the Squadron UI, navigate to Discovery → GCP in the
sidebar (it sits next to the existing AWS entry under the
Discovery group). If this is your first GCP connection, the page
opens directly on the wizard.

Enter:

- **Display name.** A human-readable label, e.g. "Production GCP"
  or "Sandbox dev project". Operators see this in the connection
  selector and in audit payloads.
- **Project ID.** The GCP project ID (not the project number).
  This is the lowercase string like `acme-prod-12345` that
  appears in your project URL. Must match the regex
  `^[a-z][-a-z0-9]{4,28}[a-z0-9]$` per GCP naming rules.
- **Region.** Optional. Leave empty to scan all regions the SA
  can reach. Set to a specific region like `us-central1` to
  restrict the scan scope (useful when you only manage a single
  region per Squadron deployment).

Click Next. The wizard advances to Step 2.

## Step 2 — Create the Service Account in GCP

Squadron needs a GCP identity to authenticate Compute Engine API
calls. The wizard step displays exact `gcloud` commands with your
project ID substituted in. Copy and run them in your terminal (or
follow the equivalent console clicks).

```sh
# Create the service account in your project.
gcloud iam service-accounts create squadron-discovery \
  --display-name "Squadron Discovery" \
  --project=<your-project-id>

# Grant the read-only Compute Engine viewer role at the project level.
gcloud projects add-iam-policy-binding <your-project-id> \
  --member="serviceAccount:squadron-discovery@<your-project-id>.iam.gserviceaccount.com" \
  --role="roles/compute.viewer"
```

### Why `roles/compute.viewer` specifically?

This is the predefined GCP role for read-only Compute Engine
access. It grants `compute.instances.list`,
`compute.instances.get`, `compute.zones.list`, and the related
read permissions Squadron's scanner needs. It does NOT grant
any write permissions — Squadron cannot start, stop, or modify
your instances even if its credentials are compromised.

If you prefer a stricter posture, create a custom role with only
`compute.instances.list`, `compute.instances.get`, and
`compute.zones.list`. The runbook in §"Custom role alternative"
below documents this path.

### Why not `roles/owner` or `roles/editor`?

Both are over-privileged. Squadron will work with either, but
the principle of least privilege says you should grant the
minimal role. If validate (Step 4) succeeds and the SA has
broader scope than `roles/compute.viewer`, tighten the binding.

## Step 3 — Download the SA key and paste it into Squadron

Still in your terminal:

```sh
gcloud iam service-accounts keys create key.json \
  --iam-account=squadron-discovery@<your-project-id>.iam.gserviceaccount.com
```

This writes a JSON file to `key.json`. Open the file, copy the
entire contents, and paste them into the textarea on Squadron's
wizard Step 3.

The wizard validates the pasted content client-side:

- Must be valid JSON.
- Must contain `client_email`, `private_key`, and `project_id`
  fields.
- `client_email` should end in `.iam.gserviceaccount.com` —
  catches the operator mistake of pasting the wrong file.

A warning banner reminds you that the key is a credential.
Squadron seals it at rest via credstore with AES-GCM and a
domain-tagged AAD (`squadron.gcp_sa.v1`) that prevents
cross-domain confusion with PATs or webhook secrets. The plaintext
bytes never appear in audit payloads, never appear in logs, never
echo through HTTP responses. The same posture as v0.89.31's
per-connection webhook secrets.

Acknowledge the warning checkbox. Next button enables. Click Next.

## Step 4 — Validate the connection

The Validate step submits the create-connection request to
Squadron (which immediately seals the SA bytes), then issues a
dry-run scan against GCP to confirm the credentials work.

Click Validate. Squadron:

1. Unseals the SA JSON in memory.
2. Parses the JSON to extract `client_email` and `project_id`.
3. Cross-checks the SA's project_id against the configured
   project_id. If they differ, returns `error_kind=project_mismatch`
   immediately — no GCP API call.
4. Constructs a Compute Engine client with the SA credentials.
5. Calls `compute.instances.list` on the first available zone in
   the configured region (or any zone if Region is empty).
6. Returns `{ok: true, instance_count: N}` on success.

If the SA scope is correct, you see something like "Connected ✓
— 12 instances visible." Next button enables.

### What errors look like

The wizard surfaces specific remediation per `error_kind`:

- **permission_denied** — "Verify the service account has
  roles/compute.viewer in project <project_id>." (Most common
  first-time error: the IAM binding command from Step 2 was
  skipped or applied to the wrong project.)
- **project_not_found** — "Verify <project_id> is correct."
  (The project ID has a typo, or the operator's identity has
  no view on the project.)
- **credentials_invalid** — "Re-check the SA JSON contents."
  (The pasted content is malformed; usually the operator
  pasted only part of the JSON.)
- **network** — "Squadron's outbound connectivity to
  compute.googleapis.com may be blocked. Check firewalls."
  (Air-gapped or restricted-egress deployments need to allow
  this domain.)
- **project_mismatch** — "The SA JSON's project is <sa_project>
  but you configured <conn_project>. Either change the
  connection, or use an SA created in <conn_project>."

## Step 5 — Run the first scan

Click Scan. Squadron walks every zone in the configured region
(or all regions if Region is empty), listing instances via
`compute.instances.list` and projecting each into a
`ComputeInstanceSnapshot` with the OTel detection rule:

```
HasOTel = any(key.lower().startswith("otel") for key in instance.labels.keys())
```

The same single-axis rule the AWS EC2 scanner uses in slice 1.
Symmetry across providers means operator muscle memory transfers.

After the scan completes the wizard transitions to the Inventory
tab and renders the result as a table:

| Resource ID | Type | OS | Region | OTel? | Labels |
|---|---|---|---|---|---|
| frontend-1 | n2-standard-4 | unknown | us-central1 | yes | otel-collector=v1, env=prod |
| db-replica-3 | n1-standard-8 | unknown | us-central1 | no | env=prod |
| api-7 | e2-medium | unknown | us-east1 | no | env=staging |

(Slice 1 leaves `OSFamily="unknown"` for GCP instances; proper
detection lands in slice 2.)

## Step 6 — Draft recommendations

Click "Draft recommendations from this scan." Squadron's
discovery proposer reads the inventory, identifies instances
without OTel labels, and emits one `gce-otel-label`
recommendation per uninstrumented instance.

The proposer's reasoning explains, for each recommendation, why
it fired:

> Instance `db-replica-3` in zone us-central1-a has no label key
> matching `otel*`. Squadron has not detected an OTel agent on
> this VM via the slice 1 label heuristic. Recommend adding
> `labels = { "otel-collector" = "v1" }` to the
> `google_compute_instance` resource in your Terraform repo so
> the collector deployment workflow can pick this instance up.

The recommendation card has the same Don't propose this again
button as the AWS recommendations (slice 2 chunk 5 of #531 added
this affordance and it works the same way for GCP). Excluding a
GCP recommendation persists to the same
`iac_recommendation_verdicts` table that holds AWS exclusions —
no separate storage by provider.

## Step 7 — Open the PR

If you have an IaC GitHub connection
([discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md))
configured for the Terraform repo that owns your GCP
infrastructure, click Open PR on any recommendation.

Squadron drafts a branch named:
```
squadron/rec/gce-otel-label/<project_id>/<region>/<short_id>
```

The branch name carries the GCP scope tuple in the same shape
the AWS branches use (`squadron/rec/<kind>/<scope>/<region>/<id>`)
so the webhook receiver and the verdict learning loop both
handle GCP recommendations the same way they handle AWS ones.
The 4th path segment is the scope_id — `project_id` for GCP,
`account_id` for AWS. The webhook receiver detects which by the
`gce-` prefix on the recommendation kind.

The PR adds a one-line patch to the `google_compute_instance`
resource:

```hcl
resource "google_compute_instance" "db_replica_3" {
  # ... existing fields preserved by HCL-aware merging ...
  labels = {
    "otel-collector" = "v1"
    # ... other existing labels preserved ...
  }
}
```

The PR body summarizes the recommendation reasoning, lists the
affected resource, and links back to the Squadron Recommendations
tab. Operators review and merge as normal.

## Step 8 — Verify the audit signal

Open the Timeline page in Squadron. Filter by event type =
`discovery.gcp.scan_completed` or by date range to see the recent
events.

Slice 1 emits these audit events for the GCP arc:

- **discovery.gcp.connection_created** — when you finish the
  wizard. Payload: `{connection_id, project_id, display_name}`.
- **discovery.gcp.connection_deleted** — when you remove a
  connection.
- **discovery.gcp.scan_started** — when you click Scan.
  Payload includes the scope tuple.
- **discovery.gcp.scan_completed** — when the scan finishes
  (including partial). Payload: `{connection_id, project_id,
  region, instance_count, instrumented_count, uninstrumented_count,
  partial: bool, partial_reason: <string>, failed_services:
  [<string>...]}`. `failed_services` uses `gce` as the service
  identifier (parallel to AWS's `ec2`).
- **discovery.gcp.scan_failed** — when the scan errors out
  hard (zero instances walked). Payload carries the error_kind.
- **discovery.gcp.recommendations_generated** — when the
  proposer drafts recommendations from a scan result. Payload
  includes the verdict_examples_used_by_state buckets from #531
  slice 2 chunk 6.

The downstream events (`recommendation.pr_opened`,
`recommendation.pr_merged`, `recommendation.pr_closed_not_merged`,
`discovery_recommendation.excluded`) work identically for GCP and
AWS — single audit type per event, with `provider: "gcp"` or
`provider: "aws"` in the payload to discriminate. SIEM consumers
can filter by `provider` or by `project_id` / `account_id`.

## Step 9 — (Optional) Tune the per-connection feedback loop

Like the AWS side, GCP connections have a
`learn_from_accepted_recommendations` flag (default true). The
flag controls whether merged-PR signal from GCP
recommendations feeds back into future GCP scans on the same
connection.

To disable on an existing connection:

```sh
curl -X PATCH https://your-squadron-host/api/v1/discovery/gcp/connections/<id> \
  -H "Authorization: Bearer $SQUADRON_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"learn_from_accepted_recommendations": false}'
```

The wizard does not surface this toggle in slice 1 — operators
flip it via API. (Wizard surfacing is a slice 2 candidate
mirroring the AWS path.)

## Troubleshooting matrix

| Symptom | Likely cause | Remedy |
|---|---|---|
| Validate returns `permission_denied` | IAM binding not applied | Re-run the Step 2 binding command, verify project ID |
| Validate returns `project_mismatch` | SA was created in a different project than configured | Either update the connection's project ID via PATCH, or re-run Step 2 commands targeting the correct project |
| Validate returns `network` | Squadron's egress is blocked from compute.googleapis.com | Allow the domain in your egress firewall |
| Scan shows partial=true with reason "gce: rate limit exceeded mid-scan" | High instance count in scope, GCP API rate limited part of the walk | Wait for the rate window to reset, re-run the scan; or restrict the region to scan less per call |
| Scan completes but instance_count is 0 | No GCE instances exist in the configured scope, or SA cannot see them | Run `gcloud compute instances list --project=<project>` and verify the SA has visibility |
| Recommendation does NOT appear for an instance with the otel-collector label | Label key is something other than `otel*` (e.g., `OTEL_COLLECTOR=true` with uppercase) | The detection rule is case-insensitive on the key prefix, so this should work — verify by listing the instance via gcloud and checking the labels block |
| PR opens but the branch name is missing the project_id segment | Squadron version is older than v0.89.49 — the 6-segment branch shape requires chunk 5 | Upgrade Squadron |
| The Don't propose this again button doesn't suppress GCP recommendations | The recommendation was excluded with a different scope tuple (region mismatch or wrong project_id) | Check the iac_recommendation_verdicts table for the recommendation row; verify the scope fields match the scan being run |

## Custom role alternative for stricter posture

If your security team requires a custom role rather than the
predefined `roles/compute.viewer`:

```sh
gcloud iam roles create squadronDiscoveryViewer \
  --project=<your-project-id> \
  --title="Squadron Discovery Viewer" \
  --description="Read-only access for Squadron's GCP discovery scanner" \
  --permissions=compute.instances.list,compute.instances.get,compute.zones.list \
  --stage=GA
```

Then bind that custom role instead:

```sh
gcloud projects add-iam-policy-binding <your-project-id> \
  --member="serviceAccount:squadron-discovery@<your-project-id>.iam.gserviceaccount.com" \
  --role="projects/<your-project-id>/roles/squadronDiscoveryViewer"
```

This grants exactly the three permissions Squadron's slice 1
scanner uses. Slices 2-5 will need additional permissions when
they add Cloud SQL / GKE / Cloud Storage / Cloud Load Balancing
scanners; the runbook for each subsequent slice documents the
additional permissions.

## Key rotation

Service Account keys do not expire automatically in GCP. Rotate
the key periodically per your security policy. Recommended:
annually, or whenever an operator with access to the Squadron
deployment leaves.

To rotate:

1. Create a new key for the SA via `gcloud iam service-accounts
   keys create new-key.json --iam-account=...`.
2. PATCH the existing connection with the new sealed_sa bytes
   (the API accepts `sealed_sa` as a partial update).
3. Test via the validate endpoint.
4. Delete the old key via `gcloud iam service-accounts keys
   delete <old_key_id> --iam-account=...`.

Squadron does not surface key rotation in the wizard for slice 1.
Slice 2 candidate.

## What this means for the universal observability claim

Slice 1 of GCP discovery is the first non-AWS arc on Squadron's
discovery surface. After this runbook lands, the
operator-facing positioning shifts from "Squadron scans your AWS
fleet" to "Squadron scans your AWS AND GCP fleets." Azure
follows in a separate design doc; GKE and Cloud SQL extend the
GCP surface in slices 2 and 3.

The arc that closes is **breadth**, not depth. Squadron's depth
on AWS keeps growing in parallel (the proposer learning loop,
the Checks API back-signal, the per-rollout exclusion, etc.).
Breadth across clouds is what makes the universal observability
claim defensible.

## Cross-references

- [GCP discovery slice 1 design doc](./proposals/gcp-discovery-slice1.md) —
  the locked spec this runbook operationalizes.
- [discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md) —
  the IaC GitHub connection (required for Open PR to work
  end-to-end).
- [webhook-listener.md](./webhook-listener.md) — the
  recommendation.pr_merged webhook arc that closes the
  recommendation lifecycle in audit. Works against GCP PRs as
  of v0.89.49 with the provider-aware audit payload shape.
- [Checks API back-signal](./checks-api.md) — Squadron writes
  check run state to Squadron-opened PRs (including GCP ones).
  The check run summary surfaces the same verdict learning
  context for GCP recommendations as it does for AWS.
- [Discovery proposer feedback loop](./discovery-proposer-learning.md) —
  the loop that informs the next scan with prior accepted
  recommendations. Scope tuple is now provider-aware:
  (connection_id, scope_id, region) where scope_id is
  account_id for AWS and project_id for GCP.
- [Audit log](./audit-log.md) — full catalog of event types
  including the new `discovery.gcp.*` family.
