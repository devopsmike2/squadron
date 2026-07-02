# Rollout engine tick stress test, v0.89

The last unmeasured deferral in `docs/scale-testing.md`: does the
rollout engine's 5s tick loop hold up when a many-stage rollout
crosses a 1000-agent fleet? Short answer: not before this pass —
the engine pushed configs to canary agents **serially, one full
ack round-trip at a time**, which put a single full-fleet stage at
~2 hours and a 100-stage rollout at ~4 days. Fixed with bounded
concurrency; measured before/after below.

## Setup

M-series Mac, all-in-one binary, fleetsim `--count=1000
--group=stress-fleet` (all online, 0% drift), rollout created via
`POST /api/v1/rollouts`: 100 percent-mode stages (1%, 2% … 100%),
`dwell_seconds: 0`, so the engine advances one stage per 5s tick as
fast as it's allowed. Tick timing observed via the new
`rollout_engine_tick_duration_seconds` / `_slow_ticks_total`
metrics (added in this pass — there was previously NO signal for
tick health).

## What the run found

1. **No tick observability (fixed).** Tick duration was not
   measured anywhere. Added `metrics.RolloutMetrics` (tick duration
   histogram, ticks/slow-ticks counters), wired via
   `Engine.SetMetrics`, plus a warn log when a tick exceeds its 5s
   interval.
2. **Serial ack-blocking pushes (fixed).**
   `SendConfigToAgentWithContext` blocks until the agent confirms
   the config applied (or 30s). `applyStage` and `rollback` called
   it in a per-agent loop. Observed ack latency against fleetsim
   agents is ~7-14s (opamp-go clients coalesce status sends), so:
   stage 0 (10 agents) took **70s in one tick**; 3 stages took
   254s; extrapolated full run ≈ **days**. The ticker silently
   drops ticks while one runs, so one big rollout starves every
   other rollout's processing. Fixed: `pushConfigToAgents` fans out
   with bounded concurrency (128), preserving per-push OTel spans,
   ack semantics, and per-agent failure tolerance.
3. **Superset re-pushes (follow-up).** Percent-mode stage K pushes
   to the FULL first-K% canary, not the delta — a 100-stage
   1000-agent rollout sends 50,500 pushes to deliver 1,000 distinct
   configs. The re-push is the implicit retry path for agents that
   failed earlier stages, so switching to delta+targeted-retry is a
   semantics change; filed, not built.
4. **The push ack-wait ignores the tick context (follow-up).** The
   30s wait in `SendConfigToAgentWithContext` is `time.After`, not
   ctx-aware; the tick's own 30s ctx doesn't bound stage
   application. That's why an 84s full-fleet stage *works* — but it
   means tick latency is unbounded by design. Filed with option
   sketches (async stage application vs ctx-aware waits).

## Before / after (same machine, same scenario)

| | serial (old) | concurrent-128 (new) |
|---|---|---|
| Stage push, 10 agents | 70s | ~13s |
| Stage push, ~500 agents | (extrapolated ~1h) | **~14s — flat in canary size** |
| Stage push, full fleet (1000) | (extrapolated ~2h) | **83.8s, one tick, 0 failures** |
| 3 stages | 254s | ~40s |
| 50 stages (~13k pushes) | (extrapolated ~2 days) | **~700s, 0 push failures** |
| 100-stage extrapolation | ~4 days | ~25 min |

Per-stage cost with concurrency is dominated by the ack wave
latency (~7-14s per 128-push wave, waves pipeline), not canary
size, until wave count grows: the full-fleet stage (8 waves) ran
83.8s. Zero push failures in ~26k pushes across all runs.

Idle behavior for reference: with an in-progress rollout just
dwelling, ticks run ~1ms at 1000 agents (the per-tick full-fleet
`ListAgents` + abort-criteria scan is cheap at this scale; it will
matter at 10k+, filed under the queue of follow-ups).

## Caveat on ack latency

The ~7-14s ack waves are measured against fleetsim's opamp-go
clients, whose status sends coalesce. Real otelcol collectors may
ack faster or slower; the serial-vs-concurrent conclusion is
insensitive to that (serialization multiplies whatever the ack
latency is by canary size), but absolute stage times will differ.
Re-measure against a real collector before quoting numbers in
launch material.

## Regression bar

- A full-fleet (1000-agent) stage must complete in ONE tick with
  zero push failures; ≤90s on M-series-class hardware against
  fleetsim.
- `rollout_engine_slow_ticks_total` staying at 0 during dwell-only
  ticks at 1000 agents (idle ticks are ~1ms; a regression here
  means someone put IO back on the scan path).
- Watch `rollout_engine_tick_duration_seconds` in any future scale
  run — it's the engine's primary health signal now.

## Reproducing

```bash
make build && ./build/squadron --config squadron.yaml   # terminal 1
make fleetsim && ./build/fleetsim --count=1000 --group=stress-fleet --ramp=25s  # terminal 2
# terminal 3: create a config, then a rollout with N percent stages
# (dwell 0) via POST /api/v1/rollouts — see this repo's
# /tmp-style bench script in the stress-test history, or the API
# reference. Then:
watch -n 5 'curl -sS localhost:8080/metrics | grep rollout_engine'
```
