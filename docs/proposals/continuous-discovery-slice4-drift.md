# Continuous discovery — drift (slice 4)

Status: shipped v0.89.253. Author: autonomous session. Builds on slices 1-3
(scan persistence + history + scheduled re-scans).

## Problem

Persistence + scheduling mean scan history now accrues. The payoff is being
able to answer "what changed in my fleet since last scan?" — new resources,
removed resources, and instrumentation turning on/off. That diff is drift.

## Approach (all four clouds — read-only over persisted scans)

`internal/discovery/discoverydrift` — a cloud-agnostic diff over two persisted
scan blobs. Every cloud's stored scan JSON uses the same field names for the
shared snapshot types (`resource_id` / `has_otel` / `has_otel_layer`), so one
parser + one diff cover AWS / GCP / Azure / OCI.

`Between(olderJSON, newerJSON) Diff` reports, per category (compute, functions,
databases, clusters): `added` + `removed` (by resource_id) and, for the two
categories with a single instrumentation boolean (compute, functions),
`instrumentation_changed` flips. Plus roll-up totals.

Endpoint (all four clouds): `GET /discovery/<cloud>/connections/:id/drift`.
With explicit `?from=&to=` scan ids it diffs those (404 if either is missing or
belongs to a different provider/scope); otherwise it diffs the two most recent
scans (older → newer). Returns 200 `{insufficient_history:true}` when fewer
than two scans exist. `agents:read`; 503 when the scan store isn't wired.

It is purely read-only over the slice-1 scan store — no scanner, no per-cloud
scan refactor, so all four clouds shipped together via thin handler wrappers
over a shared `writeDrift`.

## Scope / honest framing

- **Drift path is `/connections/:id/drift`**, not `/scans/drift` — the latter
  would collide with the `/scans/:scanID` route in gin.
- **Instrumentation flips are compute + functions only.** Databases and
  clusters carry multi-axis observability (PI/Enhanced Monitoring; api+audit
  logs), so drift reports their add/remove but not a single boolean flip — a
  later slice can add per-axis flip detection.
- **Resource identity is `resource_id`.** A resource recreated with a new id
  reads as remove+add (correct for inventory drift).
- **No alerting/notification.** This surfaces drift on request; turning a drift
  into a proactive notification (e.g. "N new uninstrumented resources since
  yesterday") is a later slice.
