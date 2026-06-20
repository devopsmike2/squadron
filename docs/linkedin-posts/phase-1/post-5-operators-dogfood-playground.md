# Post 5: Operators dogfood the proposer in the playground

**Pillar:** User friendly
**Tag at publish:** v0.84.0
**Visual evidence:** A short GIF (8-10 seconds) of the proposer
playground at `/playground/proposer` on the live deployment at the
v0.84.0 tag. The recording starts on the empty form, clicks the
"Two attrs → plan" starter, clicks Run, and ends on the populated
result panel showing `Kind: plan`, the reasoning text, evidence
chips, and the metering strip. Real API call, real numbers.
**Hashtags:** #OpenTelemetry #PlatformEngineering
**Target word count:** 200-400

## Draft

An operator should be able to ask "what would the AI do here?"
without seeding a fake cost spike into their application store.

The v0.84 playground at `/playground/proposer` is that surface.
Hand-craft a `CostSpikeContext` field by field — group, signal,
baseline USD per month, peak USD per month, severity, top
attributes — or one-click one of three starter scenarios from the
v0.83 bench corpus. Click Run. The proposer evaluates and the
result panel shows kind, reasoning text in full, evidence chips,
tokens-in, tokens-out, latency, and estimated USD from the actual
API response. No rollouts are created. No plans are written. No
audit events fire.

The three starters cover the decision shapes the proposer is
supposed to handle:

- **Single-attr rollout.** One high-cardinality attribute drives
  the spike; the proposer emits `Kind: rollout` against an
  existing target config.
- **Two-attrs plan.** Two independent attributes argue for staging
  the drops. The proposer emits `Kind: plan` with two steps. This
  is the v0.82 #550 reproducer — also the scenario screenshotted
  in the v0.84.0 release notes.
- **Empty attribution → decline.** No top attributes supplied. The
  proposer is supposed to decline with a one-sentence reason. The
  playground makes the refusal path observable, not theoretical.

The endpoint is `POST /api/v1/ai/proposer/preview`. Same
agents-read scope as the other read-only AI surfaces (Ask,
explain, fleet-query). The same code path that runs against a
real spike runs here — only the persistence step is bypassed.

Use cases:

- Validate a prompt change before pushing through CI.
- Demo the proposer's decision framework to a stakeholder
  without a real incident to point at.
- Sanity-check what the model would do under a real spike before
  the bridge daemon fires.

Repo at the v0.84.0 tag. The playground is at
`/playground/proposer` on any Squadron deployment with AI
capabilities enabled.

#OpenTelemetry #PlatformEngineering

## Visual asset spec

- **Filename:** `assets/post-5-playground-walkthrough.gif`
- **Surface:** The proposer playground at `/playground/proposer`
  on the live deployment at the v0.84.0 tag.
- **Recording flow (6-10 seconds, looped cleanly):**
  1. Start on the empty form with the three starter buttons
     visible.
  2. Cursor clicks "Two attrs → plan" — the form fields populate
     with the 312% / $400 → $1,648 / `container.id` +
     `k8s.pod.uid` values.
  3. Cursor clicks Run. A brief loading spinner.
  4. Result panel renders: `Kind: plan`, reasoning text, two
     evidence chips, metering strip with real token-in /
     token-out / estimated USD.
  5. Hold on the result panel for ~2 seconds, then loop back.
- **Annotations:** none baked into the recording. A small caption
  below the post body reads "v0.84.0 dogfood run — real API
  call, no rollouts created." The reasoning text and metering
  numbers are the point.
- **Crop:** include the browser address bar with
  `/playground/proposer` visible so the reader can verify the
  surface. Drop the OS chrome.

## Anti-pattern guard

Resists **the hype follow-up** from linkedin-rollout.md
"Anti-patterns to avoid". Posts 2 and 3 already walked the
proposer's reasoning loop and the plan-kind dispatch; the pull is
to make this post louder ("now you can play with it yourself!").
The post instead names a narrow operator workflow — preview a
proposal before the daemon fires, validate a prompt change before
CI, demo the framework without a real incident — and lists the
three starter scenarios concretely. Same volume, longer duration.
The playground is shown as a tool the operator actually uses, not
a feature announcement.
