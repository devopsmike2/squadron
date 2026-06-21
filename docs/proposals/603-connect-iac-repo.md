# #603 — Connect IaC repo for PR-based recommendation handoff

**Status:** proposal, slice-1 scoping. Slice 5+ candidate.
**See also:** [universal-discovery-design.md](../universal-discovery-design.md),
[discovery-aws-first-time-setup.md](../discovery-aws-first-time-setup.md).

## 1. Problem

Discovery scans end with the operator copy-pasting Terraform.
`HandleAWSGenerateRecommendations`
([`discovery.go:1431`](../../internal/api/handlers/discovery.go))
returns each plan step's `InlineConfigSnippet` (from
`ProposeFromDiscoveryScan`,
[`proposer_discovery.go:218`](../../internal/ai/proposer_discovery.go))
as a `recommendations.IaCSnippet`. The Recommendations tab renders it
behind a "Copy" button. The operator pastes into their terraform
repo, opens a PR by hand, their CI runs `terraform apply`. The
copy-paste step is the entire friction — operators drop the workflow
there.

"Connect IaC repo" closes the loop: the operator connects their
terraform repo via a wizard symmetric to the AWS connect wizard.
"Copy" becomes "Open PR". Squadron writes a branch, commits the
snippet at the file the operator declared at connect time, opens a
PR. The operator reviews and merges in GitHub; their existing CI runs
`terraform plan`/`apply`. Squadron never holds cloud write creds and
never runs terraform.

## 2. Non-goals (slice 1)

- GitLab, Bitbucket, Azure DevOps. GitHub only.
- Auto-merge. Every PR requires an explicit human merge.
- Monorepo path detection. Operator declares one file per
  `(provider, resource_kind)` at connect time.
- Squadron running `terraform plan`/`apply`. Operator's CI is the
  executor.
- Programmatic rollback / close of previously-opened PRs.
- Updating a PR after open. Each "Open PR" click is a new branch + PR.
- Cross-step batching. One PR per step.
- HCL-aware merging. We append; we do not edit.

## 3. Symmetry with cloud-connect

Reuses the declarative `ConnectorWizard` shell from Stream 2D
(`universal-discovery-design.md` §"Connector workflow design").
Routes mirror AWS: `POST /api/v1/iac/github/validate`
(test-before-commit, zero records — same shape as `HandleAWSValidate`,
[`discovery.go:264`](../../internal/api/handlers/discovery.go)) and
`POST /api/v1/iac/github/connections` (Save — mirrors
`HandleAWSSaveConnection`,
[`discovery.go:379`](../../internal/api/handlers/discovery.go)).

Wizard steps: (1) Provider = GitHub. (2) Install Squadron App
(deep-link to `/installations/new` on the operator's org; PAT lives
behind an Advanced disclosure — §4). (3) Pick repo from the install's
granted set, single-select. (4) Pick default branch, pre-filled from
`default_branch`. (5) Declare placement map (§6). (6) Validate —
confirms repo + branch + every declared file exists, renders one
preflight row per resource kind, same shape as
`awsValidatePreflightRow`
([`discovery.go:244`](../../internal/api/handlers/discovery.go)).
(7) Save.

Release-blocking criteria from the eleven connector principles apply
unchanged: under-5-minute connect, copy-to-clipboard everywhere,
humanized errors with `SuggestedStep` jump-backs.

## 4. Trust model: GitHub App vs PAT

**Preferred: a Squadron GitHub App, installed per-repo.**
`contents:write` + `pull_requests:write` ONLY on the repos the
operator ticked at install — a PAT with `repo` is org-wide.
Installation tokens are short-lived (1h, GitHub-issued); Squadron
mints them on-demand from the App private key, parallel to STS-token
minting on the AWS path (`universal-discovery-design.md`
§"STS token lifecycle"). One-click revoke in org admin. App identity
is the PR author.

**Fallback: classic PAT, scoped `repo`.** Ships because OSS
deployments may not have a public callback URL the App needs. Lives
behind an Advanced disclosure with a visible warning. Compliance Pack
hardening disables the PAT path entirely (parallel to
`universal-discovery-design.md` §"Compliance Pack hardening").

**Where the secret lives.** Existing credstore substrate (Stream 2A).
A new sibling table `iac_connections` mirrors `CloudConnection`'s
opaque-ciphertext shape
([`store.go:107`](../../internal/discovery/credstore/store.go));
marshalling helpers `MarshalGitHubAppCreds` /
`MarshalGitHubPATCreds` parallel `MarshalAWSCredentials`
([`aws.go:48`](../../internal/discovery/credstore/aws.go)), sealed by
the same `credstore.Key`. No plaintext token in any audit payload —
the ExternalID invariant generalizes.

## 5. Recommendation → PR flow

1. Scan completes; `ProposeFromDiscoveryScan` returns plan steps
   with `InlineConfigSnippet`. Unchanged.
2. Recommendations tab renders. When an IaC connection exists for
   the step's `(provider, resource_kind)`, "Copy" is joined by
   **"Open PR"**.
3. Operator clicks. Squadron resolves the placement-map row, creates
   branch `squadron/rec-<scan_id>-<step_idx>` off the default
   branch, commits the snippet appended to the declared file (§7),
   opens the PR with base = default branch, head = new branch.
   **Squadron never pushes to the default branch — invariant.**
4. **Operator reviews in GitHub.** Branch protection (CODEOWNERS,
   required reviewers, required CI checks) is the gate. **This is
   where the operator's hand is required and where the security
   thesis is preserved** — exactly as the operator's hand was the
   gate in the copy-paste pattern.
5. Operator merges (or closes).
6. **Operator's existing CI runs `terraform plan`/`apply`.** Squadron
   is uninvolved. Their CI is the executor — same as the copy-paste
   pattern.
7. Next scan re-walks; the proposer stops surfacing the
   recommendation because the resource now shows as covered.

Squadron's role across steps 3 is **PR authorship**. PR authorship
is not infrastructure execution. The action-runner separation
contract (`universal-discovery-design.md` §"Action runner separation
contract") holds without modification.

## 6. Operator-supplied placement map

At connect time the operator declares one file per
`(provider, resource_kind)`. Slice 1 ships seven rows covering the
slice-1+2+3a+3b proposer outputs already in `proposer_discovery.go`:
`ec2-otel-layer`, `lambda-otel-layer`, `rds-pi-em`,
`s3-access-logging`, `alb-access-logs`, `eks-cluster-logging`,
`eks-observability-addon`. Each row carries one repo-relative path
(e.g. `modules/eks/main.tf`). Stored as JSON on the IaC connection
row; additive when new kinds (ECS, GCP) ship.

If a step's kind is not in the map, the "Open PR" button is replaced
by a tooltip naming the missing row. Snippet stays copyable via the
pre-slice-1 path.

## 7. Snippet → PR translation

**Append, not edit.** Slice 1 appends the snippet at the file's end
with one trailing newline. We do not parse HCL, merge `resource`
blocks, or deduplicate. A wrong surgical edit silently corrupts the
operator's infra description; a duplicate-resource error from
`terraform plan` is immediately legible. The operator's PR review
catches anything subtler.

**Conflict handling.** Branch-name collision → append `-<unix-ts>`.
File-SHA disagrees with what we read (default branch moved) → refresh
HEAD once, note the rebase in the PR body, no further retry.

**PR title:** `Squadron: instrument <resource_kind> for <count>
resources (scan <scan_id_short>)`. **PR body:** proposer reasoning,
affected resources (linked to the inventory tab), the snippet in a
fenced block, and a footer naming the orchestrator-not-executor
posture and that Squadron will not push to this branch again.
**Labels:** `squadron`, `squadron/<resource_kind>`.

## 8. Audit story

New event types, registered alongside the `discovery.aws.*` family
([`discovery.go:534`](../../internal/api/handlers/discovery.go) for
the pattern):

- `iac.github.connection_created` — payload: `connection_id`,
  `repo_full_name`, `default_branch`, `auth_kind` (`app`|`pat`),
  `placement_map`. NEVER the token.
- `iac.github.connection_validated` — payload: `repo_full_name`,
  `default_branch`, `preflight_results[]`.
- `recommendation.pr_opened` — payload: `scan_id`, `step_idx`,
  `account_id`, `repo_full_name`, `pr_number`, `pr_url`, `branch`,
  `commit_sha`, `file_path`, `actor`. NEVER the snippet content —
  audit rows must not scale with snippet size (same rule as
  `discovery.aws.recommendations_generated`,
  [`discovery.go:1446`](../../internal/api/handlers/discovery.go)).
- `recommendation.pr_open_failed` — adds `error_code` +
  `humanized_message`.
- `recommendation.pr_merged` — webhook-driven. Drives "marked
  applied" status without a manual click.
- `recommendation.pr_closed` — webhook, closed without merge.

Family uses a new `TargetTypeIaCRecommendation` so the timeline
humanizer groups them.

## 9. Threat model

| Threat | Mitigation |
| ------ | ---------- |
| Host compromise leaks the GitHub App private key. | Per-repo install scope already bounds blast radius to the ticked repos. Compliance Pack adds HSM-backed key (parallel to `universal-discovery-design.md` §"Compliance Pack hardening"). One-click revoke in org admin. |
| Host compromise leaks a PAT. | Worse — org-wide `repo` scope. Mitigation: PAT is behind an Advanced disclosure with a visible warning; Compliance Pack disables it entirely. |
| Operator renames or transfers the repo. | GitHub returns 404 on next "Open PR". Humanized error names the recovery step ("re-run the IaC connect wizard"). Audit `recommendation.pr_open_failed`. |
| Operator force-pushes the default branch between Squadron's read and write. | Our branch is off the new HEAD; PR body notes the rebase. We never write `main` directly. |
| Operator-side bot auto-merges Squadron PRs. | A thesis violation but the operator's choice. PR-body footer names the risk. Compliance Pack can require a `squadron/manual-merge` label bots won't satisfy. |
| Malicious recommendation injection (poisoned scan → bad HCL → PR). | Same threat as today's snippet-injection path (`universal-discovery-design.md` §"Threat: malicious recommendation injection"). The PR is reviewable in GitHub. The operator's review is the same operator-in-the-loop defense as the copy-paste pattern. No auto-merge. |
| Compromised Squadron force-pushes the default branch (escalation attempt). | Operator's GitHub branch protection prevents it. The GitHub-client wrapper additionally refuses any write to the default-branch ref the same way the AWS scanner refuses write-action call sites — defense in depth at the code layer, not just GitHub-side policy. |
| Bot impersonation in the operator's repo. | App identity is the PR author on the App path; PAT-owner on the PAT path. `recommendation.pr_opened.actor` carries the Squadron operator identity for cross-system trace. |

## 10. Slice 1 contract

**In:** One GitHub repo per IaC connection, one connection per
deployment. App + PAT auth (App preferred). The seven placement-map
rows in §6. "Open PR" on each recommendation card when a placement
row exists. Append-only file edit. The six audit events in §8.
Validate + Save endpoints mirroring AWS.

**Out:** Everything in §2; multi-repo per connection; inferring
placement from existing `*.tf` content; updating an existing PR;
per-kind PR batching; the webhook-driven "marked applied" UI badge
(listener ships in slice 1; badge UI is a 1.5 follow-on).

## 11. Open questions

1. **App publishing logistics.** Does OSS ship a Squadron-published
   App, or do operators create their own App per deployment? The
   first pins all OSS to one identity; the second adds wizard time.
2. **Webhooks behind NAT.** Local deployments have no public URL.
   Ship a pull-poll fallback for `pr_merged`/`pr_closed`, or document
   webhook reachability as a Compliance Pack prereq?
3. **Empty placement map at first scan.** Fresh operators have no
   basis to pick file paths at connect time. Allow "skip for now,
   configure per-kind on first recommendation"?
4. **Tool interactions.** Atlantis / Spacelift / Terraform Cloud will
   react to PR-open by running plan. Per-tool docs or generic
   disclaimer in the PR footer?
5. **Branch hygiene.** A long-lived deployment accumulates merged
   `squadron/rec-*` branches. Delete-on-merge by default?

## 12. Acceptance tests

1. **Happy-path connect.** A fresh operator walks the wizard,
   installs the App, picks a test repo on a default branch with one
   `modules/lambda/main.tf`, declares the placement map
   (Lambda → that file), Validate green, Save. Wizard closes; row
   appears in the Connected IaC repos list. Audit log contains
   exactly one `iac.github.connection_created` event with no token
   material in its payload.

2. **Happy-path Open PR.** With the connection from test 1, a scan
   on a Lambda-only account yields one Lambda-OTel-layer
   recommendation. The card displays "Open PR". Within 5s of the
   click a PR appears: title contains the scan-ID short hash, body
   contains the proposer reasoning + snippet, base is the default
   branch, head is `squadron/rec-<scan_id>-0`. The committed diff is
   the snippet appended to `modules/lambda/main.tf` with one
   trailing newline; no other file changes. Audit log contains
   exactly one `recommendation.pr_opened` event with
   `repo_full_name`, `pr_number`, `commit_sha`, `file_path` populated
   and no snippet content.

3. **Squadron must not write the default branch.** Unit test calls
   the GitHub-client wrapper with `ref=refs/heads/main` for both
   create-ref and update-ref paths; both refuse with a typed error
   before issuing the underlying API call.

4. **Repo deletion mid-flight.** Connection exists; repo is deleted
   between scans. Operator clicks Open PR. Card renders the
   humanized "the repo `<old_full_name>` is no longer reachable;
   re-run the IaC connect wizard" with a jump-back. Audit log
   contains exactly one `recommendation.pr_open_failed` event with
   `error_code=RepoNotFound` and no `pr_number`. No partial branch
   remains.

5. **No placement configured.** Scan produces a recommendation for
   `eks-observability-addon`; placement map has no row for it. Card
   renders snippet with "Copy" but no "Open PR"; tooltip names the
   missing row. No audit event fires. The pre-slice-1 copy-paste
   path still works.
