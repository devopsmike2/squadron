# Continuous discovery — scheduled re-scans (slice 3a)

Status: building v0.89.252. Author: autonomous session. Builds on slices 1-2
(scan persistence).

## Problem

Slices 1-2 made scans persist, but scans still only happen when an operator
clicks "Scan" (or POSTs the endpoint). For discovery to be *continuous* —
history that accrues on its own, a basis for drift over time — scans must
re-run automatically. This slice adds an opt-in background scheduler.

## Approach (slice 3a — AWS, opt-in, default off)

A small, generic scheduler that on a fixed interval lists connections and runs
+ persists a scan for each. It reuses the existing scan path verbatim (no scan
logic is duplicated): the per-account run goes through the same `runAWSScan`
that the HTTP handler calls, which already emits audit events and persists via
the slice-1 scan store.

1. **`internal/discovery/scanscheduler`** — a dependency-free `Scheduler`:
   `Interval`, `Concurrency`, `ListAccounts(ctx) ([]string, error)`,
   `ScanAccount(ctx, id) error`, logger. `RunOnce` does one sweep (list + scan
   each, bounded concurrency, per-account failures logged not fatal); `Run`
   loops `RunOnce` on a ticker until the context is cancelled. Unit-testable
   with fakes, no api/handlers import.
2. **`DiscoveryHandlers.RunScanForAccount(ctx, id)`** — exported wrapper over
   `runAWSScan(ctx, id, nil, nil, "")`; returns an error on failure. Persists
   via the already-wired scan store.
3. **`Server.StartDiscoveryScanScheduler(ctx, interval)`** — builds the AWS
   handler from the server's existing deps (credstore + cred key + audit + scan
   store), wires `ListAccounts` = AWS `ListConnections` (minus the demo
   account) and `ScanAccount` = `RunScanForAccount`, and launches
   `go sched.Run(ctx)`.
4. **Config** — `SQUADRON_DISCOVERY_SCAN_INTERVAL` (Go duration, e.g. `6h`).
   Empty / unparseable / <= 0 ⇒ disabled. **Default OFF.** main.go parses it
   and starts the scheduler against a cancellable context tied to shutdown.

## Scope / honest framing

- **Opt-in, default OFF — deliberately.** Auto-scanning real cloud accounts on
  a timer has cost + API-rate implications. Operators must set the interval
  explicitly. The env doc states this plainly.
- **AWS only (slice 3a).** GCP/Azure/OCI scheduling is slice 3b (mechanical —
  the scheduler is provider-agnostic; only the per-cloud handler + list
  function differ). The sync + history endpoints already work on all four.
- **Reuses the synchronous scan path in a goroutine.** This is NOT the async
  scan HTTP API (returning a job id from POST) — that's a separate concern; the
  on-demand POST endpoint stays blocking. The scheduler simply doesn't run on
  an HTTP request, so blocking isn't a problem there.
- **First sweep after one interval** (not on boot) to avoid a surprise scan at
  startup. Noted; could add an opt-in "scan on start" later.
- **No drift yet.** Successive persisted scans are the prerequisite; diffing
  them into a drift view is the next slice.
- **Min interval floor.** A tiny interval would hammer the cloud APIs; the
  scheduler enforces a sane floor (e.g. 15m) and logs if the configured value
  is raised to it.

## Tests

- scanscheduler `RunOnce`: lists N accounts, scans all, a per-account error is
  logged + counted but does not abort the sweep; respects concurrency; empty
  list is a no-op. `Run` stops promptly on context cancel.
- `RunScanForAccount` happy path + error surfaced (reuse the handler fakes).
