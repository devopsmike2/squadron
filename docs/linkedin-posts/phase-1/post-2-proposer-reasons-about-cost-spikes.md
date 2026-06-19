# Post 2: The proposer reasons about cost spikes

**Pillar:** Intuitive remediation
**Tag at publish:** v0.84.0
**Visual evidence:** A screenshot of the proposer playground at
`/playground/proposer` on the live deployment, with the "Two attrs
→ plan" starter loaded and the result panel populated from a real
run. The reasoning text is shown in full; the metering strip shows
tokens-in, tokens-out, and estimated USD from the actual API
response.
**Hashtags:** #OpenTelemetry #SRE
**Target word count:** 200-400

## Draft

A cost spike arrives in Prod utility fleet. Baseline: $400/month.
Peak: $1,648/month. 312% over baseline. Severity: critical. The
top attributes flagged by the cost attribution are `container.id`
and `k8s.pod.uid`.

Here is what the v0.79 proposer does, end to end, from inside the
v0.84 playground:

1. The bridge wraps the spike into a `CostSpikeContext` and calls
   the proposer.
2. The proposer's decision framework evaluates: one config change
   or sequenced changes? Two independent high-cardinality
   attributes argue for staging the drops — observe between
   steps, abort if step 0 is enough on its own.
3. The model emits a `Kind: "plan"` result with two
   `PlanStepCandidate` entries. Step 0 drops `container.id` with
   an inline collector-config snippet. Step 1 drops `k8s.pod.uid`
   with its own snippet. Reasoning lives on step 0; step 1
   inherits via plan grouping.
4. The bridge dispatches on `Kind`. Rollout-kind goes through the
   v0.58 path. Plan-kind goes through `CreatePlan`, which
   materializes the two-step rollout sequence with `proposed_by:
   ai`.
5. The operator sees one approval gate at step 0. They approve.
   The v0.70 engine sequences step 0 → step 1. If anything fails,
   the v0.72 backwards walk rolls both steps back automatically.

The playground (`/playground/proposer`, shipped v0.84) is the
operator's dogfood surface for this loop. Hand-craft a spike or
one-click one of the starter scenarios. The proposer runs against
the real Anthropic API. No rollouts are created. No audit events
are written. The operator sees what the AI would do before any
side effects exist.

The whole flow is mechanism, not magic. The decision framework is
two bullet lists in the system prompt. The dispatch is a switch on
a string. The approval gate is a `requested_by != approved_by`
check enforced server-side.

Repo at the v0.84.0 tag. The playground is at
`/playground/proposer` on any Squadron deployment with AI
capabilities enabled.

#OpenTelemetry #SRE

## Visual asset spec

- **Filename:** `assets/post-2-playground-two-attrs-plan.png`
- **Surface:** The proposer playground at `/playground/proposer`
  on the live deployment at the v0.84.0 tag. Load the "Two attrs →
  plan" starter (one click in the starters strip), then click Run
  and wait for the API response. The form on the left stays
  populated with the 312% / $400 → $1,648 / `container.id` +
  `k8s.pod.uid` values; the result panel on the right shows
  `Kind: plan`, the reasoning text in full, the two-step evidence
  chips, and the metering strip with real token-in / token-out /
  estimated USD numbers.
- **Annotations:** none on the body. A small "v0.84.0 dogfood run"
  caption added below the screenshot in the LinkedIn post body, not
  baked into the image. The reasoning text and metering numbers are
  the point — let them speak.
- **Crop:** include the route in the browser address bar so the
  reader can verify the surface. Drop the OS chrome.

## Anti-pattern guard

Resists **the backwards-from-marketing post** from
linkedin-rollout.md "Anti-patterns to avoid". This post walks the
exact code path that runs on a real spike — bridge → decision
framework → dispatch → engine sequencing → approval gate — with
the numbers from a real playground run. Nothing is invented to fit
the claim. The 312% spike, the two attributes, the two-step plan
are all what the "Two attrs → plan" starter actually produces. The
mechanism is the message; the outcome ("a sequenced fix") is
something the reader infers, not something the post promises.
