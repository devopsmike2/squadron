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

# --- Verdict -------------------------------------------------------------
# Automated PASS/FAIL over results/metrics.csv (pure awk). Columns:
#   1 ts_unix 2 iso8601 3 worker_queue_bytes 4 trace_dl 5 metric_dl 6 log_dl
#   7 otlp_http_requests_total 8 otlp_http_request_errors_total
#   9 otlp_metrics_received_total 10 rollout_slow_ticks_total 11 server_rss_kb
# Dead-letters, slow-ticks, requests and errors are cumulative counters, so we
# read each one's FINAL row value. RSS stats use every non-blank sample.
# PASS requires: dead-letters==0 AND slow-ticks==0 AND no RSS leak AND OTLP
# error-rate under ERR_RATE_WARN%. Otherwise FAIL, with the reason(s) listed.
ERR_RATE_WARN="${ERR_RATE_WARN:-1.0}"   # % OTLP errors/requests -> WARN+FAIL above this
LEAK_GROWTH="${LEAK_GROWTH:-1.5}"       # last-quarter/first-quarter RSS mean ratio -> leak
LEAK_FLOOR_GB="${LEAK_FLOOR_GB:-2.0}"   # ...only when final RSS also exceeds this floor

verdict() {
  awk -F, -v ewarn="$ERR_RATE_WARN" -v lgrow="$LEAK_GROWTH" -v lfloor="$LEAK_FLOOR_GB" '
    NR==1 { next }
    {
      # cumulative counters: remember last non-blank value seen
      if ($4  != "") tdl=$4;  if ($5 != "") mdl=$5; if ($6 != "") ldl=$6
      if ($7  != "") req=$7;  if ($8 != "") errs=$8; if ($10 != "") st=$10
      if ($11 != "") { n++; rss[n]=$11+0; if ($11+0>peak) peak=$11+0; final=$11+0 }
    }
    END {
      gb = 1048576   # KiB -> GiB divisor
      dl = tdl+mdl+ldl
      # RSS leak heuristic: mean of the first 25% of samples vs the last 25%.
      q = int(n/4); if (q<1) q=1
      fs=0; for (i=1;i<=q;i++)       fs+=rss[i]; fq=(q>0?fs/q:0)
      ls=0; for (i=n-q+1;i<=n;i++)   ls+=rss[i]; lq=(q>0?ls/q:0)
      ratio   = (fq>0 ? lq/fq : 0)
      finalgb = final/gb
      leak    = (ratio > lgrow && finalgb > lfloor) ? 1 : 0
      erate   = (req>0 ? errs/req*100 : 0)

      printf "== SOAK VERDICT ==\n"
      printf "samples:            %d\n", n
      printf "peak RSS:           %.2f GB\n", peak/gb
      printf "final RSS:          %.2f GB\n", finalgb
      printf "RSS first-quarter:  %.2f GB\n", fq/gb
      printf "RSS last-quarter:   %.2f GB  (%.2fx first-quarter)\n", lq/gb, ratio
      printf "dead-letters:       %d (trace=%d metric=%d log=%d)\n", dl, tdl, mdl, ldl
      printf "slow ticks:         %d\n", st
      printf "OTLP requests:      %d\n", req
      printf "OTLP errors:        %d\n", errs
      printf "OTLP error rate:    %.4f%% (warn > %.2f%%)\n", erate, ewarn

      fail=0; reasons=""
      if (dl != 0)       { fail=1; reasons=reasons sprintf("  - dead-letters=%d (must be 0)\n", dl) }
      if (st != 0)       { fail=1; reasons=reasons sprintf("  - slow-ticks=%d (must be 0)\n", st) }
      if (leak)          { fail=1; reasons=reasons sprintf("  - possible RSS leak: last-quarter %.2fx first-quarter and final %.2fGB > %.2fGB floor\n", ratio, finalgb, lfloor) }
      if (erate > ewarn) { fail=1; reasons=reasons sprintf("  - OTLP error rate %.4f%% exceeds %.2f%%\n", erate, ewarn) }

      if (fail) { printf "VERDICT: FAIL\nfailing checks:\n%s", reasons } else { printf "VERDICT: PASS\n" }
      exit fail
    }
  ' "$CSV"
}

# Summary: otlpsim's final report block only. otlpsim writes progress as one
# unbounded ~5MB CR-delimited line, so split on CR and keep the tail instead of
# `tail -n 15` (which would slurp the whole 5MB line). Plus the first/last
# metric rows for a quick eyeball, then the automated verdict.
{
  echo "== otlpsim final report =="
  tr '\r' '\n' < "$OUTDIR/otlpsim.log" | tail -n 15
  echo
  echo "== /metrics trend (header, first sample, last sample) =="
  head -n 1 "$CSV"
  sed -n '2p' "$CSV"
  tail -n 1 "$CSV"
} >"$OUTDIR/summary.txt"

# Append + print the verdict; capture its pass/fail (pipefail makes the
# pipeline surface awk's exit code, and || keeps set -e from aborting).
verdict_rc=0
verdict | tee -a "$OUTDIR/summary.txt" || verdict_rc=$?

echo "results: $OUTDIR"

# Nonzero exit on FAIL so CI/automation can gate on the soak. The EXIT trap
# (sim cleanup) still runs on the way out.
exit "$verdict_rc"
