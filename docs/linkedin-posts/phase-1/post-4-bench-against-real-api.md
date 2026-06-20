# Post 4: We benchmark the proposer against the real API

**Pillar:** Intuitive remediation
**Tag at publish:** v0.83.0
**Visual evidence:** A screenshot of the `squadron-proposer-bench`
terminal output from a real run at the v0.83.0 tag — the aggregate
block showing outcome buckets, `tokens in p50/p95`, `tokens out
p50/p95/max` against `cap 4096`, `latency ms p50/p95/p99`, and
`total cost`. The numbers are from an actual run against the
Anthropic API on a real operator key.
**Hashtags:** #OpenTelemetry #SRE
**Target word count:** 200-400

## Draft

A regression in an AI proposer does not announce itself as a stack
trace. It announces itself as a quietly truncated response, or a
sentence of prose before the JSON object, or a previously
actionable spike that now declines. Each of those failure modes is
a different bug class — and a release that hides one behind another
is a release that ships the regression.

`squadron-proposer-bench` is the v0.83 answer. Eight hand-curated
cost-spike scenarios. Each runs against the real Anthropic API
(not a fake) and gets graded into one of six outcome buckets —
`succeeded`, `declined`, `truncated`, `parse_failed_preamble`,
`parse_failed_other`, `llm_error`. The buckets are separable on
purpose. A regression in one class of bug is visible in the report
even if the other classes stay green.

The aggregate block reports per-bucket counts, p50/p95 tokens in
and out, max tokens out against the configured cap, p50/p95/p99
latency, and total USD per run. Cost ceiling: about $0.15 to
$0.25 per run at the v0.83 corpus size.

The column to watch is `tokens_out_max` against `cap`. The v0.82
#550 truncation bug would have shown up here as `tokens_out_max ≈
1090` against the old `cap = 1024` — the model was producing a
two-step plan that did not fit, and the response truncated
mid-JSON. The bench would have caught it pre-emptively. The fix
was already shipped (raise the cap to 4096); the bench exists so
the next class of bug doesn't take a live incident to surface.

Run it locally with `ANTHROPIC_API_KEY=... ./bin/squadron-proposer-bench`,
filter to a subset with `-filter`, or emit JSON with `-output
json` and diff against a baseline file. A daily cron at $5-7 per
month is the cheapest pre-release smoke test the proposer has.

Repo at the v0.83.0 tag. Docs at `docs/proposer-bench.md`.

#OpenTelemetry #SRE

## Visual asset spec

- **Filename:** `assets/post-4-bench-aggregate-v0.83.0.png`
- **Surface:** Terminal output from a real
  `squadron-proposer-bench` run at the v0.83.0 tag, captured on
  the operator's own laptop with a real API key. The screenshot
  shows the Aggregate block — outcome buckets with their counts,
  the tokens in/out percentiles, `cap=4096` in the tokens-out
  line so the headroom signal is visible at a glance, the
  latency percentiles, and `total cost` on the bottom line.
- **What must be visible in the crop:** the `Aggregate` header,
  the six outcome bucket labels with their counts (even the zero
  ones — a `truncated 0` line is part of the calibration story),
  the `cap=4096` annotation in the tokens-out line, the `total
  cost: $0.XX` line.
- **Annotations:** one subtle highlight on the `tokens out:
  p50=... p95=... max=... cap=4096 (XX% of cap at max)` line —
  this is the line that would have caught #550. Added in
  post-processing, not baked into the terminal. A small caption
  below the screenshot reads "the line that would have caught
  the v0.82 #550 truncation pre-emptively."
- **Crop:** include the prompt line that invoked the bench so
  the reader can see the command shape.

## Anti-pattern guard

Resists **the backwards-from-marketing post** from
linkedin-rollout.md "Anti-patterns to avoid". The post does not
claim "Squadron's AI is reliable" or "we have rigorous QA". It
shows the exact mechanism — six separable outcome buckets, the
specific column that catches truncation, the dollar cost of
running the bench, and the historical bug class that would have
been caught pre-emptively. The reader infers the reliability
posture from the mechanism. No outcome is promised; the headroom
signal does the talking.
