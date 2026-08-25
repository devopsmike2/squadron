#!/usr/bin/env bash
#
# govulncheck CI gate.
#
# Runs `govulncheck -json ./...` and FAILS (exit 1) if your code is
# call-graph-REACHABLE to any Go vulnerability whose OSV ID is not listed in the
# allowlist file (default: .govulncheck-allow.txt). Imported-but-not-called
# vulns do NOT fail the gate — only reachable ones do, matching govulncheck's
# own "your code calls these" signal.
#
# Why a wrapper instead of raw `govulncheck ./...`? We need main to stay GREEN
# today despite one consciously-deferred reachable vuln (GO-2026-5158, otel
# baggage — see the allowlist), while any NEW reachable vuln turns the gate RED.
#
# Deps: govulncheck on PATH, jq. Usage: scripts/govulncheck-gate.sh [allowfile]
set -euo pipefail

ALLOW_FILE="${1:-.govulncheck-allow.txt}"
JSON_OUT="$(mktemp)"
trap 'rm -f "$JSON_OUT"' EXIT

echo "Running govulncheck (JSON)…"
# govulncheck -json exits 0 even when vulns are found; we decide from the JSON.
if ! govulncheck -json ./... > "$JSON_OUT" 2>/tmp/govulncheck-gate.err; then
  echo "::error::govulncheck failed to run:"; cat /tmp/govulncheck-gate.err >&2 || true
  exit 2
fi

# Reachable == a "finding" whose top trace frame has a function set (symbol-level
# call). Imported-only findings have no function on the top frame.
# (portable collection — no `mapfile`, which is absent on bash 3.2)
REACHABLE=()
while IFS= read -r _id; do
  [[ -n "$_id" ]] && REACHABLE+=("$_id")
done < <(jq -r 'select(.finding != null) | .finding | select(.trace[0].function != null) | .osv' "$JSON_OUT" | sort -u)

# Load allowlist (strip comments / whitespace / blanks).
ALLOW=()
if [[ -f "$ALLOW_FILE" ]]; then
  while IFS= read -r line; do
    line="${line%%#*}"
    line="$(echo "$line" | tr -d '[:space:]')"
    [[ -n "$line" ]] && ALLOW+=("$line")
  done < "$ALLOW_FILE"
fi

is_allowed() { local id="$1" a; for a in "${ALLOW[@]:-}"; do [[ "$a" == "$id" ]] && return 0; done; return 1; }

FAIL=0
if [[ "${#REACHABLE[@]}" -eq 0 ]]; then
  echo "govulncheck: 0 reachable vulnerabilities."
else
  echo "govulncheck: reachable vulnerabilities: ${REACHABLE[*]}"
  for id in "${REACHABLE[@]}"; do
    [[ -z "$id" ]] && continue
    if is_allowed "$id"; then
      echo "::notice::govulncheck: reachable vuln $id is ALLOWLISTED ($ALLOW_FILE)."
    else
      echo "::error::govulncheck: NEW reachable vulnerability $id is not allowlisted."
      FAIL=1
    fi
  done
fi

# Housekeeping: warn if an allowlisted ID is no longer reachable (safe to remove).
for a in "${ALLOW[@]:-}"; do
  [[ -z "$a" ]] && continue
  found=0; for id in "${REACHABLE[@]:-}"; do [[ "$id" == "$a" ]] && found=1; done
  [[ "$found" -eq 0 ]] && echo "::warning::govulncheck: allowlisted $a is no longer reachable — remove it from $ALLOW_FILE."
done

if [[ "$FAIL" -ne 0 ]]; then
  echo "----------------------------------------------------------------"
  echo "govulncheck gate FAILED — new reachable vulnerability(ies) above."
  echo "Fix the vuln (bump the dependency), or, only if consciously deferred,"
  echo "add its OSV ID to $ALLOW_FILE with a justification + TODO."
  echo "Full report:"; govulncheck ./... || true
  exit 1
fi
echo "govulncheck gate PASSED."
