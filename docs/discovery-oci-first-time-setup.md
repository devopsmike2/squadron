# Connect an OCI tenancy to Squadron — first-time setup

**As of v0.89.62, the unified Discovery dashboard at `/discovery` shows aggregated counts across all four clouds. See it for the cross-cloud view.**

This is the operator runbook for the v0.89.55 through v0.89.59 OCI
discovery arc that closed OCI slice 1: Squadron now scans Oracle
Cloud Compute Instance fleets for observability gaps, drafts
recommendations against your Terraform repo, and learns from the
PRs you accept — same loop as AWS, GCP, and Azure.

**After this runbook lands, Squadron covers four major clouds:
AWS, GCP, Azure, AND Oracle Cloud.** That's the universal
observability claim at its strongest defensible form: a single
OSS control plane scanning four cloud providers, one audit
timeline, one recommendation pipeline, one proposer feedback
loop. The horizontal breadth moat is materially deeper at four
clouds than three.

If you've never set up an AWS / GCP / Azure discovery connection,
start with one of those runbooks first:
[discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md)
(AWS),
[discovery-gcp-first-time-setup.md](./discovery-gcp-first-time-setup.md),
or
[discovery-azure-first-time-setup.md](./discovery-azure-first-time-setup.md).
This runbook assumes you understand the discovery → recommendation
→ IaC PR loop; the OCI version differs in credentials and the
cloud-specific wizard steps.

For a sandbox tenancy, the walkthrough takes about 15 minutes.
Production tenancies with many compartments and instances: 30
minutes plus org change-management for the user permissions.

## What we're building

The same loop as the other three clouds, on OCI:

1. **An OCI user identity** with `compute.instances:read`
   permission via a tenancy-level policy.
2. **An API signing key** (RSA 2048+) generated locally,
   public half uploaded to OCI Console, fingerprint captured.
3. **An OCIConnection row** in Squadron's `oci_connections`
   table carrying the tenancy_ocid, user_ocid, fingerprint,
   sealed_private_key, region, and per-connection feedback flag.
4. **A scan-and-recommend flow**: Squadron walks Compute
   Instances across root + first-level child compartments of
   the tenancy, projects them into `ComputeInstanceSnapshot`,
   marks `HasOTel=true` when any `otel*` tag key is present
   (case-insensitive across FreeformTags + DefinedTags), and the
   proposer drafts `compute-otel-tag` recommendations.
5. **The same IaC integration**: clicking Open PR drafts a PR
   adding the `otel-collector` freeform tag to the relevant
   `oci_core_instance` Terraform resource.

After this runbook you have a working end-to-end loop: scan →
draft → review → merge → audit → learn. Verdict learning, Checks
API back-signal, Don't propose this again affordance — all
provider-aware and work for OCI identically to the other three
clouds.

## What this is good for

- A team running quadruple-cloud (AWS + GCP + Azure + OCI)
  needing observability gaps surfaced from one control plane.
- An auditor correlating recommendations across all four
  providers in a single queryable audit source.
- A platform team building dashboards that show OTel
  instrumentation coverage across the full cloud footprint
  without per-provider tool sprawl.
- An enterprise evaluating Squadron against multi-cloud
  procurement requirements — the four-cloud claim is decisive.

## Database tier slice 2 — SHIPPED in v0.89.65 through v0.89.67

As of v0.89.65 (chunk 4 of the database tier arc — design at
[proposals/database-tier-slice2.md](./proposals/database-tier-slice2.md)),
Squadron's OCI scanner ALSO walks DB Systems AND Autonomous
Databases across the same compartments it walks for Compute
Instances. The Inventory tab gains a Databases sub-tab; the
proposer emits a new `ocidb-perfhub-enable` recommendation kind
for instances where Operations Insights / Database Management
is not enabled.

**Detection rule:**
- DB Systems: instrumented if
  `databaseManagementConfig.databaseManagementStatus == "ENABLED"`
- Autonomous Databases: instrumented if the top-level
  `databaseManagementStatus == "ENABLED"`

The two product families have slightly different response shapes
(nested vs top-level) but the semantic is identical. Squadron
case-insensitively matches `ENABLED` to defend against future API
casing drift.

Instances with `lifecycleState != "AVAILABLE"` (terminating,
provisioning, etc.) are skipped — they have no observability
surface to recommend on.

**Recommendation kind:** `ocidb-perfhub-enable`. Targets
`oci_database_db_systems_management` for DB Systems, or the
equivalent autonomous-database block.

**IAM policy additions for slice 2:** the existing
`Allow group SquadronDiscovery to read instance-family in tenancy`
does NOT cover databases. Add:

```sh
oci iam policy update \
  --policy-id <squadron-discovery-policy-ocid> \
  --statements '[
    "Allow group SquadronDiscovery to read instance-family in tenancy",
    "Allow group SquadronDiscovery to read compartments in tenancy",
    "Allow group SquadronDiscovery to read database-family in tenancy"
  ]' \
  --version-date "2025-01-01"
```

Without the `database-family` statement, DB Systems and Autonomous
Database list calls return 403 and Squadron records a partial
failure with `failed_services=["ocidb"]`. Compute results are
still emitted normally. Re-run the scan after adding the
statement.

**Service identifier in audit:** partial-failure events use
`failed_services=["ocidb"]`.

## Kubernetes tier slice 2 — SHIPPED in v0.89.70 through v0.89.72

As of v0.89.70 (chunk 4 of the Kubernetes tier arc — design at
[proposals/kubernetes-tier-slice2.md](./proposals/kubernetes-tier-slice2.md)),
Squadron's OCI scanner ALSO walks OKE clusters across the same
compartments it walks for Compute Instances and Databases. The
Inventory tab gains a Kubernetes sub-tab; the proposer emits a
new `oke-ops-insights-enable` recommendation kind for OKE
clusters not enrolled in Operations Insights.

**Detection rule:** cluster is INSTRUMENTED if it has a freeform
tag with key `operations-insights-enabled` (case-insensitive)
AND value `true` (case-insensitive, whitespace-trimmed).

Slice 2 uses this tag-based convention because OCI does not
expose a top-level "managed observability enrolled" boolean on
the cluster resource the way GCP and Azure do. Operators
self-tag the cluster when they enroll it in Operations Insights.
Slice 3 will move to a direct Operations Insights API call.

Clusters with `lifecycleState != "ACTIVE"` (mid-create,
mid-delete, etc.) are skipped — they have no observability
surface to recommend on.

**Recommendation kind:** `oke-ops-insights-enable`. Targets the
`oci_containerengine_cluster.freeform_tags` block.

**IAM policy additions for K8s slice 2:** the existing
`Allow group SquadronDiscovery to read instance-family in tenancy`
and `read database-family` do NOT cover OKE. Add:

```sh
oci iam policy update \
  --policy-id <squadron-discovery-policy-ocid> \
  --statements '[
    "Allow group SquadronDiscovery to read instance-family in tenancy",
    "Allow group SquadronDiscovery to read compartments in tenancy",
    "Allow group SquadronDiscovery to read database-family in tenancy",
    "Allow group SquadronDiscovery to read cluster-family in tenancy"
  ]' \
  --version-date "2025-01-01"
```

Without the `cluster-family` statement, OKE list calls return
403 and Squadron records a partial failure with
`failed_services=["oke"]`. Compute + database results are still
emitted normally. Re-run the scan after adding the statement.

**Service identifier in audit:** partial-failure events use
`failed_services=["oke"]`.

## What this is NOT (slice 1)

Slice 1 ships intentionally narrow:

- **~~No Autonomous Database / DB Systems scanning.~~** ✓ SHIPPED
  in v0.89.65 — see "Database tier slice 2" section above.
- **~~No OKE (Oracle Kubernetes Engine).~~** ✓ SHIPPED in
  v0.89.70 — see "Kubernetes tier slice 2" section above.
- **No OKE (Oracle Kubernetes Engine).** Slice 3.
- **No Object Storage scanning.** Slice 4.
- **No Load Balancer scanning.** Slice 5.
- **No Instance Principal authentication.** Slice 2.
- **No Resource Principal authentication.** Slice 2.
- **No multi-tenancy scans.** One tenancy per connection.
- **No multi-region scans per connection.** Slice 1 ships
  single-region (Region is REQUIRED unlike AWS/GCP/Azure).
- **No deep compartment tree walk.** Slice 1 covers root + first-
  level child compartments. Grandchild compartments are slice 2.
- **No OCI Resource Search API.** Per-resource list calls only.
- **No Azure Arc-equivalent hybrid scanning.**

The [OCI discovery slice 1 design doc](./proposals/oci-discovery-slice1.md)
§17 tracks the slice 2+ candidates.

## Prerequisites

- A Squadron deployment on v0.89.59 or later.
- An OCI tenancy you have admin rights to. Specifically: ability
  to create user policies and add API keys to a user.
- `oci` CLI installed locally (optional but recommended) OR
  `openssl` (always available on macOS / Linux).
- (Optional but recommended) An existing IaC GitHub connection
  so Open PR works end to end.

## Step 1 — Connect an OCI tenancy in Squadron

Open the Squadron UI, navigate to Discovery → OCI in the sidebar
(sits next to AWS, GCP, and Azure under the Discovery group).

Enter:

- **Display name.** e.g., "Production OCI" or "Phoenix sandbox".
- **Tenancy OCID.** Format `ocid1.tenancy.oc1..<unique_id>`.
  Find it: OCI Console → click profile (top right) → Tenancy →
  copy the OCID. Or: `oci iam compartment list --compartment-id-in-subtree=false`.
- **User OCID.** Format `ocid1.user.oc1..<unique_id>`. Find it:
  OCI Console → Identity → Users → select your user → copy OCID.
- **Region.** REQUIRED. OCI uses regional API endpoints — pick
  from the dropdown (us-phoenix-1, us-ashburn-1, eu-frankfurt-1,
  ap-tokyo-1, etc.). Match the region where your Compute
  Instances live.

Click Next.

## Step 2 — Generate the API signing key

Squadron needs an RSA private key to sign API requests. You
generate the keypair locally (the private key never travels) and
upload only the public half to OCI Console.

**Option 1 — OCI CLI helper (recommended):**

```sh
oci setup keys --output-dir ~/.oci
```

This creates `oci_api_key.pem` (private), `oci_api_key_public.pem`
(public), and outputs the fingerprint.

**Option 2 — openssl directly:**

```sh
# Generate 2048-bit RSA private key
openssl genrsa -out ~/.oci/oci_api_key.pem 2048

# Extract public key
openssl rsa -pubout -in ~/.oci/oci_api_key.pem \
  -out ~/.oci/oci_api_key_public.pem

# Compute fingerprint (you'll need this in Step 4)
openssl rsa -pubout -outform DER -in ~/.oci/oci_api_key.pem | \
  openssl md5 -c
```

The fingerprint output looks like
`xx:xx:xx:xx:xx:xx:xx:xx:xx:xx:xx:xx:xx:xx:xx:xx` (16 hex pairs).
Save it.

## Step 3 — Upload the public key to OCI Console

In the OCI Console:

1. Navigate to **Identity** → **Users**.
2. Click your user.
3. Scroll to the **API Keys** section.
4. Click **Add API Key**.
5. Choose **Paste a Public Key**.
6. Paste the contents of `~/.oci/oci_api_key_public.pem`
   (including the `-----BEGIN PUBLIC KEY-----` and
   `-----END PUBLIC KEY-----` markers).
7. Click Add.

OCI Console shows the fingerprint of the uploaded key. Verify it
matches the fingerprint from Step 2 (they MUST be identical — if
not, you uploaded the wrong key or the file is corrupted).

OCI Console also offers to copy a config snippet. You don't need
that snippet for Squadron — you'll paste fields individually in
Step 4.

## Step 4 — Paste credentials into Squadron

Back in Squadron's wizard (Step 4 of the UI):

- **Fingerprint.** Paste the 16-pair colon-separated hex from
  Step 2. The wizard validates the format client-side.
- **Private Key.** Paste the contents of `~/.oci/oci_api_key.pem`,
  including the `-----BEGIN RSA PRIVATE KEY-----` (or
  `-----BEGIN PRIVATE KEY-----`) and `-----END ...-----` markers.

Squadron seals the private key bytes via credstore with AES-GCM
and the domain-tagged AAD `squadron.oci_signing_key.v1`. This is
the **fifth** credential domain in credstore — fully isolated
from PATs, webhook secrets, GCP SA JSON, and Azure SP secrets.
The plaintext bytes never appear in audit payloads, never appear
in logs, never echo through HTTP responses.

Acknowledge the credential-handling warning checkbox. Next button
enables. Click Next.

## Step 5 — Add the IAM policy (if not already in place)

Squadron's scanner needs `compute.instances:read` and
`compartment:read` on the tenancy.

If your user already has admin rights, you have these.

If using a custom group / dedicated Squadron user, add a policy:

```
Allow group <YourSquadronGroup> to read instance-family in tenancy
Allow group <YourSquadronGroup> to read compartments in tenancy
```

Via `oci` CLI:

```sh
oci iam policy create \
  --compartment-id <tenancy-ocid> \
  --name squadron-discovery-policy \
  --description "Squadron Discovery read access" \
  --statements '["Allow group SquadronDiscovery to read instance-family in tenancy", "Allow group SquadronDiscovery to read compartments in tenancy"]'
```

## Step 6 — Validate

Click Validate. Squadron:

1. Unseals the private key in memory.
2. Parses the PEM.
3. Constructs an OCI request: GET `https://identity.<region>.oci.oraclecloud.com/20160918/compartments?compartmentId=<tenancy_ocid>&accessLevel=ANY&compartmentIdInSubtree=false`.
4. Signs the request with RSA-SHA256 per the OCI HTTP Signatures
   spec.
5. Sends the request. Returns `{ok: true, instance_count: <N>}`
   on success.

If credentials are correct and the policy is in place, you see
"Connected ✓ — N compute instances visible." Next button enables.

### What errors look like

- **permission_denied** — "Verify the user has
  compute.instances:read permission on the tenancy. Add a policy:
  `Allow group <YourGroup> to read instances in tenancy`."
- **tenancy_not_found** — "Verify the tenancy OCID matches an
  existing tenancy in region <region>."
- **fingerprint_mismatch** — "The fingerprint doesn't match the
  public key uploaded to OCI Console for this user. Re-verify
  the fingerprint via openssl, and confirm the uploaded public
  key matches the private key you pasted."
- **private_key_invalid** — "The pasted PEM is malformed or not
  an RSA key. Re-paste including the BEGIN/END markers. Ensure
  you pasted the PRIVATE key, not the public one."
- **network** — "Squadron's outbound connectivity to
  `*.oraclecloud.com` may be blocked. Allow this domain in your
  egress firewall."

## Step 7 — Run the first scan

Click Scan. Squadron walks Compute Instances across the root
compartment + first-level child compartments (slice 1; deeper
trees are slice 2).

For each instance, the scanner extracts:

- **ResourceID** — instance.DisplayName
- **InstanceType** — instance.Shape (e.g.
  "VM.Standard.E4.Flex")
- **Tags** — `FreeformTags` map + `DefinedTags` map (flattened
  by dropping the namespace prefix; slice 1 simplification —
  slice 2 may keep namespacing for richer matching)
- **HasOTel** — `true` if any tag key starts with `otel*`
  (case-insensitive)
- **OSFamily** — `unknown` for slice 1 (OCI Image lookup
  requires a secondary API call; slice 2 adds detection)
- **Region** — instance.Region

After completion, the wizard transitions to the Inventory tab:

| Name | Shape | OS | Region | OTel? | Tags |
|---|---|---|---|---|---|
| frontend-1 | VM.Standard.E4.Flex | unknown | us-phoenix-1 | yes | otel-collector=v1, env=prod |
| db-replica | VM.Standard3.Flex | unknown | us-phoenix-1 | no | env=prod |

## Step 8 — Draft recommendations

Click "Draft recommendations." Squadron's discovery proposer
identifies uninstrumented instances and emits one
`compute-otel-tag` recommendation per missing instance.

Reasoning template:

> Compute instance `db-replica` in us-phoenix-1 has no tag key
> matching `otel*`. Squadron has not detected an OTel collector
> on this instance via the slice 1 tag heuristic. Recommend
> adding `freeform_tags = { "otel-collector" = "v1" }` to the
> `oci_core_instance` resource in your Terraform repo so the
> collector deployment workflow can pick this instance up. For
> instances using DefinedTags, add to the appropriate namespace
> map.

The Don't propose this again button suppresses future
recommendations of the same kind for that scope. The exclusion
persists to the same `iac_recommendation_verdicts` table used by
the other three clouds — all four providers share the storage,
discriminated by `tenancy_ocid` / `subscription_id` /
`project_id` / `account_id`.

## Step 9 — Open the PR

Click Open PR. Branch name:

```
squadron/rec/compute-otel-tag/<tenancy_ocid>/<region>/<short_id>
```

The 4th segment is the scope_id — `tenancy_ocid` for OCI. The
webhook receiver detects the provider by the `compute-` prefix
on the recommendation kind (mirroring `gce-` → GCP, `vm-` →
Azure, default → AWS).

The PR adds a one-line patch:

```hcl
resource "oci_core_instance" "db_replica" {
  # ... existing fields preserved by HCL-aware merging ...
  freeform_tags = {
    "otel-collector" = "v1"
    # ... other existing freeform tags preserved ...
  }
}
```

## Step 10 — Verify the audit signal

Open Timeline. Filter by event type `discovery.oci.*`:

- **discovery.oci.connection_created** — `{connection_id, tenancy_ocid, display_name}`.
- **discovery.oci.connection_deleted** — when you remove.
- **discovery.oci.scan_started** — when you click Scan.
- **discovery.oci.scan_completed** — `{connection_id, tenancy_ocid, region, instance_count, instrumented_count, uninstrumented_count, partial, partial_reason, failed_services}`. `failed_services` uses `ocicompute` as slice 1's service ID.
- **discovery.oci.scan_failed** — hard error path.
- **discovery.oci.recommendations_generated** — includes the
  provider-aware `verdict_examples_used_by_state` buckets.

Downstream events work identically across all four providers:
`recommendation.pr_opened`, `recommendation.pr_merged`,
`recommendation.pr_closed_not_merged`,
`discovery_recommendation.excluded`. `provider: "oci"` in payload
discriminates.

## Step 11 — (Optional) Tune the per-connection feedback loop

```sh
curl -X PATCH https://your-squadron-host/api/v1/discovery/oci/connections/<id> \
  -H "Authorization: Bearer $SQUADRON_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"learn_from_accepted_recommendations": false}'
```

## Troubleshooting matrix

| Symptom | Likely cause | Remedy |
|---|---|---|
| Validate returns `permission_denied` | Policy missing or wrong group binding | Run the policy create command from Step 5; verify your user is in the right group |
| Validate returns `tenancy_not_found` | Wrong tenancy_ocid or wrong region | Verify both from OCI Console |
| Validate returns `fingerprint_mismatch` | Uploaded public key differs from pasted private key | Re-generate keypair (Step 2), upload fresh public key (Step 3), paste fresh private key (Step 4) |
| Validate returns `private_key_invalid` | Wrong PEM format or pasted public instead of private | Verify you pasted `oci_api_key.pem` not `oci_api_key_public.pem`. Include BEGIN/END markers |
| Validate returns `network` | Egress to `*.oraclecloud.com` blocked | Allow the wildcard in your firewall, OR specifically `identity.<region>.oci.oraclecloud.com` and `iaas.<region>.oraclecloud.com` |
| Scan shows partial=true with reason "ocicompute: rate limit exceeded mid-scan" | OCI API rate limit | Wait for window reset; or restrict to a smaller compartment scope (slice 2 feature) |
| Scan completes but instance_count is 0 | No instances exist in scope, or user can't see them | Run `oci compute instance list --compartment-id <comp-ocid>` directly and verify visibility |
| Recommendation doesn't appear for instance with otel-collector tag | DefinedTag under a namespace not flattened correctly | Slice 1 flattens by dropping the namespace prefix. Verify the tag key starts with `otel*` after the namespace strip |
| Instances in grandchild compartments not appearing | Slice 1 walks root + first-level only | Slice 2 candidate — for now, create one connection per child compartment if needed |
| PR branch missing tenancy_ocid segment | Squadron version older than v0.89.58 | Upgrade |

## Custom group + policy for stricter posture

If your security team requires a dedicated user + group with
minimum permissions:

```sh
# Create the user
oci iam user create \
  --name squadron-discovery \
  --description "Squadron Discovery read-only"

# Create the group
oci iam group create \
  --name SquadronDiscovery \
  --description "Squadron Discovery read group"

# Add user to group
oci iam group add-user \
  --user-id <squadron-discovery-user-ocid> \
  --group-id <squadron-discovery-group-ocid>

# Create the minimal policy
oci iam policy create \
  --compartment-id <tenancy-ocid> \
  --name squadron-discovery-policy \
  --description "Squadron Discovery minimum read access" \
  --statements '[
    "Allow group SquadronDiscovery to read instance-family in tenancy",
    "Allow group SquadronDiscovery to read compartments in tenancy"
  ]'
```

Then add the API key for this dedicated user (Step 3 against
the squadron-discovery user, not your own).

## Key rotation

OCI allows multiple API keys per user (up to 3). Rotation is
safe:

1. Generate a new keypair (Step 2 with a different filename).
2. Upload the new public key (Step 3).
3. PATCH the Squadron connection with the new sealed_private_key
   + new fingerprint.
4. Test via validate.
5. Delete the old key from OCI Console.

Calendar reminder for ~11 months out is good practice. OCI keys
don't expire automatically (unlike Azure SP secrets), but
periodic rotation is hygiene.

## What this means for the universal observability claim

OCI slice 1 is the fourth cloud. After this runbook lands,
Squadron's positioning is the strongest version that a single
OSS control plane can defensibly support:

**"The universal observability control plane that scans AWS,
GCP, Azure, AND Oracle Cloud fleets."**

Four major clouds, one control plane, one audit timeline, one
recommendation pipeline. The marginal cost of cloud N+1 keeps
dropping — the substrate (scanner interface, credstore credential
model, provider-aware audit shape, branch encoding, proposer
Provider discriminator) is now quadruply-proven.

The next provider arc (Alibaba Cloud, Tencent Cloud, IBM Cloud,
DigitalOcean) ships in 3 chunks given substrate maturity — chunk
1 + chunk 2 (scanner + handlers parallel) + chunk 3 (UI +
proposer parallel) — runbook + design doc bundled in the
respective chunks. Cloud N+5 should be a 2-3 day effort, not a
week.

The slice 2 work across all 4 clouds (database tier — RDS / Cloud
SQL / Azure SQL / Autonomous DB) deepens the recommendation
surface. Slice 3 extends into managed Kubernetes (EKS / GKE / AKS
/ OKE). The horizontal breadth foundation makes these vertical
extensions cheaper.

## Cross-references

- [OCI discovery slice 1 design doc](./proposals/oci-discovery-slice1.md) —
  the locked spec.
- [Azure discovery runbook](./discovery-azure-first-time-setup.md) —
  parallel arc.
- [GCP discovery runbook](./discovery-gcp-first-time-setup.md) —
  parallel arc.
- [AWS / IaC GitHub setup](./discovery-iac-first-time-setup.md) —
  the IaC integration prerequisite for Open PR.
- [Webhook listener](./webhook-listener.md) — provider-aware
  PR-merged webhook arc.
- [Checks API back-signal](./checks-api.md) — renders OCI
  recommendation summaries on Squadron-opened PRs.
- [Discovery proposer feedback loop](./discovery-proposer-learning.md) —
  scope tuple is now (connection_id, scope_id, region) where
  scope_id is tenancy_ocid for OCI.
- [Audit log](./audit-log.md) — full catalog including
  `discovery.oci.*` family.

## Object-store + load-balancer tiers — SHIPPED (coverage-parity arc)

Squadron's OCI scanner walks **Object Storage buckets** and **Load
Balancers** across the same compartments it walks for Compute,
Databases, and OKE (slice 4), and resolves their access-logging
coverage from the **OCI Logging service** (slice 6).

**Why the Logging service?** Unlike AWS S3 / ELB, OCI exposes no
inline per-bucket or per-load-balancer "access logging enabled" flag.
Access logs are delivered as *service logs* in the OCI Logging service.
Squadron therefore enumerates service logs and marks a bucket / load
balancer covered when an enabled service log references it (the same
`listLogsForOCIResource` detection used for streams, topics, and
queues). If the Logging calls fail (e.g. missing policy), the axis dims
to *uncovered* and a partial failure is recorded under
`failed_services=["ocilogging"]` rather than aborting the scan.

**Recommendation kinds:** `ocibucket-logging-enable` (object stores)
and `ocilb-logging-enable` (load balancers). Both emit Terraform that
creates an `oci_logging_log` (`log_type = "SERVICE"`) with
`configuration.source.service = "objectstorage"` / `"loadbalancer"`,
`resource = <bucket-name>` / `<load-balancer-OCID>`, and an
`oci_logging_log_group` (operator-chosen). The log destination is the
Logging service itself, so — unlike AWS — there is no target-bucket
policy prerequisite.

**IAM policy additions.** The Compute/Database/OKE statements do NOT
cover these tiers. Add:

```sh
oci iam policy update \
  --policy-id <squadron-discovery-policy-ocid> \
  --statements '[
    "Allow group SquadronDiscovery to read instance-family in tenancy",
    "Allow group SquadronDiscovery to read compartments in tenancy",
    "Allow group SquadronDiscovery to read database-family in tenancy",
    "Allow group SquadronDiscovery to read cluster-family in tenancy",
    "Allow group SquadronDiscovery to read objectstorage-namespaces in tenancy",
    "Allow group SquadronDiscovery to read buckets in tenancy",
    "Allow group SquadronDiscovery to read load-balancers in tenancy",
    "Allow group SquadronDiscovery to read log-groups in tenancy"
  ]' \
  --version-date "2025-01-01"
```

`read objectstorage-namespaces` + `read buckets` back the object-store
walk; `read load-balancers` backs the LB walk; `read log-groups` backs
the access-logging detection for both tiers (without it, both tiers
still list but render uncovered and record `ocilogging` partial
failures). Re-run the scan after adding the statements.

**Service identifiers in audit:** `ociobjectstorage`, `ocilb`,
`ocilogging`.
