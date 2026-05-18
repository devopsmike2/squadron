# Cost-Spike Alerting

The v0.29 cost-spike layer is Squadron's "tap on the shoulder"
mechanism. Every minute the detector compares the current
$/month projection against a rolling baseline. When projection
breaks above the warn or critical threshold, Squadron opens a
**cost spike event** with attribution — which agents and which
attribute keys drove the jump — and surfaces it on the
Dashboard banner + Savings page panel.

It's automatic and zero-config. The existing operator-authored
alert rules (`/api/v1/alerts/rules`) are still there for
predicates you can express as a Squadron QL query; cost spikes
cover the case "tell me when something I didn't anticipate gets
expensive."

## How detection works

Each tick:

1. Pull the current fleet $/month projection from the v0.27
   pricing layer (same code that powers `/pricing/projection`).
2. Push the value into a 60-sample rolling ring buffer.
3. Compute the **baseline** — the trimmed mean of the last 59
   samples (excluding the just-recorded one, trimming 10% top
   and bottom).
4. Compare:
   - `current ≥ baseline × (1 + critical_pct)` → critical spike
   - `current ≥ baseline × (1 + warn_pct)` → warn spike
   - otherwise → close any open spike.

Defaults: warn = 25% over baseline, critical = 50% over baseline.

The detector skips evaluation when baseline is below
`min_baseline_usd` (default $10/mo). The reason is signal-to-noise:
a 50% spike on $5/month of projected spend is $2.50 and isn't
worth waking anyone up.

## Attribution

When a spike opens, the detector immediately calls
`insights.TopAgents` and `insights.TopAttributes` for the
dominant signal and records the top 3 of each on the event row
as `attribution_json`. This is captured at **fire time** — even
hours later when the live insights state has moved on, the
operator sees the picture that was true when the alarm went off.

The UI surfaces both lists with their byte share so the operator
can tell at a glance whether it's a single noisy agent or a
fleet-wide attribute explosion.

## Lifecycle

- **open** — projection is over threshold. Banner visible.
- **warn → critical** — escalation happens in place on the same
  row; we don't open a new event for severity bumps on the same
  incident.
- **acknowledged** — operator clicked "Acknowledge" in the
  Savings panel. Banner is suppressed for the duration; the row
  stays open in the audit trail.
- **closed** — projection dropped back below the warn threshold,
  or baseline fell below `min_baseline_usd`. Sets `ended_at`.

## Configuration

In `squadron.yaml` (all optional — defaults work for most
installs):

```yaml
cost_spike:
  warn_pct: 0.25         # fire at +25% over rolling baseline
  critical_pct: 0.50     # escalate at +50%
  baseline_samples: 60   # 60 minutes of history
  min_baseline_usd: 10   # don't fire on tiny installs
  window: 1h             # insights window
```

Currently these are wired from `costspikes.DefaultConfig()` —
the YAML knob to override is a v0.29.x follow-up.

## API

| Endpoint | Purpose |
|-|-|
| `GET /api/v1/alerts/cost-spikes?status=open\|closed\|all` | List spikes. UI calls this with `status=open` every 30s. |
| `POST /api/v1/alerts/cost-spikes/:id/acknowledge` | Operator confirmation. Suppresses banner; doesn't close. |
| `POST /api/v1/alerts/cost-spikes/tick` | Force-run one detector tick. For demo + tests. |

## Why a separate package from `alerts`?

The existing `alerts` package is an operator-authored
rule-evaluator — every alert rule is a Squadron QL query plus a
threshold. Cost spikes don't need operator authoring; the
heuristic plus the pricing projection IS the rule. Mixing them
into the same storage shape would pollute the rules list with
auto-generated entries every time a spike fires.

Cost spikes also have a distinct lifecycle (open → critical →
acknowledged → closed) that doesn't map cleanly to the
"trigger → resolve" model of rule-based alerts.

## What's NOT in v0.29

- **Webhook dispatch.** The detector writes events to the store;
  it doesn't POST anywhere. Slack/PagerDuty integration is a
  v0.29.x follow-up.
- **Per-signal independent baselines.** Today we baseline the
  total $/month. A fleet whose logs ingest is steady but
  metrics doubled would currently surface as "metrics-dominated
  spike," which is right, but the signal-specific baselines
  would catch it earlier.
- **Bill-import reconciliation.** Spikes are projections, not
  invoices. Real bill comparison is a separate roadmap item.
- **YAML config for thresholds.** Detector uses
  `costspikes.DefaultConfig()` in main.go; per-install tuning
  needs to round-trip through `squadron.yaml`.

## See also

- `docs/savings.md` — the pricing layer the detector consumes.
- `docs/alerts.md` — the existing rules-based alert system.
- `docs/recommendations.md` — what to actually do about a spike
  once you see one.
