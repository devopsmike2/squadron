# GitHub Checks API back-signal — operator runbook

This is the operator-facing runbook for the v0.89.44 GitHub Checks
API back-signal arc that closes slice 1 of the
[checks-api-back-signal design doc](./proposals/checks-api-back-signal.md).
It covers verifying your PAT scope, opening a Squadron PR to confirm
the check run appears, reading the audit signal for each of the three
new event types, and the troubleshooting matrix when something
doesn't fire.

If you haven't yet wired the inbound GitHub webhook listener, start
there instead: [webhook-listener.md](./webhook-listener.md). The
Checks API arc is the inverse direction of that arc — the webhook
listener tells Squadron when a PR merges; the Checks API tells GitHub
operators what Squadron's reasoning was. The two arcs work
independently, but the lifecycle becomes legible end-to-end only when
both are live.

For a first test against a personal sandbox repo with an existing IaC
connection, budget about 10 minutes — most of it spent in GitHub's
Settings → Tokens UI verifying the PAT scope. For a production setup
against an org-owned repo, budget 30 minutes plus whatever your org's
change-management process requires for PAT scope expansion.

## What we're building

Three things, in this order:

1. **A PAT with the `checks:write` scope** (or fine-grained
   equivalent: `Checks: Read and write` on the target repos). The
   same PAT that opens Squadron PRs is the PAT that creates check
   runs — design doc §3 option A, picked for slice 1's operational
   simplicity. If your existing PAT was minted with `repo` scope
   only, the first check-run create attempt will fail open with a
   structured `iac.check_run.failed` audit event whose `error_kind`
   is `scope_missing` and the PR open still completes.
2. **The chunk-2 PR-open follow-up wired on the IaC GitHub handler**
   (`SetIaCChecksClient`, `SetSquadronHost`, optional
   `SetCheckRunName`). When wired, every Squadron-opened PR gets a
   GitHub check run created on its head commit with the proposer's
   reasoning + the verdict-learning context block from chunk 6 of
   #531 slice 2. The chunk-2 wiring is the bridge.
3. **The chunk-3 webhook handler integration and the chunk-4
   exclusion handler integration**, which PATCH the same check run
   to a final conclusion (`success` on merge, `failure` on close
   without merge, `neutral` when the operator clicks "Don't propose
   this again"). These two paths close the lifecycle.

The result: every Squadron-initiated PR shows up in the GitHub PR
review surface with a **"Squadron recommendation"** check run that
walks through Squadron's reasoning, the verdict-learning citations,
and a deep link back into Squadron's Recommendations tab. When the
operator merges, closes, or excludes, the check run transitions to
its final conclusion and the audit timeline records every
transition.

## What this is good for

- **Operator visibility in the PR review surface.** Operators
  reviewing the PR diff in GitHub see Squadron's reasoning inline,
  including the prior-accepted-PR citations from
  `verdict_examples_used_by_state`, without bouncing back to
  Squadron's UI. The PR-review-as-source-of-truth team workflow
  stays inside GitHub.
- **Audit correlation.** The `iac.check_run.created` event carries
  `recommendation_id`, `pr_url`, `check_run_id`, `head_sha`, and
  `connection_id`. SIEM consumers join the inbound
  `recommendation.pr_opened` event with the outbound
  `iac.check_run.created` event on `recommendation_id` for the
  complete lifecycle.
- **SIEM dashboarding.** The structured `error_kind` discriminator
  on `iac.check_run.failed` (`scope_missing`, `rate_limit`,
  `pr_not_found`, `network`) lets SIEM consumers build separate
  panels for each failure class. The fail-open posture means
  failures never block PR opens; the dashboards expose latent
  problems (e.g., a PAT slowly losing scope after a rotation)
  before they become operator-visible.
- **Single source of truth for "what Squadron told the operator."**
  The check-run summary embeds the same redact.go-cleaned reasoning
  the recommendations tab renders, so the summary remains stable
  even when the underlying recommendation row mutates.

## What this is NOT

Read this list carefully. Slice 1 explicitly defers each of these
to slice 2 or later; none of them are bugs in the v0.89.44
implementation. See design doc §2 for the rationale on each.

- **Required check runs that gate merge.** Slice 1 ships every
  check run as informational (`required=false`). Operators who
  want merge gating can set Squadron's check name as required in
  their repo's branch protection settings, but Squadron does not
  assume or mandate that. Required-by-default is a slice 2 design
  question with real operational weight (it changes Squadron from
  a recommendation surface into a merge gate, and not every team
  wants that).
- **Per-file / per-line annotations.** The GitHub Checks API
  supports per-annotation positioning that surfaces as inline PR
  comments. Slice 1 ships the check run with summary + text only.
  Annotations are a slice 2 candidate — the discovery proposer's
  `affected_resources` field already carries enough information to
  draft them, but the UX of inline annotations needs its own
  scoping pass.
- **Re-run / Re-evaluate action buttons on the check run.** GitHub
  supports `actions` on a check run that surface as buttons. Slice
  1 ships read-only check runs; no actions. Slice 2 candidate.
- **Custom check suites.** Slice 1 lets GitHub auto-assign
  Squadron's check runs to the default check suite for the head
  commit. We do NOT create a custom check suite. Slice 2 candidate
  if SIEM consumers want to filter Squadron's checks separately.
- **GitHub App credential.** Slice 1 uses the existing PAT-backed
  integration model from the v0.89.23 webhook arc. The PAT scope
  expands to include `checks:write`; everything else stays the
  same. Slice 2 candidate: ship a Squadron GitHub App that
  operators install per-repo for finer-grained permission
  isolation.
- **Slack / Teams notifications when a check completes.** Slice 1
  ships the check run only. Downstream notification is a separate
  arc.
- **Cross-PR linking.** When the proposer cites prior accepted
  verdicts in the summary, slice 1 links those PR URLs as plain
  markdown links. Slice 2 candidate: render them as referenced
  check runs so the GitHub UI clusters them in the PR sidebar.
- **Check runs on PRs Squadron didn't open.** Slice 1 only creates
  check runs on PRs that have a Squadron-shaped branch name
  (`squadron/rec/...`). Operator-hand-opened PRs that look like
  Squadron recommendations get NO check run.
- **Head SHA chasing on force-push.** Slice 1 does NOT subscribe
  to `pull_request.synchronize` events. If a force-push moves the
  head SHA mid-review, the check run is orphaned on the old SHA.
  Slice 2 candidate.

## Prerequisites

- A Squadron deployment on **v0.89.44 or later**. Earlier versions
  don't have the chunks 2/3/4 wiring; the `iac.check_run.*` audit
  event constants exist as of v0.89.42 (chunk 1) but the bridge
  layer that emits them ships in v0.89.43 (chunk 2) and the
  webhook + exclusion integrations ship in this release (v0.89.44).
- An existing IaC connection
  ([discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md))
  with a working PAT, configured well enough that the existing
  Open PR loop ships PRs to GitHub.
- A PAT scope that includes `checks:write`. For classic PATs:
  expanding `repo` to include `repo:status` covers `checks:write`
  in most contexts; verify with the curl in Step 1 below. For
  fine-grained PATs: the explicit scope is `Checks: Read and
  write` on the target repos.

## Step 1 — Verify or upgrade your PAT scope

The cleanest one-shot check is GitHub's `/user` endpoint, which
surfaces the PAT's effective scopes in the `X-OAuth-Scopes` response
header:

```sh
curl -i -H "Authorization: token $YOUR_PAT" https://api.github.com/user 2>&1 | grep -i x-oauth-scopes
```

You should see a line like:

```text
X-OAuth-Scopes: repo, checks:write
```

If `checks:write` (or its classic-PAT alias `repo:status`) is
absent, you have two options:

1. **Mint a new PAT** with the expanded scope (recommended for
   classic PATs — GitHub's edit-PAT flow regenerates the secret on
   every edit, so you're not saving any operational complexity by
   editing in place).
2. **Edit the existing fine-grained PAT** at GitHub Settings →
   Developer settings → Personal access tokens → Fine-grained
   tokens → your token → Permissions → Repository permissions →
   Checks → "Read and write." The token's secret stays unchanged.

For PATs minted by the Connect IaC repo wizard (v0.89.32+), the
wizard does NOT mint with `checks:write` today; that's a slice 2
wizard polish item. For now, the recommended path is: mint the PAT
manually with the expanded scope, then paste it into the wizard.

After updating the PAT, restart Squadron so the chunk-2 bridge
picks up the new credential. Squadron does NOT hot-reload PATs in
slice 1 — there's no /reload endpoint and no SIGHUP handler.

## Step 2 — Open a PR to verify the check run appears

The cleanest test loop is to drive a discovery recommendation all
the way through the existing Open PR path:

1. **Open a Squadron PR.** From the Recommendations tab on the
   Discovery page, find a recommendation against the repo you
   wired up, and click **Open PR**. Squadron creates a branch
   under `squadron/rec/<kind>/<id>`, commits the proposer's
   snippet, opens the PR, and (with chunk-2 wiring complete)
   immediately follows up with a `POST /repos/:owner/:repo/check-runs`
   call against the PR's head commit. The Timeline page shows
   **"Opened PR #N in github.com/<repo> for <kind>"** as before,
   followed by **"Squadron posted a check run on PR #N in
   <owner>/<repo> (kind=<kind>)"**.
2. **Open the PR in GitHub.** The PR's **Checks** tab now shows
   **"Squadron recommendation"** (or whatever
   `SQUADRON_CHECK_RUN_NAME` was overridden to) with the title
   **"Squadron recommendation: <kind>"** and a summary that walks
   through:
   - The scope tuple (account, region, connection).
   - **What this PR does** — the proposer's reasoning, after the
     existing redact.go + the chunk-2 markdown-injection escape
     pass.
   - **Verdict learning context** — the prior-accepted +
     closed-without-merge + operator-excluded citations from
     chunk 6 of #531 slice 2, when present. Cold-start PRs (no
     prior verdicts in scope) omit this section entirely.
   - **[View in Squadron]** — the deep link back to the
     recommendations tab anchored on this `recommendation_id`.
3. **Confirm the audit signal.** Open the Timeline page. The most
   recent event for this PR should be **"Squadron posted a check
   run on PR #N in <owner>/<repo> (kind=<kind>)"**.

If the check run doesn't appear within 5 seconds of the PR open,
the most likely cause is a missing PAT scope; see Step 4
troubleshooting matrix below. The PR itself still opens (that's
the load-bearing fail-open posture from design doc §5).

To exercise the three transition paths:

- **Merge the PR.** The chunk-3 webhook handler PATCHes the check
  run to `conclusion=success` and emits
  **"Squadron's check run marked SUCCESS on PR #N (operator
  merged)."**
- **Close the PR without merging.** The chunk-3 webhook handler
  PATCHes to `conclusion=failure` and emits
  **"Squadron's check run marked FAILURE on PR #N (operator closed
  without merging)."**
- **Click "Don't propose this again" on the recommendations tab.**
  The chunk-4 exclusion handler PATCHes to `conclusion=neutral`
  and emits **"Squadron's check run marked NEUTRAL on PR #N
  (operator excluded this kind from future recommendations)."**

Each transition fires once. The check run stays at its final
conclusion thereafter; subsequent operator actions on a completed
check run are no-ops by design (see design doc §7).

## Step 3 — Reading the audit signal

Three new audit event types ship in this arc, each carrying a
distinct payload shape. Examples below are pulled from a real
local test loop; field-by-field documentation follows each.

### `iac.check_run.created` (chunk 2 of #663 Stream 61)

Fires once per Squadron-opened PR, immediately after the existing
`recommendation.pr_opened` audit event.

```json
{
  "event_type": "iac.check_run.created",
  "actor": "system",
  "target_type": "iac_recommendation",
  "target_id": "<connection_id>",
  "action": "check_run_created",
  "payload": {
    "connection_id":       "conn-abc",
    "recommendation_id":   "rec-xyz",
    "recommendation_kind": "rds-pi-em",
    "pr_url":              "https://github.com/octo/widgets/pull/142",
    "head_sha":            "abc123def456...",
    "check_run_id":        9001,
    "owner":               "octo",
    "repo":                "widgets",
    "status":              "in_progress",
    "account_id":          "111111111111",
    "region":              "us-east-1",
    "actor":               "system",
    "recorded_at":         "2026-06-22T10:00:00Z"
  }
}
```

To query this event from the audit API:

```sh
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://your-squadron-host/api/v1/audit/events?event_type=iac.check_run.created&limit=10"
```

### `iac.check_run.updated` (chunks 3 + 4)

Fires once per PR-merge, PR-close-without-merge, or
operator-exclude transition. The chunk-3 webhook handler emits the
success / failure transitions; the chunk-4 exclusion handler emits
the neutral transition.

```json
{
  "event_type": "iac.check_run.updated",
  "actor": "system",
  "target_type": "iac_recommendation",
  "target_id": "<connection_id>",
  "action": "check_run_updated",
  "payload": {
    "connection_id":        "conn-abc",
    "recommendation_id":    "rec-xyz",
    "recommendation_kind":  "rds-pi-em",
    "pr_url":               "https://github.com/octo/widgets/pull/142",
    "head_sha":             "abc123def456...",
    "check_run_id":         9001,
    "owner":                "octo",
    "repo":                 "widgets",
    "previous_status":      "in_progress",
    "previous_conclusion":  "",
    "new_status":           "completed",
    "new_conclusion":       "neutral",
    "actor":                "system",
    "recorded_at":          "2026-06-22T11:00:00Z"
  }
}
```

The `new_conclusion` field carries the load-bearing transition
signal:

- `"success"` — operator merged the PR (chunk 3, webhook handler).
- `"failure"` — operator closed the PR without merging (chunk 3,
  webhook handler).
- `"neutral"` — operator clicked "Don't propose this again" on a
  PR that was still open (chunk 4, exclusion handler).

To query the neutral transitions specifically:

```sh
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://your-squadron-host/api/v1/audit/events?event_type=iac.check_run.updated&limit=20" \
  | jq '.events[] | select(.payload.new_conclusion == "neutral")'
```

### `iac.check_run.failed` (chunks 2/3/4)

Fires whenever any chunk's GitHub Checks API call returned a
structured `*CheckRunError`. Fail-open: the upstream PR
open / merge / close / exclude path still completed normally; only
the check-run side-effect dropped.

```json
{
  "event_type": "iac.check_run.failed",
  "actor": "system",
  "target_type": "iac_recommendation",
  "target_id": "<connection_id>",
  "action": "check_run_failed",
  "payload": {
    "connection_id":       "conn-abc",
    "recommendation_id":   "rec-xyz",
    "recommendation_kind": "rds-pi-em",
    "pr_url":              "https://github.com/octo/widgets/pull/142",
    "head_sha":            "abc123def456...",
    "error_kind":          "scope_missing",
    "http_status":         403,
    "error_message":       "PAT lacks checks:write scope",
    "actor":               "system",
    "recorded_at":         "2026-06-22T10:00:00Z"
  }
}
```

The `error_kind` field is the SIEM dashboard fan-out signal. Four
slice-1 values, in design doc §8 order:

- `"scope_missing"` — PAT lacks `checks:write` (or fine-grained
  equivalent). The Step 4 matrix below has the fix.
- `"rate_limit"` — GitHub REST API rate limit exceeded. Slice 1
  does NOT retry; the check run is dropped. The
  `error_message` field carries `reset=<unix-timestamp>` so the
  SIEM dashboard's "when does this clear?" panel can read it
  without parsing prose.
- `"pr_not_found"` — Squadron's view of the PR diverged from
  GitHub's (operator deleted the PR, force-push orphaned the
  SHA). Drop and log.
- `"network"` — transport-level errors, 5xx responses, or any
  other 4xx the wrapper doesn't classify. The "I don't know,
  drop it" branch — SIEM dashboards group these as transient.

To filter on just the scope-missing failures:

```sh
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://your-squadron-host/api/v1/audit/events?event_type=iac.check_run.failed&limit=50" \
  | jq '.events[] | select(.payload.error_kind == "scope_missing")'
```

## Step 4 — Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `iac.check_run.failed` with `error_kind=scope_missing` | PAT lacks `checks:write` (or its classic-PAT alias `repo:status`) | Add `checks:write` to the PAT at GitHub Settings → Tokens → your token → Edit (fine-grained) or Mint a new classic PAT with the expanded scope, then restart Squadron |
| `iac.check_run.failed` with `error_kind=rate_limit` | GitHub REST API rate limit (5000 req/hour per PAT) exhausted | Wait for `X-RateLimit-Reset` (the timestamp is embedded in `error_message`). Slice 1 ships a 100 req/min token bucket in the client to smooth bursts; sustained pressure requires a slice-2-style App credential with its own rate-limit budget |
| `iac.check_run.failed` with `error_kind=pr_not_found` | PR was deleted on the GitHub side between PR-open and check-run-create, OR a force-push orphaned the original head SHA | No fix — slice 1 does not chase head_sha changes. The PR open was honest; the check run is best-effort. If this fires consistently, investigate why your team is deleting PRs mid-review |
| `iac.check_run.failed` with `error_kind=network` | Transport-level error, GitHub 5xx, or an unclassified 4xx | Check Squadron's outbound connectivity to api.github.com; check GitHub's status page; if it persists, the `error_message` field carries the wrapper's diagnostic — file an issue if the message looks like a misclassification |
| PR opens, but NO `iac.check_run.*` audit event fires | Squadron deployment wasn't built / configured with the chunk-2 wiring (nil `iacChecksClient` or empty `SQUADRON_PUBLIC_HOST` / PAT) | Verify `SetIaCChecksClient` is called in your deployment's wiring layer; verify `SQUADRON_GITHUB_TOKEN` (or your fine-grained equivalent) is set with `checks:write` scope; restart Squadron |
| Check run appears on GitHub but NO `iac.check_run.created` audit event fires | The audit service is unwired on the IaCGitHubHandlers (rare; happens in test_server.go-style deployments) | Wire `WithAuditService` on the IaCGitHubHandlers builder; restart Squadron |
| Operator clicks "Don't propose this again" but the check run stays `in_progress` | One of: chunk-4 wiring missing (nil checksClient / nil checkRunStore / empty PAT), no check-run row for this `recommendation_id` (chunk-2 bridge never opened a PR for it), or the check run was already PATCHed to `completed` by the merge / close webhook | Confirm `SetIaCChecksPAT` is called in server wiring (see internal/api/server.go::discoveryTrampoline); confirm the audit log shows an earlier `iac.check_run.created` for this `recommendation_id`; if the run is already `completed`, the design doc §7 invariant intentionally prevents overwriting the final conclusion |

## Slice 2 roadmap

The slice-1 trade-offs most likely to shift in later releases, in
rough priority order (see design doc §13 for the full list with
rationale):

- **Required check (merge gating) with operator-level toggle.** A
  per-connection setting that lets the operator opt Squadron's
  check run into the repo's branch-protection required list with
  one click. The hard work is the UX: operators need to
  understand they're consenting to Squadron being able to block
  merge.
- **Per-file / per-line annotations sourced from
  `affected_resources`.** The discovery proposer already emits
  the resource list; the slice-2 work is rendering it as inline
  PR comments at the right file + line range. Coordinating the
  annotation positions with HCL patching is the design-bearing
  question.
- **Re-run action button on the check run.** Surfaces as a
  GitHub-native button operators can click to trigger a fresh
  discovery scan in scope. Requires a callback URL plus signing
  so a forged callback can't fan out a scan.
- **Squadron GitHub App installation path.** Replaces the PAT
  with an installation token cached per-repo. Better long-term
  posture for multi-team enterprise deployments. Slice 1's PAT
  posture is the right starting point; the App migration is
  meaningful only at scale (10+ repos, multi-team).
- **Head SHA chasing on force-push.** Subscribe to
  `pull_request.synchronize` webhook events and re-create the
  check run on the new head SHA. Pairs naturally with the
  webhook listener arc's slice-2 work.
- **Reconciliation job that patches drift between audit log and
  check run state on Squadron restart.** Defends against the
  conclusion-timing-race (design doc §11 Q4): if Squadron
  restarts between the audit emit and the UpdateCheckRun, the
  check run stays `in_progress` while the audit log says
  `merged`. A reconciliation sweep on startup makes them
  consistent.
- **Bin-packed check run updates for repos with many concurrent
  PRs at the same head SHA.** Optimization for high-volume
  scopes; slice 1's per-PR mutation cost is fine at single-digit
  PRs per day.

None of these ship today; everything in this runbook describes
behavior you can rely on as of v0.89.44.

## Cross-references

- [GitHub Checks API back-signal — design doc](./proposals/checks-api-back-signal.md) —
  the design rationale, the slice-1 contract, the slice-2 candidate
  list, and the threat model.
- [GitHub webhook listener](./webhook-listener.md) — the inverse
  direction: GitHub tells Squadron when a PR merges. The chunk-3
  half of this arc rides on the webhook handler; the runbook there
  documents the inbound side.
- [Discovery IaC first-time setup](./discovery-iac-first-time-setup.md) —
  prerequisite for both arcs. Walks the IaC connection wizard plus
  the "Open PR" loop. The check run is the value-add on top of the
  PR-open flow documented there.
- [Discovery proposer feedback loop](./discovery-proposer-learning.md) —
  the verdict-learning context the check-run summary cites comes
  from chunk 6 of #531 slice 2. The bridge layer reads the same
  `verdict_examples_used_by_state` map both surfaces consume.
- [Audit log](./audit-log.md) — full catalog of audit event types
  and target types. The `iac.check_run.created`,
  `iac.check_run.updated`, and `iac.check_run.failed` trio
  documents alongside the rest of the IaC arc.
- [API reference](./api-reference.md) — the REST surface for
  `/api/v1/audit/events` (the source of the curl examples above).
