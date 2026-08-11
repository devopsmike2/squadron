#!/usr/bin/env bash
#
# otel-cred-exec.sh — generic credential-injecting exec shim for the OpAMP supervisor.
#
# The OpAMP supervisor (opampsupervisor) owns the collector lifecycle: it writes the
# merged effective config and launches the process named by `agent.executable` in
# supervisor.yaml, passing `--config <supervisor-written-config>` (plus any
# feature-gate flags) as arguments. To inject credentials that must never be written
# to disk in cleartext, point `agent.executable` at THIS shim instead of the
# collector binary.
#
# The shim (1) materializes secrets into the environment, then (2) REPLACES itself
# with the collector via `exec`, forwarding the supervisor's arguments verbatim.
#
# Resulting process tree:
#   systemd -> opampsupervisor -> [this shim; exec ->] otelcol-contrib
# After the exec, the collector runs under the SAME pid the supervisor launched, so
# the supervisor tracks the collector directly (signals, exit code, health).
#
# THREE GOTCHAS THIS SHIM GETS RIGHT (each one bit the pilot's wrapper):
#
#   1. exec, NOT subprocess. Use `exec` so the collector INHERITS this shim's pid.
#      The supervisor sends restart/stop signals to — and reads the exit code of —
#      the pid it launched. If the shim spawned the collector as a child and stayed
#      alive (or forked it to the background), the supervisor would track the SHIM,
#      not the collector: signals and health would never reach the collector and it
#      would run as an invisible grandchild.
#
#   2. Pass the supervisor's args through VERBATIM ("$@"). Do NOT hardcode a
#      `--config` path. The supervisor decides the config file location (inside its
#      storage.directory) and passes it as `--config <path>`. Hardcoding a path makes
#      the collector run SOMETHING OTHER than the config the supervisor manages,
#      silently breaking closed-loop remote config.
#
#   3. Propagate the exit code. Because `exec` replaces this process image, the
#      collector's exit status becomes the shim's exit status automatically, so a
#      non-zero collector crash is visible to the supervisor. A wrapper that instead
#      did `otelcol ... ; exit 0` (or called the collector and returned 0 regardless)
#      hides crashes: systemd/the supervisor see a clean exit, so a bad config becomes
#      a silent restart loop instead of a surfaced failure.
#
set -euo pipefail

# --- (1) materialize secrets into the environment -------------------------------
# Replace this block with your real secret retrieval (e.g. `vault read ...`).
# Keep secrets in the environment only; never write them into the config file.
# Example:
#   export SPLUNK_HEC_TOKEN="$(vault kv get -field=token secret/otel/splunk)"
: "${SQUADRON_SECRET:=demo-secret-value}"   # placeholder — remove in real use
export SQUADRON_SECRET

# --- (2) exec the real collector, forwarding the supervisor's args verbatim ------
# `agent.executable` in supervisor.yaml is THIS file; the collector binary path is
# configured here (override with COLLECTOR_BIN if it lives elsewhere).
COLLECTOR_BIN="${COLLECTOR_BIN:-/otelcol-contrib}"
exec "$COLLECTOR_BIN" "$@"
