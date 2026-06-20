# Post 13: Audit-complete by default is a feature, not a check-box

**Pillar:** Universal insight
**Tag at publish:** v0.84.0
**Visual evidence:** A screenshot of the Recent Events list on
the `/timeline` page on the live deployment at the v0.84.0 tag,
showing a humanized chain that mixes a JARVIS proposer flow
(`plan.created` by `ai` → `proposal.created` by `ai` →
`rollout.approved` by an operator) with a near-by discovery
event (`discovery.aws.connection_created` or
`discovery.aws.recommendations_generated`) so the reader sees
the same humanizer treating both decision sources identically.
**Hashtags:** #OpenTelemetry #ObservabilityControlPlane
**Target word count:** 200-400

## Draft

In most regulated industries "audit log" is the field a vendor
mentions on the discovery call and the compliance reviewer
finds gaps in nine months later. The gap is rarely that events
were missing. The gap is that the events were ordered by
unrelated wall-clock columns, attributed to a `system` actor
instead of the human or model that actually decided, and named
by event-type strings that meant something only to the engineer
who wrote the schema.

Squadron's discipline is that the audit timeline is the surface
the operator looks at during an incident and the surface the
auditor looks at during a review. The same one. If the
operator cannot read it at 3am the auditor will not be able to
read it at quarter end.

Two recent changes are the receipts.

v0.76 shipped the `AuditTimeline` humanizer for the rollout
drawer. `plan.created`, `rollout.stage_applied`,
`proposal.created`, `discovery.aws.scan_completed` —
raw event types stop appearing in the UI. The humanizer renders
each as a sentence. v0.81.4 ported the humanizer server-side
for the `/timeline` Recent Events list, which had been showing
the raw strings. The compliance story is the same on both
surfaces because the humanization is the same.

v0.81.4 also fixed a small but load-bearing wire issue: the
`plan.created` audit event was being attributed to `system` in
one code path when the AI proposer was the actual actor. Read
from the auditor's chair, that one row made the chain look like
"the platform created the plan" instead of "the AI proposed and
the operator approved." Fixed in the same release that ported
the humanizer; both surfaced in the same E2E sweep.

The v0.85.0 discovery slice extends the same audit category set
— `discovery.aws.connection_created`,
`discovery.aws.scan_completed`,
`discovery.aws.recommendations_generated`,
`discovery.aws.recommendation_marked_applied` — through the
same humanizer. The auditor reading the discovery slice never
has to learn a second mental model.

That is the difference between audit-complete-by-default and
audit-as-checkbox. The architecture commits to it. The wire
fixes prove the commitment is recent and ongoing.

Repo at the v0.84.0 tag for the humanizer; v0.85.0 for the
discovery event categories.

#OpenTelemetry #ObservabilityControlPlane

## Visual asset spec

- **Filename:** `assets/post-13-timeline-humanized-mixed-sources.png`
- **Surface:** the Recent Events list at the bottom of the
  `/timeline` page on the live deployment at the v0.84.0 tag,
  after a dogfood session that ran (a) a JARVIS cost-spike
  proposer flow and (b) a /discovery/aws connect-and-scan flow
  back to back. The two flows interleave in the recent-events
  list — that is the point.
- **What must be visible in the crop:** at least one
  `plan.created` row attributed to `ai`, one
  `rollout.approved` row attributed to an operator account,
  and one `discovery.aws.*` row. Each row shows the humanized
  title (not the raw event_type string), the actor, and the
  timestamp. The humanized titles and the actor column are
  the load-bearing elements.
- **Annotations:** one small marker on the `plan.created` row's
  Actor column with the caption "v0.81.4 actor wire fix —
  attributed to ai, not system", added in post-processing. No
  annotation on the discovery row — the row itself, sitting in
  the same humanized list as the JARVIS rows, is the
  unification story.
- **Crop:** include the page header so the reader knows they
  are looking at `/timeline`. The browser address bar with the
  route is part of the frame.

## Anti-pattern guard

Resists **the metrics post that's actually a vanity post** from
linkedin-rollout.md "Anti-patterns to avoid". The post does
not claim "Squadron has logged N million audit events" or
"audit coverage hit 100%." It names specific event types, the
v0.81.4 wire fix that made the actor attribute correctly, the
discovery audit categories that extend the same humanizer, and
the one operational property that matters — the operator and
the auditor read the same surface. The takeaway is the shape
of the trail, not the size of the log file.
