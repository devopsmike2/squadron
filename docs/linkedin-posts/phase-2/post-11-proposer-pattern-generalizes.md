# Post 11: The proposer pattern generalizes

**Pillar:** Intuitive remediation
**Tag at publish:** v0.85.0
**Visual evidence:** A screenshot of the `/discovery/aws`
Recommendations tab on the live deployment at the v0.85.0 tag,
after a real Generate-recommendations run. The screenshot shows
the proposer reasoning quoted at the top, at least one
recommendation card with its title and detail, and the Terraform
HCL preview expanded so the per-step `inline_config_snippet`
content is readable. The recommendation card's "Discovery scan
&lt;ref_id&gt;" caption is visible — that is the typed
`discovery_scan` source the post is about.
**Hashtags:** #OpenTelemetry #SRE
**Target word count:** 200-400

## Draft

Same proposer engine. Two entry points. Yesterday it reasoned
about a cost spike. Today it reasons about an uninstrumented AWS
account. The plan-kind output is the same JSON shape. The audit
posture is the same audit posture. The bi-modal claim is not
aspirational — it is running.

The cost-spike entry point is `ProposeFromCostSpike`. The
discovery entry point is `ProposeFromDiscoveryScan`. Both live in
`internal/ai/`. Both call the same `callMessages` wrapper. Both
return a `*ProposalResult`. The v0.66 recommendations engine
carries a typed `Source` field with values `cost_spike`,
`discovery_scan`, and `manual`. The Recommendations tab on the
`/discovery/aws` page renders the `discovery_scan` source as a
caption on each card. The wire labels the path the recommendation
took to reach the operator.

The proposer prompt is different per entry point — discovery
asks for batching by category (Lambda batch, EC2 batch); cost
spike asks about staged attribute drops. That is the part the
prompt owns. Everything below the prompt is shared: the bridge,
the dispatch on `Kind`, the audit trail, the approval gate.
v0.79 chose plan-kind as the output shape for cost spikes. v0.85
forced discovery to plan-kind too — discovery is always staged
so the operator can observe between batches. The handler
rejects a rollout-kind response from the discovery path.

This is the moment "the pattern generalizes" stops being a
promise and becomes a property of the code. The proposer is the
substrate. Cost spike and discovery are sources. Future sources
(SLO regressions, security drift, capacity forecasting) plug in
at the same interface — typed source, typed action payload,
same engine, same surface, same audit. Adding a source is
shipping a new prompt and a new handler, not redesigning the
recommendation layer.

The reader who clones the repo can see this end to end:
`internal/ai/proposer_discovery.go`,
`internal/ai/proposer_discovery_prompt.go`, and
`internal/recommendations/recommendations.go` with the typed
`SourceDiscoveryScan` constant.

Repo at the v0.85.0 tag.

#OpenTelemetry #SRE

## Visual asset spec

- **Filename:** `assets/post-11-discovery-recommendations-tab.png`
- **Surface:** the `/discovery/aws` Recommendations tab on the
  live deployment at the v0.85.0 tag, after running a scan that
  produces uninstrumented resources and then clicking Generate
  recommendations. Two model runs may be needed to land a
  screenshot with both EC2 and Lambda batched recommendations
  visible — that is fine; the v0.84 playground proved the
  re-run discipline.
- **What must be visible in the crop:** the proposer-reasoning
  blockquote at the top (real model output, not placeholder
  text); at least one recommendation card with its title, the
  "Discovery scan &lt;ref_id&gt;" caption, and the Terraform
  HCL preview expanded with the `inline_config_snippet` content
  readable. The Tab header showing the Recommendations tab
  active is part of the frame.
- **Annotations:** one small marker on the "Discovery scan
  &lt;ref_id&gt;" caption with the text "typed source —
  discovery_scan", added in post-processing. The caption is the
  visible proof the typed source field is real; the marker
  names it.
- **Crop:** include the route in the browser address bar so the
  reader can verify the surface.

## Anti-pattern guard

Resists **the backwards-from-marketing post** from
linkedin-rollout.md "Anti-patterns to avoid". The pull is to
claim "Squadron's AI can reason about anything." The post
instead names two specific entry points
(`ProposeFromCostSpike`, `ProposeFromDiscoveryScan`), the file
they live in, the wrapper they share, the typed source field
in the recommendations engine, and the prompt difference (per-
category batching for discovery, staged attribute drops for cost
spike). Every claim points at a file the reader can open at the
v0.85.0 tag. The generalization is the mechanism the code
already exhibits, not a forward-looking promise.
