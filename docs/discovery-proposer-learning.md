# Discovery proposer feedback loop — operator runbook

This is the operator-facing runbook for the v0.89.28 implementation
of [#643 slice 1](./proposals/643-discovery-proposer-verdict-learning.md):
the discovery proposer now learns from accepted recommendations and
stops re-proposing them on the next scan. It explains what the loop
does, the per-connection flag, what the prompt block looks like, how
to read the audit signal, and the worked example end-to-end.

If you're not yet running v0.89.23's webhook listener, start there
first: [webhook-listener.md](./webhook-listener.md). This runbook
assumes the webhook is live and recording
`recommendation.pr_merged` events — that's the signal this loop
reads.

For a first test against a sandbox repo with one accepted PR, the
walkthrough takes about 10 minutes. For a production deployment
with multi-account scans across several connections, budget 20
minutes to verify the scope tuple lines up across the full path.

## What we're building

The proposition is the same as the cost-spike side
([proposer-learning-loop.md](./proposer-learning-loop.md)): close
a feedback loop without a fine-tune, an embedding store, or a
RAG layer. Just look at what the operator already accepted, list
it in the next prompt, and tell the model not to re-propose it.

The new wire is:

1. **The webhook receiver** (v0.89.23) records every
   Squadron-opened PR that merges as a
   `recommendation.pr_merged` audit event. As of v0.89.28 the
   payload also carries `account_id` and `region`, parsed from
   the branch name's path segments.
2. **A new bridge method**
   ([`internal/proposer/bridge.go::assembleAcceptedRecommendations`](../internal/proposer/bridge.go))
   queries the audit table for `pr_merged` events that match
   the next scan's scope tuple (connection × account × region),
   filters by the 30-day recency window, caps at N=4 newest
   first, and returns the examples.
3. **A new prompt block** in `buildDiscoveryUserMessage` lists
   those examples in the user message before the model
   generates recommendations. The instruction line tells the
   model to use them as preference signal and NOT to re-propose
   the same kind against the same resource.
4. **A new audit event** `discovery_proposal.created` records
   which prior PRs informed each proposer call, so SIEM
   consumers can correlate.
5. **A per-connection flag**
   `IaCConnection.LearnFromAcceptedRecommendations`
   (default true) opts a connection in or out of the loop.

No new tables. No new endpoints. No system-prompt edits. Cold
start (no accepted PRs in the scope, or the connection flag is
off, or the recency window is empty) renders a prompt
byte-for-byte identical to v0.85.

## What this is good for

- A team that uses Squadron's discovery scans across a small
  number of AWS accounts and regions and wants the proposer's
  recommendations to stop repeating themselves between scans.
- An auditor who needs to correlate "Squadron suggested X,
  operator accepted X" as a queryable audit fact, not a manual
  cross-check across the Recommendations tab and the GitHub PR
  list.
- A platform team building automation on top of Squadron that
  wants `discovery_proposal.created` as the trigger event for
  downstream notifications, dashboards, or MTTR metrics.

## What this is NOT

Read this list carefully. The first three are features, not
limitations.

- **No cross-account learning.** A merged recommendation in
  account A does not surface as positive signal when scanning
  account B. Same-account-strict for slice 1 per
  [#643 §2](./proposals/643-discovery-proposer-verdict-learning.md).
- **No cross-region learning.** Even within the same account, a
  merged rds-pi-em recommendation in us-east-1 does not surface
  when scanning us-west-2. Same-region-strict for slice 1. The
  reasoning is intentional: each region is its own compliance
  surface, and operators routinely accept a recommendation in
  one region while rejecting it in another.
- **No cross-kind learning.** A merged eks-observability-addon
  PR does not influence rds-pi-em recommendations. Same-kind-
  strict for slice 1.
- **No rejected signal.** Slice 1 reads PR merges only. A PR
  closed without merge does not count as a rejection; the
  proposer continues to propose the same kind. The rejected-
  signal definition is an open question deferred to slice 2.
- **No per-recommendation suppression.** v0.89.26 shipped this
  for cost-spike rollouts (`Rollout.ExcludeFromLearning`). The
  discovery analog would need to store recommendations
  themselves, which slice 1 does not do — recommendations are
  computed on-demand from the scan + proposer call. Slice 2
  candidate.
- **No wizard UI for the per-connection flag.** Slice 1 ships
  the API path and the storage column; the wizard step to
  surface the flag during the connection setup is slice 2.
  Operators flip the flag via API for now (Step 2 below).
- **Squadron is still not the executor.** The loop reads
  `pr_merged` events that operators (or their CI) committed
  through normal GitHub workflows. Squadron's role is unchanged:
  open the PR, record the merge, learn from the acceptance.

## Prerequisites

- A Squadron deployment on v0.89.28 or later. Earlier versions
  have the audit event constant but not the storage query, the
  bridge method, or the prompt extension.
- An active IaC GitHub connection
  ([discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md)).
  The connection's `id`, the AWS account it scans, and the
  region are the scope tuple the bridge uses for matching.
- The webhook listener wired
  ([webhook-listener.md](./webhook-listener.md)) with
  `SQUADRON_GITHUB_WEBHOOK_SECRET` set and the GitHub repo
  webhook configured for `pull_request` events. Without this,
  no `recommendation.pr_merged` events get recorded and the
  loop has nothing to read.
- At least one Squadron-opened PR has merged on the connection
  in the last 30 days. PRs opened before v0.89.28 are written
  to branches in the legacy 4-segment shape
  (`squadron/rec/<kind>/<short_id>`); the parser handles that
  shape but the resulting audit event has empty `account_id`
  and `region`, so it does NOT match any scope tuple in the
  loop's filter. PRs opened from v0.89.28 forward use the 6-
  segment shape (`squadron/rec/<kind>/<account>/<region>/<id>`)
  and DO match.

## Step 1 — Decide the per-connection policy

The flag is per-connection, not per-deployment. Default to
**enabled** for connections where:

- The operator team that approves Squadron PRs is the same
  team that pulls insight from the next scan.
- The PRs Squadron opens are reviewed substantively (not
  rubber-stamped) — accepted PRs reflect deliberate operator
  judgment, which is the signal the loop reads.

Default to **disabled** for connections where:

- The PR queue is high-volume and triaged in bulk, so accepted
  PRs do not necessarily reflect a per-recommendation policy
  decision.
- The connection covers a shared multi-team repo where
  acceptance criteria vary per service, and feeding one team's
  acceptances into another team's scan would be noise.

The flag is reversible. Cold-start parity holds when the flag
is off — flipping it later has no historical effect, only
forward effect on the next scan.

## Step 2 — Flip the per-connection flag

Slice 1 ships the flag via API only. The wizard UI step is
slice 2.

To disable on an existing connection:

```sh
curl -X PATCH https://your-squadron-host/api/v1/iac/github/connections/<id> \
  -H "Authorization: Bearer $SQUADRON_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"learn_from_accepted_recommendations": false}'
```

To re-enable:

```sh
curl -X PATCH https://your-squadron-host/api/v1/iac/github/connections/<id> \
  -H "Authorization: Bearer $SQUADRON_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"learn_from_accepted_recommendations": true}'
```

The handler does partial-update semantics: sending only the
new flag preserves every other field (repo, default branch,
placement map, PAT). Use the same Bearer token your other
write paths use.

Confirm the flag landed by reading the connection back:

```sh
curl -H "Authorization: Bearer $SQUADRON_API_TOKEN" \
  https://your-squadron-host/api/v1/iac/github/connections/<id> | \
  jq .learn_from_accepted_recommendations
```

Expected output: `true` or `false`.

## Step 3 — Understand the selection policy

When the policy is enabled and the connection has scoped PR
merges, `Bridge.assembleAcceptedRecommendations` picks examples
per [#643 §5](./proposals/643-discovery-proposer-verdict-learning.md).
The mechanics:

| Constant | Value | Why |
| --- | --- | --- |
| `acceptedRecsWindow` | 30 × 24h | Recency window, fixed in slice 1 |
| Per-call cap | 4 | Matches the cost-spike side's N=4 |
| Scope tuple | connection × account × region | Per [#643 §2](./proposals/643-discovery-proposer-verdict-learning.md), no cross-scope leakage |

Selection mechanics:

- The SQL query filters `audit_events.event_type =
  'recommendation.pr_merged'` plus payload predicates on
  `connection_id`, `account_id`, and `region` (via SQLite's
  `json_extract`), ordered by timestamp DESC.
- Rows with empty `account_id` or `region` payload fields
  (legacy 4-segment branches) do not match the scope filter —
  the loop ignores them.
- The bridge returns up to 4 examples newest-first. No per-kind
  cap (unlike the cost-spike side, which splits N=4 across
  approved + rejected buckets).
- Each example's `RecommendationKind` is parsed from the
  branch name's first segment after `squadron/rec/`.

If the connection has 50 merged PRs in the scope, the model
sees the 4 newest. If it has 1, the model sees just the 1. If
it has 0 (cold start), the prompt is byte-for-byte identical
to v0.85.

## Step 4 — What the prompt looks like

The user message grows a block immediately before the final
"Return your recommendations" instruction. Verbatim:

```text
Recently accepted recommendations for this scope (operator merged a Squadron-opened PR):

[ACCEPTED] kind=rds-pi-em
  pr_url: https://github.com/acme/infra/pull/142
  branch: squadron/rec/rds-pi-em/123456789012/us-east-1/abc1234
  merged_at: 2026-06-15T14:32:00Z
  merged_by: alice

[ACCEPTED] kind=eks-observability-addon
  pr_url: https://github.com/acme/infra/pull/138
  branch: squadron/rec/eks-observability-addon/123456789012/us-east-1/def5678
  merged_at: 2026-06-12T09:11:00Z
  merged_by: bob

Use these as preference signal. Do NOT re-propose recommendations
of the same kind against the same resource that was already
accepted within the window above. The accepted snapshot may have
drifted — if a resource clearly NEEDS a fresh recommendation
(the previous PR was reverted, the resource's instrumented state
is missing again), propose it with a note in the reasoning
explaining the divergence.
```

A few subtleties worth knowing:

- **The "do not re-propose" instruction line is load-bearing.**
  Without it, the model sometimes interprets "accepted" as "do
  nothing on this scope ever again," which is wrong. The
  resource can drift — the operator's CI can roll back the
  change, the resource can be re-created, the recommendation
  can be valid again. The instruction tells the model: propose
  if you have fresh evidence, but cite the divergence.
- **Empty list → entire block omitted.** Cold-start parity
  holds — no block header, no examples, no instruction. The
  prompt matches v0.85 exactly. The
  `TestDiscoveryProposerLearning_ColdStartParity` acceptance
  test pins this.
- **Inline config snippets from past PRs are NEVER shipped to
  the model.** Slice 1's example shape is PR URL, branch,
  merged_by, kind — no diff contents. Configs are where
  sensitive material lives; we exclude them at the bridge
  layer the same way the cost-spike side does.

## Step 5 — Reading the audit timeline

The new `discovery_proposal.created` event fires every time
the discovery proposer is invoked through
`POST /api/v1/discovery/aws/connections/:id/recommendations`.
Payload:

```json
{
  "event_type": "discovery_proposal.created",
  "actor": "ai",
  "target_type": "iac_recommendation",
  "action": "create",
  "payload": {
    "scan_id": "<deterministic hash of scan inputs>",
    "connection_id": "<iac_connection.id, may be empty in slice 1 adapter path>",
    "account_id": "<aws account scanned>",
    "region": "<aws region>",
    "recommendation_count": <int>,
    "verdict_examples_used": [
      "https://github.com/acme/infra/pull/142",
      "https://github.com/acme/infra/pull/138"
    ]
  }
}
```

Field-by-field:

- **`actor`** is `ai` for events generated by a proposer call,
  matching the cost-spike side's `proposal.created` actor.
- **`scan_id`** is a deterministic hash of the scan's inputs
  (account + region + resource fingerprints). Two identical
  scans produce identical scan_ids so SIEM consumers can
  dedupe.
- **`connection_id`** in slice 1 may be empty when the
  recommendations route's per-call adapter walks all connections
  (one connection per repo today). Future slices that key the
  route by connection_id directly will populate this field.
- **`verdict_examples_used`** is `[]string` of PR URLs (NOT
  rollout IDs like the cost-spike side's `verdict_examples_used`
  on `proposal.created`). Different event types, different ID
  schemes per [#643 §11 Q5](./proposals/643-discovery-proposer-verdict-learning.md).
  Empty array (not omitted) on cold start so SIEM consumers
  can filter cold-start cases.

The Timeline page humanizer renders this event:

- Cold start (`verdict_examples_used` empty): **"Discovery
  recommendations generated"**.
- Cited 1+ PRs: **"Discovery recommendations generated
  (informed by N prior accepted PRs)"** — exact wording per
  the `handleIaCAuditEvent` humanizer at
  [`internal/api/handlers/timeline.go`](../internal/api/handlers/timeline.go).

If you've written external log parsers against pre-v0.89.28
audit shapes, no change is needed — `discovery_proposal.created`
is a new event type. Your existing rules for
`recommendation.pr_*` events are unchanged.

## Step 6 — Backward compatibility on branch names

The branch-name encoding extension is the trickiest piece of
slice 1 to get right. Squadron opens PRs from branches under
`squadron/rec/`. Two shapes exist:

- **Legacy (pre-v0.89.28):** `squadron/rec/<kind>/<short_id>`
  (4 segments). The parser still recognizes this shape and
  returns `kind` correctly. But `account_id` and `region` come
  back empty, so the resulting `recommendation.pr_merged` audit
  event has empty payload fields for those two, and the bridge
  filter does NOT match this event when looking up examples by
  scope tuple.
- **Current (v0.89.28+):**
  `squadron/rec/<kind>/<account_id>/<region>/<short_id>`
  (6 segments). The parser returns all three fields. The audit
  event payload carries them. The bridge filter matches.

What this means in practice:

- PRs Squadron opened before v0.89.28 stay in the audit log
  (the `pr_merged` event still fires on merge) but do NOT
  contribute to the discovery feedback loop. They are
  "uncorrelated history" from the loop's perspective.
- PRs Squadron opens from v0.89.28 forward DO contribute. The
  next scan after a v0.89.28+ PR merges will read it.
- Operators who hand-pushed branches matching `squadron/rec/`
  (rare but possible) get the same treatment based on segment
  count.
- The `parseRecommendationScopeFromBranch` helper is the
  single source of truth for this logic. It lives in
  [`internal/api/handlers/iac_github_webhook.go`](../internal/api/handlers/iac_github_webhook.go)
  and is shared between the webhook receiver and any future
  caller that needs to derive scope from a branch name.

There is no backfill path in slice 1. If you need pre-v0.89.28
PRs to feed the loop, the operator path is: re-open the same
recommendation through the Recommendations tab on the next
scan, which generates a fresh branch under the new shape, and
merge that one. The audit lineage will then carry the scope.

## Step 7 — Worked example

A platform team running Squadron against AWS account
`123456789012`, region `us-east-1`, connection `conn-acme-infra`
pointed at `github.com/acme/infra`.

1. **Day 0 — Initial scan.** Squadron scans account
   `123456789012/us-east-1` and surfaces 15 uninstrumented
   resources across six categories. The proposer batches by
   category. The first PR Squadron opens is for the rds-pi-em
   recommendation, against the mysql instance.
2. **Day 0 — Branch + PR.** Squadron creates branch
   `squadron/rec/rds-pi-em/123456789012/us-east-1/abc1234`
   and opens PR #142 against `main`. The proposer's "Why"
   section is in the PR body.
3. **Day 1 — Operator merges.** The reviewer approves, the CI
   passes, the operator clicks Squash and Merge.
4. **Day 1 — Webhook fires.** GitHub posts the
   `pull_request closed + merged=true` event to
   `/api/v1/webhooks/github`. The receiver verifies the
   signature against `SQUADRON_GITHUB_WEBHOOK_SECRET`. The
   parser pulls `kind=rds-pi-em`, `account=123456789012`,
   `region=us-east-1`. The audit event
   `recommendation.pr_merged` lands with the full scope
   payload.
5. **Day 7 — Next scan.** A new scan kicks off against the
   same account + region. The recommendations handler calls
   `assembleAcceptedRecommendations(conn-acme-infra,
   123456789012, us-east-1)`. The query returns the 1 example
   from Day 1.
6. **Day 7 — Prompt enriched.** The user message gains the
   Recently accepted recommendations block listing the rds-pi-
   em PR. The instruction tells the model not to re-propose
   the same kind against the same resource.
7. **Day 7 — Proposer output.** The proposer returns
   recommendations for the OTHER 5 categories. The rds-pi-em
   slot is absent (the model honored the instruction). The
   handler emits `discovery_proposal.created` with
   `verdict_examples_used = ["https://github.com/acme/infra/pull/142"]`.
8. **Day 7 — Timeline.** The Timeline page renders the new
   event as **"Discovery recommendations generated (informed
   by 1 prior accepted PR)"**.

End-to-end: zero operator clicks between merge and the next
scan reading the accepted recommendation. The loop is
fire-and-forget once the per-connection flag is on.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| New scan still proposes a recommendation operator merged | The merged PR is on a pre-v0.89.28 branch shape (4 segments) | Confirm with `git branch -a` on the IaC repo. Re-open the recommendation from the Recommendations tab — the new PR uses the 6-segment shape and will contribute |
| Audit timeline shows raw `discovery_proposal.created` | Squadron version mismatch (humanizer was added in v0.89.28) | Upgrade Squadron; existing audit events will re-render with the new humanizer |
| `verdict_examples_used: []` on every event | Per-connection flag is off, OR no PRs merged in the 30-day window, OR all merged PRs are legacy 4-segment branches | Check the flag via `GET /api/v1/iac/github/connections/:id`; check `recommendation.pr_merged` payloads for `account_id` + `region` populated |
| Examples surfacing from wrong scope (cross-account leak) | This is a bug in slice 1; the bridge filter must enforce same connection × account × region. File an issue with the audit event payload that leaked | N/A — slice 1 design forbids this |
| Webhook delivery shows 200 ignored but examples don't surface | The merge happened from a non-Squadron repo (no `iac_connection` matched) — the audit event records the merge with empty `connection_id`, so the scope filter never matches | Expected behavior; only Squadron-managed connections feed the loop |
| `connection_id` empty in audit payload despite Squadron-opened PR | Slice 1's handler-emit adapter walks all connections rather than keying on one — see #643 spec §11 Q4. PR URL in `verdict_examples_used` still allows SIEM correlation | Use PR URL for correlation; slice 2 will populate `connection_id` directly |

## Slice 2 roadmap

Slice 2 is now a locked design doc:
[#531 slice 2](./proposals/531-proposer-learning-slice2.md)
(v0.89.33). It covers the items below in priority order, plus
the architectural decision to share a `verdictsel` +
`verdictprompt` layer across the cost-spike and discovery
surfaces while keeping storage surface-local. The items
actually shipping in slice 2 implementation:

- **Rejected-signal definition — answered.** Slice 2 promotes
  two negative signals: a new
  `recommendation.pr_closed_not_merged` audit event (the
  webhook receiver already no-ops on this case; slice 2 turns
  the no-op into a proper audit emit) plus an operator-set
  exclusion via a new `iac_recommendation_verdicts` table.
- **Per-recommendation suppression — partially answered.**
  The new `iac_recommendation_verdicts` table is the storage
  scope 1 of #643 deferred. It holds the exclusion flag (and
  optional `resource_id` for resource-level vs kind-level
  exclusion). Discovery recommendations are still computed
  on-demand; only the exclusion verdict persists.
- **Don't propose this again affordance.** A button on the
  Recommendations tab that POSTs to a new exclusion endpoint,
  emits a `discovery_recommendation.excluded` audit event, and
  filters out future proposals of that kind (or kind +
  resource).
- **Hot/cold tier window.** The 30d cliff becomes a 7d hot tier
  + 7-30d cold tier; hot examples are emitted before cold.
- **Kind diversity cap.** Within the N=4 example slot, at most
  2 examples of any one kind. Prevents an operator with 50
  accepted `rds-pi-em` PRs from getting a prompt that teaches
  only that one pattern.
- **Wizard UI for the per-connection flag.** Still deferred to
  a later slice — settings JSON works; the wizard step is
  lower priority than the affordances above.
- **Cross-scope learning with namespace mode.** Deferred to
  slice 3. No real operator ask yet.
- **Configurable recency window.** Deferred to slice 3. Slice
  2 ships hard-coded 7d / 30d / N=4 / MaxPerKind=2 constants
  in `internal/proposer/verdictsel/consts.go`.
- **Backfill of pre-v0.89.28 PRs.** Deferred. Not blocking
  anyone; useful only for retroactive analytics.

Read [#531 slice 2](./proposals/531-proposer-learning-slice2.md)
for the locked spec — the architectural decision in §3, the
selection policy in §6, the prompt format in §7, the 10
acceptance tests in §12. None of slice 2 ships in v0.89.28;
everything in this runbook describes slice 1 behavior you can
rely on today.

## Cross-references

- [Proposer learning loop (cost-spike side)](./proposer-learning-loop.md) —
  the sibling runbook for the cost-spike proposer feedback loop
  (#531 slice 1, shipped v0.89.17). Same shape, different
  signal source, different scope tuple.
- [GitHub webhook listener](./webhook-listener.md) — the
  upstream signal source. Without the webhook live, this
  loop has no input.
- [Connect IaC repo first-time setup](./discovery-iac-first-time-setup.md) —
  prerequisite for the IaC connection that owns the per-
  connection flag.
- [#643 design doc](./proposals/643-discovery-proposer-verdict-learning.md) —
  the locked slice-1 spec this runbook operationalizes.
- [Audit log](./audit-log.md) — full catalog of event types
  including the new `discovery_proposal.created`.
