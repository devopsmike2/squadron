# Post 6: Every AI decision is in the audit timeline

**Pillar:** Universal insight
**Tag at publish:** v0.81.4
**Visual evidence:** A screenshot of the Recent Events list on the
`/timeline` page on the live deployment at the v0.81.4 tag,
showing a humanized chain of three consecutive events from a real
proposer run — `plan.created` by the AI, `proposal.created`,
`rollout.approved` by the operator. The humanized titles are
visible; the raw event_type strings ("plan.created" etc.) are not.
**Hashtags:** #OpenTelemetry #ObservabilityControlPlane
**Target word count:** 200-400

## Draft

Audit-complete is not a checkbox feature. It is the architecture
move that lets a regulated-industry security team finish their
review in weeks instead of quarters. Every AI decision Squadron
makes — the proposer's plan creation, the proposal write, the
operator's approval, the engine's stage applies, the backwards
rollback walk on failure — lands as one event in one timeline,
in order, with the actor on each.

But "every event lands" is the easy half. The hard half is making
the timeline readable when an SRE is staring at it during an
incident.

v0.76 shipped the `AuditTimeline` humanizer for the rollout
drawer's "Show history" view. Raw event types — `plan.created`,
`rollout.stage_applied`, `proposal.created` — get rendered as
sentences. v0.81.4 ported the humanizer server-side for the
`/timeline` Recent Events list, which had been rendering the raw
strings directly. Same humanization rule, applied where the
operator actually reads the events first.

The chain a real proposer run produces, in order:

1. `plan.created` by `ai` — the v0.79 plan-kind dispatch fired
   and materialized a two-step plan.
2. `proposal.created` by `ai` — the underlying proposal record
   for step 0 was written.
3. `rollout.approved` by a human operator — the v0.61 two-person
   rule enforced. The AI proposed; a human approved.

v0.81.4 also fixed a small but real wire issue: the
`plan.created` audit event's actor field was being set to
`system` instead of `ai` in one code path. The Timeline read like
the platform itself had created the plan. Fixed in the same
release that ported the humanizer — both gaps were surfaced by
the same E2E sweep.

That's the discipline. The audit substrate exists from v0.51. The
humanization makes it readable. The actor wire fix makes the
chain attribute correctly. Each release moves the timeline closer
to "you can hand this to your auditor."

Repo at the v0.81.4 tag. The timeline lives at `/timeline` on
any Squadron deployment.

#OpenTelemetry #ObservabilityControlPlane

## Visual asset spec

- **Filename:** `assets/post-6-timeline-humanized-chain.png`
- **Surface:** The Recent Events list at the bottom of the
  `/timeline` page on the live deployment at the v0.81.4 tag,
  after a recent dogfood proposer run so the three relevant rows
  are at the top of the list.
- **What must be visible in the crop:** at least three
  consecutive rows in this order — `plan.created` by `ai`,
  `proposal.created` by `ai`, `rollout.approved` by the
  operator's username. Each row shows the humanized title (not
  the raw event_type string), the actor, and the timestamp.
- **Annotations:** one small marker on the `rollout.approved`
  row's Actor column with the caption "human approver — v0.61
  two-person rule enforced", added in post-processing. The
  humanized titles are the point; the actor on the third row is
  the auditability story made concrete.
- **Crop:** include the page header so the reader knows they're
  looking at `/timeline`, and include the route in the browser
  address bar.

## Anti-pattern guard

Resists **the metrics post that's actually a vanity post** from
linkedin-rollout.md "Anti-patterns to avoid". The post does not
claim "Squadron logged N audit events this quarter." It names
three specific events the operator sees in one chain, the v0.76
client-side humanizer + v0.81.4 server-side port, and the wire
fix that made the actor attribute correctly. The takeaway is how
the chain reads, not how long it has been running. That's the
right framing because the audience discounts adoption counters
and respects "what does this look like at 3am during an incident"
walkthroughs.
