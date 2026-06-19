# Proposer bench

`squadron-proposer-bench` is the v0.83 command for measuring the
proposer's behavior against the real Anthropic API. It runs a small
fixed corpus of cost-spike scenarios and emits a graded report so
an operator (or a scheduled CI job) can spot regressions across
releases.

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

At v0.83 corpus size + token sizes, expect roughly **\$0.15–\$0.25 per
run**. The bench prints `total cost` on every run so the operator
sees the number after each invocation.

For scheduled CI: a daily run for thirty days is ~\$5-7. Costs scale
linearly with corpus size; v0.84 expansions to the corpus should
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

Eight hand-curated scenarios at v0.83:

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

The list is intentionally small. v0.84 will refactor the
`internal/proposer` stress corpus into a shared module so the bench
and the stress test exercise the same scenarios. Until then, expand
the list in `cmd/squadron-proposer-bench/main.go` directly.

## Reading the report

A clean baseline looks like:

```
Aggregate
  outcome buckets:
    succeeded     6
    declined      2
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
