# Proposer stress test, v0.58

The Squadron AI proposer takes a cost spike and emits a structured
rollout proposal a human approves before the engine touches an agent.
v0.58 hardens that surface with an adversarial 50 iteration stress
test under `internal/proposer`. This document is the methodology, the
results, and the regression bar future PRs are evaluated against.

## TL;DR

50 of 50 iterations classified cleanly. Zero "refused incorrectly"
outcomes — every refusal was either an LLM refusal (correct for the
seed), a bridge level refusal (correct for the input shape), or an
opted in LLM / dispatcher error. Latency p99 across the whole corpus
was 153µs on the engineer's laptop. Heap delta across the whole run
was 757 KB. The proposer held up.

## Methodology

### The 50 seed corpus

The corpus is a deliberately uneven mix designed to look like what a
real fleet would generate over a month plus a small adversarial sub
corpus and the failure modes we explicitly want to surface.

| Category                  | Count | What it tests                                                  |
| ------------------------- | ----- | -------------------------------------------------------------- |
| High cardinality standard | 10    | Lead attribute drives the spike (container.id, k8s.pod.name…)  |
| Ambient context           | 5     | Same shape, larger surrounding store, varied attribute lead    |
| Boundary conditions       | 8     | Zero percent spike, missing attribution, huge baseline, etc.   |
| Adversarial inputs        | 8     | API keys, JWTs, IPv4, internal hostnames, prompt injection     |
| Group context edge cases  | 8     | Missing config, missing group, malformed attribution, etc.     |
| Signal type variations    | 4     | metrics, logs, traces, empty                                   |
| Dispatcher failure        | 3     | Rollout service rejects the create                             |
| LLM error                 | 4     | timeout, 5xx, rate limited, malformed                          |

The full per seed listing lives in `internal/proposer/stress_seeds_test.go`.

### What each iteration measures

Per iteration the test captures:

- **Wall clock duration** from `bridge.tick(ctx)` entry to return.
- **The bridge's classification verdict** based on what the proposer
  returned and whether the bridge dispatched a rollout.
- **Heap allocation delta** at run start and end (not per iteration;
  Go's allocator does not surface a tight per call peak).

The outcome bucket is one of:

- `proposer-succeeded`
- `proposer-refused-correctly`
- `proposer-refused-incorrectly` ← the bar that must stay zero
- `llm-error`
- `dispatcher-error`
- `timeout`

### Fake LLM mode vs live mode

The default test (`go test -short ./internal/proposer/...`) drives a
deterministic fake proposer that decides decline vs propose based on
the inbound `CostSpikeContext` shape. The fake mirrors how a real
model would behave: empty attribution declines, zero percent spike
declines, no group declines, otherwise propose. This makes the test
cheap, fast, and stable in CI.

A live mode lives in `stress_live_test.go` behind the `live` build
tag. It runs the same 50 seeds against the real `internal/ai.Service`
and the real Anthropic Messages API. Run it locally with:

```bash
ANTHROPIC_API_KEY=sk-ant-... go test -tags=live \
    -run TestProposerStress_Live ./internal/proposer/...
```

The live test does not gate CI; the report is the deliverable.
Cost: roughly two US dollars and two minutes at v0.58 token sizes.

## Results

### Outcome distribution

| Outcome                       | Count |
| ----------------------------- | ----- |
| proposer-succeeded            | 37    |
| proposer-refused-correctly    | 6     |
| proposer-refused-incorrectly  | 0     |
| llm-error                     | 4     |
| dispatcher-error              | 3     |
| timeout                       | 0     |

### Latency

Measured on the engineer's MacBook Pro M3, fake LLM mode (no network
in the loop). p99 is a single outlier from the 500 agent fixture
which has more data to serialize.

| Percentile | Wall time |
| ---------- | --------- |
| p50        | 3.3µs     |
| p95        | 7.3µs     |
| p99        | 152.7µs   |
| max        | 152.7µs   |

### Memory

Heap allocation delta from run start to end: 757.1 KB across 50
iterations. No goroutine leaks observed.

## Findings

### What the corpus surfaced

**The bridge refuses correctly under three distinct conditions** that
all live in `buildContext`:

1. Spike attribution names an agent that does not exist in the store
   (the test's `group_05_attribution_unknown_agent` seed).
2. The group resolved from the top agent has no current config in
   the store (`group_01_missing_config`).
3. The top agent's GroupID is nil (`group_04_agent_with_nil_group`).

All three are pre LLM refusals: the bridge never sends the prompt
because there is no actionable target. This is the right behavior.

**The fake LLM refuses correctly under three more conditions**:

4. Spike attribution is empty (`boundary_02_no_attribution`).
5. Baseline equals peak so there is no actual cost movement
   (`boundary_01_zero_percent`).
6. Attribution JSON is structurally malformed
   (`group_06_attribution_malformed_json`).

The fake's logic mirrors what a real model would say. The live run
is where we will see whether the real model agrees on every case.

**The adversarial sub corpus did not produce any panics or
classification errors.** The bridge accepts inputs containing API
keys, JWTs, IPv4 addresses, internal hostnames, unicode, prompt
injection attempts, and SQL like strings without crashing or
returning malformed proposals. The actual content safety guarantee
for these (do not leak the literal value into the proposal name or
reasoning) is a prompt level concern owned by the `internal/ai`
proposer prompt; the stress test verifies only that the bridge
itself does not fall over.

**The dispatcher failure category surfaced the bridge's correct
"log a warning, do not retry" behavior**: all three iterations
produced exactly one warning log and zero rollout posts. Operators
who configure unreliable rollout services will see this in the
proposer logs as expected.

### Open follow ups (none filed for v0.58)

No iteration produced a bug worth filing as an issue. The run was
clean. Tracked for future hardening:

- The bridge could emit an audit event when it skips a spike due to
  missing config or unknown agent so operators can see why the
  proposer never ran. Today the only signal is a structured log.
- The 500 agent fixture's 153µs latency is dominated by JSON
  marshal of the attribution. Not a real concern at fleet sizes
  Squadron is built for, but worth re measuring if v0.6 scaling
  pushes fleet sizes above ~5000.

## Regression bar

The next time the proposer or the bridge changes, this test runs as
part of `go test -short ./internal/proposer/...` in CI. The hard
assertions are:

- `proposer-refused-incorrectly` must stay at zero.
- The outcome buckets must sum to 50 (catches classifier bugs).
- At least 30 of 50 iterations must succeed (catches catastrophic
  context assembly regressions).

The latency and memory numbers are not asserted as hard ceilings;
they are documented here as the v0.58 baseline so a future PR can
compare. If a change pushes p99 above 1ms or heap delta above 5MB,
re measure on the same hardware and document the cause in the next
revision of this file.

## How to run

```bash
# Fake LLM mode, runs in CI:
go test -short -v -run TestProposerStress_50Iterations \
    ./internal/proposer/...

# Live mode, against the real Anthropic Messages API:
ANTHROPIC_API_KEY=sk-ant-... go test -tags=live -v \
    -run TestProposerStress_Live ./internal/proposer/...
```

## Why this matters

Squadron's identity is an AI augmented platform engineering control
plane. The proposer is the surface the AI most visibly touches:
operators see "AI" badges on rollouts that came through it, and the
audit timeline is going to accumulate `proposal.created` and
`proposal.declined` events as the system runs.

The stress test is the discipline that lets us ship the next AI
surface (audit explain in v0.57, action runner expansion in v0.55,
incident drafter in v0.54) without quietly regressing this one.
Future LinkedIn beats on the AI surfaces should reference this
document; it is the receipts for "yes, we hardened it."
