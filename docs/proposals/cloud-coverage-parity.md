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
