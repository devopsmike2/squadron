# GitHub Checks API back-signal — slice 1 design

**Status:** design doc, locked for slice 1 implementation. Builds on
the v0.89.23 / 24 / 30 / 31 / 32 webhook listener arc that closed
on Sunday: Squadron now READS PR merge / close events from GitHub.
This proposal adds the inverse direction: Squadron WRITES check
run state to GitHub via the Checks API so operators see "what
Squadron is seeing" inside the PR review surface itself rather
than only inside Squadron's audit timeline.

**See also:**
[webhook-listener.md](../webhook-listener.md),
[discovery-iac-first-time-setup.md](../discovery-iac-first-time-setup.md),
[ai-features.md](../ai-features.md),
[discovery-proposer-learning.md](../discovery-proposer-learning.md),
[#531 slice 2 (unified verdict learning)](./531-proposer-learning-slice2.md).

## 1. Problem

A Squadron-opened PR today lands in GitHub with a title, a body
that lists the recommendation reasoning, and the diff. That's
it. The operator reviewing the PR sees what the PR proposes but
not the broader picture: was this recommendation informed by
prior accepted verdicts in the same scope? Has the proposer
seen this same kind closed-without-merge before? Has the
operator already excluded this kind for this account?

All of that context exists in Squadron. Some of it lives in the
new `verdict_examples_used_by_state` audit payload field (chunk 6
of #531 slice 2, shipped today). Some of it lives in the
`iac_recommendation_verdicts` table (chunk 4). The operator can
click into Squadron's audit timeline to see it, but the PR
review flow itself stays inside GitHub. Operators do not bounce
back and forth between tabs in practice; they review in GitHub
or they review in Squadron, rarely both.

The strategic frame: every observability platform that wants to
matter at scale meets operators where they already work.
ArgoCD pushes status to PR Checks. Renovate posts dependency
diff explanations as Check Runs. Sentry posts release health
gates. Squadron is the next layer of that stack: an SRE
recommendation flowing directly into the PR Checks lane,
explained, with full provenance one click away.

The GitHub Checks API gives us exactly that surface. A check
run on a Squadron-opened PR's head commit, with title +
summary + markdown text + optional annotations, costs Squadron
nothing structurally (one outbound POST per PR open, one PATCH
per merge / close), and gives operators a real-time view of
Squadron's reasoning right where they're already looking.

## 2. Non-goals (slice 1)

- **Required checks that gate merge.** GitHub Checks support a
  `required` flag that blocks merge until conclusion is success.
  Slice 1 ships every check run as informational (no required
  flag). Operators who want gating can mark the check as required
  in their repo's branch protection settings, but Squadron does
  not assume or mandate this. Required-by-default is a slice 2
  design question with real operational weight: it changes
  Squadron from a recommendation surface into a merge gate, and
  not every team wants that.
- **Annotations on specific files / lines.** The Checks API
  supports per-annotation positioning that surfaces as inline
  PR comments. Slice 1 ships the check run with summary + text
  only (no annotations). Annotations are a slice 2 candidate —
  the discovery proposer's `affected_resources` field already
  carries enough information to draft them, but the UX of inline
  annotations needs its own scoping pass.
- **Re-running checks on operator action.** GitHub supports
  `actions` on a check run that surface as buttons (e.g., "Re-run
  Squadron's analysis"). Slice 1 ships read-only check runs;
  no actions. Slice 2 candidate: a "Re-evaluate" action that
  triggers a fresh discovery scan in scope and updates the
  check run conclusion.
- **Check suites.** GitHub groups check runs into check suites.
  Slice 1 lets GitHub auto-assign Squadron's check runs to the
  default check suite for the head commit. We do NOT create a
  custom check suite. Slice 2 candidate if SIEM consumers want
  to filter Squadron's checks separately.
- **External app installation.** The Checks API is most natively
  consumed via a GitHub App installation. Slice 1 uses the
  existing PAT-backed integration model from v0.89.23: the same
  PAT that opens PRs also creates check runs. PAT scope expands
  to include `repo:status` / `checks:write` (verified in §5).
  Slice 2 candidate: ship a Squadron GitHub App that operators
  install per-repo for finer-grained permission and rate-limit
  isolation.
- **Slack / Teams notifications when a check completes.** Slice
  1 ships the check run only. Downstream notification is a
  separate arc.
- **Cross-PR linking.** When the proposer cites prior accepted
  verdicts in the summary, slice 1 links those PR URLs as plain
  markdown links. Slice 2 candidate: render them as referenced
  check runs so the GitHub UI clusters them in the PR sidebar.
- **Check runs on PRs Squadron didn't open.** Slice 1 only
  creates check runs on PRs that have a Squadron-shaped branch
  name (`squadron/rec/...`). Operator-hand-opened PRs that look
  like Squadron recommendations get NO check run. This avoids
  Squadron writing to arbitrary PRs in repos it has read access
  to.

## 3. Architectural decision

Three architectural options surfaced during scoping. Picking one
materially shapes slice 2 and beyond.

### Option A — PAT-backed Checks API calls from existing iac_github client

Reuse the existing PAT credential. Add Checks API methods to the
existing `internal/iacrepo/githubclient.go`. Same auth, same
rate-limit pool, same audit trail. Slice 1 cost: small.

**Picked for slice 1.** Operational simplicity wins. PAT scope
upgrade (`checks:write`) is documented as a one-line edit in the
runbook; existing PATs auto-fail with a clear error if scope is
missing.

### Option B — Dedicated Squadron GitHub App

Install Squadron as a GitHub App per repo. App-level credential
isolation, per-repo permission scoping, separate rate-limit
budget, better long-term posture for multi-team enterprise
deployments. Slice 1 cost: large (app registration flow, OAuth
callback handler, installation token caching, separate audit
trail).

**Deferred to slice 2 (or later).** The benefit only matters at
scale (10+ repos, multi-team). Slice 1 operators with one or
two managed repos see no daylight between options A and B for
the first six months of usage. Building the App-level path
prematurely adds complexity without immediate operator value.

### Option C — Hybrid: PAT for opens, App for checks

Use the PAT for the existing Open PR path, then switch to an
App credential exclusively for Checks API writes. Avoids
expanding PAT scope. Slice 1 cost: medium (one new credential
path, dispatched separately).

**Rejected.** Splits the credential model in a way that confuses
operators. "Why does Squadron need two GitHub identities?" is a
question we don't want to answer in the runbook. If we move to
an App in slice 2, we move the entire integration; the hybrid
state is worse than either endpoint.

## 4. Signal direction

The webhook listener arc was unidirectional: GitHub → Squadron.
Squadron observed; the operator's PR review was the source of
signal. The Checks API arc inverts that direction: Squadron →
GitHub. Squadron now SPEAKS into the operator's review surface.

The signal carried in each direction:

**Inbound (existing, from webhook arc):**
- `recommendation.pr_merged` event with merge metadata
- `recommendation.pr_closed_not_merged` event (slice 2 chunk 3)
- `discovery_recommendation.excluded` event (slice 2 chunk 4,
  not directly observable from GitHub — operator-set)

**Outbound (slice 1 of this arc):**
- Check run created on Squadron-opened PR's head commit at the
  moment the PR is opened
- Check run UPDATED to conclusion=success when the inbound
  `recommendation.pr_merged` event fires
- Check run UPDATED to conclusion=failure when the inbound
  `recommendation.pr_closed_not_merged` event fires
- Check run UPDATED with new summary when the operator clicks
  Don't propose this again in the Recommendations tab (slice 2
  chunk 5, the new affordance shipped today) — the check run
  for the open PR gets a "Squadron now excluding this kind"
  banner in summary

The architectural elegance is that we already have all four
inbound signals wired. The outbound path is the missing leg.

## 5. PAT scope changes

The existing v0.89.23 webhook integration documents the PAT
scope as `repo` (full repo read + write). For check runs to work,
the PAT also needs:

- `repo:status` (alias for `checks:write` in most contexts — verify
  against GitHub's current scope docs at implementation time)

For a fine-grained PAT, the scope is `Checks: Read and write` on
the target repos.

**Migration posture:** slice 1 ships a runbook update that
documents the additional scope. Existing PATs that lack it will
fail the first `POST /check-runs` call with HTTP 404 (GitHub's
opaque "endpoint not found" for missing-scope cases) or HTTP 403
("Resource not accessible by integration"). Squadron catches
this, emits a structured `iac.check_run.scope_missing` audit
event with a humanized "Your PAT is missing the checks:write
scope. Add it at GitHub Settings → Tokens → <token> → Edit." and
returns a clean error from the create attempt. The PR itself
still opens; check run creation fails open.

This fail-open posture is load-bearing. Operators upgrading
from v0.89.x to the Checks API release MUST NOT have their PR
opens broken because their PAT is at the old scope. The check
run is a value-add; its absence does not block the existing
workflow.

## 6. Storage

Minimal additions. The check run ID is the only durable state
that needs to round-trip from creation to update.

### 6.1 New columns on the existing recommendation tracking row

The existing `iac.pr_opened` audit payload already carries
`pr_url`, `branch`, `head_sha`, `connection_id`, and
`recommendation_kind`. Slice 1 adds a sibling row in the IaC
recommendation tracking layer:

```sql
-- New optional columns on the iac_recommendation_verdicts table
-- (introduced in #531 slice 2 chunk 4). Reusing this table
-- because the lifecycle is tied to the same recommendation_id.
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_id INTEGER;
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_head_sha TEXT;
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_status TEXT;
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_conclusion TEXT;
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_updated_at TIMESTAMP;
```

Schema bump v8 → v9.

The reuse is deliberate: every recommendation that's been
exposed to the operator (whether merged, closed_not_merged, or
operator_excluded) has a row in this table already. The check
run is a parallel piece of the same lifecycle. Joining on
recommendation_id keeps the read path simple.

For recommendations that don't yet have a row (the operator
hasn't acted on them), check run state lives in a transient
in-memory map keyed by `(connection_id, pr_url)` until the row
is created on first audit-emitting action.

### 6.2 Alternative considered

A dedicated `iac_check_runs` table with its own foreign key to
the recommendation row. Cleaner separation, but the lifecycle
is too tightly coupled to justify the join cost. Two tables for
one logical entity violates the same principle that kept us
from creating a unified `verdicts` table in #531 slice 2.

## 7. Lifecycle

The check run state machine:

```
                 (Squadron opens PR)
                         |
                         v
                 status=in_progress
                conclusion=null
              "Squadron recommendation pending review"
                         |
        +----------------+------------------+
        |                                   |
   (PR merged via webhook)         (PR closed without merge via webhook)
        |                                   |
        v                                   v
  status=completed                    status=completed
  conclusion=success                  conclusion=failure
  "Operator merged. Squadron will     "Operator declined. Squadron will
   learn from this acceptance."        treat this shape as rejected
                                       signal on the next scan."
```

Optional intermediate transitions:

- Operator clicks Don't propose this again in Squadron's UI →
  check run updates to `status=completed`, `conclusion=neutral`,
  summary explains the exclusion was set. (Only fires if the
  underlying PR is still open; otherwise the merge/close
  conclusion has already fired.)
- A discovery rescan picks up that the resource state has
  reverted (slice 3 candidate) → check run reopens to
  `status=in_progress` with a new summary.

### 7.1 Timing guarantees

- Check run creation happens AFTER the PR creation succeeds.
  Order matters: if PR creation fails, no check run is created.
  If PR creation succeeds but check run creation fails, the PR
  still exists and is visible; the check run is best-effort.
- Check run updates happen INSIDE the webhook handler that
  consumes `pull_request.closed` events. The handler emits the
  audit event first (load-bearing), then issues the PATCH to
  GitHub. If the PATCH fails, the audit event still records
  the merge / close correctly; the check run stays in whatever
  state it was last left.

### 7.2 Idempotency

Every check run mutation includes the `head_sha` in the URL
path, which GitHub uses as part of the addressable identity.
If a force-push changes the head_sha mid-review, the check run
is orphaned on the old SHA. Slice 1 does NOT chase head_sha
changes — the check run stays on the original commit. Slice 2
candidate: subscribe to `pull_request.synchronize` webhook
events and re-create the check run on the new head SHA.

## 8. Audit trail

Three new audit event types covering the check run lifecycle:

```go
AuditEventIaCCheckRunCreated  = "iac.check_run.created"
AuditEventIaCCheckRunUpdated  = "iac.check_run.updated"
AuditEventIaCCheckRunFailed   = "iac.check_run.failed"
```

`iac.check_run.created` payload:

```go
{
  "connection_id": "<iac_connection.id>",
  "pr_url":        "https://github.com/acme/infra/pull/142",
  "head_sha":      "abc123...",
  "check_run_id":  12345678,
  "recommendation_kind": "rds-pi-em",
  "status":        "in_progress"
}
```

`iac.check_run.updated` payload: same fields plus
`previous_status` + `previous_conclusion` (nullable) +
`new_status` + `new_conclusion`. Records the transition.

`iac.check_run.failed` payload: connection_id, pr_url, head_sha,
plus an `error_kind` discriminator. Error kinds slice 1 handles:

- `scope_missing` — PAT lacks `checks:write`. Operator sees a
  humanized banner: "Squadron couldn't post a check run because
  your IaC PAT is missing the checks:write scope."
- `rate_limit` — GitHub API rate limit exceeded. Squadron logs
  the reset timestamp; the check run is dropped (no retry in
  slice 1).
- `pr_not_found` — Squadron's view of the PR diverged from
  GitHub's (e.g., operator deleted the PR before Squadron's
  Checks API call landed). Drop and log.
- `network` — any other transient error.

`iac.check_run.failed` is fail-open: the existing PR opens,
audit records `iac.pr_opened` as before, and the check run
failure surfaces as a separate event. SIEM consumers can build
dashboards on the failed event without polluting the success
path.

### 8.1 Humanizer extensions

In `internal/api/handlers/timeline.go`:

- `iac.check_run.created`: "Squadron posted a check run on PR
  #N in <repo> (kind=<kind>)."
- `iac.check_run.updated`: format the transition:
  - in_progress → success → "Squadron's check run marked SUCCESS
    on PR #N (operator merged)."
  - in_progress → failure → "Squadron's check run marked FAILURE
    on PR #N (operator closed without merge)."
  - in_progress → neutral → "Squadron's check run marked NEUTRAL
    on PR #N (operator excluded this kind from future
    recommendations)."
- `iac.check_run.failed`: "Squadron couldn't post a check run on
  PR #N: <error_kind>." With kind-specific suffix copy.

### 8.2 Existing audit events unchanged

`iac.pr_opened` and `recommendation.pr_merged` /
`recommendation.pr_closed_not_merged` keep their existing
payloads. The check run events are siblings, not extensions.

## 9. API integration

The Checks API client lives in `internal/iacrepo/checks.go`
(new file). Three methods:

```go
type CheckRunRef struct {
    Owner    string
    Repo     string
    CheckID  int64
    HeadSHA  string
}

type CheckRunCreate struct {
    Owner       string
    Repo        string
    HeadSHA     string
    Name        string         // "Squadron recommendation"
    Status      string         // "in_progress"
    StartedAt   time.Time
    Output      CheckRunOutput
}

type CheckRunOutput struct {
    Title   string  // "Recommendation: <kind>"
    Summary string  // first 65535 chars of markdown summary
    Text    string  // longer markdown if needed (optional)
}

type CheckRunUpdate struct {
    Ref         CheckRunRef
    Status      string  // "completed"
    Conclusion  string  // "success" | "failure" | "neutral"
    CompletedAt time.Time
    Output      CheckRunOutput  // updated summary
}

func (c *Client) CreateCheckRun(ctx context.Context,
    pat string, req CheckRunCreate,
) (CheckRunRef, error)

func (c *Client) UpdateCheckRun(ctx context.Context,
    pat string, req CheckRunUpdate,
) error
```

The client follows the existing `GoldenPath` pattern from
`githubclient.go`: structured request type → REST call →
structured response. Errors are wrapped into kind discriminator
strings that the audit emit path matches against.

### 9.1 Summary content

The summary markdown that the check run displays:

```markdown
**Squadron recommendation: <kind>**

This PR was opened by Squadron based on a discovery scan of
account <account>, region <region>, connection <connection_id>.

**What this PR does**
<recommendation reasoning, redacted via existing redact.go>

**Verdict learning context**
- Informed by N prior accepted PRs in this scope: [#142, #138]
- Informed by M prior closed-without-merge PRs in this scope: [#145]
- P operator-set exclusions in this scope (filtered before this scan)

**Why this recommendation**
<short rationale from the proposer's reasoning field>

[View in Squadron](https://your-squadron-host/discovery/aws/<connection_id>/recommendations#<recommendation_id>)
```

The summary is composed inside the bridge layer when the PR
opens, using the same `verdict_examples_used_by_state` map
chunk 6 added to the audit payload. Single source of truth for
the citation data; no duplicate query.

### 9.2 Update summary on conclusion

When the check run conclusion is set (merged or closed):

```markdown
**Squadron recommendation: <kind>** — <SUCCESS|FAILURE|NEUTRAL>

Operator <verb> this PR on <date>.

**What Squadron learned from this decision**
- Merged → "Squadron will treat <kind> as preference signal in
  this scope for the next 30 days."
- Closed without merge → "Squadron will treat <kind>=<shape> as
  rejected signal in this scope for the next 30 days. You may
  propose a different variation if evidence supports it."
- Neutral (operator-excluded) → "Squadron will not propose
  <kind> in this scope until you restore the recommendation
  from the Squadron Recommendations tab."

[View in Squadron](https://your-squadron-host/discovery/aws/<connection_id>/recommendations)
```

## 10. Slice 1 contract

**In:**

1. `internal/iacrepo/checks.go` (NEW): CreateCheckRun + UpdateCheckRun + the supporting types.
2. Audit event constants: `iac.check_run.created`, `iac.check_run.updated`, `iac.check_run.failed`.
3. Storage migration v8 → v9: 5 new optional columns on `iac_recommendation_verdicts` for check_run_id, head_sha, status, conclusion, updated_at.
4. New ApplicationStore methods: `SetCheckRunForRecommendation(ctx, recID, ref, status, conclusion)` and `GetCheckRunForRecommendation(ctx, recID) (ref, exists, err)`. Memory + sqlite impls.
5. Bridge integration: when the existing PR open path succeeds, follow up with `CreateCheckRun` using the verdict learning summary from #531 chunk 6's by-state map. On `iac.check_run.created` audit emit.
6. Webhook handler integration: when `recommendation.pr_merged` or `recommendation.pr_closed_not_merged` fires, look up the check run via `GetCheckRunForRecommendation` and call `UpdateCheckRun` with the success / failure conclusion + the lifecycle summary. Emit `iac.check_run.updated`.
7. Recommendations tab exclusion handler integration: when an operator excludes a kind that has an in-flight check run, look up the check run and call `UpdateCheckRun` with conclusion=neutral + exclusion summary. Emit `iac.check_run.updated`.
8. Fail-open posture: every Checks API call wraps errors into structured `error_kind` strings, emits `iac.check_run.failed` audit event, and returns nil to the caller. Existing PR open / merge / close paths complete normally.
9. Humanizer extensions in `timeline.go` for the three new event types.
10. Runbook update in `docs/webhook-listener.md` documenting the PAT scope requirement + the check run lifecycle expectation from the operator's perspective.
11. Tests:
    - TestCreateCheckRun_HappyPath
    - TestCreateCheckRun_MissingScope_EmitsFailedAudit
    - TestCreateCheckRun_RateLimited_EmitsFailedAudit
    - TestUpdateCheckRun_OnMerge_SuccessConclusion
    - TestUpdateCheckRun_OnClose_FailureConclusion
    - TestUpdateCheckRun_OnOperatorExclude_NeutralConclusion
    - TestCheckRunStorage_RoundTrip
    - TestHumanize_CheckRunEvents

**Out:**

- Required check (merge gating).
- File / line annotations.
- Operator actions (re-run button).
- Custom check suites.
- GitHub App installation path (option B from §3).
- Slack / Teams notification when check completes.
- Cross-PR linking that renders cited PRs as referenced check runs.
- Check runs on PRs Squadron didn't open.
- head_sha chasing on force-push.
- Slice 1 → slice 2 migration path for moving to a Squadron App.

## 11. Open questions

1. **PAT scope reality check.** GitHub's scope vocabulary
   (`repo`, `repo:status`, `checks:write`) has evolved across
   their PAT vs fine-grained PAT eras. Verify at implementation
   time which scope strings work today. Document the version
   tested.

2. **Check run name conflicts.** If an operator has another tool
   (Renovate, Dependabot) posting check runs on the same head
   commit, do they conflict on the same `name`? GitHub allows
   multiple check runs with the same name on the same commit
   (they show as a list); slice 1 ships with name="Squadron
   recommendation" and accepts the multi-run reality. Operators
   who want a single check can override via env-var
   `SQUADRON_CHECK_RUN_NAME` (added in slice 1).

3. **Recommendation IDs that don't have a row yet.** §6.1
   describes a transient in-memory map for check run state
   while the row doesn't exist. Concretely: when a PR opens but
   the operator hasn't acted yet, where does check_run_id live?
   Option A: create the iac_recommendation_verdicts row
   immediately on PR open with all status fields null except
   the check_run_id. Option B: keep a separate in-memory map.
   Pick option A — durable storage of "this recommendation has
   a check run" is more robust to Squadron restart than a
   memory map. The row exists with exclude_from_learning=0 and
   excluded_at/excluded_by both null; only the check_run_*
   fields are populated. Slice 1 implementation MUST pick
   option A even though it changes the existing meaning of
   "row exists in iac_recommendation_verdicts" from "operator
   has acted" to "Squadron has a check run on this PR."

4. **Conclusion timing race.** Webhook handler fires on PR
   merge, emits audit event, then calls UpdateCheckRun. What
   if Squadron's process restarts between the audit emit and
   the UpdateCheckRun? Slice 1: drop the check run update.
   The audit log records the merge correctly; the check run
   stays in_progress. Slice 2 candidate: a reconciliation job
   that compares iac_recommendation_verdicts.check_run_status
   against the audit log on startup and patches drift.

5. **Markdown XSS in operator notes.** The recommendation
   reasoning string is operator-untrusted in some contexts
   (e.g., the proposer ingested it from a Slack message via
   Ask Squadron). It's already redacted via redact.go, but
   markdown rendering in GitHub Check Runs could surface
   different injection vectors than the proposer's prompt
   builder cares about. Slice 1 escapes markdown special
   characters in the reasoning field before embedding into the
   check run summary. Verify with a targeted test that
   injection strings render as literal characters in the
   GitHub UI.

6. **Cost-spike side.** This entire proposal is discovery-side
   (PRs Squadron opens against IaC repos). The cost-spike side
   doesn't open PRs (it modifies Squadron's own rollouts), so
   there's no check run analog there. Confirmed scope: this
   arc is IaC / discovery only. Cost-spike has its own
   feedback loop (Slack notification on rollout state change)
   that lives outside this arc.

## 12. Threat model

The webhook listener arc's threat model was "untrusted POSTs
into Squadron's listener." This arc inverts: Squadron makes
outbound writes to GitHub. The threats are different.

### 12.1 Credential compromise

The PAT now has `checks:write` in addition to `repo`. A
compromised PAT can:

- Open PRs (existing risk; not new)
- Read all repo contents (existing risk; not new)
- Create check runs that gate merge (NEW risk if operator's
  branch protection treats Squadron's check name as required)

Mitigation:
- The runbook documents that operators should NOT mark
  Squadron's check as required in branch protection unless
  they've validated the recommendation quality for their
  team's risk tolerance.
- Squadron's check run name is namespaced (`Squadron
  recommendation`) — a compromised PAT can't impersonate a
  different tool's check.
- Slice 2's App-based credential model addresses this more
  cleanly via per-repo permission scoping.

### 12.2 Audit payload data exfiltration

The check run summary embeds operator-untrusted reasoning
text. The summary is markdown rendered by GitHub. Markdown
features that could exfiltrate data:

- Image embedding with arbitrary URLs (`![alt](http://exfil/...)`).
  GitHub does NOT auto-load images in markdown rendered by
  check runs (verified empirically at GitHub's docs); they
  require user click. Still: redact image markdown from the
  summary entirely. Recommendation reasoning is text, not
  embedded images.
- Link targets with URL-encoded payloads. Redact / escape
  links to ensure only `https://your-squadron-host/...` survives
  the encoder. Reasoning text gets ALL non-Squadron-host link
  syntax stripped.
- HTML embedding. GitHub's markdown renderer disallows raw HTML
  in check run summaries by default; verify and add a strip
  pass anyway.

### 12.3 Rate limit pressure

GitHub's REST API rate limit is 5000 requests/hour per
authenticated user. Squadron's PR open path is already inside
this budget; check run creates double the per-PR write cost.
For deployments with 100+ Squadron-opened PRs per day, this
could approach the limit.

Mitigation:
- Slice 1 ships a token bucket on the iacrepo client capped at
  100 requests/minute (well under the API ceiling) to smooth
  bursts.
- Squadron's rate-limit failure path (§8 `error_kind=rate_limit`)
  is fail-open. Drop the check run, log the reset timestamp,
  let the next request succeed.
- Slice 2 candidate: bin-pack multiple check run updates if
  the same head_sha has multiple recommendations on different
  PRs.

### 12.4 Markdown injection in operator-supplied verdict notes

The verdict examples used by state map (chunk 6) carries
operator-typed approver_notes / rejecter_notes from the
cost-spike side and PR URLs from the discovery side. The PR
URL path is safe (GitHub validates), but the notes are
untrusted user input that flows into the check run summary.

Mitigation: pass the cost-spike notes through the existing
redact.go pipeline before embedding. For the discovery side,
no notes carry through (only PR URLs); no additional mitigation.

## 13. Acceptance tests

1. **Happy path PR open → check run created.** Open a Squadron
   recommendation PR via the existing path. Assert: the
   `iac.pr_opened` audit event fires (existing), THEN the
   `iac.check_run.created` audit event fires with non-zero
   check_run_id, then `GetCheckRunForRecommendation` returns
   the stored ref.

2. **Missing PAT scope → check run fails, PR survives.** Open
   a PR with a PAT that lacks `checks:write`. Assert: PR is
   created (existing path), the `iac.check_run.failed` audit
   event fires with `error_kind=scope_missing`, the PR URL is
   accessible, no check_run_id stored.

3. **PR merged via webhook → check run conclusion=success.**
   Pre-seed a check run via #1. Fire a `pull_request` webhook
   with `merged=true`. Assert: `recommendation.pr_merged`
   fires (existing), then `iac.check_run.updated` fires with
   transition `in_progress → success`, then the stored check
   run row has conclusion=success.

4. **PR closed without merge → check run conclusion=failure.**
   Pre-seed a check run via #1. Fire a `pull_request` webhook
   with `merged=false`. Assert: `recommendation.pr_closed_not_merged`
   fires (existing, chunk 3), then `iac.check_run.updated`
   fires with transition `in_progress → failure`.

5. **Operator excludes kind → check run conclusion=neutral.**
   Pre-seed a check run. Operator POSTs to
   `/api/v1/discovery/aws/recommendations/exclude` (slice 2
   chunk 4 handler) with `excluded=true`. Assert:
   `discovery_recommendation.excluded` fires (existing chunk 4),
   then `iac.check_run.updated` fires with transition
   `in_progress → neutral`.

6. **Rate-limited → check run fails, PR survives.** Mock the
   iacrepo client to return rate_limit error on
   `CreateCheckRun`. Open a PR. Assert: PR is created, the
   `iac.check_run.failed` audit event fires with
   `error_kind=rate_limit`.

7. **Force-pushed head_sha → check run stays on original SHA.**
   Pre-seed a check run with head_sha=abc. Fire a
   `pull_request.synchronize` webhook with new head_sha=def.
   Assert: stored check_run_head_sha is still abc; no new
   check run is created.

8. **Storage round-trip.** Insert a check_run via
   `SetCheckRunForRecommendation`. Query via
   `GetCheckRunForRecommendation`. Assert: all fields match.

9. **Humanizer for created event.** Audit event with payload
   `{pr_url, recommendation_kind, check_run_id}` →
   `Squadron posted a check run on PR <N> in <repo> (kind=<kind>)`.

10. **Humanizer for updated event covering all 3 transitions.**
    Three audit events with the three transitions
    (in_progress → success / failure / neutral). Assert: each
    humanizes to the operator-friendly transition copy from §8.1.

11. **Markdown injection in operator notes is escaped.**
    Construct a cost-spike rollout verdict with notes
    containing `![exfil](http://attacker.com/leak.png)` and
    `<script>alert(1)</script>`. Build the check run summary
    via the slice 1 path. Assert: the summary contains the
    literal characters (escaped); no image markdown is
    preserved; no raw HTML survives the redaction pass.

12. **Cold-start parity.** Connection with zero recommendations,
    open a PR. Assert: `iac.pr_opened` fires as before, the
    `verdict_examples_used_by_state` field in the audit payload
    is absent (cold-start path from chunk 6), the check run
    summary still renders correctly with the cold-start
    template (no "informed by..." line).

---

**Slice 2 candidates (NOT in slice 1):**

- Required check (merge gating) with operator-level toggle
- Per-file / per-line annotations sourced from
  `affected_resources`
- Re-run action button on the check run
- Custom check suite for namespacing Squadron's checks
- Squadron GitHub App installation path (replaces PAT)
- Slack / Teams notification on check completion
- Cross-PR linking via referenced check runs
- Head SHA chasing on force-push (subscribe to
  `pull_request.synchronize`)
- Reconciliation job that patches drift between audit log and
  check run state on Squadron restart
- Bin-packed check run updates for repos with many concurrent
  PRs at the same head SHA
- Check runs on operator-hand-opened PRs that match a
  recommendation kind heuristic (operator opens an
  `eks-observability-addon` PR by hand; Squadron posts an
  informational check)
