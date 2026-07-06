# Sustained soak (24h) — fleetsim + otlpsim

`scripts/soak.sh` drives synthetic ingest (otlpsim) and, optionally, a synthetic
OpAMP fleet (fleetsim) against a running Squadron instance for a configurable
duration, snapshotting `/metrics` (plus the server's RSS) into a CSV on an
interval. It's built for the pre-GA 24h endurance run: the point of a soak isn't
peak throughput (that's the ingest/rollout stress reports) but **trends over
time** — does the write backlog grow without bound, do dead-letters accumulate,
does memory leak?

## Run it

Build once, then run:

```bash
make build fleetsim otlpsim
./build/squadron --config squadron.yaml            # terminal 1 (or a service)

# terminal 2 — the 24h soak:
DURATION=24h OTLP_RATE=500 AGENTS=500 FLEET_COUNT=1000 \
  SQUADRON_PID=$(pgrep -f 'squadron .*--config') \
  scripts/soak.sh
```

Knobs (all env, with defaults): `DURATION=2m`, `OTLP_RATE=200`, `AGENTS=100`,
`FLEET_COUNT=0` (0 = otlp-only), `SCRAPE_INTERVAL=15` (s), `SQUADRON_PID=`
(enables the RSS leak column), `METRICS_URL`, `OTLP_TARGET`, `OPAMP_TARGET`,
`BIN_DIR=./bin`, `OUTDIR`. A short `DURATION=2m` run is a good smoke of the
harness itself.

## What it captures

A CSV (`$OUTDIR/metrics.csv`), one row per scrape:

- `squadron_worker_queue_bytes` — the volatile ack-to-durable **write backlog**
  in bytes (the real memory-in-flight bound; ADR 0004). Should stay bounded and
  return to ~0 after load stops. *Trend this, not `worker_queue_depth`* (the
  depth gauge is historically not updated).
- `squadron_worker_{trace,metric,log}_dead_letters_total` — writes dropped after
  exhausting retries. Should stay **flat at 0**; any climb is data loss.
- `squadron_otlp_http_requests_total` / `_request_errors_total` — ingest
  throughput + error count.
- `squadron_rollout_engine_slow_ticks_total` — rollout ticks that overran the
  interval (only meaningful with `FLEET_COUNT>0`); should stay **flat at 0**.
- `server_rss_kb` — the server process resident memory (via `ps`), sampled when
  `SQUADRON_PID` is set. This is the **primary leak signal**: over 24h it should
  plateau, not trend upward. (Go/process collector metrics are not on Squadron's
  `/metrics` registry, so RSS is sampled directly.)

`$OUTDIR/summary.txt` has otlpsim's final report (items/bytes, p50/p95/p99) plus
the first and last metric rows for a quick eyeball. `otlpsim.log` /
`fleetsim.log` hold the raw sim output.

## Reading a soak

Healthy 24h run: `worker_queue_bytes` oscillates under load and drains to ~0
between bursts; dead-letters and slow-ticks flat at 0; `server_rss_kb` plateaus
(a steady upward slope over 24h = leak). Reconcile client-side items (otlpsim's
report) against the server's `otlp_batches` accounting via
`GET /api/v1/insights/volume?window=1h`.

## Harness smoke (verified 2026-07-06)

A 30s otlp-only run against a HEAD build captured a clean series —
`squadron_otlp_http_requests_total` climbing 0→5009 in lockstep with otlpsim's
5009 sent, dead-letters/errors/slow-ticks flat at 0, and `server_rss_kb`
tracking ~186–191 MB — confirming the drive + scrape + CSV + summary path. The
full 24h endurance run is an operator-launched validation, not a code
deliverable.
