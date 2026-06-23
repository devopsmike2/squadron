# Azure discovery — slice 1 design

**Status:** design doc, locked for slice 1 implementation. Second
non-AWS discovery arc, following the GCP slice 1 close in v0.89.49.
The AWS arc (#558) and the GCP arc (gcp-discovery-slice1.md)
established the scanner interface, the credstore credential
substrate, the wizard framework, the provider-aware audit shape,
the proposer's Provider discriminator, and the branch encoding
that carries provider-agnostic scope_id values.

After Azure slice 1 ships, the universal observability claim
becomes concretely defensible: "Squadron scans your AWS, GCP, AND
Azure fleets for observability gaps and drafts the IaC PRs that
close them across all three clouds." That positioning supports a
real GTM moment.

**See also:**
[GCP discovery slice 1 design](./gcp-discovery-slice1.md),
[AWS discovery design (#558)](./558-discovery-design.md),
[discovery-iac-first-time-setup.md](../discovery-iac-first-time-setup.md),
[ai-features.md](../ai-features.md).

## 1. Problem

Squadron's discovery surface now spans two clouds (AWS + GCP).
Operators with Azure-resident fleets see no inventory, no
recommendations, no IaC PRs from Squadron's surface. The universal
observability claim still has a gap; the third major cloud is
silent.

The substrate that makes this fixable in slice 1 is the work done
in the GCP arc:

- The scanner.Scanner interface is provider-agnostic.
- The credstore substrate supports domain-tagged AAD sealing for
  arbitrary credential bytes (PAT, webhook secret, GCP SA JSON).
- The audit payload carries provider-agnostic scope_id with the
  provider discriminator routing it to `account_id` (AWS) or
  `project_id` (GCP) or — adding to that pattern — `subscription_id`
  (Azure).
- The branch encoding `squadron/rec/<kind>/<scope>/<region>/<id>`
  works identically for any cloud. The `vm-` prefix on the
  recommendation kind tells the webhook receiver this is an Azure
  PR; no other infrastructure changes.
- The discovery proposer's Provider field already accepts any
  provider value; slice 1 adds "azure" to the discriminator set.

The cost to add Azure is materially less than GCP cost because the
GCP arc established the patterns. Estimated 4-5 chunks for slice 1
implementation instead of GCP's 6 chunks.

## 2. Non-goals (slice 1)

- **Azure SQL Database.** The RDS / Cloud SQL analog. Slice 2.
- **Azure Kubernetes Service (AKS).** The EKS / GKE analog. Slice 3.
- **Azure Blob Storage.** The S3 / Cloud Storage analog. Slice 4.
- **Azure Load Balancer / Application Gateway.** Slice 5.
- **Workload Identity Federation.** Slice 2. Slice 1 ships
  Service Principal client_secret authentication.
- **Managed Identity.** Slice 2. Squadron does not require running
  on Azure to scan Azure; managed identity inheritance is a
  convenience for deployments that ARE on Azure.
- **Multi-subscription scans.** Slice 1 ships single-subscription
  per connection (mirrors GCP single-project). Multi-subscription
  orchestrator is slice 2.
- **Cross-tenant federation.** Out of scope for slice 1.
- **Azure Arc-enabled servers.** Hybrid on-premises VMs visible
  via Arc. Slice 3+ once the core Azure VM scanner is solid.
- **Azure Resource Graph queries.** A more efficient batched query
  surface than per-resource list calls. Slice 2 candidate.

## 3. Architectural decision: credential model

Azure authentication has four real options. Reasoning parallels
GCP's three options choice (gcp-discovery-slice1.md §3):

### Option A — Service Principal with client_secret

Operator creates an Azure AD app registration, generates a client
secret, grants the SP `Reader` role at the subscription level.
Squadron stores `{tenant_id, client_id, client_secret}` in
credstore with a domain-tagged AAD. Scanner uses Azure Identity
SDK's `ClientSecretCredential` to authenticate Azure REST API
calls.

**Picked for slice 1.** Mirrors the GCP SA JSON pattern.
Credstore substrate exists. Wizard is straightforward: paste
three strings + grant one role. Operators familiar with the GCP
flow will recognize the shape.

### Option B — Workload Identity Federation (WIF)

Operator configures a federated credential in the Azure AD app
registration pointing at Squadron's deployment identity (OIDC
issuer, Kubernetes service account, AWS IAM role, etc.). Squadron
exchanges a federated token for an Azure access token at scan
time. No long-lived client_secret to manage.

**Deferred to slice 2.** Same reasoning as GCP WIF: wizard
complexity, deployment-identity requirements, operators with
sandbox subscriptions don't need it day one. Slice 2 ships it for
production posture.

### Option C — Managed Identity (if Squadron runs on Azure)

Squadron running on an Azure VM / App Service / AKS pod can use
its system-assigned or user-assigned managed identity to
authenticate without explicit credentials. The wizard would
detect this and offer "Use the host managed identity" as an
option.

**Deferred to slice 2.** Same reasoning as GCP ADC: couples
Squadron to one specific deployment platform. Squadron's value
is platform-agnostic.

### Option D — Service Principal with certificate

Instead of a client_secret, use a certificate as the SP
credential. Better posture (private key never travels in
plaintext). More complex wizard (operator generates a cert,
uploads to Azure AD, pastes the private key into Squadron).

**Deferred to slice 2.** Same reasoning as deferring GCP keys
vs WIF — security posture upgrade that doesn't fit slice 1
operator simplicity.

## 4. Scanner interface

Reused. New `internal/discovery/azure` package implementing
`scanner.Scanner`. The package owns:

- An Azure SDK client (azidentity + armcompute).
- A subscription_id field.
- A region (Azure: "location") filter.

`Scan(ctx)` walks `VirtualMachinesClient.NewListAllPager`
returning all VMs in the subscription, projects them into
`ComputeInstanceSnapshot`, applies the `otel*` tag detection rule
(case-insensitive), and returns `scanner.Result` with computes
populated.

Azure's resource model has a single "Tags" map per resource
(unlike GCP's separate labels-vs-network-tags distinction); the
projection is straightforward.

## 5. Storage

New `AzureConnection` row type paralleling `GCPConnection` in the
gcpconnstore package shape:

```go
type AzureConnection struct {
    ID         string
    DisplayName string
    TenantID    string
    SubscriptionID string
    ClientID    string
    SealedSecret []byte  `json:"-"`  // credstore-sealed client_secret
    Location    string  // region restriction, empty = all
    LearnFromAcceptedRecommendations bool
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

A new `azureconnstore` package paralleling `gcpconnstore` exactly:
own SQLite file (`azureconnstore.db`), own schema versioning, own
Create / Get / List / Update / Delete / SetClientSecret /
GetClientSecret methods.

Migration: `CREATE TABLE azure_connections (...)` with index on
`subscription_id`.

## 6. Credstore extension

New `internal/discovery/credstore/azure_sp.go` with:

```go
func SealAzureClientSecret(key *Key, plaintext []byte) ([]byte, error)
func UnsealAzureClientSecret(key *Key, sealed []byte) ([]byte, error)
```

Domain-tagged AAD: `"squadron.azure_client_secret.v1"`. Pattern
parallels `gcp_sa.go` (v0.89.46) exactly, including version byte +
defense-in-depth domain separation against PAT, webhook secret,
and GCP SA bytes.

## 7. API endpoints

Mirror the GCP surface at `/api/v1/discovery/azure/*`:

- POST   `/api/v1/discovery/azure/connections`
- GET    `/api/v1/discovery/azure/connections`
- GET    `/api/v1/discovery/azure/connections/:id`
- PATCH  `/api/v1/discovery/azure/connections/:id`
- DELETE `/api/v1/discovery/azure/connections/:id`
- POST   `/api/v1/discovery/azure/connections/:id/validate`
- POST   `/api/v1/discovery/azure/connections/:id/scan`
- POST   `/api/v1/discovery/azure/connections/:id/recommendations`

Body shapes mirror GCP exactly. Create request:
`{display_name, tenant_id, subscription_id, client_id, sealed_secret: <base64>, location}`.

### 7.1 Validate flow

Mirror GCP §6.1:
1. Unseal client_secret.
2. Build azidentity.ClientSecretCredential.
3. Build armcompute.VirtualMachinesClient with subscription_id.
4. Call NewListAllPager and fetch the first page (don't paginate
   the full list during validate).
5. Return `{ok: true, instance_count: <first page count>}`.

Error_kinds: `permission_denied` (403, "AuthorizationFailed"),
`subscription_not_found` (404), `tenant_invalid` (auth failure
with tenant-related message), `credentials_invalid` (auth failure
with secret/client mismatch), `network`.

## 8. UI

`/discovery/azure` page mirroring `/discovery/aws` and
`/discovery/gcp`. Wizard / Inventory / Recommendations tabs.

The wizard has 5 steps:

**Step 1: Connect an Azure subscription**
- Display name
- Tenant ID (Azure AD tenant — UUID format validated)
- Subscription ID (UUID format)
- Location (optional, e.g. "eastus")

**Step 2: Create the Service Principal**
- Read-only az CLI commands:
  ```
  az ad sp create-for-rbac \
    --name "Squadron Discovery" \
    --role "Reader" \
    --scopes "/subscriptions/<subscription_id>"
  ```
- The output JSON includes `appId`, `password`, `tenant`.
  Wizard explains: `appId` is the Client ID, `password` is the
  Client Secret, `tenant` is the Tenant ID. Operator pastes the
  three values into the next step.

**Step 3: Paste credentials**
- Client ID input
- Client Secret textarea (password-masked input)
- Acknowledgment checkbox warning
- Wizard validates that all three are present and have the
  expected UUID / secret formats.

**Step 4: Validate**
- Calls validate endpoint, surfaces error_kind-specific
  remediation per §7.1.

**Step 5: Scan**
- Triggers scan, transitions to Inventory.

## 9. Slice 1 service category: Virtual Machines

Walks `armcompute.VirtualMachinesClient.NewListAllPager` for all
VMs in the subscription, filtered to `location == s.Location` if
non-empty.

Each VM projects to `ComputeInstanceSnapshot`:
- ResourceID: VM Name
- InstanceType: vm.Properties.HardwareProfile.VMSize (e.g.
  "Standard_D4s_v3")
- Tags: VM Tags map
- HasOTel: any tag key matching otel* case-insensitive
- OSFamily: derived from `vm.Properties.StorageProfile.OsDisk.OSType`
  (Linux or Windows; Azure exposes this cleanly unlike AWS/GCP)
- Region: vm.Location

The OS family detection bonus is worth noting: Azure exposes it
in the same response as the VM listing, so we get it for free.
AWS and GCP slice 1 leave OSFamily="unknown" — Azure does not.

Recommendation kind: `vm-otel-tag`.

Service identifier for partial failures: `azurevm`.

## 10. Proposer integration

Extends DiscoveryScanContext.Provider to accept "azure". Adds
TenantID + SubscriptionID fields, populated when Provider="azure".

`ScopeID()` helper extended:
```go
func (c *DiscoveryScanContext) ScopeID() string {
    switch c.Provider {
    case "gcp":
        return c.ProjectID
    case "azure":
        return c.SubscriptionID
    default: // "aws"
        return c.AccountID
    }
}
```

System prompt extension lists the new `vm-otel-tag` kind targeting
the `azurerm_virtual_machine` Terraform resource (or
`azurerm_linux_virtual_machine` / `azurerm_windows_virtual_machine`
for the newer split resources — pick based on the OSFamily field).

Branch encoding stays the same shape. `vm-` prefix on the kind
indicates Azure. Webhook handler payload composition:

```go
provider := "aws"
if strings.HasPrefix(kind, "gce-") {
    provider = "gcp"
} else if strings.HasPrefix(kind, "vm-") {
    provider = "azure"
}
```

Audit payload carries `account_id` (AWS), `project_id` (GCP), or
`subscription_id` (Azure) with the other two empty, plus
`provider`.

The `ListDiscoveryVerdicts` storage query extends to OR-match a
third payload field:
```sql
payload->>'account_id' = ?
OR payload->>'project_id' = ?
OR payload->>'subscription_id' = ?
```

## 11. Audit events

Six new constants paralleling GCP:

```go
const AuditEventDiscoveryAzureConnectionCreated      = "discovery.azure.connection_created"
const AuditEventDiscoveryAzureConnectionDeleted      = "discovery.azure.connection_deleted"
const AuditEventDiscoveryAzureScanStarted             = "discovery.azure.scan_started"
const AuditEventDiscoveryAzureScanCompleted           = "discovery.azure.scan_completed"
const AuditEventDiscoveryAzureScanFailed              = "discovery.azure.scan_failed"
const AuditEventDiscoveryAzureRecommendationsGenerated = "discovery.azure.recommendations_generated"
```

Payload shapes match GCP/AWS analogs with `subscription_id`
instead of `project_id`/`account_id`.

## 12. Threat model

Inherits from GCP and AWS arcs with one Azure-specific addition:

**Service Principal client_secret rotation.** Azure SPs support
multiple concurrent secrets per app registration, which makes
rotation safer than GCP SA keys (no SA-without-key gap during
rotation). Document this in the runbook as an advantage.

**Reader role scope.** The wizard creates the SP with `Reader`
at the subscription level — read access to ALL resources in the
subscription, not just VMs. Slice 1 accepts this for simplicity;
slice 2 can document creating a custom role with only
`Microsoft.Compute/virtualMachines/read` and
`Microsoft.Compute/virtualMachines/instanceView/action` permissions.

**Subscription mismatch.** Validate cross-checks the configured
subscription_id against what the SP can actually access. If the
SP was scoped to a different subscription, validate returns
`subscription_not_found` with a humanized remediation.

## 13. Slice 1 contract

**In:**

1. `internal/discovery/azure/scanner.go`: Scanner implementing
   scanner.Scanner via azidentity + armcompute.
2. `internal/discovery/azure/types.go`, consts.go.
3. `internal/discovery/azureconnstore/`: storage package (sqlite
   + memory + migrations) mirroring gcpconnstore exactly.
4. Schema migration in azureconnstore: new gcpconnstore-style
   own-db / own-version (do not bump applicationstore SchemaVersion).
5. `internal/discovery/credstore/azure_sp.go`: Seal +
   UnsealAzureClientSecret with domain-tagged AAD.
6. 6 audit event constants.
7. HTTP handlers in `internal/api/handlers/discovery_azure.go`
   per §7 endpoint surface.
8. UI page at `ui/src/pages/DiscoveryAzure.tsx` paralleling
   DiscoveryGCP.tsx, with wizard / inventory / recommendations.
9. Wizard step data at `ui/src/data/azureDiscoveryWizard.ts`.
10. Proposer extension: Provider="azure" path,
    vm-otel-tag kind, system prompt addition, branch encoding
    `vm-` prefix detection.
11. ListDiscoveryVerdicts OR-match third payload field.
12. Operator runbook at `docs/discovery-azure-first-time-setup.md`.

**Out:**

- Azure SQL, AKS, Blob, Load Balancer scanners.
- WIF / Managed Identity / Certificate SP credentials.
- Multi-subscription orchestration.
- Cross-tenant federation.
- Azure Resource Graph batched queries.
- Azure Arc-enabled servers.

## 14. Open questions

1. **azurerm_virtual_machine vs split resources.** Newer
   Terraform azurerm provider versions split into separate
   `azurerm_linux_virtual_machine` and
   `azurerm_windows_virtual_machine` resources. The proposer
   should target the right one based on OSFamily. Slice 1
   ships this discrimination since Azure gives us OS for free.
2. **Tag key normalization.** Azure tag keys are case-sensitive
   in storage but case-insensitive in comparison contexts. The
   otel* detection rule uses lowercase comparison — confirm in
   testing that mixed-case keys round-trip cleanly.
3. **Pagination cap.** A subscription with 10000+ VMs takes a
   while to enumerate. Slice 1 walks the full pager with no
   artificial cap; slice 2 candidate is per-page rate limit
   and a soft "show first N" mode.
4. **Reader role across all VMs vs targeted scope.** Some
   teams will refuse `Reader` at subscription scope. The
   custom-role alternative path needs runbook documentation.
5. **Service Principal secret expiry handling.** Azure SP
   secrets have a default 1-year expiry. The validate endpoint
   surfaces `credentials_invalid` when the secret expires;
   slice 2 candidate is proactive expiry detection before
   scan time.

## 15. Acceptance tests

1. Round-trip create + read AzureConnection. sealed_secret
   absent from response.
2. Seal + unseal client_secret. Domain separation tests against
   PAT, webhook secret, GCP SA bytes.
3. Validate with mocked Azure REST API (httptest server). Happy
   path returns ok+instance_count.
4. Validate with permission_denied. Mock returns 403 with
   AuthorizationFailed body. error_kind=permission_denied.
5. Validate with subscription_not_found. Mock returns 404.
6. Validate with subscription_mismatch.
7. Scan returns ComputeInstanceSnapshot entries with OSFamily
   correctly set from VM osType.
8. Scan partial failure on rate limit (429).
9. Recommendations endpoint emits vm-otel-tag kind.
10. UI wizard step 1 tenant_id + subscription_id UUID validation.
11. UI wizard step 4 surfaces all 5 error_kinds with specific
    remediation copy.
12. AWS + GCP cold-start parity preserved.
13. Branch encoding round-trip: `squadron/rec/vm-otel-tag/<subscription_id>/<region>/<id>`
    parses correctly; audit payload carries
    `subscription_id` + `provider="azure"`.

## 16. Implementation chunks

Tighter than GCP because the substrate is now twice-proven:

- **Chunk 1: Foundation** — storage type + azureconnstore + SP
  sealing + audit constants. ~600-800 lines. v0.89.51.
- **Chunk 2: Scanner** — internal/discovery/azure package +
  VM scanner + tests against mocked REST API. ~600-800 lines.
  v0.89.52.
- **Chunk 3: API handlers** — HTTP endpoints + validate +
  scan + tests. ~700-900 lines. v0.89.52 (parallel with chunk 2).
- **Chunk 4: UI page + wizard** — DiscoveryAzure.tsx + wizard
  data + tests. ~800-1000 lines. v0.89.53.
- **Chunk 5: Proposer integration** — Provider="azure" path +
  vm-otel-tag kind + branch encoding refresh + ListDiscoveryVerdicts
  third OR-match. ~500-700 lines. v0.89.53 (parallel with chunk 4).
- **Chunk 6: Runbook** — discovery-azure-first-time-setup.md.
  ~400-600 lines. v0.89.54.

Total estimated 4-5 release tags across 6 chunks (chunks 2+3 and
4+5 parallelize cleanly).

## 17. Slice 2+ candidates

Listed in §2 plus:

- Multi-subscription orchestrator (parallels v0.89.7a)
- Azure SQL scanner (slice 2 primary)
- AKS scanner (slice 3 primary)
- Azure Blob scanner
- Azure Load Balancer / Application Gateway scanner
- Workload Identity Federation auth
- Managed Identity inheritance
- Service Principal certificate credentials
- Azure Resource Graph batched queries for higher throughput
- Azure Arc-enabled servers (hybrid on-premises VMs)
- Custom Reader-equivalent role with minimum permissions
- Cross-tenant federation
- Multi-tenant Squadron isolation review
- SP secret expiry proactive detection

---

**Strategic frame:**

This is the third cloud and the second non-AWS arc. The
substrate (scanner interface, credstore, audit shape, branch
encoding, proposer Provider discriminator) has been twice-proven
in production after GCP slice 1; the marginal cost of adding
each subsequent cloud should keep dropping.

After Azure slice 1 ships, the public positioning becomes
"Squadron is the universal observability control plane that
scans AWS, GCP, AND Azure fleets." This is the moment to land
the Tuesday LinkedIn arc Michael's been holding (#645): the
post can lead with "Squadron now scans all three major clouds"
and back-reference the cost-spike + drift detection thesis from
the Thursday tease. The credibility weight of "three clouds" is
materially different from "one cloud" or "two clouds."

The next provider after Azure (if there's demand) would be
Oracle Cloud or Alibaba Cloud. Both reuse the substrate fully.
Slices for each should ship in 3-4 chunks once the patterns are
this well-established.
