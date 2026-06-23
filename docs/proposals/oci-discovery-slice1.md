# OCI (Oracle Cloud) discovery — slice 1 design

**Status:** design doc, locked for slice 1 implementation. Fourth
cloud arc following AWS (#558), GCP (gcp-discovery-slice1.md), and
Azure (azure-discovery-slice1.md). The substrate is now triply-
proven across three clouds: scanner.Scanner interface, credstore
domain-tagged AAD sealing, provider-aware audit shape,
provider-agnostic branch encoding with kind-prefix detection,
DiscoveryScanContext.Provider discriminator,
ListDiscoveryVerdicts multi-payload-field OR-match,
provider-aware UI page template, runbook structure.

The marginal cost of adding cloud N+1 keeps dropping; this design
doc is tighter than GCP's (695 lines) or Azure's (514 lines) because
the architectural decisions are now decisively made by precedent
and the OCI-specific delta is small. Slice 1 implementation
estimated at 4-5 chunks, faster than GCP's 6 or Azure's 5-6.

After OCI slice 1 ships, the universal observability claim
expands to four clouds: "Squadron is the universal observability
control plane that scans AWS, GCP, Azure, AND Oracle Cloud
fleets." This is the strongest version of the claim that a
single OSS control plane can credibly support.

**See also:**
[Azure discovery slice 1 design](./azure-discovery-slice1.md),
[GCP discovery slice 1 design](./gcp-discovery-slice1.md),
[AWS discovery design (#558)](./558-discovery-design.md),
[discovery-iac-first-time-setup.md](../discovery-iac-first-time-setup.md).

## 1. Problem

Operators with Oracle Cloud-resident workloads see no inventory,
no recommendations, no IaC PRs from Squadron's surface today.
After Azure slice 1, the universal claim covers three major
clouds — but Oracle Cloud is significant in enterprise (especially
database-heavy workloads, large institutional users) and the gap
weakens the claim against enterprise buyers evaluating Squadron
against multi-cloud requirements.

OCI slice 1 closes the gap with one OCI service category (Compute
Instances) and the substrate to grow into the other categories
(Autonomous Database, OKE managed Kubernetes, Object Storage,
Load Balancer) in slices 2-5.

## 2. Non-goals (slice 1)

- **Autonomous Database / DB Systems.** Slice 2.
- **OKE (Oracle Kubernetes Engine).** Slice 3.
- **Object Storage.** Slice 4.
- **Load Balancer.** Slice 5.
- **Instance Principals (running Squadron on OCI VM).** Slice 2.
  Parallel to Azure Managed Identity / GCP ADC reasoning: couples
  Squadron's deployment to OCI.
- **Resource Principals (for OCI Functions / Container Engine).** Same.
- **Multi-tenancy scans.** Slice 2 candidate (parallel to AWS
  multi-account v0.89.7a).
- **Multi-region scans.** Slice 1 ships single-region per
  connection (or empty = all regions). Multi-region orchestrator
  is slice 2.
- **OCI Resource Search API.** A higher-throughput batched query
  surface analogous to Azure Resource Graph. Slice 2 candidate.

## 3. Architectural decision: credential model

OCI uses **API Signing Keys** — RSA keypair-based authentication.
The operator generates an RSA keypair, uploads the public key to
OCI Console (or via CLI), and gets back a key fingerprint. The
private key + fingerprint + tenancy_ocid + user_ocid + region
constitute the full credential set.

This is materially different from:
- AWS (assumed role + ExternalID)
- GCP (Service Account JSON)
- Azure (Service Principal client_secret)

Three credential model options for OCI:

### Option A — API Signing Key (operator-managed)

Operator generates the keypair locally, uploads public key to OCI,
pastes private key + the OCID fields + fingerprint into Squadron.
Squadron seals the private key via credstore with domain-tagged
AAD `squadron.oci_signing_key.v1`. Scanner signs each ARM API
request using the unsealed private key.

**Picked for slice 1.** Native OCI auth pattern. Mirrors the
"paste a credential" wizard step pattern operators already know
from GCP SA JSON and Azure SP secret. The signing-per-request
overhead is negligible (operations bounded, signing is in-memory).

### Option B — Instance Principal (Squadron running on OCI)

Squadron on an OCI Compute instance can authenticate without
explicit credentials via the Instance Principal mechanism.

**Deferred to slice 2.** Same reasoning as GCP ADC and Azure
Managed Identity: couples Squadron's deployment to one specific
cloud. Squadron's value is platform-agnostic.

### Option C — Resource Principal

For OCI Functions / Container Engine workloads.

**Deferred to slice 2.** Niche use case for slice 1.

## 4. Scanner interface

Reused. New `internal/discovery/oci` package implementing
scanner.Scanner. Uses the OCI Go SDK's ComputeClient to list
instances.

Authentication: the SDK accepts a `common.ConfigurationProvider`
that holds tenancy_ocid + user_ocid + fingerprint + private_key +
region. Squadron constructs this from credstore-unsealed fields.

For the slice 1 scope (Compute Instances), the relevant SDK call
is `ListInstances(ctx, ListInstancesRequest)` per compartment.
OCI organizes resources by Compartments (similar to GCP folders
but more deeply nested). Slice 1 walks all compartments visible
to the user_ocid; slice 2 adds compartment scoping.

## 5. Storage

New `OCIConnection` row type:

```go
type OCIConnection struct {
    ID            string
    DisplayName   string
    TenancyOCID   string  // ocid1.tenancy.oc1..<unique_id>
    UserOCID      string  // ocid1.user.oc1..<unique_id>
    Fingerprint   string  // key fingerprint, e.g. xx:xx:xx:...
    SealedPrivateKey []byte `json:"-"`  // credstore-sealed RSA private key (PEM)
    Region        string  // e.g. "us-phoenix-1"
    LearnFromAcceptedRecommendations bool
    CreatedAt     time.Time
    UpdatedAt     time.Time
}
```

New `ociconnstore` package paralleling `azureconnstore` /
`gcpconnstore` exactly: own SQLite file
(`ociconnstore.db`), own schema versioning, own Store interface
with Create/Get/List/Update/Delete methods.

Migration: `CREATE TABLE oci_connections (...)` with index on
`tenancy_ocid`.

Note OCI requires `Region` always — unlike AWS / GCP / Azure
which allow empty Region for "scan all". OCI's API endpoints
are regional, so the scanner must know which region to query.
Slice 1 ships single-region per connection; multi-region
orchestrator is slice 2.

## 6. Credstore extension

New `internal/discovery/credstore/oci_signing_key.go`:

```go
func SealOCIPrivateKey(key *Key, plaintext []byte) ([]byte, error)
func UnsealOCIPrivateKey(key *Key, sealed []byte) ([]byte, error)
```

Domain-tagged AAD: `"squadron.oci_signing_key.v1"`. Pattern
parallels gcp_sa.go and azure_sp.go exactly. Domain separation
tests verify the sealed bytes cannot be mis-unsealed as PAT,
webhook secret, GCP SA, OR Azure client_secret (defense in depth
across the FOUR credential domains now present in credstore).

## 7. API endpoints

Mirror Azure surface at `/api/v1/discovery/oci/*`:

- POST   /api/v1/discovery/oci/connections
- GET    /api/v1/discovery/oci/connections
- GET    /api/v1/discovery/oci/connections/:id
- PATCH  /api/v1/discovery/oci/connections/:id
- DELETE /api/v1/discovery/oci/connections/:id
- POST   /api/v1/discovery/oci/connections/:id/validate
- POST   /api/v1/discovery/oci/connections/:id/scan
- POST   /api/v1/discovery/oci/connections/:id/recommendations

Create request: `{display_name, tenancy_ocid, user_ocid, fingerprint, sealed_private_key: <base64>, region}`.

### 7.1 Validate flow

1. Unseal private_key.
2. Construct OCI ComputeClient with credentials.
3. Call ListInstances on the user's home compartment with
   limit=1.
4. Return `{ok: true, instance_count: <count>}` on success.

Error_kinds:
- **permission_denied** — 403 / "NotAuthorizedOrNotFound" body.
- **tenancy_not_found** — 404 on tenancy.
- **fingerprint_mismatch** — auth error mentioning key fingerprint.
- **private_key_invalid** — local parse/RSA error before request.
- **network** — transport-level.

## 8. UI

New `/discovery/oci` page mirroring `/discovery/azure`. 5-step
wizard:

**Step 1: Connect an OCI tenancy**
- Display name
- Tenancy OCID (regex validation: `^ocid1\.tenancy\.oc1\.\..+`)
- User OCID (regex: `^ocid1\.user\.oc1\.\..+`)
- Region (required, e.g. "us-phoenix-1") with dropdown of common
  OCI regions

**Step 2: Generate the API signing key**
- Read-only oci CLI commands:
  ```
  oci setup keys --output-dir ~/.oci
  ```
- Or via openssl:
  ```
  openssl genrsa -out ~/.oci/oci_api_key.pem 2048
  openssl rsa -pubout -in ~/.oci/oci_api_key.pem -out ~/.oci/oci_api_key_public.pem
  openssl rsa -pubout -outform DER -in ~/.oci/oci_api_key.pem | openssl md5 -c
  ```
- The last command outputs the key fingerprint. Save it.

**Step 3: Upload public key to OCI**
- Instructions: Navigate to OCI Console → Identity → Users →
  select user → API Keys → Add API Key → paste public key.
- Console returns the fingerprint and a copyable config snippet.

**Step 4: Paste credentials into Squadron**
- Fingerprint input
- Private key textarea (paste contents of oci_api_key.pem
  including BEGIN/END PRIVATE KEY markers)
- Acknowledgment checkbox

**Step 5: Validate + Scan**
- Submit triggers createOCIConnection + validate, then scan on
  Next.

## 9. Slice 1 service category: Compute Instances

Walks `ListInstances` across all compartments visible to the user
(slice 1 walks the root compartment + first-level child
compartments; slice 2 adds full compartment tree walk).

Each instance projects to ComputeInstanceSnapshot:
- ResourceID: instance.DisplayName
- InstanceType: instance.Shape (e.g. "VM.Standard.E4.Flex")
- Tags: instance.FreeformTags + instance.DefinedTags (flattened)
- HasOTel: any tag key matching otel* case-insensitive
- OSFamily: derived from instance.LaunchOptions or Image
  metadata if accessible; default "unknown" for slice 1
- Region: instance.Region

Recommendation kind: `compute-otel-tag`.

Service identifier for partial failures: `ocicompute`.

## 10. Proposer integration

Extends Provider field to accept "oci". Adds TenancyOCID +
UserOCID + CompartmentID fields on DiscoveryScanContext.

`ScopeID()` extended:
```go
case "oci":
    return c.TenancyOCID
```

System prompt extension lists `compute-otel-tag` kind targeting
`oci_core_instance` Terraform resource (the OCI Terraform
provider's resource type for compute instances).

Branch encoding: `compute-` prefix detection routes to provider="oci".

ListDiscoveryVerdicts query extends to a fourth OR-match:
```sql
payload->>'account_id' = ?
OR payload->>'project_id' = ?
OR payload->>'subscription_id' = ?
OR payload->>'tenancy_ocid' = ?
```

## 11. Audit events

Six new constants:

```go
const AuditEventDiscoveryOCIConnectionCreated         = "discovery.oci.connection_created"
const AuditEventDiscoveryOCIConnectionDeleted         = "discovery.oci.connection_deleted"
const AuditEventDiscoveryOCIScanStarted                = "discovery.oci.scan_started"
const AuditEventDiscoveryOCIScanCompleted              = "discovery.oci.scan_completed"
const AuditEventDiscoveryOCIScanFailed                 = "discovery.oci.scan_failed"
const AuditEventDiscoveryOCIRecommendationsGenerated    = "discovery.oci.recommendations_generated"
```

## 12. Threat model

Inherits from the three prior arcs. OCI-specific addition:

**Private key at rest.** RSA private keys are the strongest
credential type Squadron handles (full asymmetric authentication
material). Same credstore sealing posture; same never-log,
never-embed-in-audit, never-echo invariants. The runbook
explicitly notes the private key value should never be pasted
into Slack, email, or any other transient surface.

**Key fingerprint validation.** The wizard could compute the
fingerprint client-side from the pasted private key and
cross-check against the operator-entered fingerprint. Catches
the operator mistake of pasting wrong key. Slice 1 ships
client-side fingerprint computation.

**Key rotation.** OCI supports multiple concurrent API keys per
user (up to 3). Rotation is safe: add new key, update Squadron,
delete old key. Same posture as Azure SP multi-secret.

## 13. Slice 1 contract

**In:**
1. internal/discovery/oci/scanner.go: Scanner.
2. internal/discovery/ociconnstore/: storage package.
3. credstore/oci_signing_key.go: Seal+Unseal helpers.
4. 6 audit event constants.
5. HTTP handlers in discovery_oci.go.
6. UI page DiscoveryOCI.tsx + wizard data.
7. Proposer Provider="oci" path + compute-otel-tag kind +
   branch encoding compute- prefix + ListDiscoveryVerdicts
   fourth OR-match.
8. Operator runbook discovery-oci-first-time-setup.md.

**Out:** Autonomous DB, OKE, Object Storage, Load Balancer,
Instance Principals, Resource Principals, multi-tenancy,
multi-region orchestrator, Resource Search API.

## 14. Open questions

1. **OCI Go SDK as dependency.** The slice 1 scanner can use
   github.com/oracle/oci-go-sdk/v65 or implement the request
   signing manually. SDK adds significant transitive deps; manual
   signing is ~150 lines (request canonicalization + RSA-SHA256
   sign + Authorization header). Pick manual for slice 1
   consistency with the GCP/Azure scanners' "manual REST"
   approach. Slice 2 can revisit if multi-service scope makes
   the SDK worth pulling in.

2. **Compartment scope.** Slice 1 walks root + first-level
   child compartments. Operators with deep compartment trees
   miss instances in grandchild compartments. Slice 1 documents
   this; slice 2 adds full tree walk.

3. **Region required.** OCI's regional endpoints mean Region is
   mandatory unlike AWS/GCP/Azure where empty = scan all. The
   wizard enforces this; the runbook explains.

4. **Tenancy vs compartment as scope_id.** Audit payload carries
   `tenancy_ocid` as scope_id. Recommendation verdicts scope on
   tenancy. Slice 2 candidate: per-compartment scope filtering
   for tenants with isolated team compartments.

5. **OS detection.** Slice 1 leaves OSFamily="unknown" — OCI
   exposes OS via the Image relationship which needs a
   secondary lookup. Slice 2 adds detection.

## 15. Acceptance tests

1. Round-trip create + read OCIConnection. sealed_private_key
   absent from response.
2. Seal + unseal private key. Domain separation against PAT,
   webhook secret, GCP SA, AND Azure client_secret bytes.
3. Validate with mocked OCI API. Happy path returns
   ok+instance_count.
4. Validate with permission_denied (403).
5. Validate with tenancy_not_found (404).
6. Validate with fingerprint_mismatch.
7. Validate with private_key_invalid (malformed PEM).
8. Scan returns ComputeInstanceSnapshot entries.
9. Scan partial failure on rate limit (429).
10. Recommendations endpoint emits compute-otel-tag kind.
11. UI wizard step 1 OCID format validation.
12. UI wizard step 4 surfaces all 5 error_kinds with specific
    remediation copy.
13. AWS + GCP + Azure cold-start parity preserved.
14. Branch encoding round-trip for compute-otel-tag.

## 16. Implementation chunks

Tighter than Azure because patterns are decisively proven:

- **Chunk 1: Foundation** — storage + signing key sealing +
  audit constants. ~600-800 lines. v0.89.56.
- **Chunk 2: Scanner** — internal/discovery/oci package + manual
  request signing + Compute Instance walker + tests. ~700-900
  lines. v0.89.57.
- **Chunk 3: API handlers** — HTTP endpoints + validate + scan
  + tests. ~700-900 lines. v0.89.57 (parallel with chunk 2).
- **Chunk 4: UI page + wizard** — DiscoveryOCI.tsx + wizard
  data + tests. ~800-1000 lines. v0.89.58.
- **Chunk 5: Proposer integration** — Provider="oci" +
  compute-otel-tag + branch encoding + ListDiscoveryVerdicts
  fourth OR-match. ~500-700 lines. v0.89.58 (parallel with
  chunk 4).
- **Chunk 6: Runbook** — discovery-oci-first-time-setup.md.
  ~400-600 lines. v0.89.59.

Total 4 release tags across 6 chunks. Faster than Azure's 5
because chunk 1 + 6 are tighter.

## 17. Slice 2+ candidates

Listed in §2 plus:

- Compartment tree full walk
- Multi-region orchestrator (parallels v0.89.7a)
- Autonomous Database scanner (slice 2 primary)
- OKE scanner (slice 3)
- Object Storage scanner
- Load Balancer scanner
- Instance / Resource Principal auth
- Multi-tenancy isolation review
- Resource Search API
- OS family detection via Image lookup
- Client-side public key fingerprint verification

---

**Strategic frame:**

OCI slice 1 brings Squadron to 4 clouds. The universal
observability claim is now "AWS, GCP, Azure, AND Oracle Cloud" —
the strongest version that a single OSS control plane can
defensibly support without becoming a SaaS dependency hell.
Operators evaluating Squadron against multi-cloud enterprise
requirements see all 4 major Western clouds covered.

After OCI, the natural next moves are:

1. **Slice 2 deepening** across all 4 clouds simultaneously
   (Cloud SQL / Azure SQL / RDS / Autonomous DB — the database
   tier).
2. **Alibaba / Tencent Cloud** for global enterprise reach
   (slice 1 for each in 3 chunks given substrate maturity).
3. **Cross-provider topology view** in the UI that unifies the
   four inventories visually.
4. **Operator-facing "Connect any cloud" universal wizard**
   that picks the right provider sub-wizard based on credential
   shape detected from paste.

The horizontal breadth moat is materially deeper after 4 clouds
than after 3.
