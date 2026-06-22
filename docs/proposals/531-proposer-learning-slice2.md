# #531 slice 2 — Unified verdict learning across proposer surfaces

**Status:** design doc, locked for slice 2 implementation. Slice 1
of #531 SHIPPED (v0.89.17 / 18 / 19 / 22). The §10 Q3 cost-spike
per-rollout suppression candidate SHIPPED in v0.89.26. The
discovery-side parallel arc #643 slice 1 SHIPPED in v0.89.27 /
28 / 29. Slice 2 of #531 picks up where those three arcs left
off and answers the architectural question slice 1 explicitly
deferred: **do the two proposer surfaces share a substrate, and
what does negative signal look like across both.**

**See also:**
[#531 slice 1 (cost-spike side)](./531-proposer-learns-from-accepted-rejected.md),
[#643 (discovery side, slice 1)](./643-discovery-proposer-verdict-learning.md),
[proposer-learning-loop.md](../proposer-learning-loop.md),
[discovery-proposer-learning.md](../discovery-proposer-learning.md),
[ai-features.md](../ai-features.md),
[multi-step-plans-design.md](../multi-step-plans-design.md),
[audit-log.md](../audit-log.md),
[webhook-listener.md](../webhook-listener.md).

## 1. Problem

Two parallel feedback loops shipped today and they share more
than the runbooks let on. The cost-spike proposer
([`proposer.go:228`](../../internal/ai/proposer.go)) reads recent
verdicts from the `rollouts` table; the discovery proposer
([`proposer_discovery.go::ProposeFromDiscoveryScan`](../../internal/ai/proposer_discovery.go))
reads recent merged-PR events from the `audit_events` table.
The query shapes diverge, the verdict types diverge, the prompt
block formats diverge — but the *purpose* is the same: learn
from operator history, surface a small in-context block, bound
the example payload, redact secrets, opt out per surface.

Slice 1 was right to ship the two arcs independently. Each had
its own load-bearing storage substrate, its own audit signal,
its own runbook to write. Coupling them earlier would have
delayed both. Slice 2 inherits the working slice 1 and asks the
questions slice 1 explicitly deferred:

1. **Negative signal on the discovery side.** Slice 1 of #643
   shipped accepted-only. Operators close PRs without merging
   them all the time. Slice 2 of #531 says: PR closed without
   merge is rejected signal, on the same footing as the
   cost-spike rollout-rejected signal that has worked since
   v0.89.17.

2. **Operator-marked exclusion on the discovery side.** v0.89.26
   shipped `Rollout.ExcludeFromLearning` for the cost-spike
   side. The discovery side has no parallel today, partly
   because discovery recommendations are computed on-demand and
   not persisted. Slice 2 introduces a thin
   `iac_recommendation_verdicts` table that carries the
   exclusion flag for recommendations the operator has
   explicitly suppressed without ever opening a PR.

3. **Decay vs hard recency cliff.** Both slice 1s use a 30-day
   hard cliff. A verdict from yesterday and a verdict from 29
   days ago count the same. Operators tend to remember recent
   decisions and forget old ones; the proposer should mirror
   that. Slice 2 introduces a two-tier window (0-7d weighted
   first, 7-30d backfills).

4. **Kind diversity at high cardinality.** Both slice 1s cap at
   `N=4` examples and select newest-first. An operator who
   accepts 50 `rds-pi-em` recommendations in a month gets a
   prompt that teaches the model that one pattern and nothing
   else. Slice 2 introduces a per-kind cap of 2 within the N=4
   slot so the model sees varied patterns when varied patterns
   exist.

5. **A shared selection + prompt layer.** Storage stays
   separate — rollouts and audit-events are not the same data
   and shouldn't be force-merged. But the selection policy
   (tier windows, kind diversity, opt-out, redaction) and the
   prompt block construction (line format, instructional copy)
   absolutely should share code. Slice 2 extracts
   `internal/proposer/verdictsel` and
   `internal/proposer/verdictprompt` as the shared layer.

## 2. Non-goals (slice 2)

- Cross-surface verdict pollination. A cost-spike rejected
  verdict does NOT become negative signal for the discovery
  proposer, and vice versa. The surfaces stay logically
  isolated; they share *code*, not *evidence*.
- Cross-account / cross-region learning on the discovery side.
  Same posture as #643 slice 1; revisit when there's operator
  demand backed by a real ask.
- Cross-group learning on the cost-spike side. Same posture as
  #531 slice 1.
- Storage unification. The two surfaces continue to read from
  separate tables. A shared `learning.Store` interface is
  tempting but premature — the row schemas genuinely diverge.
- Fine-tuning, RAG, embedding stores. Prompt-only stays the
  contract.
- Recommendation-level history beyond what slice 2 needs for
  the exclusion flag. The new
  `iac_recommendation_verdicts` table is intentionally minimal:
  it carries verdict state, not the recommendation body.
- Wizard UI step for the exclude affordance. Slice 2 settles
  for a "Don't propose this again" button on the discovery
  Recommendations tab; the wizard onboarding flow doesn't
  need to teach this yet.
- Per-tenant cross-group sharing flag mentioned in the
  brainstorm. Defer to slice 3 — operators with multi-team
  concerns haven't asked for this.
- Automatic decay constant tuning. Slice 2 ships the two-tier
  window with fixed boundaries (7d, 30d). If we learn the
  boundaries are wrong, slice 3 makes them config-driven.

## 3. Architectural decision

The architectural question slice 1 deferred is: **shared
substrate, or shared shape?** Slice 2 picks shared shape.

The rejected option was a `verdicts` table that both surfaces
write into and read from. The row would have to accommodate
both rollout verdicts (rich reasoning + approver notes) and
discovery merges (PR URLs + branch names + merged_by). The
union of those fields is a wide row with a `surface` enum
discriminator and a lot of nullable columns. Worse, the rollup
write path would have to fork at every audit-emit point — every
`rollout.approved`, every `rollout.rejected`, every
`recommendation.pr_merged`, every (slice 2 new)
`recommendation.pr_closed_not_merged` would need to land both
in its existing audit row AND in the new `verdicts` table.
Double writes invite drift.

The shipped reality is that the two surfaces read directly off
the data the operator already wrote: rollouts for cost-spike,
audit events for discovery. Adding a third table that duplicates
this data buys nothing and costs schema complexity. The
*selection logic* — how to pick N from M candidates, how to
apply the recency window, how to enforce kind diversity, how
to fold in the opt-out flag, how to wire redaction — is what
the two surfaces actually share. That logic is functional;
extract it.

Concretely, slice 2 introduces:

```go
// internal/proposer/verdictsel
type Verdict struct {
    ID        string    // rollout_id OR PR URL
    Kind      string    // rollout kind OR recommendation_kind
    State     string    // "approved" / "rejected" / "merged" / "closed_not_merged"
    Timestamp time.Time // approved_at / rejected_at / merged_at / closed_at
    Body      string    // reasoning + notes (cost-spike) OR branch + merged_by (discovery)
    Excluded  bool      // operator-set; selection drops on true
}

type SelectOpts struct {
    Now        time.Time
    Window     time.Duration // 30d for both surfaces
    HotWindow  time.Duration // 7d (slice 2 addition)
    MaxTotal   int           // 4
    MaxPerKind int           // 2 (slice 2 addition)
    PreferNeg  bool          // true for cost-spike (rejection-weighted),
                             // false for discovery (no rejection in pool yet)
}

func Select(rows []Verdict, opts SelectOpts) []Verdict
```

```go
// internal/proposer/verdictprompt
type RenderOpts struct {
    Surface          Surface // SurfaceCostSpike or SurfaceDiscovery
    AcceptanceHeader string  // surface-specific intro line
    RejectionHeader  string  // ditto
    InstructionTail  string  // load-bearing "use as signal" copy
}

func Render(approved, rejected []Verdict, opts RenderOpts) string
```

Both surfaces call `Select` with their surface's rows and
`Render` with their surface's prompt copy. The line format
(`[APPROVED] kind=...` / `[REJECTED] kind=...`) is shared. The
selection policy (tier window, kind cap, redaction) is shared.
The actual storage queries stay surface-local. This keeps the
slice 2 surface area honest: ~400 lines of pure functions plus
~150 lines of test coverage, no schema work beyond the new
`iac_recommendation_verdicts` table for the exclusion flag.

## 4. Signal source

### 4.1 Cost-spike side (already shipped, slice 2 changes)

Inputs unchanged from slice 1: `rollouts` rows with
`ProposedBy="ai"` and either `ApprovedAt` or `RejectedAt`
non-NULL, plus `ExcludeFromLearning` (v0.89.26).

Slice 2 changes: the per-bucket `verdictsMaxPerBucket=2` becomes
the `MaxPerKind=2` cap in `SelectOpts`. The hot/cold window
split adds a 7-day boundary inside the existing 30-day cap. The
`PreferNeg=true` flag preserves the rejection-bucket bias from
slice 1.

### 4.2 Discovery side (slice 2 net-new)

Slice 1 (#643) shipped accepted-only on
`recommendation.pr_merged` audit events. Slice 2 adds two
negative-signal sources:

**(a) PR closed without merge.** The webhook receiver
([`api/handlers/iac_github_webhook.go`](../../internal/api/handlers/iac_github_webhook.go))
already emits no-op responses for `pull_request.closed` events
where `merged==false`. Slice 2 promotes those to a proper audit
event:

```go
AuditEventRecommendationPRClosedNotMerged = "recommendation.pr_closed_not_merged"
```

Payload mirrors `recommendation.pr_merged` exactly, with
`closed_at` and `closed_by` replacing `merged_at` and
`merged_by`. The branch is still parsed for kind / account /
region via `parseRecommendationScopeFromBranch`. Rows from this
event become `State: "closed_not_merged"` in the verdict pool.

**(b) Operator-set exclusion.** Slice 2 adds a "Don't propose
this again" affordance to the discovery Recommendations tab.
Clicking it creates or updates a row in the new
`iac_recommendation_verdicts` table:

```sql
CREATE TABLE iac_recommendation_verdicts (
    recommendation_id      TEXT PRIMARY KEY,
    connection_id          TEXT NOT NULL,
    account_id             TEXT NOT NULL,
    region                 TEXT NOT NULL,
    recommendation_kind    TEXT NOT NULL,
    resource_id            TEXT,                -- nullable, exact match if present
    exclude_from_learning  INTEGER NOT NULL,    -- 0 / 1
    excluded_at            TIMESTAMP,
    excluded_by            TEXT,
    created_at             TIMESTAMP NOT NULL,
    updated_at             TIMESTAMP NOT NULL
);
CREATE INDEX idx_iac_rec_verdicts_scope
    ON iac_recommendation_verdicts(connection_id, account_id, region, exclude_from_learning);
```

`recommendation_id` is the deterministic ID the discovery
proposer assigns when it generates a recommendation; the row
is created lazily on the first exclude click. The proposer's
selection pass filters out any verdict whose
`recommendation_id` appears here with `exclude_from_learning=1`.

The two negative-signal sources are deliberately separate from
the `recommendation.pr_merged` accepted signal:

- **PR closed without merge** is a *behavioral* signal —
  the operator engaged with the recommendation, opened the PR,
  then closed it. Strong "don't propose this shape" signal,
  but the operator may still want the kind for other resources.
- **Operator-set exclusion** is an *explicit* signal —
  the operator clicked Don't propose this again. Strongest
  possible "drop this from learning entirely" signal.

The verdict pool feeds both into `Select` with `State` set
accordingly. The prompt block (§6) surfaces both as rejected
examples, with the line text differentiating.

### 4.3 Pending signals NOT used

Slice 2 still does NOT treat any of these as signal:

- **PR opened, no merge or close yet.** Same as slice 1 — the
  PR might still merge. Counts as nothing.
- **Recommendation appeared in prior scan, operator never
  opened a PR.** Same as slice 1. Too noisy; operators triage
  in bulk.
- **Operator hand-merged a branch with a non-Squadron format.**
  The branch parse returns empty kind; the row is filtered out.

## 5. Storage

### 5.1 Cost-spike side

No schema changes. The slice 1 schema (v4) with
`rollouts.exclude_from_learning` and the
`idx_ai_verdicts` index is sufficient. Slice 2 reads the same
columns; only the selection policy changes.

### 5.2 Discovery side

Two changes:

**(a) New audit event type emitted on PR-close-not-merge.** No
schema change — the `audit_events` table already accepts
arbitrary event types. The webhook receiver gains the emit
call. The existing partial index
`idx_audit_pr_merged_scope` (v0.89.28) is extended to cover
the new event:

```sql
DROP INDEX idx_audit_pr_merged_scope;
CREATE INDEX idx_audit_recommendation_verdict_scope
    ON audit_events(event_type, timestamp DESC)
    WHERE event_type IN ('recommendation.pr_merged',
                         'recommendation.pr_closed_not_merged');
```

The migration bumps the application schema version (currently
v6 after v0.89.30) to v7. The drop+create is safe because the
index is partial and small.

**(b) New table `iac_recommendation_verdicts`** as defined in
§4.2. Migration adds the CREATE TABLE plus the scope index.
This is the first table outside `audit_events` that the
discovery proposer's learning loop reads, so slice 2's
`ListDiscoveryVerdicts` query is a two-source join in the
storage layer:

```go
ListDiscoveryVerdicts(ctx,
    connectionID, accountID, region string,
    since time.Time, limit int,
) ([]DiscoveryVerdict, error)
```

The implementation:

1. Pull recent audit rows matching scope from `audit_events`
   for both `recommendation.pr_merged` and
   `recommendation.pr_closed_not_merged`, ordered newest first,
   capped at `limit * 4` for headroom.
2. Pull the `iac_recommendation_verdicts` rows in scope where
   `exclude_from_learning=1`, capped at `limit * 4`.
3. Project each into a `DiscoveryVerdict` row, with `State`
   distinguishing merged / closed_not_merged / excluded.
4. Pass the unioned slice to `verdictsel.Select`.

The double-source query is the only non-trivial storage work
in slice 2. The two sources are both already-cheap reads (one
partial-indexed audit query, one indexed table scan); the
union happens in Go, not SQL.

### 5.3 New opt-out flag on the connection (already exists)

`IaCConnection.LearnFromAcceptedRecommendations` shipped in
v0.89.28 (#643). Slice 2 renames it semantically:
the flag now controls learning from BOTH accepted and rejected
signal. The column name stays `learn_from_accepted_
recommendations` to avoid a migration; the runbook explains the
broadened meaning.

## 6. Selection policy

`verdictsel.Select(rows, opts)` is the shared core. The
algorithm:

1. **Filter.** Drop any verdict with `Excluded=true` (the
   operator-set exclusion). Drop any whose `Timestamp <
   now - Window`.
2. **Bucket by state.** Approved into one slice; rejected /
   merged into the approved bucket; closed_not_merged into the
   rejected bucket. (Surface-level state strings map to the
   binary approved/rejected outcome via a small table.)
3. **Tier by hot window.** Within each bucket, partition into
   `hot` (`Timestamp >= now - HotWindow`) and `cold`
   (`now - HotWindow > Timestamp >= now - Window`). Hot
   examples are emitted before cold examples of the same
   bucket.
4. **Enforce kind cap.** Walk the bucket newest-first within
   each tier; skip any verdict whose `Kind` has already
   appeared `MaxPerKind` times in the bucket. If kind diversity
   would underfill the bucket, the remaining slots stay open.
5. **Bucket cap & PreferNeg.** If `PreferNeg` is true (cost-
   spike), fill the rejected bucket up to half of `MaxTotal`
   before filling approved. Otherwise, fill them in parallel.
6. **Total cap.** Stop at `MaxTotal` examples across buckets.
7. **Return** with rejected examples first (the prompt orders
   them for emphasis), approved examples second.

The algorithm is intentionally deterministic given the input
slice — no time-based seeded random — so cold-start parity
tests and golden-output regression tests are practical.

Constants for slice 2 (in `internal/proposer/verdictsel/consts.go`):

```go
const (
    DefaultWindow     = 30 * 24 * time.Hour
    DefaultHotWindow  = 7  * 24 * time.Hour
    DefaultMaxTotal   = 4
    DefaultMaxPerKind = 2
)
```

Both surfaces use defaults. Config-driven constants are slice 3.

## 7. Prompt integration

`verdictprompt.Render(approved, rejected, opts)` returns the
text block; surface-specific copy is in `opts`.

### 7.1 Shared line format

Each line is a 4-line stanza:

```text
[STATE] kind=<kind>
  reference: <surface-specific identifier>
  when: <relative time, e.g. "merged 3 days ago" or "rejected yesterday">
  reason: <short, redacted summary>
```

Surface variations live in the `reference:` line:

- **Cost-spike:** `reference: rollout_id=rlt_8ax9`
- **Discovery (merged):** `reference: pr_url=https://github.com/acme/infra/pull/142`
- **Discovery (closed):** `reference: pr_closed=https://github.com/acme/infra/pull/142`
- **Discovery (excluded):** `reference: operator_excluded=2026-06-15`

The `when:` line is computed by `verdictprompt` from the
verdict timestamp, not the storage layer. Operators reading the
audit-event payload of a `proposal.created` or
`discovery_proposal.created` event see absolute timestamps
elsewhere; the prompt prefers relative phrasing because that's
how the model interprets recency.

### 7.2 Surface-specific headers and instruction tail

**Cost-spike side** (slice 1 prompt extended):

```text
Prior verdicts for this group (operator decisions on past AI proposals):

[REJECTED] kind=cost-spike-rollout
  reference: rollout_id=rlt_7q12
  when: rejected 2 days ago
  reason: dropped k8s.pod.uid plus k8s.namespace in one step.

[APPROVED] kind=cost-spike-rollout
  reference: rollout_id=rlt_8ax9
  when: approved 4 days ago
  reason: container.id was driving 60% of the spike; canary 10% / 600s.

Use these as preference signal. Match the shape of approved
proposals; avoid the shape of rejected ones. Do NOT cite these
rollout_ids in your evidence — they're operator history, not
evidence for this spike.
```

**Discovery side** (slice 1 prompt extended to include the new
rejected examples):

```text
Recent operator decisions on past Squadron recommendations for this scope:

[CLOSED_NOT_MERGED] kind=rds-pi-em
  reference: pr_closed=https://github.com/acme/infra/pull/145
  when: closed 1 day ago
  reason: operator closed PR without merging.

[OPERATOR_EXCLUDED] kind=eks-observability-addon
  reference: operator_excluded=2026-06-15
  when: excluded 6 days ago
  reason: operator marked this recommendation kind as do-not-propose.

[ACCEPTED] kind=rds-pi-em
  reference: pr_url=https://github.com/acme/infra/pull/142
  when: merged 3 days ago
  reason: operator merged the PR.

Use these as preference signal. Do NOT re-propose the same
kind+resource against the same scope that the operator has
already accepted or explicitly excluded within the window. For
closed-without-merge entries, the operator engaged but
declined this specific shape — you may propose a different
variation if you have evidence; cite the divergence in your
reasoning. If the operator-excluded entry applies to this
scope, drop the entire kind from your recommendations.
```

Empty pool on both sides → entire block omitted, prompt is
byte-for-byte identical to the slice 1 cold-start prompt. This
is the cold-start parity acceptance test pin.

### 7.3 Why "closed_not_merged" is rejected signal, not neutral

The model needs an unambiguous read on what closing-without-
merge means. Three interpretations are possible:

1. The recommendation was wrong (operator rejected the
   substance).
2. The recommendation was right but the timing was bad
   (operator deferred).
3. The operator opened the PR to test the wizard flow and
   never intended to merge.

Slice 2's posture: treat case (1) as the assumption, with the
prompt instructing the model that case (2) and (3) are
recoverable — "you may propose a different variation if you
have evidence." This isn't perfect; a few operators in case
(2) will lose recommendations they would have eventually
accepted on a retry. But the alternative (treat
closed_not_merged as neutral) eats the entire negative-signal
benefit of slice 2 on the discovery side, and case (1)
operators get repeated bad recommendations indefinitely. The
trade favors the strict interpretation.

## 8. Privacy + opt-out

The three opt-out levels in slice 2 (across both surfaces):

| Level | Cost-spike | Discovery |
|---|---|---|
| Per-tenant | `Group.LearnFromVerdicts` (slice 1) | `IaCConnection.LearnFromAcceptedRecommendations` (slice 1) |
| Per-verdict | `Rollout.ExcludeFromLearning` (v0.89.26) | `iac_recommendation_verdicts.exclude_from_learning` (slice 2 new) |
| Per-prompt-call | none | none |

Both per-verdict flags route through `verdictsel.Select` step 1
(filter Excluded=true). Both per-tenant flags short-circuit
the proposer-side assemble call before any database read,
preserving the slice 1 cold-start parity guarantee.

The redaction pass through
[`internal/ai/redact.go`](../../internal/ai/redact.go)
continues to apply on both surfaces. Slice 2's shared
`verdictprompt.Render` invokes redaction on the `reason:`
field of every emitted verdict; surface-local code paths
unchanged.

PR URLs themselves are not redacted — slice 1 of #643 ships
them in the prompt verbatim. Operators who want PR URLs hidden
disable learning at the connection level. The runbook flags
this explicitly.

## 9. Audit trail

Three additions, all surface-local emits with shared payload
shape from the slice 2 shared layer:

**(a) `recommendation.pr_closed_not_merged`** — new event type
emitted by the webhook receiver. Payload mirrors
`recommendation.pr_merged` exactly with `closed_at` and
`closed_by` instead of `merged_at` and `merged_by`. Humanizer:
`Operator closed PR #N in <repo> without merging (kind=<kind>)`.

**(b) `discovery_recommendation.excluded`** — new event type
emitted when the operator clicks the Don't propose this again
affordance. Payload:

```go
{
  "connection_id": "<iac_connection.id>",
  "account_id": "<aws account>",
  "region": "<region>",
  "recommendation_kind": "<kind>",
  "recommendation_id": "<deterministic id>",
  "resource_id": "<optional>",
  "excluded_by": "<operator>"
}
```

Humanizer:
`Operator excluded recommendation kind=<kind> from future scans`.
The event fires on transitions only (false → true). A later
un-exclude (operator changes their mind) fires the inverse
event `discovery_recommendation.exclude_cleared` with the same
payload shape minus the `excluded_by` (replaced with
`cleared_by`).

**(c) Extended `proposal.created` and `discovery_proposal.created`
payloads** — both gain a `verdict_examples_used_by_state`
field that breaks the existing `verdict_examples_used` array
into per-state buckets:

```json
{
  "verdict_examples_used": [...],
  "verdict_examples_used_by_state": {
    "approved":          ["rlt_8ax9"],
    "rejected":          ["rlt_7q12", "rlt_6m08"],
    "merged":            ["https://..."],
    "closed_not_merged": ["https://..."],
    "operator_excluded": ["rec_id_abc"]
  }
}
```

The existing `verdict_examples_used` array stays for backward
compat with SIEM consumers. The new
`verdict_examples_used_by_state` is the structured version.
Humanizer extension (slice 2): when the by-state field
contains any non-empty bucket, the timeline entry surfaces the
state mix, e.g. `Discovery recommendations generated
(informed by 2 accepted + 1 rejected + 1 excluded)`.

## 10. Slice 2 contract

**In:**

1. New package `internal/proposer/verdictsel` — pure functions,
   `Verdict` type, `SelectOpts`, `Select(rows, opts) []Verdict`.
2. New package `internal/proposer/verdictprompt` — pure
   functions, `Render(approved, rejected, opts) string`, line
   format helpers.
3. Both packages have unit tests covering: hot/cold tier
   ordering, kind-cap enforcement, PreferNeg behavior,
   filter-on-Excluded, empty-pool cold-start parity.
4. Cost-spike `Bridge.assembleVerdicts` refactored to feed
   `verdictsel.Select` + `verdictprompt.Render`. Existing
   prompt output byte-for-byte unchanged given the same input
   verdicts (golden test pins this).
5. Discovery `Bridge.assembleAcceptedRecommendations` renamed
   to `assembleDiscoveryVerdicts`. Now reads from the unioned
   `ListDiscoveryVerdicts` storage method. Calls
   `verdictsel.Select` + `verdictprompt.Render` with the
   discovery-surface render opts. Existing slice 1 accepted-
   only prompt output preserved on a connection that has zero
   negative signal rows (golden test pins this).
6. Webhook receiver emits the new
   `recommendation.pr_closed_not_merged` audit event on
   `pull_request.closed` events with `merged=false`. Existing
   no-op response is preserved; the change is the audit emit.
7. New `iac_recommendation_verdicts` table + migration to
   schema v7 + storage methods
   `SetRecommendationExclusion(ctx, recID, excluded bool,
   operator string)` and
   `ListExcludedRecommendations(ctx, connID, accID, region,
   limit) ([]ExcludedRecommendation, error)`.
8. New audit events `discovery_recommendation.excluded` and
   `discovery_recommendation.exclude_cleared` with humanizer
   entries.
9. Discovery Recommendations tab gains a "Don't propose this
   again" button per recommendation row. Click POSTs to a new
   handler `POST /api/v1/discovery/aws/recommendations/exclude`
   which calls `SetRecommendationExclusion` and emits the audit
   event.
10. `verdict_examples_used_by_state` field on both
    `proposal.created` and `discovery_proposal.created` audit
    payloads. Humanizer extension emits the state-mix breakdown.

**Out:**

- Cross-surface evidence pollination.
- Cross-account / cross-region / cross-group learning.
- Storage unification (the verdicts table option from §3).
- Configurable window / hot-window / cap constants (defaults
  hard-coded in `verdictsel/consts.go`).
- Wizard UI step for the discovery exclusion affordance.
- Fine-tuning, RAG, embedding stores.
- A per-tenant cross-group sharing flag.
- Slice 3 candidates (recommendation history table, model-side
  fine-tuning, learned rejection patterns, automatic decay
  constant tuning).

## 11. Open questions

1. **Closed-not-merged interpretation.** §7.3 lays out the
   strict interpretation (treat as rejected signal) with the
   prompt's recovery affordance for cases (2) and (3). Worth
   running an offline test against
   [`proposer_live_test.go`](../../internal/ai/proposer_live_test.go)
   with a corpus that includes legitimate "close to defer"
   PRs to measure how often the recovery affordance fires.

2. **Hot-window boundary.** 7 days is the proposed boundary,
   matching common sprint cadence. Worth checking against real
   operator timelines: are most accept/reject decisions made
   within 7 days of the PR opening, or longer? If longer,
   the boundary moves.

3. **MaxPerKind=2 at low cardinality.** If an operator has
   only 1 accepted of `rds-pi-em` and 1 accepted of `eks-
   observability-addon` in the last 30 days, the cap doesn't
   bind. The current algorithm doesn't compensate by widening
   the cap; it just emits the 2 examples. Is that the right
   posture, or should the algorithm relax the cap when the
   pool is small?

4. **Excluded-by-resource-id scoping.** The new
   `iac_recommendation_verdicts.resource_id` is nullable.
   Slice 2 supports two exclusion granularities: kind-level
   ("never propose `rds-pi-em` against this scope") and
   resource-level ("never propose `rds-pi-em` against this
   specific DB instance"). The UI should expose both. The
   prompt's "drop the entire kind" instruction in §7.2
   applies only when `resource_id` is null on the
   excluded-row. When `resource_id` is set, the model needs
   different instruction text. Slice 2 implementation must
   surface this distinction in `verdictprompt.Render`.

5. **PR-merged-then-reverted.** §10 Q3 in #643 flagged this.
   Slice 2 doesn't fix it. Slice 3 candidate: a discovery
   rescan that detects the resource's instrumented state has
   reverted, plus a manual "this recommendation was reverted"
   flag the operator can set.

6. **Cross-tenant cross-group sharing.** Brainstormed in §1
   bullet 5 but excluded from slice 2 scope. Real question:
   does any operator actually want this? Until a deployment
   asks, leave it deferred.

7. **Audit payload shape parity across surfaces.** Slice 1
   shipped `verdict_examples_used` as opaque rollout_ids on
   the cost-spike side and PR URLs on the discovery side.
   Slice 2's `verdict_examples_used_by_state` keeps the same
   surface-specific identifiers inside each bucket. SIEM
   consumers that want a unified identifier-shape will have
   to adapt. Defer to slice 3 if this becomes a real ask.

## 12. Acceptance tests

1. **Cold start parity — cost-spike.** Group with zero AI-
   originated rollouts: `ProposeFromCostSpike` produces a
   prompt byte-for-byte identical to the slice 1 (v0.89.17)
   output. Existing `proposer_test.go` golden passes
   unchanged.

2. **Cold start parity — discovery.** Connection with zero
   `recommendation.pr_merged`, zero
   `recommendation.pr_closed_not_merged`, zero excluded
   recommendations: `ProposeFromDiscoveryScan` produces a
   prompt byte-for-byte identical to the slice 1 (v0.89.28)
   output. Existing `proposer_discovery_test.go` golden
   passes unchanged.

3. **Hot/cold tier ordering — cost-spike.** Seed 2 approved
   rollouts: one dated 2 days ago, one dated 20 days ago. Both
   land in the prompt. Assert: the 2-day-old example appears
   first within the approved bucket regardless of which
   rollout has the lexicographically smaller ID.

4. **Kind diversity cap — discovery.** Seed 5 merged PRs in
   scope, all of kind `rds-pi-em`. Fire a discovery proposal.
   Assert: at most 2 examples of `rds-pi-em` appear in the
   prompt; the remaining 2 slots of MaxTotal=4 stay empty
   because no other kinds exist in the pool.

5. **Negative signal — closed_not_merged surfaces.** Seed 1
   `recommendation.pr_closed_not_merged` audit event in scope,
   kind=`rds-pi-em`, closed 1 day ago. Fire a discovery
   proposal. Assert: the user message contains
   `[CLOSED_NOT_MERGED] kind=rds-pi-em`. The
   `discovery_proposal.created` audit event's
   `verdict_examples_used_by_state.closed_not_merged` contains
   the PR URL.

6. **Negative signal — operator exclusion surfaces.** Operator
   clicks Don't propose this again on a recommendation of
   kind=`eks-observability-addon` with `resource_id=null`.
   Fire a discovery proposal in the same scope. Assert: the
   user message contains
   `[OPERATOR_EXCLUDED] kind=eks-observability-addon` with
   `resource_id=null` semantics. The proposer's output does
   NOT include any `eks-observability-addon` recommendations.

7. **Per-verdict exclude respected — cost-spike.** Seed 3
   approved rollouts in group G. Mark one as
   `ExcludeFromLearning=true`. Fire a spike proposal. Assert:
   the excluded rollout's ID does NOT appear in
   `verdict_examples_used_by_state.approved` and does NOT
   appear in the prompt.

8. **Per-tenant opt-out short-circuits — discovery.** Connection
   C has `LearnFromAcceptedRecommendations=false`. Seed 3
   merged + 2 closed_not_merged events in scope. Fire a
   discovery proposal. Assert: no examples block in the
   prompt, `verdict_examples_used_by_state` is all empty
   buckets, and the database query for verdicts is NOT made
   (verified via store-call counter on the fake).

9. **Recency window enforced — both surfaces.** Seed one
   example dated 31 days ago on each surface. Fire fresh
   proposals on both. Assert: the 31-day-old example does NOT
   appear in either prompt, and is NOT in any of the audit
   payload buckets.

10. **Shared layer regression — verdictsel pure function.**
    Unit-test `verdictsel.Select` with a curated input slice of
    20 verdicts spanning all states / both tiers / 5 kinds.
    Assert: the output is deterministic (calling twice with
    the same input gives byte-identical output), honors all
    caps, and the rejected examples come first in the slice.
    Golden output pinned.

---

**Slice 3 candidates (NOT in slice 2):**

- Recommendation history table: persist every discovery
  recommendation the proposer emits with deterministic ID,
  so the UI can show "this recommendation has appeared in 4
  past scans" and "you accepted this 3 times before."
- Cross-tenant cross-group sharing flag with tenant-level
  policy.
- Configurable window / hot-window / kind-cap constants via
  the AI service config block.
- Wizard UI step for the discovery exclude affordance,
  taught during onboarding.
- Model-side fine-tuning on accepted-verdict corpora.
- Learned rejection patterns: a separate model pass that
  reads the rejected pool and synthesizes a "what NOT to do"
  set of rules surfaced in the system prompt.
- GitHub Checks API back-signal: when a Squadron-opened PR
  fails CI, capture that as additional negative signal.
- Per-merge-revert detection via rescan.
