# Proposer bench

`squadron-proposer-bench` is the v0.83 command for measuring the
proposer's behavior against the real Anthropic API. It runs a small
fixed corpus of cost-spike AND discovery scenarios and emits a graded
report so an operator (or a scheduled CI job) can spot regressions
across releases.

As of v0.86 the bench is **bi-modal**: it exercises both proposer
entry points — `ProposeFromCostSpike` and `ProposeFromDiscoveryScan` —
in the same run. The same metrics surface across both arcs, and
regression detection works the same way on each: a bucket count
moving in the wrong direction (a `discovery` seed flipping from
`succeeded` to `parse_failed_preamble`, say) shows up in the same
report the cost-spike arc has shown up in since v0.83.

It is the runtime sibling of `internal/proposer/stress_live_test.go`:
the live stress test gates on pass/fail; the bench reports
distributions an operator can compare release-over-release. They
both exist because the failure modes the proposer can land in are
not a single bit — `truncated` is a different bug class from
`parse_failed_preamble`, and a regression in one shouldn't be
hidden by the other staying green.

## What it measures

Per scenario:

  - **outcome bucket** — one of `succeeded`, `declined`, `truncated`,
    `parse_failed_preamble`, `parse_failed_other`, `llm_error`. The
    buckets are deliberately separable so a regression in one class
    of bug is visible in the report.
  - **tokens in / out** — gives the operator the headroom signal.
    `tokens_out_max` against `proposer_max_tokens` is the column to
    watch. The v0.82 #550 truncation would have been visible here as
    `tokens_out_max ≈ 1090` against the old `cap = 1024`.
  - **latency_ms** — wall clock per scenario.
  - **estimated_usd** — Sonnet 4.6 pricing applied to the actual
    token counts the API returned.

Aggregated:

  - per-bucket counts
  - p50 / p95 tokens (in + out)
  - p50 / p95 / p99 latency
  - total estimated cost for the run

## Cost ceiling

At the v0.88 corpus size (17 seeds: 8 cost-spike + 9 discovery) and
typical token sizes, expect roughly **\$0.18–\$0.32 per run**. The
bench prints `total cost` on every run so the operator sees the
number after each invocation. Discovery seeds run at a similar token
profile to cost-spike seeds — the per-seed cost is comparable, so
adding 9 discovery seeds on top of 8 cost-spike seeds keeps the
total inside the same envelope. The v0.87 RDS seed
(`discovery_rds_mixed_coverage`) and the v0.88 S3 + ALB seeds
(`discovery_s3_mixed_coverage`, `discovery_alb_mixed_coverage`)
carry larger inventory lists than the slice-1 discovery seeds
(5–10 rows per category instead of 2–3); the per-seed cost stays
inside the discovery-arc range.

For scheduled CI: a daily run for thirty days is ~\$5-7. Costs scale
linearly with corpus size; v0.84+ expansions to the corpus should
update this number.

## Running

Local, with the operator's own API key:

```bash
ANTHROPIC_API_KEY=sk-ant-...   \
    ./bin/squadron-proposer-bench
```

Filter to a subset of seeds while iterating on a prompt change:

```bash
ANTHROPIC_API_KEY=sk-ant-...   \
    ./bin/squadron-proposer-bench -filter '^plan_'
```

JSON output, for piping into a diff tool against a baseline file:

```bash
ANTHROPIC_API_KEY=sk-ant-...   \
    ./bin/squadron-proposer-bench -output json > bench-$(date +%F).json
```

CI: schedule the same invocation as a cron job. Publish the report
artifact somewhere the team can see. A release-blocker regression
shows up as a bucket count moving in the wrong direction —
`succeeded` going down, or `parse_failed_preamble` going non-zero.

## Corpus

Seventeen hand-curated scenarios at v0.88 — split across the two
proposer arcs.

### Cost-spike arc (8 seeds, drives `ProposeFromCostSpike`)

| Seed                          | What it tests                                                 |
| ----------------------------- | ------------------------------------------------------------- |
| `rollout_clean_single_attr`   | Baseline rollout-kind happy path                              |
| `plan_two_indep_attrs`        | Plan-kind, two-step (the v0.82 #550 reproducer)               |
| `plan_three_related_attrs`    | Plan-kind, three-step — pushes the token budget               |
| `declined_zero_spike`         | Baseline = peak; should decline                               |
| `declined_no_attribution`     | Empty `TopAttributes`; should decline                         |
| `adversarial_prompt_injection`| Injected instruction inside an attribute name                 |
| `boundary_tiny_fleet`         | Single-agent fleet                                            |
| `sparse_context_low_signal`   | Low-confidence spike (25% over baseline)                      |

### Discovery arc (9 seeds, drives `ProposeFromDiscoveryScan`)

| Seed                                   | What it tests                                                                                       |
| -------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `discovery_small_fleet_uninstrumented` | 3 EC2 + 2 Lambda, all uncovered — minimal plan happy path                                           |
| `discovery_mixed_coverage`             | 10 EC2 (4 covered) + 8 Lambda (3 covered); plan must skip the covered                               |
| `discovery_zero_resources`             | Empty inventory; should decline (`declined` bucket through discovery)                               |
| `discovery_fully_instrumented`         | 8 EC2 + 5 Lambda, all covered; should decline                                                       |
| `discovery_windows_heavy`              | 6 Windows EC2, 0 Lambda — exercises OS-family reasoning                                             |
| `discovery_lambda_runtime_variety`     | 12 Lambda across 5 runtimes; exercises per-runtime OTel layer batching                              |
| `discovery_rds_mixed_coverage`         | 5 RDS across 4 engines (PI/EM mixed) + 3 EC2 + 2 Lambda; exercises slice 2 RDS PI/EM independent-levers reasoning |
| `discovery_s3_mixed_coverage`          | 8 S3 buckets (3 logging-enabled, 5 not) + 3 EC2 + 2 Lambda; exercises slice 3a S3 Server Access Logging single-axis recommendation with operator-fill-in target |
| `discovery_alb_mixed_coverage`         | 5 ALBs (2 covered to different S3 buckets, 3 uncovered) + 2 instrumented S3 buckets + 4 EC2 + 3 Lambda; exercises slice 3a ALB Access Logs single-axis recommendation with the ALB→S3 cross-reference rule (target bucket should be one Squadron already sees) |

The list is intentionally small. v0.84 will refactor the
`internal/proposer` stress corpus into a shared module so the bench
and the stress test exercise the same scenarios. Until then, expand
the list in `cmd/squadron-proposer-bench/main.go` directly.

### Bi-modal posture

Outcome bucketing is shared across arcs — `succeeded`, `declined`,
`truncated`, `parse_failed_preamble`, `parse_failed_other`,
`llm_error` apply cleanly to both `ProposeFromCostSpike` and
`ProposeFromDiscoveryScan`. The report's `by kind:` summary line
splits the bucket counts per arc so an operator can see at a glance
that, say, the cost-spike arc is green while the discovery arc has
a `truncated` regression — without that split a single global
counter would hide which prompt drifted. Same calibration discipline
v0.83 established for the cost-spike proposer now covers the
discovery path; the next regression that would have shipped as a
viral failure story gets caught here for the discovery prompt
before it hits production.

The discovery arc has 9 seeds at v0.88 (up from 6 at v0.86 and 7
at v0.87) — slice 3a (S3 + ALB) added two seeds at the same
discipline. Each new scanner category that lands in production
gets a paired bench seed so the proposer's prompt extension
surface stays calibrated.

## Reading the report

A clean baseline looks like:

```
Aggregate
  by kind:
    cost_spike    8 seeds | succeeded: 6 | declined: 2 | failed: 0
    discovery     6 seeds | succeeded: 4 | declined: 2 | failed: 0
  outcome buckets:
    declined      4
    succeeded     10
  tokens in  : p50=1850  p95=1880
  tokens out : p50=900  p95=1200  max=1500  cap=4096 (37% of cap at max)
  latency ms : p50=12000  p95=18000  p99=22000
  total cost : $0.18
```

Failure-mode signals to watch:

  - `truncated` > 0  →  raise `ai.ProposerMaxTokens` and re-measure
    (or pivot to a diff-encoded `inline_config_snippet` per the
    note in `docs/ai-features.md`).
  - `parse_failed_preamble` > 0  →  the v0.83 #552 prompt fix has
    regressed. The model is narrating before the JSON object again.
    Re-tighten the system prompt and re-run.
  - `tokens_out_max` > 80% of `cap`  →  truncation is likely on the
    next prompt change. Pre-emptive raise of the cap.
  - `succeeded` dropping below the corpus's `succeeded` expectation
    →  the prompt or model changed in a way that flipped a previously
    actionable spike into a decline. Inspect the per-seed `error`
    field.
