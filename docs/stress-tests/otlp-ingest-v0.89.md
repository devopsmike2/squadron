# OTLP ingest stress test, v0.89

The OTLP receiver → worker pool → DuckDB path is the half of Squadron's
telemetry plane that `docs/scale-testing.md` deliberately deferred
("OTLP receiver under load — needs a synthetic OTLP generator").
v0.89 closes that gap with `otlpsim`, a synthetic OTLP/HTTP load
generator (sibling of `fleetsim`), and this first measured pass over
the ingest path. This document is the methodology, the results, the
findings, and the regression bar.

## TL;DR

At moderate load the path is clean: zero loss, exact accounting, p99
6ms. At saturation the receiver **accepts ~5-10× faster than the
workers can persist**, so a burst leaves up to ~500k items of
202-acknowledged telemetry sitting volatile in the in-memory queue
for 60-90+ seconds — and none of the shipped metrics can see it:
the queue-depth gauge and the OTLP HTTP metrics were declared but
never wired. Nothing is lost if the process stays up; a
crash/restart silently discards the backlog. Data eventually
reconciles exactly (client items == `otlp_batches` == telemetry
rows, to the digit). Four findings filed, two fixed in this pass.

## The tool: otlpsim

```bash
make otlpsim
./build/otlpsim --rate=200 --duration=60s                # baseline shape
./build/otlpsim --agents=500 --rate=1000 --senders=16
./build/otlpsim --rate=2000 --ramp=30s --gzip
```

- Real protobuf `ExportRequest`s over standard OTLP/HTTP
  (`/v1/metrics`, `/v1/logs`, `/v1/traces`) — the same bytes a
  production collector sends.
- Deterministic agent identity **shared with fleetsim**: both derive
  the same UUIDv5 per index and set it as `service.instance.id`,
  which the parser adopts verbatim as `agent_id`. Run both together
  and the OTLP traffic attributes to the live OpAMP fleet.
  `cmd/otlpsim/main_test.go` pins this end-to-end against the real
  parser — if it breaks, every cross-check below is garbage.
- Exact signal mix (`--signal-mix=metrics:70,logs:20,traces:10` is
  dealt out proportionally per 100 requests, not sampled), so client
  counts reconcile against `otlp_batches` exactly.
- Counts 503s as backpressure and does NOT retry them — the point is
  to observe the server's shed behavior, not mask it.

## Environment

Sandboxed Linux VM, 4 cores, arm64, local NVMe. All-in-one binary,
default `squadron.yaml` (worker queue 10,000 / 3 workers / 5s submit
timeout), rollups + AI + pricing disabled. **Absolute numbers are
machine-bound; the ratios and failure modes are the finding.** The
v0.22 fleetsim baselines were a different machine — don't compare
across documents.

## Results

### Baseline — 200 req/s (10k items/s), 100 agents, 25s

| Measure | Result |
|---|---|
| Requests | 4,509 sent, 4,509 OK, **0** 503 / errors |
| Latency | p50 537µs, p95 3.1ms, p99 6.1ms |
| Reconciliation | client 225,450 items / 23,451,496 bytes == `otlp_batches` **exactly**, 0 dropped |
| Queue / dead letters | depth 0, zero dead letters |
| Insights during load | 22ms cold / ~1ms warm (matches v0.24 steady-state numbers) |

### Saturation — 2,000 req/s requested (100k items/s), 32 senders

| Measure | Result |
|---|---|
| Achieved accept rate | ~575-1,240 req/s (client-observed; accept latency ballooned) |
| Request latency | p50 1.9-2.8ms, **p95 141-165ms, p99 231-245ms, max 505ms** |
| 503 backpressure | **0** — queue (10k requests) never filled during 8-25s bursts |
| Worker drain rate | ~6-11k items/s (~115-215 req/s at 50 items/req) |
| Ack-to-durable lag | backlog at burst end ≈ 10k requests ≈ 500k items ≈ **60-90+s of volatile data** |
| Eventual consistency | counts kept climbing post-load (99k → 248k → 323k at +2/+16/+29s); no loss with process alive |

Post-mortem goroutine dumps (13s after load end) show all three
workers `[runnable]` inside `WriteLogsFromOTLP` /
`WriteMetricsFromOTLP` — per-row `stmt.ExecContext` through cgo
inside a per-batch transaction. Not stuck; just slow relative to the
accept path.

## Findings

1. **Ack-to-durability lag is invisible (fixed this pass).**
   `squadron_worker_queue_depth` was declared but never updated —
   permanently 0 — and the OTLP HTTP request counters/histogram
   (`otlp_http_requests_total`, `otlp_http_request_duration_seconds`)
   were declared but never recorded in the HTTP handlers. An operator
   watching /metrics during the saturation run sees a healthy system
   while ~500k acked items sit volatile in memory. Both are now
   wired; the queue-depth gauge is the load-shedding signal.
2. **`otlp_batches` had no retention GC (fixed this pass).**
   `CleanupOldData` swept `metrics_*`, `logs`, `traces`,
   `pipeline_health_samples` — but not the v0.24 `otlp_batches`
   accounting table, which grows unbounded exactly like
   `pipeline_health_samples` did before it was added to that list.
   Now swept with the same retention ceiling.
3. **Write path is per-row cgo Exec; queue is sized in requests, not
   items/bytes — BOTH HALVES RESOLVED.** (a) The per-row
   `stmt.ExecContext` write path (~6-11k items/s) was replaced by the
   DuckDB Appender bulk path in **v0.89.379-era slice 2** (~11k → ~50k
   items/s). (b) The queue-bounds half is closed in **v0.89.380**: a
   10,000-**request** queue could hold 10k–millions of items depending on
   batch size, so the byte-budget bound `worker.max_queue_bytes` (default
   256 MiB; the request-count cap is kept as a secondary belt) now bounds
   the volatile ack-to-durable window in DATA. Item counts aren't known
   until the worker parses, so bytes (`len(RawData)`) are the only cheap
   signal at ingest — an item-count bound would require parsing in the
   receiver hot path, defeating the 202-fast-ack design. Over-budget
   submits wait up to `submit_timeout` then 503 (contract unchanged); a
   single payload larger than the whole budget is rejected immediately;
   new gauge `worker_queue_bytes`. See ADR 0004.
4. **Enricher does N identical lookups per batch — RESOLVED (v0.89.379).**
   `enrichTelemetry` called `agentService.GetAgent` once per item; a
   50-item single-agent batch did 50 identical SQLite lookups.
   Fixed with a per-batch, caller-owned local memo (`map[agentID]*Agent`,
   threaded through `enrichTelemetry`) that caches both hits and misses, so
   the lookup collapses to once per unique agent id per batch (~98% fewer on
   single-agent bursts). Local (not a struct field) => the singleton
   `Enricher` stays race-free under the worker pool's concurrent workers
   (pinned by `enricher_test.go`, incl. a `-race` concurrency test). Output
   is byte-identical to the pre-memo path.

Design intent worth writing down: 202-then-async is a legitimate
OTLP receiver shape, but its contract is "the queue is small and
drains fast." Finding 3 is what makes that contract true; finding 1
is what lets an operator verify it.

## After the Appender rework (finding 3 — closed)

The follow-up landed immediately after this report: the four hot
writers (`traces`, `logs`, `metrics_sum`, `metrics_gauge`) now go
through the DuckDB Appender inside an explicit transaction on a
pinned connection (`internal/storage/telemetrystore/duckdb/append.go`).
Histograms, pipeline-health, and batch-meta keep the prepared path
(low volume; histogram LIST columns need separate appender
verification).

Measured old-vs-new on the SAME machine (M-series Mac, both variants
built from adjacent commits, identical 2,000 req/s × 12s × 32-sender
scenario, fresh data dir each):

| | per-row Exec (old) | Appender (new) |
|---|---|---|
| Accepted | 13,900 req (971/s — server-throttled) | 22,099 req (1,826/s — full client rate) |
| Accept latency | p95 129ms / p99 214ms / max 307ms | **p95 3.0ms / p99 5.3ms / max 15.9ms** |
| Durable at load end | 195,400 / 695,000 (28%) | 654,450 / 1,104,950 (59%) |
| Drain complete | t+45s | **t+≤15s** (first post-load poll) |
| Sustained persist rate | ~11-12k items/s | **~50k items/s while accepting full load** |
| Reconciliation | exact | exact, zero 503s |

The ack-to-durability window at saturation shrank from ~45s to ≤15s
while ingesting 59% more data. Two semantics notes: (a) the appender
path is *more* atomic than before — sums/gauges previously committed
every 50 rows, so a mid-batch failure could leave partial chunks;
now the whole batch is one transaction and a retry cannot duplicate
(`TestAppendRows_ErrorLeavesZeroRows` pins this, and it's load-bearing:
go-duckdb v1.8.3's `Appender.Close` flushes even on the error path,
so the rollback is what guarantees zero rows). (b) `AppendRow`
requires every schema column in order — the writer round-trip tests
in `writers_test.go` pin column order, JSON columns, and
empty-string→NULL flattening against a real DuckDB file.

Still open from finding 3: item/byte-based queue bounds (the queue is
still 10,000 *requests*). With the faster drain the volatile window
is much smaller, but the bound is still in the wrong unit.

## Regression bar

- `cmd/otlpsim` tests run in CI (`go test ./cmd/otlpsim/`); the
  agent-attribution test must stay green or scale runs silently
  attribute to wrong identities.
- Re-run the baseline scenario after any change to the receiver,
  worker pool, or DuckDB writers: baseline (200 req/s) must stay
  zero-503 / zero-dead-letter with exact reconciliation.
- The Appender rework landed (see "After the Appender rework"):
  ~50k items/s sustained persist on M-series hardware is the new
  number to beat, with drain-complete ≤15s after a 12s saturation
  burst. The writer round-trip tests + the zero-rows-on-error test
  must stay green.

## How to run

```bash
# Terminal 1
make build && ./build/squadron --config squadron.yaml

# Terminal 2
make otlpsim
./build/otlpsim --rate=200 --duration=60s          # baseline
./build/otlpsim --rate=2000 --duration=60s --senders=32   # saturation

# Terminal 3 — watch the (now real) backlog signal
watch -n 1 'curl -sS localhost:8080/metrics | grep -E "worker_queue_depth|dead_letters|otlp_http_requests"'

# Reconcile after drain (cache TTL is 15s; wait it out)
curl -sS "localhost:8080/api/v1/insights/volume?window=1h"
```

Compare `otlpsim`'s final `items ok` per signal against the
`by_signal` item counts — they must match exactly once the queue
drains.
