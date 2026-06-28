# Arc: cloud coverage parity (object-stores + load-balancers)

AWS discovery covers 8 tiers; Azure/GCP/OCI cover 6 each, missing
**object stores** and **load balancers**. This arc brings those two
tiers to the other three clouds so all four match, then (later arcs)
adds net-new tiers (API gateways, caches, CDN).

The snapshot types are already cloud-agnostic
(`scanner.ObjectStoreSnapshot`, `scanner.LoadBalancerSnapshot`) and
`scanner.Result` already carries `ObjectStores` / `LoadBalancers` — the
non-AWS scanners simply don't populate them yet. Instrumented-count
axes mirror AWS: object store = access/usage logging configured; load
balancer = access logs enabled.

Slices (each: scanner walk + Result wiring + instrumented count +
scan-response marshal + tests; UI sub-tab + proposer snippet follow):

1. GCP object stores (GCS buckets) — this slice.
2. GCP load balancers.
3. Azure object stores (Blob) + load balancers.
4. OCI object storage + load balancers.
5. UI sub-tabs + proposer snippets for the new tiers.

## Slice 5 — surfacing + recommendations (shipped)

**5a (UI, v0.89.282).** The two new tiers render in the GCP/Azure/OCI
Inventory tabs via a shared `InventoryTierTables` component (Object
stores + Load balancers sub-tabs), matching the inventory AWS already
showed.

**5b (proposer recommendations, v0.89.283).** Object-store and
load-balancer rows now reach the proposer with a `provider` tag so the
model routes to the correct per-cloud observability lever:

| Cloud | Object store lever | Kind | Load balancer lever | Kind |
|-------|--------------------|------|---------------------|------|
| AWS   | S3 server access logging | (existing) | ALB/NLB access logs → S3 | (existing) |
| GCP   | GCS bucket logging (`logging.log_bucket`) | `gcs-logging-enable` | Backend-service `log_config.enable` | `gclb-logging-enable` |
| Azure | Blob diagnostic setting (StorageRead/Write/Delete) | `azblob-diag-enable` | LB diagnostic setting | `azlb-diag-enable` |
| OCI   | **detection deferred — inventory only** | — | **detection deferred — inventory only** | — |

**OCI honesty note.** OCI object-store and load-balancer access logs are
delivered through the OCI **Logging service**, which Squadron's
read-only discovery scan does not yet inspect. The inline flags the
scanner *can* read (`objectEventsEnabled` for buckets; nothing for LBs)
are not telemetry levers, so emitting "enable logging" recommendations
off them would produce false positives. Both OCI tiers therefore render
as `detection deferred (inventory only — do not recommend)` and the
proposer prompt instructs the model not to recommend for them. Closing
this gap requires reading the OCI Logging service during the scan — a
future slice.
