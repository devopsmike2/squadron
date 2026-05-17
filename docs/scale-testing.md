# Scale testing

Squadron ships with `fleetsim`, a synthetic OpAMP load generator,
so you can validate behavior at fleet sizes that are inconvenient
to assemble for real. Each simulated agent is a real `opamp-go`
client speaking the standard protocol — they exercise the same
server code path a production OpenTelemetry Collector would.

This page documents how to run it and what to expect.

## Running fleetsim

```bash
make fleetsim                                  # builds ./fleetsim
./fleetsim --count=100                         # 100 agents, default settings
./fleetsim --count=500 --ramp=60s --drift-pct=10 --offline-pct=2
./fleetsim --count=1000 --ramp=90s --label-prefix=loadtest
```

Flags worth knowing:

| Flag | Default | Notes |
|------|---------|-------|
| `--target` | `ws://localhost:4320/v1/opamp` | OpAMP endpoint to dial. |
| `--count` | 100 | How many synthetic agents to spawn. |
| `--ramp` | 30s | Spread initial connects across this window. 0 = all at once. |
| `--drift-pct` | 10 | Percent of agents that mark themselves as drifted (server-side detection requires both intent + effective config; see caveats below). |
| `--offline-pct` | 0 | Percent of agents that connect once, then disconnect. Useful for testing offline-status detection. |
| `--version` | `0.119.0` | `service.version` reported in `AgentDescription`. |
| `--group` | empty | `agent.group_name` label applied to every simulated agent. |
| `--label-prefix` | `fleetsim` | Value of the `simulated.fleet` label — filterable in the UI to distinguish sim agents from real ones. |
| `--health-interval` | 15s | How often each simulated agent sends a health ping. |

Stop with `Ctrl+C`. Connection drains cleanly; agents transition to
offline on the server side as the WebSocket close fires.

## Baseline scale findings (v0.22.0)

Run against a single all-in-one Squadron instance (Docker dev mode,
M-series Mac, 8 GB RAM available to the container).

| Agents | RAM | CPU | /agents (cold) | /agents payload | UI render |
|---|---|---|---|---|---|
| 2 (baseline) | ~80 MiB | 1% | 8 ms | 4 KB | instant |
| 100 | 195 MiB | 11% | 13 ms | 64 KB | instant |
| 500 | 414 MiB | 29% | 10 ms | 342 KB | <1s |
| 1000 (882 connected at sample) | 446 MiB | 68% | 30 ms | 594 KB | ~2s |

**Headline result: the OpAMP server, app-store, dashboard, and Fleet
Map render correctly at 1000 agents with no errors and a 500MB
memory footprint.** No request failures, no panics, no log warnings
of any kind in the squadron container during ramp or steady state.

### Where the bottlenecks live

These aren't broken; they're untapered. Documented here so the right
fixes land in the right release rather than as ad-hoc patches.

#### 1. `/api/v1/agents` returns the full record map

Today's endpoint serializes every agent into one JSON object keyed
by ID. At 1000 agents that's a 594 KB payload — fine on localhost,
painful on a slow link or with 10× the fleet.

**Recommended fix:** add `offset` + `limit` query params and a
`X-Total-Count` header. The UI already paginates visually (card
grid only fills the viewport); the API just needs to follow.

**Tracked as a v0.23.x prerequisite** before cost-optimization
features go in.

#### 2. UI table + card-grid render eagerly

The Agents page renders all N cards/rows in one shot. At 500 the
page is responsive but slow to first paint (~1s). At 1000+ rows
the DOM has 10k+ nodes and scrolling starts to lag.

**Recommended fix:** virtualize the card grid (use
`react-window` or `react-virtual`). Same for the table mode.
Threshold: enable virtualization above 200 visible rows so small
fleets don't pay the abstraction cost.

#### 3. Heartbeat interval vs. online cutoff

At 1000 agents, every health-ping cycle (15s in fleetsim) generates
a burst of WebSocket frames. Some agents end up with a
`last_seen` skew of >30s, which Squadron currently classifies as
"offline" — even though the connection is still open.

This was visible in the 1000-agent run: 871 online / 11 offline at
steady state, where ~10 of the offline agents were actually
healthy but had recently-stale `last_seen` due to ping batching.

**Recommended fix:** decouple "is the WebSocket connected?" from
"did we hear from this agent in the last N seconds?". Today
they're conflated. Two distinct states:
- **connected** — TCP/WS is alive (cheap to know).
- **reporting** — recent telemetry within the threshold.

A configurable threshold + `online_threshold` server setting
(default 60s) would fix this.

#### 4. Drift detection requires both `intent` and `effective` config

The `--drift-pct` flag in fleetsim marks agents as "internally
drifted" but doesn't actually publish a *different* effective
config. The server-side drift detector needs both sides of the
comparison; absent a stored intent config, every fleetsim agent
shows up as `drift_status=no_intent`.

**To exercise drift detection at scale:**
1. Create a config in the UI and assign it to a group.
2. Apply that group's label to fleetsim's agents
   (`--group=my-group`).
3. Wait for the rollout / config push.
4. Drift will register once fleetsim is enhanced to send a tweaked
   effective config back. See follow-up issue.

#### 5. Group label matching appears greedy

During 1000-agent runs, the demo group's "agent count" in the
sidebar showed `1002 agents` rather than just the 2 real ones.
Cause: the demo-group's label matcher treats empty/missing labels
permissively — likely matches every agent that doesn't explicitly
opt out.

Worth confirming in a focused test before patching; could be a
display bug rather than a matcher bug.

### After v0.23: pagination + virtualization

The v0.22 baseline measured a single eager-fetch of every agent.
v0.23 introduces `?offset=&limit=` on `/api/v1/agents` plus
threshold-based UI virtualization. Re-running the same 1000-agent
fleetsim scenario:

| Metric                          | v0.22       | v0.23           | Delta |
|---------------------------------|-------------|-----------------|-------|
| First-page `/agents` payload    | 594 KB      | 137 KB          | **−77%** |
| First-page `/agents` response   | 30 ms       | 20 ms           | −33% |
| Agents page time-to-first-paint | ~2 s        | <1 s            | Snappy |
| DOM nodes at 1000 agents        | ~10k        | ~2k (virtualized) | −80% |
| Memory while scrolling          | grows w/ DOM | bounded         | — |

The UI now loads the first 100 agents in a single round-trip,
then fetches subsequent pages on scroll (200 → 300 → 400 …). At
200 rows the virtualizer takes over so the DOM stays bounded
regardless of how far the operator scrolls. Below 200 rows the
non-virtualized grid + table render directly (no virtualizer
overhead).

The header summary line ("980/1002 reporting · 1 drifted") uses
three small `limit=1` queries against the new pagination
endpoint to read fleet-wide totals without re-fetching every
agent — those queries cost about 5 ms each.

Trade-off worth noting: the response payload now contains both
the new `items` array AND the legacy `agents` map, so the
serialized body is roughly 2× the size of items alone. This is
v0.23's deliberate back-compat cost; a future major version will
drop the legacy map and reclaim that overhead.

### After v0.24: telemetry volume insights

v0.24 adds an ingest-side `otlp_batches` accounting table and a
read-only insights API under `/api/v1/insights/volume`. The query
layer fans out three GROUP BYs (per signal, per agent, per
attribute key) with a 15s in-process cache so the UI's polling
loop doesn't hammer DuckDB.

Verified in a 3-agent dev setup with `demo-supervisor` writing
metrics every ~30ms over 24h:

| Endpoint                                  | Window | Result |
|-------------------------------------------|--------|--------|
| `/insights/volume?window=24h`             | 24h    | 863 MB / 4.35M items / 3 agents — fleet-wide totals |
| `/insights/volume?window=1h`              | 1h     | 158.8 MB / metrics-dominated (~100%) |
| `/insights/volume/agents?window=1h`       | 1h     | Outlier ranking: top agent at 158.7 MB (99.9%) |
| `/insights/volume/agents/:id?window=24h`  | 24h    | otelcol-contrib: 690.2 KB (588.8 KB metrics / 101.4 KB logs) |
| `/insights/volume/attributes?signal=metrics&window=1h` | 1h | Top attribute keys via sampled extrapolation (~2000 rows) |

Bug caught and fixed while wiring this up: DuckDB widens
`SUM(BIGINT)` to **HUGEINT** (128-bit), which the
`marcboeker/go-duckdb` driver reifies as `*big.Int`. The first cut
of the insights service unboxed rows through a tolerant type-switch
that only knew native int types — so every aggregated column read
as zero, even though `otlp_batches` had real rows. Fixed two ways
(defense in depth): every `SUM()` in the insights queries is now
wrapped in `CAST(... AS BIGINT)` to collapse the HUGEINT at the DB
boundary, and the row-scan helper now accepts `*big.Int` /
`big.Int` and saturates to `MaxInt64` on overflow. A regression
test in `internal/insights/insights_test.go` pins this so a future
driver upgrade can't sneak the bug back in.

Perf measured with `./fleetsim --count=1000 --ramp=60s` plus the
two real demo agents (1002/1002 online at sample time), with the
`otlp_batches` table populated by the demo collector emitting
metrics every ~30ms over the prior 24h. Five samples per endpoint;
cold = first call after the 15s service-cache TTL expires, warm =
immediate re-call:

| Endpoint                                       | Cold | Warm p50 | Gate     |
|------------------------------------------------|------|----------|----------|
| `/insights/volume?window=5m`                   | 5 ms | 1 ms     | fleet (<500) |
| `/insights/volume?window=1h`                   | 9 ms | 1 ms     | fleet (<500) |
| `/insights/volume?window=24h`                  | 13 ms| 1 ms     | fleet (<500) |
| `/insights/volume/agents?window=1h&limit=20`   | 8 ms | 1 ms     | fleet (<500) |
| `/insights/volume/agents/:id?window=24h`       | 16 ms| 1 ms     | **agent (<100)** |
| `/insights/volume/attributes?signal=metrics`   | 10 ms| 1 ms     | fleet (<500) |
| `/insights/volume/drops?window=1h`             | 3 ms | 2 ms     | fleet (<500) |

**Headline: agent-detail at 16ms is >6× under the 100ms gate;
worst fleet-overview at 13ms is >38× under the 500ms gate.** The
15s in-process cache makes warm reads effectively free (~1ms),
which matches the UI's polling interval — most user-visible
refreshes will be cache hits.

For reference at the same fleet size, the existing
`/api/v1/agents?limit=100` was 17-25ms and `/agents/stats` was
13-21ms; the insights endpoints land in the same ballpark.

Why so far under target: the `otlp_batches` table is narrow (8
columns, no JSON), the composite indexes on `(agent_id, time)` and
`(signal_type, time)` cover every WHERE clause, and the `CAST(...
AS BIGINT)` shrinks the SUM result before it crosses the cgo
boundary. The top-attributes endpoint pays a slightly higher cost
(the sampler ORDER BY random() LIMIT 2000 has to actually read
rows) but is still 10ms cold — well below where caching becomes a
correctness concern rather than a perf optimization.

What's NOT in this measurement: this is steady-state read latency
against a populated table. Insights query latency *during* a heavy
OTLP ingest burst is unmeasured; the cache mostly absorbs that, but
a v0.24.x stress pass with concurrent `otlp_batches` writers is
worth doing before any enterprise GA claim.

### What we deliberately did NOT load-test

| Path | Why deferred |
|------|--------------|
| OTLP receiver under load | Different code path from OpAMP. Needs a synthetic OTLP generator. Will be v0.22.x. |
| DuckDB telemetry write throughput | Tied to the OTLP path above. |
| Insights API under concurrent ingest | See "After v0.24" — steady-state reads are measured and well under target, but query latency *during* a heavy OTLP burst is unmeasured. The 15s cache likely absorbs most of it. Wants a v0.24.x stress pass with concurrent `otlp_batches` writers. |
| Rollout engine under N agents | Tested implicitly (10s tick across 1000 agents had no observable lag), but a dedicated test that creates a 100-stage rollout and watches the tick budget is worth doing. |
| Long-running stability (24h+) | Not a single-session test. Run fleetsim under a sustained-load harness for a day before any GA claim. |

### Reproducing

```bash
# Terminal 1: run Squadron however you normally do
docker compose up squadron

# Terminal 2: load
make fleetsim
./fleetsim --count=500 --ramp=60s --drift-pct=10

# Terminal 3: observe
watch -n 1 'curl -sS http://localhost:8080/api/v1/agents/stats; echo'
```

Then open the UI at http://localhost:5173 and exercise:
- Fleet Status dashboard
- Agents page (cards + table)
- Fleet Map (pipeline / data flow / fleet tabs)

### Next steps

The v0.23-v0.25 cost-optimization arc is gated on fixing #1 and
the headline polish on #2. Both are small (< 1 day each); they
land before any new feature work.
