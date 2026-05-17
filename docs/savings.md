# Savings Dashboard

The Savings dashboard is Squadron's answer to "how much is this
costing me, and what can I do about it?" — in dollars, not bytes.
It's built on the v0.24 Cost Insights byte numbers, the v0.25
recommendation engine, and a small v0.27 pricing layer that maps
bytes to $/month using configurable per-destination rules.

## What you'll see

- **Estimated monthly spend** — projected from the last 1h or 24h
  of ingest at your configured pricing rules. This is your "what
  Squadron sees us spending today" number.
- **Potential monthly savings** — the sum of $/month savings
  across all active recommendations. The "if you apply these,
  here's what you'd save" number.
- **Quick Wins** — recommendations ranked by $/month, each with an
  Apply button that deep-links to the config editor with the
  recommended snippet pre-filled. Operator reviews, saves, rolls
  out via the existing staged-rollout flow.
- **Destination spend** — $/month broken down by configured
  exporter destination (Datadog, Honeycomb, etc.). Pro-rated from
  the v0.24 destination attribution.
- **Pricing assumptions** — every rate Squadron is using, visible
  at the bottom of the page. Operators see what feeds their
  numbers and can edit the rules in `squadron.yaml`.

## Configuration

Pricing ships **enabled by default** with a conservative starter
rule set. The defaults bias high so projected savings don't
overpromise. To turn pricing off entirely, set
`pricing.enabled: false` — the Savings page will collapse to a
single-line nudge and the $ figures will disappear from the
recommendation cards.

The full shape in `squadron.yaml`:

```yaml
pricing:
  enabled: true
  currency: USD  # ISO 4217; informational, no FX
  rules:
    # First-match-wins. `match` is a substring against the
    # destination_key built by the v0.24 exporter parser
    # (e.g. "honeycomb:Honeycomb (api.honeycomb.io)").
    - match: "datadog"
      label: "Datadog"
      price_per_gb: 0.40         # base rate; used when per-signal
                                  # below is zero
      traces:  1.50              # per-signal overrides
      logs:    0.10
      metrics: 0.50
    - match: "honeycomb"
      label: "Honeycomb"
      price_per_gb: 1.20         # event-based pricing rolls up
                                  # to ~$1-2/GB at typical sizes
    - match: ""                  # catch-all (auto-appended if you
                                  # don't include one)
      label: "Other"
      price_per_gb: 0.30
```

The dashboard's footer always shows the active rule set, so the
operator can see exactly what feeds their projection.

## Default rates

Squadron ships starter rates for the major OTel destinations:

| Destination       | Base $/GB | Notes                                     |
|-------------------|-----------|-------------------------------------------|
| Datadog           | $0.40     | logs $0.10, metrics $0.50, traces $1.50   |
| Honeycomb         | $1.20     | event-based pricing rolls up to ~$1-2/GB  |
| New Relic         | $0.40     | telemetry ingest                          |
| SigNoz Cloud      | $0.30     | telemetry ingest                          |
| Grafana Cloud     | $0.50     | logs $0.10 (Loki is cheap)                |
| Splunk            | $0.80     | typical enterprise ingest                 |
| Other (catch-all) | $0.30     | conservative default for unmatched dests  |

**These are not accurate enough to make procurement decisions
with.** Real prices vary by retention tier, region, contract,
commit, and discount. Tune the rules against your actual invoice
before treating the projection as more than a ballpark.

## How the math works

For each per-signal byte rate observed in the last hour:

```
$/month = (bytes_per_hour / 1 GB) × 730 hours/month × $/GB
```

For a recommendation, the engine's existing
`est_savings_bytes` (bytes saved per window) is normalized to
bytes/hour, then run through the same formula at the catch-all
rate. The byte figure stays alongside the $ figure on every
recommendation, so operators who want to audit can see both.

The per-destination breakdown is computed in the UI by walking
each agent's `effective_config`, pro-rating that agent's volume
evenly across its configured exporters, and pricing the result
through the same rules. This is the same byte-attribution heuristic
the v0.24 Cost Insights destination panel uses; the only addition
in v0.27 is the $/GB multiplication.

## API

| Endpoint                        | What it returns                            |
|---------------------------------|--------------------------------------------|
| `GET /api/v1/pricing/config`    | Active rules + currency + enabled flag     |
| `GET /api/v1/pricing/projection?window=1h\|24h` | Fleet $/month + per-signal breakdown + assumptions |

The recommendation endpoints also gain a new field on every item:

```json
"est_savings_per_month_usd": 33.77
```

Existing clients that don't know about the field ignore it; the
v0.25 wire shape is otherwise unchanged.

## What's NOT in v0.27.0

- **Retrospective tracker** ("you saved $X last month after
  applying these fixes"). Needs audit-log integration to know when
  each rec was applied; targeted for v0.27.1.
- **Settings UI for rates.** Today it's `squadron.yaml` only; UI
  editor with live preview is v0.27.x.
- **Per-destination signal split** in the Savings dashboard's
  destination breakdown. Today every destination is priced at its
  base rate; differentiating logs vs metrics vs traces per
  destination needs server-side destination attribution (currently
  UI-side).
- **Real egress measurement.** Squadron estimates from byte
  counts × per-GB rates; doesn't yet instrument the exporter to
  measure actual bytes sent. v0.28+ if there's demand.
- **Multi-currency / FX.** `currency` is informational; everything
  is computed in the configured currency directly.
- **Volume discount tiers.** Real cloud contracts have step
  pricing (cheaper above N GB/mo). Modeling that needs a more
  expressive rule shape.

Tracked for v0.27.x / v0.28.

## See also

- `docs/scale-testing.md` — Cost Insights endpoint perf at 1000
  agents (the same endpoints power Savings).
- `docs/recommendations.md` — the recipe set the Quick Wins panel
  is ranking.
- `docs/ai-assist.md` — the Explain button on each recommendation
  comes from the v0.26 AI layer.
