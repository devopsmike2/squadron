#!/usr/bin/env bash
#
# soak.sh — sustained fleetsim + otlpsim run with periodic /metrics capture.
#
# Drives synthetic OTLP ingest load (otlpsim) and, optionally, a synthetic OpAMP
# fleet (fleetsim) against a running Squadron instance for a configurable
# duration, snapshotting the Prometheus /metrics surface on an interval into a
# CSV so a long soak can be trended for backlog growth, dead-letters, and memory
# leaks. Built for the 24h pre-GA soak (docs/stress-tests/sustained-soak.md); a
# short run (DURATION=2m) is a good smoke of the harness itself.
#
# Prereqs: a Squadron server running (make build && ./build/squadron --config
# squadron.yaml) and the sims built (make fleetsim otlpsim). otlpsim self-
# terminates on -duration; fleetsim has no -duration and is killed at the end.
#
# Usage:
#   DURATION=24h OTLP_RATE=500 AGENTS=500 FLEET_COUNT=1000 scripts/soak.sh
#   DURATION=2m scripts/soak.sh            # quick harness smoke (otlp-only)
set -euo pipefail

DURATION="${DURATION:-2m}"                 # otlpsim -duration (Go duration)
OTLP_RATE="${OTLP_RATE:-200}"              # aggregate ExportRequests/sec
AGENTS="${AGENTS:-100}"                    # distinct otlp agent identities
FLEET_COUNT="${FLEET_COUNT:-0}"            # 0 = skip fleetsim (otlp-only soak)
SCRAPE_INTERVAL="${SCRAPE_INTERVAL:-15}"   # seconds between /metrics snapshots
METRICS_URL="${METRICS_URL:-http://localhost:8080/metrics}"
OTLP_TARGET="${OTLP_TARGET:-http://localhost:4318}"
OPAMP_TARGET="${OPAMP_TARGET:-ws://localhost:4320/v1/opamp}"
BIN_DIR="${BIN_DIR:-./bin}"
SQUADRON_PID="${SQUADRON_PID:-}"      # optional: server PID → sampled RSS (leak check)
OUTDIR="${OUTDIR:-./soak-results/$(date -u +%Y%m%dT%H%M%SZ)}"

mkdir -p "$OUTDIR"
CSV="$OUTDIR/metrics.csv"

# Metric families to trend. Each is summed across all its label series. The
# canaries: worker_queue_bytes (the real backlog / memory bound), *_dead_letters
# (drops after retries), process_resident_memory_bytes + go_goroutines +
# heap_inuse (a 24h leak shows here), otlp_http_* (throughput/errors), and the
# rollout tick health (fleetsim runs).
# Squadron metrics carry the `squadron_` namespace; go_*/process_* are the
# standard Go/process collectors (no namespace).
METRICS=(
  squadron_worker_queue_bytes
  squadron_worker_trace_dead_letters_total
  squadron_worker_metric_dead_letters_total
  squadron_worker_log_dead_letters_total
  squadron_otlp_http_requests_total
  squadron_otlp_http_request_errors_total
  squadron_otlp_metrics_received_total
  squadron_rollout_engine_slow_ticks_total
)

# CSV header.
{ printf "ts_unix,iso8601"; for m in "${METRICS[@]}"; do printf ",%s" "$m"; done; printf ",server_rss_kb\n"; } >"$CSV"

scrape() {
  local now iso page
  now=$(date -u +%s)
  iso=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  page=$(curl -sS --max-time 5 "$METRICS_URL" 2>/dev/null || true)
  printf "%s,%s" "$now" "$iso" >>"$CSV"
  for m in "${METRICS[@]}"; do
    # Sum every series of the family (name optionally followed by {labels}).
    val=$(printf "%s\n" "$page" | awk -v M="$m" '$1 ~ "^"M"($|\\{)" {s+=$2} END{if (s=="") printf ""; else printf "%s", s}')
    printf ",%s" "${val:-}" >>"$CSV"
  done
  # Server RSS (KB) — the primary 24h leak signal, since go_/process_ collectors
  # are not on Squadron's /metrics. Blank when SQUADRON_PID isn't provided.
  local rss=""
  [ -n "$SQUADRON_PID" ] && rss=$(ps -o rss= -p "$SQUADRON_PID" 2>/dev/null | tr -d ' ')
  printf ",%s\n" "${rss:-}" >>"$CSV"
}

cleanup() {
  [ -n "${FLEET_PID:-}" ] && kill "$FLEET_PID" 2>/dev/null || true
  [ -n "${OTLP_PID:-}" ] && kill "$OTLP_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "soak: duration=$DURATION otlp_rate=$OTLP_RATE agents=$AGENTS fleet=$FLEET_COUNT scrape=${SCRAPE_INTERVAL}s"
echo "      metrics=$METRICS_URL  out=$OUTDIR"

# Preflight: server reachable?
if ! curl -sS --max-time 5 "$METRICS_URL" >/dev/null 2>&1; then
  echo "ERROR: $METRICS_URL not reachable — start Squadron first (./build/squadron --config squadron.yaml)" >&2
  exit 1
fi

# Optional synthetic fleet (no -duration → killed at end via trap).
FLEET_PID=""
if [ "$FLEET_COUNT" -gt 0 ]; then
  "$BIN_DIR/fleetsim" -target "$OPAMP_TARGET" -count "$FLEET_COUNT" -group soak-fleet \
    >"$OUTDIR/fleetsim.log" 2>&1 &
  FLEET_PID=$!
  echo "fleetsim pid=$FLEET_PID (count=$FLEET_COUNT)"
fi

# OTLP ingest load (self-terminates on -duration).
"$BIN_DIR/otlpsim" -target "$OTLP_TARGET" -agents "$AGENTS" -rate "$OTLP_RATE" -duration "$DURATION" \
  >"$OUTDIR/otlpsim.log" 2>&1 &
OTLP_PID=$!
echo "otlpsim pid=$OTLP_PID"

# Snapshot /metrics until otlpsim finishes.
while kill -0 "$OTLP_PID" 2>/dev/null; do
  scrape
  sleep "$SCRAPE_INTERVAL"
done
scrape  # final sample after otlpsim exits

wait "$OTLP_PID" 2>/dev/null || true

# Summary: otlpsim's own report + first/last metric rows for a quick eyeball.
{
  echo "== otlpsim final report =="
  tail -n 15 "$OUTDIR/otlpsim.log"
  echo
  echo "== /metrics trend (header, first sample, last sample) =="
  head -n 1 "$CSV"
  sed -n '2p' "$CSV"
  tail -n 1 "$CSV"
} | tee "$OUTDIR/summary.txt"

echo "results: $OUTDIR"
