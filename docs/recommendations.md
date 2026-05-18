# Cost Recommendations

The Squadron recommendations engine (v0.25+) is a small heuristic
layer over the [Telemetry Volume Insights](./scale-testing.md)
surface. It answers a different question than the volume panel:
not "where are my bytes going," but "what should I actually do
about it."

This page documents what the engine looks for, how confident you
should be in each recipe, and how the UI surfaces the output.

## What it does

Every recommendation is generated from a fresh fleet snapshot — no
historical store, no background worker. Recipes are pure functions
over the insights surface; the same fleet shape produces the same
output across runs, which is also why dismissals work: the
recommendation ID is a deterministic hash of (recipe, scope), not
a random UUID.

Output is ranked by **severity** (critical > warn > info), then by
**estimated savings** (descending). The Cost Insights page shows
the top six on the fleet panel; the agent detail drawer shows up
to four narrowed to that agent.

## The v0.25 recipe set

### Noisy attribute — `noisy_attribute`

The single highest-ROI optimization in production OTel
deployments. Looks at the sampled `/insights/volume/attributes`
output for each signal and flags any key whose `pct_of_signal` is
≥ 15% (warn) or ≥ 30% (critical).

Snippet: an `attributesprocessor` with an `action: delete` for the
flagged key, plus a header comment showing where to wire it into
`service.pipelines.<signal>.processors`. The snippet is not a
drop-in replacement for your config — it's a fragment to merge.

Confidence: medium. The attribute size is sampled (~2000 rows per
query) and extrapolated, so the byte estimate is a ballpark. The
fact that the attribute is dominating, however, is usually
trustworthy at any reasonable sample size.

### Outlier agent — `outlier_agent`

Flags any agent producing ≥ 2× the fleet median bytes (warn) or
≥ 5× (critical). Median is computed across the top 50 agents to
avoid skewing the threshold with quiet/idle agents that aren't
relevant to the question.

No single snippet — outlier causes vary (missing sampling step,
exporter retry loop, verbose log severity, host running too many
collectors). The recommendation points the operator at the agent
and explains the common culprits.

Confidence: high on detection, low on prescription. The agent IS
loud relative to its peers; figuring out why takes a human.

Recipe is skipped entirely on fleets with fewer than 4 agents —
outlier detection is meaningless on N=2.

### Drop hotspot — `drop_hotspot`

Looks at the per-signal drop rate (`dropped_count` ÷
`item_count + dropped_count`) and flags signals exceeding 1%
(warn) or 5% (critical).

Snippet: a `batch/larger` processor with a bigger
`send_batch_size` and an explicit `send_batch_max_size` ceiling.
That's the single most common fix for dead-letter pressure in
production deployments. Estimated savings is `-1` because this
recipe is about reliability, not bytes.

Confidence: medium. The recipe is correct for the most common
cause (undersized batch processor), but other causes (exporter
queue saturation, network issues to the destination) need
different fixes the engine doesn't currently distinguish.

### Empty signal — `empty_signal`

Per agent, flags any signal type the agent has zero bytes of
during the window while the rest of the fleet emits it. Suggests
pruning the receiver/exporter pair from the agent's config to cut
memory and connection overhead.

No snippet — there's no single right way to delete a pipeline
branch; the operator edits their config directly.

Confidence: high. If the agent's reporting zero bytes for a
signal it ships a pipeline for, the pipeline is dead weight.

Recipe is skipped on fleets with fewer than 3 agents, and on
agents with less than 10 KB of total volume (avoids
false-positives on idle agents).

### High cardinality — `high_cardinality` (v0.28)

The other axis of telemetry cost: metric series count. Many
backends bill not by ingested bytes but by *active series* (one
unique combination of metric name + labels = one series). A
metric that's modest in bytes can still be ruinous if its label
set explodes — a `request_duration` with `user_id` tagged blows
up to one series per user.

The recipe runs a sampled `COUNT(DISTINCT metric_attributes)` per
metric name over the window:

- **critical** — ≥ 10,000 distinct attribute combinations in the
  window.
- **warn** — ≥ 2,000 distinct attribute combinations.

Snippet: a `metricstransform` processor entry that drops the
highest-cardinality label on the offending metric. The recipe
samples 200 rows of the metric's attributes JSON to pick the
single key with the most distinct values; that's the one the
snippet's `action: drop_label` targets.

Confidence: medium-high for the detection (DISTINCT counts are
exact within the sampling window). Lower for the snippet — the
"wrong" label depends on what the metric is *for*. Always review
the suggested drop against your dashboards.

`est_savings_per_month_usd` is zero for these recommendations.
The cost model in Squadron is per-byte; per-series billing is
backend-specific and we'd rather not guess at it. The Quick Wins
panel still ranks high-cardinality findings by severity so they
surface, just without a $ figure.

## Acting on a recommendation

Each card on the Cost Insights page offers three actions:

- **Copy snippet** — clipboard write of the suggested YAML.
  Useful when you have the target config open in your own editor.
- **Open in editor** — deep-links to the new-config form
  (`/configs/new`) with the snippet prefilled inside a clearly-
  marked recommendation banner, on top of the baseline scaffolding.
  The operator names it, optionally lints it, and saves it as a
  normal config. No auto-create, no auto-rollout.
- **Dismiss** — POST to `/api/v1/recommendations/:id/dismiss`.
  Suppresses the recommendation on subsequent evaluates without
  affecting other operators' views.

Dismissals persist in the application store; restore via the
`/api/v1/recommendations/:id/restore` endpoint or by deleting the
row from `recommendation_dismissals`.

## Estimates are sampled — read this once

Every recommendation carries `estimated: true` and every byte
figure ends up in the panel labeled accordingly. The sampler is
~2000 rows per query, ORDER BY random() — fine for
"this attribute dominates" calls; ballpark for absolute byte
forecasts. Validate against your own bill before adopting a fix
that affects production.

## What's NOT in v0.25

- $/month projection from raw bytes
- Historical recommendation trends ("this was active last week")
- LLM-generated explanations or recipe synthesis
- Automatic config patching / unattended rollouts
- High-cardinality metric detection (needs new insights queries)
- Per-tenant threshold overrides (the thresholds in
  `internal/recommendations/recommendations.go:Engine` are
  hardcoded today)

Those are tracked for v0.25.x and v0.26+.

## Engine internals (for contributors)

Everything lives in `internal/recommendations/`:

- `recommendations.go` — types, recipes, engine, helpers
- `recommendations_test.go` — recipe behavior + stable-ID
  + dismissal regression tests

Adding a recipe is a small change:

1. Add a `Category` constant.
2. Write a `func (e *Engine) recipeXXX(...) []Recommendation` —
   pure function over the insights snapshot.
3. Call it from `Evaluate`.
4. Use `idFor("recipe_name", ...scope_fields)` so dismissals stay
   stable across runs.

The UI doesn't need a deploy for new categories — unknown
categories render with the default treatment (severity stripe +
title + actions). The card has hooks for future per-category icon
or color routing if you decide that's worth the per-render switch.
