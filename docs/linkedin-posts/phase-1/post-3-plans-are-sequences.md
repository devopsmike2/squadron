# Post 3: Plans are sequences, not single rollouts

**Pillar:** Intuitive remediation
**Tag at publish:** v0.84.0
**Visual evidence:** A screenshot of the Plan detail page
(`/plans/:id`) on the live deployment, showing a two-step plan
created from the v0.79 plan-kind dispatch path — step 0 in
`succeeded`, step 1 in `in_progress` or `succeeded`, with the
sequencing relationship visible. The plan badge from the v0.75
Plans UI is in the header.
**Hashtags:** #OpenTelemetry #PlatformEngineering

**Target word count:** 200-400

## Draft

A cost spike with two independent high-cardinality attributes is
not one problem. It is two — staged.

This is why the v0.79 proposer schema is a discriminated union, not
a single rollout shape. `Kind: "rollout"` means one config change
against an existing target config: clean, atomic, fits the v0.58
path. `Kind: "plan"` means N sequenced rollouts with inline config
snippets, each materialized server-side at create time.

The decision framework lives in the system prompt as two bullet
lists. Use a single rollout when one config change is sufficient
and a target config already exists in storage. Use a plan when
progressive staged changes reduce regression risk, or when multiple
related changes benefit from observation between them.

For a two-attribute spike, "observe between them" is the load-
bearing phrase. Drop `container.id` at step 0. Watch the cost
graph. If the spike is already gone, abort — the second drop was
unnecessary and would have removed a useful attribute. If the
spike persists, step 1 drops `k8s.pod.uid`. Two steps, one
approval, full automatic rollback on failure via the v0.72
backwards walk.

The dispatch is a switch on the `Kind` string in
`internal/proposer/bridge.go`. Empty `Kind` decodes as rollout so
older model outputs degrade gracefully. Unknown `Kind` logs and
skips so a future schema drift does not halt the daemon. Plan-kind
calls `CreatePlan`, which materializes the steps as `services.
RolloutInput` entries with `proposed_by: ai` on every step,
reasoning + evidence on step 0 only, plan grouping linking the
sequence.

What the operator sees: one Plan detail page in the v0.75 UI, a
plan badge that distinguishes it from a single rollout, the steps
in order with their individual states, one approve button at step
0, automatic sequencing once approved.

Single-step problems get single rollouts. Sequenced problems get
sequenced plans. The proposer picks; the operator approves; the
engine runs.

Repo at the v0.84.0 tag. The plan-kind path is `Move 3` in the
internal arc notes — closed end to end across v0.69 through v0.79.

#OpenTelemetry #PlatformEngineering

## Visual asset spec

- **Filename:** `assets/post-3-plan-detail-two-step.png`
- **Surface:** The Plan detail page at `/plans/:id` on the live
  deployment at the v0.84.0 tag, viewing a real two-step plan
  created from the "Two attrs → plan" playground starter run
  promoted into the application store (or seeded via
  `squadron-demo-seed`). Step 0 must be in `succeeded`; step 1 in
  `succeeded` or `in_progress` is fine — the sequencing
  relationship is the point, not a final clean state.
- **What must be visible:** the plan badge in the header (v0.75);
  both steps in order with their state pills; the approval-gate
  indicator on step 0 only (steps 1+ inherit via plan grouping);
  the `proposed_by: ai` indicator on step 0; the timestamps showing
  step 1 started after step 0 succeeded.
- **Annotations:** one small arrow connecting step 0's success
  timestamp to step 1's start timestamp, with the caption
  "step 1 starts after step 0 reaches succeeded — v0.70 sequencing"
  added in post-processing, not baked into the page. This is the
  one place a callout earns its keep because the sequencing
  relationship is what the post is about.
- **Crop:** include the route in the browser address bar.

## Anti-pattern guard

Resists **the metrics post that's actually a vanity post** from
linkedin-rollout.md "Anti-patterns to avoid". The post does not
claim "we sequenced N plans" or "the engine handled X rollouts."
The numbers in the post are about mechanism — two bullet lists in
the prompt, one switch in the bridge, one approval gate at step 0,
one backwards walk on failure. The reader takes away how the
sequencing works, not how many times it has happened. That's the
right metric to lead with at this phase, because the audience
discounts adoption numbers and respects mechanism walkthroughs.
