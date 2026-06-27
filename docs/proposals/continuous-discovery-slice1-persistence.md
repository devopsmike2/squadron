# Continuous discovery — scan persistence (slice 1)

Status: building v0.89.250. Author: autonomous session.

## Problem

Discovery scans are **synchronous and non-persisted**. `runAWSScan` (and the
GCP/Azure/OCI equivalents) run the scanner inline, emit `scan_started` /
`scan_completed` audit events, and return the typed result to the HTTP caller.
Nothing is stored. The handler comments say so plainly: "slice 3 introduces
scheduled scans with the result persisted asynchronously."

Consequences for a real adopter (first-adopter-readiness.md gap #2, "the
biggest 'is it actually continuous?' gap"):

- No scan history — you cannot see what a previous scan found.
- No drift — you cannot diff today's inventory against last week's.
- No foundation for scheduled re-scans (the eventual continuous engine).

Every scan recomputes from scratch and the result evaporates when the HTTP
response is sent. The only durable trace is the `scan_completed` audit event,
which carries summary counts but not the inventory.

## Approach (slice 1 — persistence + history, AWS first)

The `scanner.Result` is already self-describing (scan_id, started/completed
timestamps, provider, scope, regions, all snapshots, partial flag). Persisting
it is mostly serialization.

1. **ScanStore** — a new method group on the application store (memory +
   sqlite): `SaveDiscoveryScan`, `ListDiscoveryScans(provider, scopeID,
   limit)`, `GetDiscoveryScan(scanID)`. Backed by a `discovery_scans` table.
2. **ScanRecord** type — scan_id, provider, scope_id, regions, started/
   completed, partial + reason, a `summary` map (category→count for cheap
   listing), and the full marshaled `result_json` (the inventory, for the
   detail view + future drift). Listing omits `result_json`; the get-one
   endpoint includes it.
3. **Persist after scan** — `runAWSScan` calls a best-effort `recordScan`
   right after the `scan_completed` audit event. Persistence failure logs but
   never fails the scan (same fail-open posture as the audit emission).
4. **History endpoints** (AWS, slice 1):
   - `GET /discovery/aws/connections/:id/scans` — newest-first list (summary
     rows, no inventory blob).
   - `GET /discovery/aws/connections/:id/scans/:scanID` — one scan with the
     full inventory.
   Both are `agents:read`, mirror the existing discovery route + trampoline
   wiring, and 503 with a clear message when the store isn't wired (same
   posture as the exclusion store).

## Scope / honest framing

- **AWS only in slice 1.** The store + ScanRecord are provider-neutral; slice 2
  wires the persist call + history endpoints for GCP/Azure/OCI (small,
  mechanical — they share `recordScan`).
- **Still synchronous.** This slice persists results; it does NOT make scans
  async or scheduled. That is slice 3 (a scheduled-scan engine). The route
  shape stays stable so slice 3 can shrink the POST to "scan_id, queued".
- **No drift computation yet.** Storing successive results is the prerequisite;
  diffing two scans is slice 2/3 once multiple clouds persist.
- **Retention.** Slice 1 stores every scan unbounded. A retention cap (keep
  last N per scope, or TTL) is a slice-2 follow-up — noted, not built, so we
  don't silently grow the DB without saying so.
- **Demo scans are NOT persisted** — the demo connection short-circuits at the
  top of runAWSScan (returns the canned result before the persist call), so the
  trial Inventory stays history-free. Harmless and avoids demo clutter.

## Note on existing per-instance tables

`serverless_instance` / `orchestration_instance` / `event_source_instance`
tables exist with Save/List methods, but they are **dormant** — defined +
unit-tested, never wired into the scan path (no non-test call sites). They are
a finer (per-resource) grain; `discovery_scans` is the whole-scan primitive
that directly answers "list past scans / show one scan" and carries per-scan
metadata (started/completed/partial) those tables lack. Not redundant.

## Tests

- Store (memory + sqlite parity): Save → List (newest-first, result_json
  omitted) → Get (full). Empty/nonexistent returns.
- Handler: a scan persists a record; list returns it; get returns the full
  inventory; get of an unknown scan_id 404s; cross-scope get (scanID belongs to
  a different account) 404s; unwired store 503s.
