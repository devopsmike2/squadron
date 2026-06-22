# Proposer learning loop — operator runbook

This is the operator-facing runbook for the v0.89.17 + v0.89.18
feedback loop ([#531](./proposals/531-proposer-learns-from-accepted-rejected.md)
slice 1). It explains what the loop does, how to control it,
how to read the audit signal, and a worked example. If you've
already enabled or disabled the policy on a group and want to
verify what the proposer is now seeing, jump to §"Reading the
audit timeline".

If you've not yet wired up the AI proposer at all, start with
[ai-features.md](./ai-features.md). The feedback loop only
fires on cost-spike proposals; the discovery proposer stays
on the v0.79 prompt shape until a follow-on slice.

For a first test against a group that already has a small
verdict history (one or two prior approvals + rejections),
the walkthrough takes about 10 minutes. For a first-time
operator on a fresh deployment, budget 15 minutes to plant a
seed history before the loop has anything to cite.

## What we're building

A simple proposition: today's cost-spike proposer makes the
same mistakes twice on the same group. The fix isn't fine-
tuning, it isn't an embedding store, and it isn't RAG —
it's the most boring possible mechanism. Before each new
proposal, fetch the operator's recent verdicts (approvals and
rejections) on past AI rollouts for the same group, attach
them to the prompt as in-context few-shot examples, and let
the model do the rest.

Concretely:

1. **One new storage query.** `ListAIVerdictsForGroup(groupID,
   since, limit)` returns recent AI-originated rollouts that
   the operator either approved or rejected. The query is
   indexed on `(group_id, proposed_by,
   COALESCE(approved_at, rejected_at) DESC)`.
2. **One new bridge step.** Before dispatching the user
   message, `Bridge.assembleVerdicts` runs the query, picks up
   to N=4 examples per the selection policy below, redacts
   them through the existing `redact.go`, and threads them
   into the prompt builder.
3. **One new prompt block.** When verdicts are present, the
   user message grows a "Prior verdicts for this group"
   section per §6 of the locked spec. When the verdict list
   is empty (cold start, opt-out, or no rows in the recency
   window), the prompt is byte-for-byte identical to
   v0.79's.
4. **One per-group toggle.** `Group.LearnFromVerdicts`
   controls the feedback loop per group. Defaults to ON
   (matches the storage column default of 1). Flippable via
   the Groups UI, the API, or settings JSON.
5. **One audit field.** The `proposal.created` payload gains
   `verdict_examples_used: []string{...}` — the rollout IDs
   actually shown to the model. Empty array (not omitted) on
   cold start so SIEM consumers can filter cold-start cases
   without joining tables.

That's it. No new tables. No new endpoints. No system-prompt
edits. The system prompt at the top of `proposer_prompt.go`
is identical to v0.79; only the user message changes when
verdicts are present.

## What this is good for

- **Operator preferences propagate without you typing them
  twice.** The operator who rejects "100% rollout in one
  stage, no canary" with a note "always canary at 10% first"
  doesn't have to repeat that next time the same group spikes.
  The proposer sees the rejection + the note and shapes the
  next proposal accordingly.
- **Group-specific shape signals stay group-specific.** A
  group running an experimental workload where 100% rollouts
  are acceptable doesn't get punished for what the production
  group rejected — examples are pulled from the same group
  only.
- **Honest history outranks the system prompt.** Operator
  verdicts are denser preference signal than any prompt edit
  could be, and they accumulate naturally as the team uses
  Squadron.

## What this is NOT

Read this list carefully. The first three are features, not
limitations.

- **Squadron is not fine-tuning, distilling, or RAG-ing.**
  Prompt-only. The same model serves every cost-spike call;
  the only thing that changes is the user message has more
  context. If you want the cost of fine-tuning, the cost of
  embedding storage, or the cost of an inference-time vector
  retrieval, you have to opt in to a later slice. Slice 1
  ships the cheapest possible mechanism that closes the
  feedback loop, on purpose.
- **Cross-group learning is out.** A rejection on `web-prod`
  does NOT influence proposals on `web-staging`. Same-group
  only is the slice-1 contract. Cross-group is a slice-2
  question; the spec's open Q3 covers it.
- **Per-rollout suppression is out.** There is no "exclude
  this rollout from learning" checkbox. The flag is per-group.
  If an individual rollout's reasoning contains material you
  don't want fed back into the proposer, your options today
  are: flip `Group.LearnFromVerdicts=false` for the whole
  group, or hard-delete the offending rollout row. Per-row
  suppression is the spec's open Q3 and a slice-2 candidate.
- **Discovery proposer is NOT in scope.** Slice 1 is cost-
  spike proposer only. The discovery proposer (the one that
  recommends OTel instrumentation against an AWS scan) stays
  on the v0.79 prompt shape. Wiring the same loop into
  discovery is a follow-on slice with its own design
  questions (what counts as an "accepted" discovery
  recommendation when the operator hasn't merged the IaC PR
  yet?).
- **Squadron does NOT edit the system prompt.** The model's
  understanding of cost spikes, rollout shapes, and the JSON
  output contract lives in the system prompt at the top of
  `internal/ai/proposer_prompt.go`. That file is unchanged.
  All the new context lands in the user message. If you've
  customized the system prompt in your fork, this release
  doesn't touch it.
- **Inline config snippets from past rollouts are NEVER
  shipped to the model.** Only the reasoning + the
  approval/rejection notes flow through. Configs are where
  the sensitive material lives; the spec's §7 deliberately
  excludes them. Squadron's storage layer can read those
  fields, but `assembleVerdicts` doesn't put them on the wire.

## Prerequisites

- A Squadron deployment on v0.89.18 or later. v0.89.17 has
  the storage + bridge + prompt + audit pieces, but the
  per-group toggle wasn't yet exposed through the API or UI;
  v0.89.18 closed that gap. Earlier versions silently ignore
  the column.
- The cost-spike proposer enabled. If you've never seen a
  `proposal.created` event in your audit timeline, the
  proposer isn't wired up — see [ai-features.md](./ai-features.md)
  for the cost-spike setup.
- At least one AI-originated rollout in the target group's
  history, either approved or rejected, within the last 30
  days. Without a seed history the loop has nothing to cite
  and the prompt falls back to v0.79's shape (cold start).
  This is by design — the cold-start parity test exists
  precisely to guarantee the loop is a no-op until there's
  signal to share.
- An auth token with `groups:write` if you intend to flip
  the toggle through the API. UI users authenticated to the
  Groups page already have the scope.

## Step 1 — Decide the per-group policy

The flag is per-group, not per-deployment. The decision is
"should the proposer reading verdicts from this group's
history help or hurt the next proposal?"

Default to **enabled** for groups where the operator team is
small and consistent, where rejection notes carry team
convention ("we always canary at 10% first"), and where the
operator wants the proposer's shape to track their evolving
preferences.

Default to **disabled** for groups where the proposer's
verdict history contains material the team doesn't want fed
back into the model. The most common case is: a group whose
prior rejections carry operator notes containing PII,
customer names, or internal context that shouldn't end up in
a cloud-API request. Redaction handles secrets via
`redact.go`, but the operator is the source of truth for
"what's sensitive in our notes."

Default to **disabled** during a transition period if the
operator team has just changed convention. The model sees
old conventions as positive signal until the new ones
accumulate enough verdicts to outvote them.

## Step 2 — Flip the toggle

Three paths, all of which write the same `LearnFromVerdicts`
column on the same row:

1. **Groups page, per-row chip.** Each group row carries a
   `Brain` chip rendering `Learning` (green) or `Off`
   (muted). One click flips the value and refetches the
   list. Fastest path for a single group.
2. **Groups page, create form.** When creating a new group,
   the "Learn from prior accepted/rejected AI proposals"
   toggle is in the create drawer. Defaults ON for the
   reasons in §1; untick before submitting to opt the new
   group out from creation.
3. **API.** `PUT /api/v1/groups/:id` with body
   `{"learn_from_verdicts": false}` flips the policy off.
   Partial-update semantics: nil leaves untouched, explicit
   `false` opts out, explicit `true` opts in. The handler
   enforces the same auth as every other group update.

`squadronctl groups` does not yet have a subcommand for the
flag. If you need to flip the policy from a script, hit the
API directly — `curl -X PUT -d '{"learn_from_verdicts":false}'
http://squadron/api/v1/groups/<id>` is the one-liner. A
CLI subcommand is a candidate follow-on once we see usage.

## Step 3 — Understand the selection policy

When the policy is enabled and the group has verdict history,
`Bridge.assembleVerdicts` picks examples per §5 of the spec.
The full constants live in `internal/proposer/bridge.go`:

| Constant | Value | Why |
| --- | --- | --- |
| `verdictsMaxExamples` | 4 | §5 cap on total examples per prompt |
| `verdictsMaxPerBucket` | 2 | Independent caps on approved + rejected |
| `verdictsWindow` | 30 × 24h | §5 recency window, fixed in slice 1 |
| Reasoning summary cap | 240 chars | Inherited from the existing `summarize` helper |

Selection mechanics:

- The SQL fetches rows ordered by
  `COALESCE(approved_at, rejected_at) DESC` — newest verdict
  first across both states.
- The bridge walks the result, fills the rejected bucket up
  to 2 entries, fills the approved bucket up to 2 entries,
  and stops when both are full or the row list runs out.
- **Rejections render first in the prompt.** The §5 design
  says rejections are denser signal — "don't do this again"
  beats "this was fine" — so the prompt puts them at the top
  where the model reads them first.
- Each reasoning string is truncated to 240 characters with
  a trailing ellipsis (per the existing `summarize` helper),
  then passed through `ai.RedactSecrets` which redacts
  anything matching the `redact.go` regex set
  (passwords, API keys, AWS access tokens, etc.).
- Approval/rejection notes are passed through the same
  redaction pass.

If the group has 50 prior rejections and 50 prior approvals,
the model sees the 2 newest of each. If the group has 1
rejection and 0 approvals, the model sees just the 1
rejection.

## Step 4 — What the prompt looks like

The user message grows a "Prior verdicts" block immediately
before the final "Return your proposal" line. Verbatim:

```text
Prior verdicts for this group (operator decisions on past AI proposals):

[REJECTED] rollout_id=rlt_7q12
  reasoning: dropped k8s.pod.uid plus k8s.namespace in one step.
  rejecter_notes: "too aggressive — split into a plan, drop one attr per step"

[REJECTED] rollout_id=rlt_6m08
  reasoning: 100% rollout in a single stage, no canary.
  rejecter_notes: "always canary at 10% first"

[APPROVED] rollout_id=rlt_8ax9
  reasoning: container.id was driving 60% of the spike; canary 10% / 600s.
  approver_notes: "good plan, ship it"

Use these as preference signal. Match the shape of approved
proposals; avoid the shape of rejected ones. Do NOT cite these
rollout_ids in your evidence — they're operator history, not
evidence for this spike.
```

A few subtleties worth knowing:

- **Rejections precede approvals.** This is intentional per
  §5 — denser signal first. Don't try to "fix" the ordering;
  the cold-start parity test pins it.
- **Empty notes are omitted.** If the operator approved or
  rejected without typing a note, the `approver_notes` /
  `rejecter_notes` line is absent. Cold-start parity holds
  for empty-notes-only verdict histories too.
- **The `Do NOT cite these rollout_ids in your evidence` line
  is load-bearing.** Without it, the model sometimes
  hallucinates evidence references like "see rlt_6m08 for
  why" — which is real signal pollution because the audit
  consumer expects evidence_refs to point at telemetry, not
  prior rollouts. The instruction keeps the buckets separate.

When the verdict list is empty (cold start, opt-out, or
nothing in the 30-day window), the entire block — header,
examples, and instruction line — is omitted. The prompt is
byte-for-byte identical to v0.79's. This is what the
`TestProposerVerdicts_ColdStartParity` acceptance test
exists to enforce.

## Step 5 — Reading the audit timeline

The `proposal.created` audit event grows one new field:

- **`verdict_examples_used`** — `[]string` of rollout IDs
  shown to the model. Empty array (not omitted) on cold
  start so SIEM consumers can filter cold-start cases with
  `verdict_examples_used:[]`.

Example payload (with examples cited):

```json
{
  "event_type": "proposal.created",
  "target_id": "rlt_new",
  "actor": "ai",
  "payload": {
    "group_id": "web-prod",
    "spike_attribute": "container.id",
    "reasoning_summary": "drop container.id, canary 10% / 600s",
    "verdict_examples_used": ["rlt_6m08", "rlt_8ax9"],
    "...": "..."
  }
}
```

Example payload (cold start):

```json
{
  "event_type": "proposal.created",
  "target_id": "rlt_new",
  "actor": "ai",
  "payload": {
    "group_id": "web-staging",
    "spike_attribute": "k8s.pod.uid",
    "verdict_examples_used": [],
    "...": "..."
  }
}
```

The field is additive. If you've written external log parsers
or SIEM rules against the v0.79 `proposal.created` shape, they
don't need to change — the new field is appended and the
existing top-level keys are unchanged.

The Timeline page does not currently render a special
humanizer for the field; it shows up as a regular payload
key under the event's expanded view. A humanizer that surfaces
"This proposal cited 2 prior verdicts (1 rejection, 1
approval)" is a candidate follow-on.

## Step 6 — Worked example

A maintenance-style cost spike on `web-prod`. The group has
two prior AI-originated rollouts:

- `rlt_6m08` — REJECTED 5 days ago. Reasoning: "100% rollout
  in a single stage, no canary." Operator note: "always
  canary at 10% first."
- `rlt_8ax9` — APPROVED 3 days ago. Reasoning: "container.id
  was driving 60% of the spike; canary 10% / 600s." Operator
  note: "good plan, ship it."

A new spike fires on `web-prod`. Walking the call:

1. **Bridge.handleSpike runs.** Group lookup → `web-prod`
   exists, `LearnFromVerdicts=true`, so the verdict path is
   taken.
2. **assembleVerdicts pulls history.** SQL returns both
   rows; bridge fills the rejected bucket (1 entry) and
   approved bucket (1 entry), neither at the cap.
3. **Each reasoning + notes pass through `ai.RedactSecrets`.**
   Nothing redacted in this example (no secrets in the text).
4. **VerdictExample list returns** with the rejection first,
   approval second. RolloutIDs: `["rlt_6m08", "rlt_8ax9"]`.
5. **buildProposeUserMessage renders the §6 block.** The
   model receives the spike context plus the two examples
   plus the instruction to match approved shapes and avoid
   rejected ones.
6. **Model returns a proposal.** Shape: canary at 10% for
   600s, then 100%, with reasoning "drop container.id —
   same root cause as rlt_8ax9; staging the rollout
   matches the team convention from rlt_6m08."
7. **Bridge emits `proposal.created` audit.** Payload
   includes `verdict_examples_used: ["rlt_6m08", "rlt_8ax9"]`.

The operator sees a proposal that already obeys the team
convention — without anyone having to retype "always canary
at 10%" or tell the model about it directly. The next time
this same group spikes, both this new rollout's verdict + the
two existing ones become candidates for the next call. The
loop closes.

## Step 7 — Disable, re-enable, and verify

If you decide a group should opt out:

1. Flip the `LearnFromVerdicts` chip on the Groups row to
   `Off`. The chip turns muted and the next proposal for
   this group will fall back to the v0.79 prompt shape.
2. Trigger a spike (or wait for a real one) on that group.
3. Inspect the next `proposal.created` event. Confirm
   `verdict_examples_used: []` is present and the array is
   empty. That's the proof the opt-out fired.

If you decide to re-enable, the row's chip flips back to
`Learning` and the next proposal pulls verdicts again.
There is no warmup period and no cached state — the
selection runs fresh on every call.

## Per-rollout suppression

The group flag is a coarse lever — it disables the whole
loop for an entire fleet. v0.89.26 (#642, slice 2 of #531
§10 Q3) adds a finer-grained per-rollout lever:
`Rollout.ExcludeFromLearning`. Use it when ONE AI proposal's
reasoning or rejection note contains material that should
not flow into the next proposal — a customer name typed
into an approval comment, an internal incident identifier,
a PII fragment — but the rest of the group's history is
still safe to learn from.

The two filters compose: the group-level
`LearnFromVerdicts=false` short-circuits BEFORE the
per-rollout filter, so flipping `ExcludeFromLearning` on
a single row is irrelevant when the group is already
opted out. Use the group flag for "this whole fleet's
history is off limits"; use the per-rollout flag for
"this one specific rollout's notes are off limits."

**UI.** Open the rollout drawer. AI-originated rollouts
(those with `proposed_by === "ai"`) show a green
`Included in learning` chip next to the AI reasoning
panel. Click it once — it flips to a muted
`Excluded from learning` chip and the next AI proposal
for the same group will not cite this rollout in its
few-shot block. Click again to re-include it. The chip
does not appear on operator-originated rollouts; the
bridge's `proposed_by='ai'` filter already makes the
flag a no-op there, so the surface stays focused.

**API.** `POST /api/v1/rollouts/:id/exclude-from-learning`
takes `{"excluded": true|false, "reason": "optional"}`.
The `reason` is omitted by the UI but available to
scripted callers (squadronctl, automation) for forensic
context — when non-empty it lands verbatim on the
audit payload's `reason` field. Auth: `rollouts:write`
scope (same as approve/reject).

**Audit.** Each toggle emits one
`rollout.excluded_from_learning` row. Payload contract:
`{rollout_id, previous_state, new_state, reason?}`. SIEM
consumers can fan out on the row's `action` verb —
`"exclude_from_learning"` when `new_state=true`,
`"include_in_learning"` when `new_state=false` — without
cracking the payload.

**Spec reference.** This closes
[`docs/proposals/531-proposer-learns-from-accepted-rejected.md`
§10 Q3](proposals/531-proposer-learns-from-accepted-rejected.md).
Slice 1's `verdict_examples_used` wire shape on
`proposal.created` is preserved — the new filter changes
which IDs land in the array, not the array's shape, so
external SIEM rules parsing slice 1's contract keep
working unchanged.

## Privacy and operator-note hygiene

The §7 design picks per-group flag deliberately over per-
rollout. The argument: per-rollout is more surface for the
operator to manage, and most of the value comes from being
able to say "this group's history is sharable, this one's
isn't." If you have a group whose verdicts contain material
that shouldn't leave your VPC (PII, customer identifiers,
internal incident names), the right move is to flip
`LearnFromVerdicts=false` for that group and accept the
slightly cold-start-like behavior.

Redaction handles a specific set of secrets — passwords, API
keys, AWS tokens, and other well-known shapes — but it is
NOT a general PII scrubber and you should not trust it as
one. The operator owns the policy decision.

Inline config snippets from past rollouts are excluded from
the prompt regardless of the flag. This is a separate
exclusion at the `assembleVerdicts` layer; even with the
loop enabled, the model never sees the contents of a past
rollout's config body. See
`TestProposerVerdicts_AssembleVerdicts_InlineSnippetsExcluded`
for the acceptance test.

## Roadmap touchpoints

The slice-1 trade-offs most likely to shift in later slices:

- **Per-rollout suppression** (spec §10 Q3). A
  `Rollout.ExcludeFromLearning bool` column on the rollouts
  table, set via the rollout-detail UI's "Don't use as
  example" toggle. Operator gets fine-grained control
  without sacrificing the rest of the group's history.
- **Cross-group learning with namespace mode** (spec §5
  refinement). A namespace-style "groups in the same labels
  set share verdicts" rule, gated by a deployment flag.
  Helps teams with N near-identical groups (e.g. one per
  region) where the convention is consistent across the
  set.
- **Configurable 30-day window** (spec §10 Q5). Replace
  the constant with a per-deployment setting on the AI
  service block. Useful for teams with slow-changing
  conventions who want a longer memory.
- **Discovery proposer integration** — **SHIPPED in v0.89.28**.
  See [discovery-proposer-learning.md](./discovery-proposer-learning.md)
  for the operator runbook and
  [#643](./proposals/643-discovery-proposer-verdict-learning.md)
  for the locked design. The "accepted" question is answered:
  `recommendation.pr_merged` within 30 days of `pr_opened`
  on the same connection × account × region × kind is the
  positive signal. Rejected signal on the discovery side is
  now locked as slice 2 work in
  [#531 slice 2](./proposals/531-proposer-learning-slice2.md).
- **#531 slice 2 — unified verdict learning across surfaces**
  (design DOC LOCKED in v0.89.33,
  [./proposals/531-proposer-learning-slice2.md](./proposals/531-proposer-learning-slice2.md)).
  Adds negative signal on the discovery side
  (`recommendation.pr_closed_not_merged` audit event plus an
  operator-set "Don't propose this again" exclusion table),
  a two-tier hot/cold recency window (7d + 30d), a kind
  diversity cap (max 2 examples per kind within the N=4
  total), and a shared `verdictsel` + `verdictprompt`
  selection and prompt-rendering layer that both proposer
  surfaces call into. Storage stays surface-local; the shared
  layer is functional and unit-tested in isolation.

These slice candidates are tracked under
[#531](./proposals/531-proposer-learns-from-accepted-rejected.md)
and [#531 slice 2](./proposals/531-proposer-learning-slice2.md).
None ship in v0.89.18; everything in this runbook
describes slice 1 behavior you can rely on today.

## Cross-references

- [#531 proposal](./proposals/531-proposer-learns-from-accepted-rejected.md) —
  the locked slice-1 spec this implementation promotes.
  Read this if you want the exact storage shape, the §5
  selection policy reasoning, or the §11 acceptance tests
  by name.
- [AI features overview](./ai-features.md) — the cost-spike
  proposer's broader context (where it fires, what the
  system prompt teaches, how the JSON contract works).
- [Multi-step plans — design](./multi-step-plans-design.md) —
  the proposer can now also emit `kind=plan` proposals; if
  the proposer cites a verdict on a plan-kind rejection,
  this is the doc that explains the plan shape.
- [Audit log](./audit-log.md) — the `proposal.created`
  event-type docs, including the additive
  `verdict_examples_used` payload field this release adds.
- [Connect IaC repo first-time setup](./discovery-iac-first-time-setup.md) —
  the parallel example of a discovery-side feedback loop
  that doesn't yet feed verdicts; useful background for
  where the discovery proposer integration is heading.
