# #643 — Discovery proposer learns from accepted recommendations

**Status:** proposal, slice-1 scoping. v0.89.27 candidate.
**See also:** [#531 (cost-spike side)](./531-proposer-learns-from-accepted-rejected.md),
[ai-features.md](../ai-features.md),
[proposer-learning-loop.md](../proposer-learning-loop.md),
[webhook-listener.md](../webhook-listener.md),
[discovery-iac-first-time-setup.md](../discovery-iac-first-time-setup.md).

## 1. Problem

The discovery proposer
([`ai/proposer_discovery.go::ProposeFromDiscoveryScan`](../../internal/ai/proposer_discovery.go))
generates the same recommendation against the same resource on
every scan, even after the operator has accepted it. An EC2
instance flagged for ADOT SSM association gets the recommendation
on Monday's scan, Tuesday's scan, and Wednesday's scan — even
when Monday's scan ended with the operator clicking Open PR,
the PR merging on Tuesday, and Wednesday's reality being "this
instance is already instrumented."

The discovery side is supposed to be feedback-driven: scan,
recommend, accept-or-decline, learn, scan again. Today it's a
dumb diff — the proposer has no awareness of what the operator
accepted last week.

The signal to fix this finally exists: v0.89.23's
`recommendation.pr_merged` audit event records every Squadron-
initiated PR that lands. The webhook receiver writes
`{repo_full_name, pr_number, branch, merged_by, recommendation_
kind, connection_id}` into the audit log on every merge, and
the branch name encodes the recommendation_kind that the
proposer originally proposed. That's the "accepted" datum slice
1 of this loop needs.

This proposal locks slice 1 of the discovery proposer feedback
loop on the accepted signal alone. Rejected signals (PR closed
without merge, operator-typed declines) and cross-scope
learning are explicit slice 2+ work.

## 2. Non-goals (slice 1)

- Cost-spike proposer changes. Slice 1 of #531 already covers
  that surface and slice 2 work is its own arc.
- Cross-account learning. A scan of account A can't surface
  accepted recommendations from account B as positive signal.
  Same-account-strict for slice 1.
- Cross-region learning. Even within the same account, an
  accepted recommendation from us-east-1 doesn't surface as
  positive signal in a us-west-2 scan. Same-region-strict
  for slice 1.
- Cross-resource-kind learning. An accepted `rds-pi-em`
  recommendation doesn't make an `eks-observability-addon`
  recommendation less likely. Same-kind-strict for slice 1.
- The "rejected" signal. Slice 1 is accepted-only. PR closed
  without merge, operator explicitly typed exclude on a past
  recommendation, the operator never opened a PR — all of
  these are slice 2 questions that depend on per-recommendation
  state we don't track today.
- Per-recommendation suppression. v0.89.26 shipped this for
  cost-spike rollouts via `Rollout.ExcludeFromLearning`. The
  discovery analog is a slice 2 question (would need an
  `iac_recommendations` storage row to carry the flag —
  recommendations are computed on-demand today, not stored).
- Fine-tuning, RAG, embedding stores. Same as #531: prompt-
  only is the slice 1 contract.

## 3. Signal source

Every datum slice 1 needs is on existing audit events — no
new persistence for the signal itself.

- **Accepted signal**: a
  `recommendation.pr_merged` audit event
  ([`services/audit_service.go::AuditEventRecommendationPRMerged`](../../internal/services/audit_service.go))
  whose payload's `merged_at` falls within the last 30 days
  AND whose payload's `connection_id`, `recommendation_kind`,
  and (derived from the recommendation's scan context)
  `account_id` + `region` match the next discovery proposer
  call's scope.
- **Provenance of the original recommendation**: the same
  audit event's `branch` field decodes via
  [`api/handlers/iac_github_webhook.go::parseRecommendationKindFromBranch`](../../internal/api/handlers/iac_github_webhook.go)
  into the recommendation_kind. The kind is what the proposer
  proposed originally; the merge says "the operator accepted
  it."
- **Connection lookup**: the
  `recommendation.pr_merged` payload's `connection_id`
  identifies the IaC connection that authored the PR. Connection
  records carry the repo_full_name; slice 1 uses
  the connection_id directly to scope examples without joining
  on repo. Empty `connection_id` means the webhook receiver
  saw a merge from a repo Squadron doesn't manage; those rows
  are NOT positive signal for slice 1 (defer to slice 2 — the
  proposer can't be sure the PR was actually about a Squadron
  recommendation).

Pending signal (PR opened, no PR merged yet) is NOT used as
positive OR negative signal in slice 1. The PR may still merge
tomorrow; the recommendation isn't accepted yet, but it isn't
declined either. Treating pending as "the operator implicitly
accepted by opening the PR" would be wrong — operators routinely
open PRs to discuss, then close them. Slice 1 only counts merges.

## 4. Storage

No new tables. Slice 1 adds one query method on
`ApplicationStore` mirroring the v0.89.17 shape:

```go
ListAcceptedDiscoveryRecommendations(ctx,
    connectionID, accountID, region string,
    since time.Time, limit int,
) ([]AcceptedRecommendation, error)
```

`AcceptedRecommendation` is a minimal projection:

```go
type AcceptedRecommendation struct {
    PRMergedAt         time.Time
    PRURL              string
    Branch             string
    MergedBy           string
    RecommendationKind string
}
```

SQL: `SELECT … FROM audit_events
WHERE event_type = 'recommendation.pr_merged'
  AND timestamp >= ?
  AND payload->>'connection_id' = ?
  AND payload->>'account_id' = ?
  AND payload->>'region' = ?
ORDER BY timestamp DESC
LIMIT ?`

SQLite supports `json_extract` (or the `->>` syntax via the
JSON1 extension) on JSON-typed columns; the audit payload column
is already JSON-text in the existing schema. Verify slice 1's
implementation includes a covering index:
`CREATE INDEX idx_audit_pr_merged_scope
ON audit_events(event_type, timestamp DESC)
WHERE event_type = 'recommendation.pr_merged'`. Partial index
keeps storage lean.

The v0.89.23 `recommendation.pr_merged` audit payload does NOT
currently carry `account_id` or `region`. Slice 1's implementation
MUST extend the webhook receiver's audit payload to include
these fields — the original recommendation context (account +
region) needs to round-trip from the discovery scan through
the PR open through the webhook merge so the lookup query can
filter on it. Mechanism: the v0.89.0 `recommendation.pr_opened`
emit point already has scan context in scope (the discovery
scan that produced the recommendation); slice 1 plumbs
`account_id` + `region` into the branch name's path segments
OR into the IaC connection's per-recommendation metadata. Pick
branch-name encoding for slice 1 because it's stateless: the
webhook receiver parses the branch as
`squadron/rec/<kind>/<account_id>/<region>/<short_id>` and the
audit payload fills `account_id` + `region` from the parse. The
existing `parseRecommendationKindFromBranch` helper extends
naturally to a new `parseRecommendationScopeFromBranch` that
returns all three values. Operators on pre-slice-1 branch
shapes still get correct audit events for `kind`; `account_id`
+ `region` come back empty (they're optional in the payload).

Opt-out flag on
[`iac_connections`](../../internal/discovery/iacconnstore/types.go):
new `LearnFromAcceptedRecommendations bool`, default `true`.
Add via the iacconnstore migration mechanism (whichever pattern
that package uses — verify in slice 1; the v0.89.17 sqlite
storage layer's migration system isn't necessarily what
iacconnstore uses).

## 5. Selection policy

`Bridge.assembleAcceptedRecommendations(connectionID, accountID,
region string)` returns up to `N=4` examples, deterministic
given `(connection_id, account_id, region, now)`:

- **Scope**: same connection_id + account_id + region. No
  cross-scope leakage.
- **Recency**: `since = now - 30d`. Hard-coded constant in
  slice 1, matches #531 §5.
- **Selection**: newest accepted first, capped at N=4.
- **Kind mix**: NO per-kind cap. The cost-spike side caps
  approved/rejected separately because rejection signal is
  denser; slice 1 here has no rejection signal at all, so the
  N=4 is a simple top-recent newest-first slice. When slice 2
  adds rejected signal, revisit.
- **Cold start**: zero rows → empty examples → prompt
  byte-for-byte identical to v0.85's `buildDiscoveryUserMessage`.
  Cold-start parity test pins this.
- **Cap**: each example's per-field payload is short
  (PR URL, branch, merged_by, recommendation_kind). The total
  example payload bounded to ~500 tokens — well under the
  cost-spike side's ~1.5K budget because discovery examples
  are structurally smaller.

## 6. Prompt integration

Block appended to `buildDiscoveryUserMessage`
([`ai/proposer_discovery_prompt.go:394`](../../internal/ai/proposer_discovery_prompt.go))
before the final "Return your recommendations" line. System
prompt unchanged. Verbatim shape:

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

Empty accepted list → entire block omitted. The prompt is
byte-for-byte identical to v0.85's, which is what the cold-start
parity acceptance test pins.

The instruction line is load-bearing: without it, the model
sometimes interprets "accepted" as "do nothing on this scope
ever again," which is wrong — the recommendation may be valid
again because the operator's CI rolled back the change, or
because the resource was re-created. Slice 1 explicitly tells
the model "the world may have drifted, propose if you have
evidence."

## 7. Privacy + opt-out

Pick: **per-connection flag**, not per-recommendation.

- `IaCConnection.LearnFromAcceptedRecommendations = false`
  → `assembleAcceptedRecommendations` returns empty.
- Flipped via the existing connections settings handler
  (`PATCH /api/v1/iac/github/connections/:id`); slice 1 extends
  this handler's payload to accept the flag.
- Per-recommendation suppression is a slice 2 question
  (mirrors #531 §10 Q3 → v0.89.26 timeline). Discovery
  recommendations are computed on-demand today, not stored,
  so per-recommendation suppression first needs storage. That's
  bigger than slice 1's scope.
- Examples route through `redact.go`
  ([`internal/ai/redact.go`](../../internal/ai/redact.go))
  before hitting the prompt. The discovery-side payload is
  structurally less likely to carry secrets than the cost-spike
  side (PR URLs and branch names vs operator reasoning + notes),
  but slice 1 keeps the redaction pass for defense in depth.
- The audit event extension (§8) lists which PR URLs informed
  the proposal so SIEM consumers can correlate. Operators with
  PR URLs that themselves carry sensitive context (a "secrets
  rotation" PR title showing in the URL slug) can flip the
  per-connection flag off.

## 8. Audit trail

The discovery proposer does NOT currently emit a dedicated audit
event when it produces recommendations — the existing
`POST /api/v1/discovery/aws/connections/:id/recommendations`
route returns the proposal payload directly without writing to
audit. Slice 1 introduces:

```go
AuditEventDiscoveryProposalCreated = "discovery_proposal.created"
```

Payload (mirrors the v0.89.17 cost-spike side's `proposal.created`
shape):

```go
{
  "scan_id": "<deterministic hash of scan inputs>",
  "connection_id": "<iac_connection.id>",
  "account_id": "<aws account scanned>",
  "region": "<aws region>",
  "recommendation_count": <int>,
  "verdict_examples_used": [
    "https://github.com/acme/infra/pull/142",
    "https://github.com/acme/infra/pull/138"
  ]
}
```

`verdict_examples_used` carries PR URLs (not opaque rollout IDs
like the cost-spike side) because that's what's actually
identifying for accepted discovery recommendations. SIEM
consumers correlate by PR URL back to the originating merge
event. Empty array on cold-start; same posture as #531 §8.

Humanizer entry (mirrors v0.89.22): when `verdict_examples_used`
is non-empty, title becomes
`Discovery recommendations generated (informed by N prior accepted PRs)`.
Cold-start fallback: `Discovery recommendations generated`.

## 9. Slice 1 contract

**In:**
1. `ApplicationStore.ListAcceptedDiscoveryRecommendations` +
   index + audit-events JSON predicate.
2. Audit payload extension: `account_id` + `region` added to
   `recommendation.pr_merged`. Branch-name encoding parser
   gains `parseRecommendationScopeFromBranch`.
3. `IaCConnection.LearnFromAcceptedRecommendations` column +
   migration in `iacconnstore`. Default true.
4. PATCH /iac/github/connections/:id handler accepts the flag.
5. `Bridge.assembleAcceptedRecommendations` + selection policy
   (§5).
6. Prompt block in `buildDiscoveryUserMessage` (§6).
7. Redaction pass on examples through existing `redact.go`.
8. New audit event `discovery_proposal.created` + payload
   shape per §8 + humanizer entry.
9. Cost-spike proposer unchanged. Discovery proposer only.

**Out:**
- Rejected signal (PR closed without merge as negative signal,
  or any other negative signal definition).
- Per-recommendation suppression flag.
- Cross-account learning.
- Cross-region learning.
- Cross-resource-kind learning.
- Configurable recency window (30d fixed for slice 1, same as
  cost-spike side).
- Wizard UI for the `LearnFromAcceptedRecommendations` flag
  on the IaC connection wizard (settings-page UI is enough for
  slice 1; wizard step is slice 2).

## 10. Open questions

1. **Branch-name encoding format.** Pick
   `squadron/rec/<kind>/<account_id>/<region>/<short_id>`
   for slice 1. Pre-slice-1 branches that don't carry account
   + region just emit empty fields in the audit payload; new
   PRs use the new format from this release forward. Cleaner
   than a parallel column on `iac_connections` (which would
   need a join at audit-emit time).

2. **PR-merged but recommendation_kind doesn't parse.** Operator
   hand-pushed a branch like `squadron/rec/` with no kind suffix
   (parseRecommendationKindFromBranch returns `("", false)`).
   Slice 1: skip the row entirely. Don't surface it as accepted
   signal. The audit event still fires, but the bridge filters
   on `recommendation_kind != ""`.

3. **What if the operator merged but then reverted?** Slice 1
   doesn't know. The accepted-signal definition is "PR merged
   in the last 30 days." A later revert is invisible to this
   layer. Slice 2 candidate: GitHub Checks API back-signal that
   notes the resource's actual instrumented state, OR a
   discovery-side rescan that detects the resource has reverted
   and lets the operator manually flip the recommendation back
   to "active."

4. **Multi-tenant per-connection vs per-repo.** Same as #531
   slice 2 cross-group: do operators with N connections to the
   same repo (e.g. one per managing team) want the accepted
   signal to cross those? Slice 1 says no — connection-strict.
   Slice 2 with a namespace mode would answer yes.

5. **What about scan_id correlation?** The cost-spike side's
   audit field is rollout IDs; the discovery side's is PR URLs.
   Both are reasonable but inconsistent — would SIEM consumers
   prefer the same field shape across both? Defer to slice 2:
   slice 1 ships PR URLs because that's what identifies an
   accepted discovery recommendation. If we later promote to
   "stored recommendation IDs," the audit field can grow a
   recommendation_id without breaking the existing payload.

## 11. Acceptance tests

1. **Cold start parity.** Connection with zero
   `recommendation.pr_merged` events: a call to
   `ProposeFromDiscoveryScan` produces a prompt byte-for-byte
   identical to v0.85's (no accepted block emitted). Existing
   `proposer_discovery_test.go` golden passes unchanged.

2. **Accepted example surfaces.** Seed one
   `recommendation.pr_merged` event in connection C with
   account A, region R, recommendation_kind = `rds-pi-em`,
   merged 5 days ago. Fire a new
   `ProposeFromDiscoveryScan` call on C/A/R. Assert: the user
   message contains
   `[ACCEPTED] kind=rds-pi-em`, the audit event
   `discovery_proposal.created` payload contains
   `verdict_examples_used = ["<that PR URL>"]`.

3. **Scope filter.** Seed 5 `recommendation.pr_merged` events
   across (C, A, R), (C, A, R-different), and (C, A-different,
   R). Fire on (C, A, R). Assert: only the 1 event matching
   the full scope tuple appears in the prompt; the other 4 do
   not leak.

4. **Opt-out flag respected.** Connection C has
   `LearnFromAcceptedRecommendations = false`. Seed 3 accepted
   merges in scope. Fire on C. Assert: no accepted block in
   prompt, `verdict_examples_used = []` on the audit event.

5. **Recency window.** Seed one accepted merge dated 31 days
   ago. Fire a fresh proposal. Assert: that merge does NOT
   appear in the prompt and is NOT in `verdict_examples_used`.
