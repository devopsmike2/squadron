# Connect an Azure subscription to Squadron — first-time setup

This is the operator runbook for the v0.89.50 through v0.89.54
Azure discovery arc that closed Azure slice 1: Squadron now scans
Azure Virtual Machine fleets for observability gaps, drafts
recommendations against your Terraform repo, and learns from the
PRs you accept — same loop as AWS (since v0.85) and GCP (since
v0.89.49).

**After this runbook lands, Squadron's operator-facing claim is
"the universal observability control plane that scans your AWS,
GCP, AND Azure fleets."** Three clouds, one control plane, one
audit timeline, one recommendation pipeline.

If you've never set up an AWS or GCP discovery connection on
Squadron, start with one of those runbooks first:
[discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md)
(AWS) or
[discovery-gcp-first-time-setup.md](./discovery-gcp-first-time-setup.md)
(GCP). This runbook assumes you understand the discovery →
recommendation → IaC PR loop in general; the Azure version
differs only in credentials and the cloud-specific wizard steps.

For a first test against a sandbox subscription, the walkthrough
takes about 15 minutes. Production subscriptions with many VMs
across regions: 30 minutes plus org change-management for the
Service Principal.

## What we're building

The same loop as AWS and GCP, on the third cloud:

1. **An Azure AD Service Principal** scoped to the subscription
   you want to scan, with the `Reader` role granted.
2. **A client_secret** Squadron seals at rest via credstore and
   uses to authenticate Azure ARM API calls during each scan.
3. **An AzureConnection row** in Squadron's `azure_connections`
   table (separate from `aws_connections` and `gcp_connections`,
   each with their own SQLite file and lifecycle) carrying the
   tenant_id, subscription_id, client_id, sealed_secret, optional
   location, and the per-connection feedback loop flag.
4. **A scan-and-recommend flow**: Squadron walks Azure VMs in the
   subscription, projects them into `ComputeInstanceSnapshot`,
   marks `HasOTel=true` when any `otel*` tag key is present
   (case-insensitive, same as AWS slice 1 and GCP slice 1), and
   the proposer drafts `vm-otel-tag` recommendations.
5. **The same IaC integration** as AWS and GCP: clicking Open PR
   drafts a PR against your connected GitHub repo, adding the
   `otel-collector` tag to the relevant
   `azurerm_linux_virtual_machine` or `azurerm_windows_virtual_machine`
   Terraform resource (picked based on the VM's detected OS
   family — Azure exposes this cleanly so the proposer routes
   correctly).

After this runbook you have a working end-to-end loop: scan →
draft → review → merge → audit → learn. The proposer feedback
loop (#531 slice 2), the Checks API back-signal arc (v0.89.39
through v0.89.44), and the Don't propose this again affordance
(slice 2 chunk 5) all work against Azure recommendations
identically to AWS and GCP. Provider-aware scope tuples mean
verdict learning is correctly isolated per subscription.

## What this is good for

- A team running tri-cloud (AWS + GCP + Azure) needing
  observability gaps surfaced from a single control plane with
  a single audit timeline.
- An auditor correlating "Squadron suggested X, operator
  accepted X" across all three providers in one queryable audit
  source.
- A platform team building dashboards that show OTel
  instrumentation coverage across the entire cloud footprint
  without per-provider tool sprawl.

## What this is NOT (slice 1)

Slice 1 ships intentionally narrow. The following are slice 2+
candidates:

- **No Azure SQL Database scanning.** The RDS / Cloud SQL
  equivalent. Slice 2 work.
- **No Azure Kubernetes Service (AKS) scanning.** Slice 3.
- **No Azure Blob Storage / Load Balancer / Application Gateway
  scanning.** Slices 4-5.
- **No Workload Identity Federation.** Slice 1 ships SP
  client_secret authentication. WIF (recommended for production)
  is slice 2.
- **No Managed Identity.** Squadron running ON Azure could use
  the host's managed identity natively in slice 2; slice 1
  requires explicit SP credentials.
- **No SP certificate authentication.** Slice 2 — slightly better
  posture but more wizard complexity.
- **No multi-subscription orchestration.** Single-subscription
  per connection in slice 1. If you have 5 subscriptions, create
  5 connections. Multi-subscription orchestrator is slice 2
  (parallel to AWS v0.89.7a).
- **No cross-tenant federation.** Within-tenant only.
- **No Azure Arc-enabled servers.** Hybrid on-premises VMs
  visible via Azure Arc are slice 3+ work.
- **No Azure Resource Graph queries.** Slice 1 uses per-resource
  list calls; Resource Graph batched queries are slice 2.

If any of these matter, the
[Azure discovery slice 1 design doc](./proposals/azure-discovery-slice1.md)
§17 lists slice 2+ candidates.

## Prerequisites

- A Squadron deployment on v0.89.54 or later.
- An Azure subscription you have admin rights to. Specifically:
  ability to create app registrations in Azure AD and grant
  role assignments at the subscription scope. The combination
  of `Application Administrator` (Azure AD) +
  `User Access Administrator` (subscription) is sufficient.
- `az` CLI installed locally (or comparable access via the
  Azure portal).
- (Optional but recommended) An existing IaC GitHub connection
  ([discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md))
  so Open PR works end to end.

## Step 1 — Connect an Azure subscription in Squadron

Open the Squadron UI, navigate to Discovery → Azure in the
sidebar (sits next to AWS and GCP under the Discovery group). If
this is your first Azure connection, the page opens directly on
the wizard.

Enter:

- **Display name.** A human-readable label, e.g. "Production
  Azure" or "EastUS sandbox".
- **Tenant ID.** The Azure AD tenant where you'll create the
  Service Principal. UUID format (`xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`).
  Find it with `az account show --query tenantId -o tsv`.
- **Subscription ID.** The Azure subscription to scan. UUID
  format. Find it with `az account show --query id -o tsv`.
- **Location.** Optional. Leave empty to scan all regions
  visible to the SP. Set to a specific Azure region like
  `eastus` to restrict scope.

Click Next.

## Step 2 — Create the Service Principal

Squadron needs an Azure AD app registration with subscription-
scoped Reader role. The wizard step displays the exact `az` CLI
command with your subscription_id substituted:

```sh
az ad sp create-for-rbac \
  --name "Squadron Discovery" \
  --role "Reader" \
  --scopes "/subscriptions/<your-subscription-id>"
```

The command outputs JSON like:

```json
{
  "appId": "abcd1234-...",
  "displayName": "Squadron Discovery",
  "password": "BIG_GENERATED_SECRET_VALUE",
  "tenant": "efgh5678-..."
}
```

**Save the `password` (Client Secret) somewhere safe immediately.**
Azure shows it exactly once at creation; if you lose it, you'll
need to generate a new credential.

### Why `Reader` specifically?

Azure's predefined `Reader` role grants read-only access to ALL
resources in the subscription. Squadron only NEEDS read access
to virtual machines for slice 1, but `Reader` is the
operator-friendly default. It does NOT grant any write or
delete permissions.

If your security team prefers a custom role with only
`Microsoft.Compute/virtualMachines/read` and related minimum
permissions, see the "Custom role alternative" section below.

### SP secret expiry

By default, `az ad sp create-for-rbac` generates a secret that
expires in 1 year. Slice 1 does not proactively warn before
expiry — you'll see `credentials_invalid` from the validate
endpoint when the secret expires. Slice 2 candidate: proactive
expiry detection. For now, set a calendar reminder for ~11
months out to rotate.

To set a different lifetime when creating:
```sh
az ad sp create-for-rbac \
  --name "Squadron Discovery" \
  --role "Reader" \
  --scopes "/subscriptions/<your-subscription-id>" \
  --years 2
```

## Step 3 — Paste credentials into Squadron

Back in the Squadron wizard (Step 3), enter:

- **Client ID:** the `appId` value from the SP creation output.
- **Client Secret:** the `password` value.

The Client Secret field is password-masked. Squadron seals it
at rest via credstore with AES-GCM and the domain-tagged AAD
`squadron.azure_client_secret.v1`, which prevents cross-domain
confusion with PATs, webhook secrets, or GCP SA bytes. The
plaintext value never appears in audit payloads, never appears
in logs, never echoes through HTTP responses. Same posture as
v0.89.31's per-connection webhook secrets and v0.89.46's GCP SA
JSON.

Acknowledge the credential-handling warning checkbox. Next button
enables. Click Next.

## Step 4 — Validate

Squadron submits the create-connection request (sealing the
client_secret immediately), then issues a dry-run scan against
Azure to confirm credentials work.

The validation flow:
1. Unseal client_secret in memory.
2. Acquire an OAuth2 token from
   `https://login.microsoftonline.com/<tenant_id>/oauth2/v2.0/token`
   using client_credentials grant.
3. Call `GET https://management.azure.com/subscriptions/<sub>/providers/Microsoft.Compute/virtualMachines?api-version=2024-07-01`
   to verify the SP has read access.
4. Return `{ok: true, instance_count: <first page count>}` on
   success.

If everything's wired correctly, you see "Connected ✓ — N
virtual machines visible." Next button enables.

### What errors look like

The wizard surfaces specific remediation per `error_kind`:

- **permission_denied** — "Verify the Service Principal has
  Reader role on subscription <subscription_id>. Re-run the
  `az ad sp create-for-rbac` command from Step 2 if needed."
  Most common first-time error: the SP was created but the
  role assignment didn't propagate. Wait 60 seconds and retry,
  or re-run the create command (it's idempotent on existing
  apps).
- **subscription_not_found** — "Verify <subscription_id> is
  correct and the SP has access to it." Could be a typo, or
  the SP was scoped to a different subscription.
- **tenant_invalid** — "Verify <tenant_id> matches the Azure AD
  tenant where the SP was created." The tenant_id in the
  connection doesn't match where the SP exists.
- **credentials_invalid** — "Re-check the Client ID and Client
  Secret. The secret may have expired (Azure SP secrets default
  to 1 year)." Most often: typo on paste, or the secret
  expired. Rotate via
  `az ad sp credential reset --id <appId>`.
- **network** — "Squadron's outbound connectivity to
  management.azure.com may be blocked." Air-gapped or
  restricted-egress deployments need to allow this domain.
- **subscription_mismatch** — "The SP's accessible subscriptions
  don't include <subscription_id>." Rare — happens when the SP
  was scoped to a different subscription than the connection.

## Step 5 — Run the first scan

Click Scan. Squadron walks the configured subscription via the
ARM API:

```
GET https://management.azure.com/subscriptions/<sub>/providers/Microsoft.Compute/virtualMachines?api-version=2024-07-01
```

For each VM, the scanner extracts:

- **ResourceID** — VM Name
- **InstanceType** — `vm.Properties.HardwareProfile.VMSize`,
  e.g. `Standard_D4s_v3`
- **Tags** — the VM's Tags map
- **HasOTel** — `true` if any tag key starts with `otel*`
  (case-insensitive)
- **OSFamily** — `linux` or `windows`, derived from
  `vm.Properties.StorageProfile.OsDisk.OSType`. Azure exposes
  this in the same response as the VM listing, so slice 1 gets
  proper OS detection (unlike AWS and GCP slice 1, which leave
  OSFamily="unknown" pending later slice work).
- **Region** — `vm.Location`

After completion, the wizard transitions to the Inventory tab:

| VM Name | Size | OS | Location | OTel? | Tags |
|---|---|---|---|---|---|
| frontend-1 | Standard_D4s_v3 | linux | eastus | yes | otel-collector=v1, env=prod |
| db-replica-3 | Standard_E8s_v5 | linux | eastus | no | env=prod |
| api-7 | Standard_B2ms | windows | westus2 | no | env=staging |

## Step 6 — Draft recommendations

Click "Draft recommendations from this scan." Squadron's
discovery proposer reads the inventory, identifies uninstrumented
VMs, and emits one `vm-otel-tag` recommendation per missing
instance.

The proposer's reasoning explains each:

> Virtual machine `db-replica-3` in eastus has no tag key
> matching `otel*`. Squadron has not detected an OTel collector
> on this VM via the slice 1 tag heuristic. Recommend adding
> `tags = { "otel-collector" = "v1" }` to the
> `azurerm_linux_virtual_machine` resource in your Terraform
> repo (selected based on detected OSFamily=linux) so the
> collector deployment workflow can pick this instance up.

The Don't propose this again button suppresses future
recommendations of the same kind for that scope (slice 2 chunk
5 of #531 ships this affordance and it works the same for
Azure).

## Step 7 — Open the PR

If you have an IaC GitHub connection
([discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md)),
click Open PR.

Branch name:
```
squadron/rec/vm-otel-tag/<subscription_id>/<location>/<short_id>
```

The 4th segment carries the scope_id — `subscription_id` for
Azure, `project_id` for GCP, `account_id` for AWS. The webhook
receiver detects which provider by the kind prefix (`vm-` →
Azure, `gce-` → GCP, default → AWS).

The PR adds a one-line patch to the appropriate Terraform
resource:

```hcl
resource "azurerm_linux_virtual_machine" "db_replica_3" {
  # ... existing fields preserved by HCL-aware merging ...
  tags = {
    "otel-collector" = "v1"
    # ... other existing tags preserved ...
  }
}
```

For Windows VMs the proposer targets `azurerm_windows_virtual_machine`
based on the detected OSFamily. For older azurerm provider
versions using the unified `azurerm_virtual_machine`, the PR
body flags the resource type difference.

## Step 8 — Verify the audit signal

Open the Timeline page. Filter by event type `discovery.azure.*`
to see the Azure-arc events:

- **discovery.azure.connection_created** — payload:
  `{connection_id, subscription_id, display_name}`.
- **discovery.azure.connection_deleted** — when you remove.
- **discovery.azure.scan_started** — when you click Scan.
- **discovery.azure.scan_completed** — payload:
  `{connection_id, subscription_id, location, instance_count, instrumented_count, uninstrumented_count, partial: bool, partial_reason: <string>, failed_services: [<string>...]}`.
  `failed_services` uses `azurevm` as the slice 1 service
  identifier.
- **discovery.azure.scan_failed** — hard error path. Payload
  carries error_kind.
- **discovery.azure.recommendations_generated** — payload
  includes `verdict_examples_used_by_state` buckets from #531
  slice 2 chunk 6 (provider-aware: subscription_id-scoped
  verdicts only).

The downstream events (`recommendation.pr_opened`,
`recommendation.pr_merged`, `recommendation.pr_closed_not_merged`,
`discovery_recommendation.excluded`) work identically for Azure
and GCP and AWS. `provider: "azure"` in the payload discriminates.
SIEM consumers can filter by `provider` or by `subscription_id` /
`project_id` / `account_id`.

## Step 9 — (Optional) Tune the per-connection feedback loop

Like AWS and GCP, Azure connections have a
`learn_from_accepted_recommendations` flag (default true).

```sh
curl -X PATCH https://your-squadron-host/api/v1/discovery/azure/connections/<id> \
  -H "Authorization: Bearer $SQUADRON_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"learn_from_accepted_recommendations": false}'
```

Wizard surfacing is slice 2.

## Troubleshooting matrix

| Symptom | Likely cause | Remedy |
|---|---|---|
| Validate returns `permission_denied` | Role assignment hasn't propagated yet, OR SP scoped differently | Wait 60s and retry; or re-run the create-for-rbac command targeting the right subscription |
| Validate returns `subscription_mismatch` | SP can only see different subscriptions than configured | Either update the connection's subscription_id via PATCH, or re-scope the SP |
| Validate returns `tenant_invalid` | tenant_id doesn't match SP's home tenant | Verify tenant_id with `az account show --query tenantId -o tsv` |
| Validate returns `credentials_invalid` | Client Secret expired or typo on paste | Rotate via `az ad sp credential reset --id <appId>` and re-paste |
| Validate returns `network` | Egress to management.azure.com blocked | Allow the domain in your egress firewall |
| Scan shows partial=true with reason "azurevm: rate limit exceeded mid-scan" | Subscription has many VMs, ARM rate limited | Wait for the rate window to reset; or restrict by Location |
| Scan completes but instance_count is 0 | No VMs exist in scope, or SP can't see them | Run `az vm list --subscription <id>` and verify SP visibility |
| Recommendation does NOT appear for VM with otel-collector tag | Tag key is something other than `otel*` (e.g., `OTel.Collector=v1` — Azure tag keys are case-sensitive in storage) | The detection rule is case-insensitive on the key prefix, so this should work — verify by listing the VM and checking the tags block |
| PR opens but branch missing subscription_id segment | Squadron version older than v0.89.54 — 6-segment branch shape requires chunk 5 of Azure arc | Upgrade Squadron |
| Multiple SP secret expiry warnings | One SP across many connections | Rotate the secret once; PATCH each connection with the new sealed_secret |

## Custom role alternative for stricter posture

If your security team requires a custom role instead of
predefined `Reader`:

```sh
# Create the custom role definition.
cat > squadron-vm-viewer.json <<EOF
{
  "Name": "Squadron VM Viewer",
  "IsCustom": true,
  "Description": "Read-only VM access for Squadron's Azure discovery scanner",
  "Actions": [
    "Microsoft.Compute/virtualMachines/read",
    "Microsoft.Compute/virtualMachines/instanceView/action"
  ],
  "AssignableScopes": ["/subscriptions/<your-subscription-id>"]
}
EOF

az role definition create --role-definition @squadron-vm-viewer.json

# Assign the custom role to the SP.
az role assignment create \
  --assignee <appId-from-step-2> \
  --role "Squadron VM Viewer" \
  --scope "/subscriptions/<your-subscription-id>"
```

This grants exactly the two permissions Squadron's slice 1
scanner uses. Slices 2-5 will need additional permissions when
they add Azure SQL / AKS / Blob / Load Balancer scanners.

## Secret rotation

Service Principal secrets in Azure support multiple concurrent
credentials per app registration, which makes rotation safer
than GCP SA key rotation (no SP-without-secret gap).

To rotate:

1. Generate a new secret for the same SP:
   ```sh
   az ad sp credential reset --id <appId> --append
   ```
   The `--append` flag preserves the existing secret while
   adding a new one.
2. PATCH the existing Squadron connection with the new
   sealed_secret bytes.
3. Test via the validate endpoint.
4. Delete the old credential:
   ```sh
   az ad sp credential delete --id <appId> --key-id <old-key-id>
   ```

Slice 2 candidate: wizard surfacing of rotation.

## What this means for the universal observability claim

Azure slice 1 is the third cloud. After this runbook lands,
Squadron's positioning is concretely **"the universal
observability control plane that scans AWS, GCP, AND Azure
fleets."** This is materially different from "one cloud" or
"two clouds" — the three-major-cloud claim is what makes
"universal" defensible to enterprise buyers.

The substrate (scanner interface, credstore credential model,
provider-aware audit shape, branch encoding, proposer Provider
discriminator) is now triply-proven. The next provider arc
(Oracle Cloud, Alibaba, IBM Cloud, etc.) should ship in 3-4
chunks because every architectural pattern is established. The
marginal cost of cloud N+1 keeps dropping.

The natural slice 2 work for each cloud (Azure SQL / Cloud SQL /
RDS-equivalent) deepens the recommendation surface across the
data tier. Slice 3 (AKS / GKE / EKS) extends into managed
Kubernetes. The horizontal moat is real after this runbook.

## Cross-references

- [Azure discovery slice 1 design doc](./proposals/azure-discovery-slice1.md) —
  the locked spec this runbook operationalizes.
- [discovery-gcp-first-time-setup.md](./discovery-gcp-first-time-setup.md) —
  the parallel GCP runbook.
- [discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md) —
  AWS / IaC GitHub connection prerequisite.
- [webhook-listener.md](./webhook-listener.md) — provider-aware
  PR-merged webhook arc.
- [Checks API back-signal](./checks-api.md) — renders Azure
  recommendation summaries on Squadron-opened PRs.
- [Discovery proposer feedback loop](./discovery-proposer-learning.md) —
  scope tuple is now (connection_id, scope_id, region) where
  scope_id is subscription_id for Azure.
- [Audit log](./audit-log.md) — full catalog including
  `discovery.azure.*` family.
