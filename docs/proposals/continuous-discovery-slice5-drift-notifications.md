# Continuous discovery — drift notifications (slice 5)

Status: shipped v0.89.255. Author: autonomous session. Builds on slice 4
(drift) + slice 3 (scheduler).

## Problem

Drift (slice 4) answers "what changed?" on request. The continuous engine runs
scans unattended on a timer (slice 3) — so the natural next step is to PUSH the
"what changed" signal instead of waiting for an operator to poll the drift
endpoint.

## Approach

After each successful SCHEDULED scan, the scheduler diffs the new scan against
the previous one and, when anything changed, records a
`discovery.scan_drift_detected` audit event. That event flows through the
existing audit timeline and SIEM forwarding with no extra wiring — the operator
gets drift in whatever channel already consumes Squadron's audit stream.

- `emitDriftIfChanged` (per-cloud, in the scheduler wiring) lists the two most
  recent persisted scans for the scope, runs `discoverydrift.Between`, and emits
  only when `total_added + total_removed + total_instrumentation_changed > 0`.
- `scanAccountWithDrift` wraps each cloud's `ScanAccount` so the diff+emit runs
  after the scan persists. All four clouds.
- Payload: provider, scope_id, from/to scan ids, the three totals, and
  `instrumentation_regressions` — the highest-signal subset (resources whose
  OTel turned OFF between scans).

## Scope / honest framing

- **Scheduled scans only.** On-demand scans are interactive (the operator sees
  the result), so they don't push. Drift notifications are a property of the
  continuous engine, which is already opt-in via SQUADRON_DISCOVERY_SCAN_INTERVAL.
- **First scheduled scan never emits.** With one scan there's nothing to compare,
  so no "everything is new" noise.
- **Audit event, not a bespoke channel.** Routing it to Slack/email/PagerDuty is
  the SIEM/notification layer's job (audit events already forward there); this
  slice produces the signal, not a new delivery mechanism.
- **Cooldown (v0.89.256).** SQUADRON_DISCOVERY_DRIFT_COOLDOWN caps per-scope
  drift-event frequency below the scan cadence (in-memory, resets on restart).
  Default off. The payload also carries capped added/removed resource id lists
  so a notification is self-contained.
