# Discovery IaC (GitHub) — first-time setup

**As of v0.89.62, the unified Discovery dashboard at `/discovery` shows aggregated counts across all four clouds. See it for the cross-cloud view.**

This is the operator-facing runbook for connecting a Squadron
deployment to a GitHub-hosted Terraform repository for the first
time. It covers the GitHub-side PAT bootstrap plus the in-product
wizard walk, and finishes with the "Open PR" loop that closes the
distance between a Squadron recommendation and a real change in your
infrastructure.

If you have not yet connected an AWS account to Squadron, start
there instead: [discovery-aws-first-time-setup.md](discovery-aws-first-time-setup.md).
Squadron's IaC connection sits on top of an existing AWS connection;
recommendations come out of an AWS scan, and the "Open PR" button
needs both halves wired before it lights up.

For a first test against a sandbox Terraform repo on a personal
GitHub account, the walkthrough takes roughly 10 minutes. For a
production setup against an org-owned repo with branch protection
rules, budget 20 minutes plus your org's review for the PAT.

## What we're building

Three things, in this order:

1. **A GitHub Personal Access Token (PAT)** scoped to `repo` only,
   created on the GitHub account that owns the Terraform repository.
   This is the credential Squadron will hold to open pull requests.
   Squadron seals it with the same AES-GCM substrate as your AWS
   credentials and never logs the token bytes.
2. **A Squadron IaC connection** pointing at one GitHub repository.
   The connection records the repo, the default branch, the repo
   layout (mono- or multi-), and the placement map (§Step 6) — the
   table that tells Squadron which Terraform file should receive
   each kind of recommendation.
3. **The "Open PR" loop** on the existing AWS Recommendations tab.
   Once a connection exists with the right placement row for a
   recommendation's resource kind, the card grows an Open PR button
   alongside Copy. Click it, and Squadron creates a branch, commits
   the proposer's snippet at the declared file path, and opens a PR
   against your default branch.

The trust model is the same as the AWS side: Squadron writes the
branch and opens the PR. Squadron never pushes to your default
branch. Your branch protection rules and reviewers are the gate.
Your existing CI is what runs `terraform plan` and `terraform apply`
on merge. Squadron is the orchestrator; your operator and your CI
are the executor.

## What this is good for

- You already use a GitOps workflow for Terraform (PR → review → CI
  applies on merge).
- You want Squadron to take the proposer's snippet and put it where
  it actually lives in your repo, with the right reviewers tagged,
  in 5 seconds instead of you copying it manually.
- You want every Squadron-initiated PR to land through your normal
  review process, not bypass it.

## What this is NOT

Read this list carefully. The first three are features, not
limitations.

- **Squadron does not run `terraform plan` or `terraform apply`.**
  Your existing CI does, on merge, with whatever credentials you
  already trust your CI with. Squadron holds no cloud write creds.
- **Squadron never pushes to your default branch.** It can only
  create branches under a configurable prefix (default
  `squadron/rec/...`) and open PRs against the default branch. This
  is enforced in two places in the code: at the GitHub-client
  wrapper layer and at the handler layer. Either layer's check
  alone would catch the violation; defense in depth.
- **Squadron does not edit existing Terraform.** The slice-1 PR
  flow appends the proposer's snippet at the end of the declared
  file with one trailing newline. It does not parse HCL, merge
  resource blocks, or deduplicate. If a resource already exists with
  the same name, `terraform plan` in your CI will surface a
  duplicate-resource error immediately and legibly — same signal
  you'd see if you'd pasted the snippet manually.
- **Slice 1.5 (v0.89.11) softens this for 4 of 9 kinds.** For the
  five recommendation kinds whose Terraform shape is a NET-NEW
  top-level resource (EC2 ADOT SSM association, S3 access logging,
  EKS observability addon, DynamoDB contributor insights), Squadron
  now writes a SIBLING file `squadron_<resource_kind>.tf` in the
  placement file's directory — clean drop-in, no conflict. For the
  five kinds that MODIFY an existing resource block (Lambda OTel
  layer, RDS PI/EM, ALB access logs, EKS cluster logging, ECS
  container insights), the append-only behavior is preserved but
  the PR is clearly labeled `[needs manual merge]` so you know
  hand integration is required before clicking. See §"PR
  disposition" below for the full table.
- **Slice 2 (v0.89.12) closes the gap entirely.** The five
  patch_existing kinds now get HCL-aware merging: Squadron parses
  the placement file as Terraform, locates the existing resource
  block, applies the proposer's structured per-attribute edits
  in place, and ships a clean drop-in PR. The
  `[needs manual merge]` title prefix and the
  `squadron/needs-manual-merge` label drop away on the merged
  path. If the placement file fails to parse, the target resource
  address resolves to nothing, or any other slice-2 precondition
  is violated, the handler falls back cleanly to the slice-1.5
  append-only behavior — the operator never loses a
  recommendation; the PR just opens with the slice-1.5 marker
  plus a one-line note in the body naming the fallback reason.
  See
  [proposals/603-slice-2-hcl-aware-merging.md](proposals/603-slice-2-hcl-aware-merging.md)
  for the full design.
- **Squadron does not auto-merge.** Every PR is reviewable in the
  normal GitHub workflow. Auto-merge tools (Mergify, Auto-merge
  apps) on your side will treat Squadron PRs the same as any other
  PR; if that's not what you want, exclude the `squadron/*` labels
  or the configured branch prefix from your auto-merge rules.
- **GitHub App-based auth is not in this release.** Slice 1 ships
  PAT only. PATs are org-wide for the user that owns them — that's a
  real privilege concentration vs the per-repo scoping a Squadron
  GitHub App would offer. The App path is on the roadmap for slice
  2. For OSS deployments today, PAT is what you have.
- **GitLab, Bitbucket, and Azure DevOps are not in this release.**

## Prerequisites

- A working AWS connection on the Squadron Discovery page (see the
  AWS first-time-setup runbook).
- A GitHub repository where your Terraform lives, and the ability
  to create a PAT on whichever GitHub account owns or has push
  access to that repository.
- An understanding of which file (or files) in that repo should
  receive each kind of Squadron recommendation. You don't need to
  know all seven up front — you can skip rows and configure them
  later when their recommendations first appear.

## Step 1 — Create a GitHub Personal Access Token

In the Squadron wizard at step 2 (Authentication), there is a deep
link to GitHub's create-token page with the scope pre-filled. You
can click it now or follow this manually:

1. Open <https://github.com/settings/tokens/new>.
2. Set **Note** to something recognizable, e.g.
   `Squadron — opens PRs against my-org/infra-terraform`.
3. Pick an **Expiration**. 90 days is a reasonable default for a
   first test; pick whatever your org policy requires. Squadron will
   surface a 401 error with a humanized "Authentication failed —
   re-run the IaC connect wizard with a fresh PAT" message when the
   token expires, so you don't need automation around rotation for
   slice 1 (see §Rotation below).
4. Under **Select scopes**, check exactly one: **`repo`**. Squadron
   uses this for creating refs, reading file content, writing file
   content on the new branch, and opening PRs.

   `repo` is org-wide on the user's accessible repos. If you have
   multiple Terraform repos across multiple orgs and you don't want
   one PAT to access all of them, create a separate PAT per repo
   or wait for the GitHub App path in slice 2.
5. Click **Generate token**. **Copy the token now.** GitHub shows
   it once.

If your org uses fine-grained PATs instead of classic PATs, the
slice-1 server tests classic-style `ghp_*` tokens. Fine-grained
tokens with `contents: read+write` and `pull_requests: write` on
the specific repo work the same in practice — they're stricter
which is a security win — but slice 1 does not validate the
fine-grained flow end-to-end. If you use a fine-grained PAT and
hit an unexpected scope error, the workaround is a classic PAT
scoped `repo` until slice 2.

## Step 2 — Open the IaC wizard

In the Squadron UI, navigate to **Discovery → IaC (GitHub)** in the
sidebar. The connections list page renders. Click **Connect IaC
repo**. The wizard opens.

The wizard has six steps. You can step backward at any time without
losing earlier inputs. Closing the wizard mid-flow discards
everything except the validate-step preflight results (which are
read-only anyway). The PAT input never persists to local storage,
session storage, or the URL — closing the wizard means you'll need
to paste the PAT again. This is intentional.

## Step 3 — Authentication

Two tiles render. **GitHub App** is disabled with a "Coming in slice
2" badge — choose the **Personal Access Token** tile.

Paste the PAT into the token field. The field is `type=password`
with `autocomplete=off` so browsers and password managers will not
echo or persist it. Squadron sends the token to the server over
HTTPS (if your deployment terminates TLS) or directly to the local
process; it is sealed in the connection row using your deployment's
`SQUADRON_SECRETS_KEY` before it ever lands in SQLite.

Click **Next**.

## Step 4 — Repository

Enter your repo in `owner/repo` format. Examples:

- `acme-corp/infra-terraform`
- `my-team/eks-terraform`
- `mike-personal/lambda-poc`

The wizard validates the format inline. No GitHub API call happens
at this step — that's deferred to step 7 (Validate) so the operator
can change their mind cheaply before a network round-trip.

Click **Next**.

## Step 5 — Layout, branch, and advanced settings

This is the step where Squadron asks the question that most affects
the rest of the setup: **how is your Terraform organized?**

### Repo layout

Pick **Mono-repo** or **Multi-repo**.

**Multi-repo** is the default. Pick it when this repository holds
**one environment or one domain**: e.g. `my-team/eks-terraform` is
just the EKS resources, or `acme-corp/prod-infra` is just the prod
environment's resources.

Why this matters for placement: in a multi-repo, the placement map
in step 6 points at files like `modules/eks/main.tf` or `main.tf` —
shallow paths, one file per resource kind, no environment
qualifier.

**Mono-repo** is for the platform-team pattern where one big
repository holds **multiple environments and multiple services**,
organized by directory: e.g. `acme-corp/infra` with subtrees like
`environments/prod/eks/`, `environments/staging/eks/`,
`modules/lambda/`. Pick it when the same resource kind shows up at
multiple paths.

Why this matters for placement: in a mono-repo, the placement map
points at deeper paths like `environments/prod/eks/main.tf`. The
slice-1 placement map captures **one path per resource kind** — if
your mono-repo declares the same kind in multiple environments,
slice 1 lands every PR in the first path. Connect a second IaC
repo entry for the staging path (or wait for slice 2's per-path
routing).

If you genuinely can't decide, pick the layout that matches the
*directory depth* of the file you'll declare in step 6. Mono- vs
multi- only affects the placeholder text and example file paths;
nothing else changes between the two paths.

### Default branch

Pre-filled with `main`. Change it if your repo's default branch is
something else (`master`, `develop`, etc.). Squadron will overwrite
this field with the live value from the GitHub API at the Validate
step (§Step 7) — the field here is a hint, not a commitment.

### Advanced (collapsed by default)

Two optional fields. Skip both if you're not sure.

- **Branch prefix.** Default `squadron/rec`. Squadron's PR branches
  are named `<this>/<scan_id_short>-<step_idx>`, e.g.
  `squadron/rec/abc1234-0`. Change it if your CI has branch-name
  rules (Mergify only acts on `release/*`, Atlantis only plans on
  `terraform/*`, etc.).
- **Reviewer team handle.** Empty by default. Format: `org/team`,
  e.g. `acme-corp/sre`. If set, Squadron requests a review from this
  team on every PR it opens. The team must already exist and have
  read access on the repo; Squadron logs a warning and continues if
  the review request fails (a typo in the team handle does not block
  PR creation).

Click **Next**.

## Step 6 — Placement map

This is the substantive step. Squadron's proposer emits nine kinds
of recommendations today:

| `resource_kind` | What the snippet does |
| --- | --- |
| `ec2-otel-layer` | Installs an ADOT collector on EC2 instances via SSM Run Command. |
| `lambda-otel-layer` | Attaches the ADOT Lambda layer to uninstrumented Lambda functions. |
| `rds-pi-em` | Enables Performance Insights + Enhanced Monitoring on RDS instances. |
| `s3-access-logging` | Enables S3 access logging on buckets that don't have it. |
| `alb-access-logs` | Enables ALB access logs on load balancers that don't have them. |
| `eks-cluster-logging` | Enables EKS control plane logging (api, audit, authenticator). |
| `eks-observability-addon` | Installs the `amazon-cloudwatch-observability` EKS addon. |
| `dynamodb-contributor-insights` | Enables CloudWatch Contributor Insights on DynamoDB tables to surface top-accessed keys and most-throttled keys. |
| `ecs-container-insights` | Enables CloudWatch Container Insights on ECS clusters to surface task and service metrics. |

For each row, declare the **one file path** in your repo where
Squadron should append the snippet. Placeholder examples adapt to
your repo-layout choice from step 5.

You can **Skip** any row. Skipped rows render a "Copy" button only
when their recommendations appear in the Recommendations tab — no
Open PR button until you come back and configure the row. The
deep-link from the Recommendations tab will land you right back on
this step, at the missing row, when an operator clicks the
"Configure placement" affordance.

There is also a **Pattern apply** affordance ("apply
`modules/{kind}/main.tf` to all rows" — substitutes the
`{kind}` token per row) and a **Skip all** for operators who want
to configure per-kind on first hit instead of up front.

The rows you save here are stored on the connection. You can edit
them later from the connections list or via the deep-link, and the
audit timeline records the change as
`iac.github.placement_map_updated`.

Click **Next**.

## Step 7 — Validate

Squadron calls GitHub with the PAT and runs a preflight:

1. **Repo reachable?** Calls `GET /repos/{owner}/{repo}`. Failure
   modes: `RepoNotFound` (typo, or repo is private and the PAT does
   not have access to it), `AuthFailed` (PAT is invalid, expired,
   or missing `repo` scope).
2. **Default branch detection.** Squadron reads the live default
   branch from the same API call and overwrites your step-5 entry.
3. **Placement files exist?** For each non-skipped placement row,
   Squadron calls `GET /repos/{owner}/{repo}/contents/{path}` and
   records whether the file exists and (if it does) its short SHA.

Each row renders one of three icons: ✓ found, ✗ missing, ⊘ skipped.
A missing file is **not** a blocker — you may want to add the file
as part of the first PR Squadron opens. The Save button explicitly
calls out which rows will be saved as-is vs which will be skipped on
first PR open.

A repo-level failure (RepoNotFound, AuthFailed) **is** a blocker —
the Save button stays disabled and the humanized error renders with
a jump-back button to the relevant step (Authentication, Repository).

## Step 8 — Save

Click **Save Connection**. Two things happen:

1. The connection is created in Squadron's `iac_connections` table.
   The PAT is sealed before insert. The connection_id is a UUID.
2. The audit event `iac.github.connection_created` is recorded with
   payload `{connection_id, repo_full_name, default_branch,
   auth_kind, placement_map}` — never the token.

The wizard closes. You land on the connections list page with a
green "Connected" toast and your new row. The connection is
immediately live; the next AWS scan will surface Open PR buttons on
recommendation cards that match your placement map.

## Step 9 — Open your first PR

Navigate to **Discovery → AWS** and either trigger a fresh scan or
open an existing one. Click **Generate recommendations**.

For each recommendation whose `resource_kind` has a row in your
connection's placement map, the card grows an **Open PR** button
alongside the existing **Copy** button. Click Open PR. The button
shows a spinner ("Opening PR…") for roughly 3–5 seconds while
Squadron:

1. Reads the current default-branch HEAD SHA.
2. Creates a branch off that SHA, named
   `<branch_prefix>/<scan_id_short>-<step_idx>`.
3. Reads the current content of the declared placement file.
4. Appends the proposer's snippet with one trailing newline.
5. PUTs the new content to the branch.
6. Opens a PR with base = default branch, head = new branch.
7. Adds the `squadron` and `squadron/<resource_kind>` labels.
8. Requests review from the configured team if set.

On success, the card collapses into a success panel: the PR number
and URL (target=_blank), the file path, and a footer that mirrors
the language in the PR body — "Squadron will not push to this
branch again." The Open PR button is removed; the Copy button
persists in case you want to also paste the snippet somewhere else.

Open the PR in GitHub and review. Merge when ready. Your existing CI
will pick up the merge and run `terraform plan` / `terraform apply`
the same way it does for every other PR.

Re-run the Squadron scan after the apply lands. The previously
uninstrumented resource should now report as instrumented; the
recommendation drops from the Recommendations tab on its own.

## PR disposition — new_file vs patch_existing

Slice 1.5 (v0.89.11, #626 Stream 27) routes each Open PR through
one of two dispositions based on the resource_kind's structural
Terraform shape:

- **`new_file`** — the snippet defines a NET-NEW top-level Terraform
  resource. Squadron writes a SIBLING file named
  `squadron_<resource_kind>.tf` in the placement file's directory.
  Clean drop-in; `terraform plan` in your CI passes on first try.
  The PR has the usual `squadron` + `squadron/<resource_kind>`
  labels and no manual-merge marker.
- **`patch_existing`** — the snippet modifies an EXISTING top-level
  resource block (e.g. adding `layers = [...]` to an
  `aws_lambda_function` your module already declares).
  - **Slice 2 path (v0.89.12, the default).** Squadron parses the
    placement file as HCL, locates the existing resource block by
    `<resource_type>.<name>`, applies the proposer's structured
    patch in place (e.g. `list_append_dedupe` on `layers`,
    `map_merge` on `environment.variables`), and ships the result
    as a clean drop-in PR. No `[needs manual merge]` title prefix;
    no `squadron/needs-manual-merge` label. If the target resource
    carries `lifecycle { ignore_changes = [...] }` referencing a
    patched attribute, the PR body adds a one-line note — the
    file change still lands but `terraform apply` will no-op the
    corresponding attribute until you edit the ignore_changes
    entry.
  - **Slice 1.5 fallback.** When the placement file fails to parse,
    the target resource address doesn't resolve to anything, the
    proposer didn't emit a structured patch (older prompts), or
    any other slice-2 precondition is violated, Squadron falls
    back to the slice-1.5 append-only behavior: appends to the
    placement file, prefixes the title with `[needs manual merge]`,
    applies the `squadron/needs-manual-merge` label, and renders
    the "Manual merge required" callout in the PR body. The PR
    body's callout names the fallback reason (parse_error,
    resource_not_found, etc.) so you can act on it.

The disposition is STRUCTURAL — Squadron's server applies the same
mapping on every request, independent of the proposer's output.
The Recommendations card surfaces the disposition BEFORE you click:
patch_existing kinds with a slice-2 structured patch get a small
green "HCL-merged" checkmark next to Open PR; patch_existing kinds
without a structured patch get a small amber "Needs manual merge"
badge.

Per-kind table:

| resource_kind                   | disposition       | Terraform shape                                        |
|---------------------------------|-------------------|--------------------------------------------------------|
| `ec2-otel-layer`                | `new_file`        | `aws_ssm_association` is its own top-level resource    |
| `lambda-otel-layer`             | `patch_existing`  | modifies `aws_lambda_function.layers`                  |
| `rds-pi-em`                     | `patch_existing`  | modifies `aws_db_instance` attributes                  |
| `s3-access-logging`             | `new_file`        | `aws_s3_bucket_logging` is its own top-level resource  |
| `alb-access-logs`               | `patch_existing`  | modifies `aws_lb.access_logs` nested block             |
| `eks-cluster-logging`           | `patch_existing`  | modifies `aws_eks_cluster.enabled_cluster_log_types`   |
| `eks-observability-addon`       | `new_file`        | `aws_eks_addon` is its own top-level resource          |
| `dynamodb-contributor-insights` | `new_file`        | `aws_dynamodb_contributor_insights` is its own resource|
| `ecs-container-insights`        | `patch_existing`  | modifies `aws_ecs_cluster.setting` nested block        |

Slice 2 (v0.89.12) ships the HCL-aware merger. The five
patch_existing kinds above each have a locked patch shape the
proposer emits; see
[proposals/603-slice-2-hcl-aware-merging.md](proposals/603-slice-2-hcl-aware-merging.md)
for the full design and per-kind schema.

### HCL merge fallback reasons

When Squadron's slice-2 HCL merger refuses, the PR opens via the
slice-1.5 append-only path and the body callout names the reason.
The codes the audit payload's `hcl_patch_failure_reason` field
carries:

| Reason                | Meaning                                                                |
|-----------------------|------------------------------------------------------------------------|
| `parse_error`         | Existing placement file is not valid HCL today. Fix it and re-scan.    |
| `resource_not_found`  | The target resource address (`<type>.<name>`) doesn't exist in the file. The operator may have renamed the resource since the scan. |
| `ambiguous_resource`  | Multiple resource blocks match the same address (shouldn't happen — Terraform itself rejects this — but the check is defensive). |
| `unknown_op`          | The proposer emitted an HCL patch op outside the 5-op vocabulary. Re-running the proposer (newer prompt) usually fixes it. |
| `invalid_value_type`  | The proposer emitted a value whose Go-side type doesn't match the op (e.g. `scalar_set` with a list). Same fix as `unknown_op`. |
| `no_patch_emitted`    | The proposer is on a pre-v0.89.12 prompt and didn't emit a structured patch at all. Re-run the scan after upgrading. |
| `other`               | Unclassified merge error. Open an issue with the audit payload.        |

## Trust thesis — what Squadron does and does not do

Read this if you're going to deploy Squadron in a regulated
environment, or if you're explaining the security model to a
reviewer.

**Squadron holds these credentials:**

- One AWS IAM access key (read-only on the resource kinds Squadron
  scans), held in `~/.aws/credentials` under the
  `[squadron-bot]` profile.
- One GitHub PAT per IaC connection (`repo` scope), held in the
  `iac_connections.cred_ciphertext` column, sealed with
  `SQUADRON_SECRETS_KEY`.

**Squadron does these writes:**

- To GitHub: `POST /git/refs` (create branch under
  `<prefix>/...`), `PUT /contents/{path}` (write file on the new
  branch), `POST /pulls` (open PR against default branch),
  `POST /issues/{n}/labels` (add labels),
  `POST /pulls/{n}/requested_reviewers` (request review).
- To Squadron's own SQLite: connection rows, audit event rows.

**Squadron does NOT do these writes:**

- It does NOT write to your AWS account beyond the read-only scopes
  declared in the AWS IAM policy. The IAM policy in the AWS doc has
  no `Put*`, `Update*`, `Modify*`, `Delete*`, `Create*` actions on
  AWS resources, and no `iam:*` actions of any kind. The IAM policy
  upgrade-path table in the AWS doc names every action that ships
  in every slice.
- It does NOT write to your GitHub default branch ref. The GitHub
  client wrapper refuses any `POST /git/refs` or `PATCH
  /git/refs/heads/{default}` call where the branch arg equals the
  repo's default branch, and the handler layer enforces the same
  check before the wrapper is ever called. Either layer alone would
  catch the violation; defense in depth.
- It does NOT run `terraform plan` or `terraform apply`. Your CI
  does.

**Squadron audit trail of every write:**

- Every Squadron-initiated GitHub write produces a
  `recommendation.pr_opened` audit row (or
  `recommendation.pr_open_failed` on failure). The payload includes
  `repo_full_name, pr_number, pr_url, branch, commit_sha, file_path,
  actor` — never the snippet content, never the PAT, never the
  file's prior content.
- Every connection lifecycle change produces an
  `iac.github.connection_created` / `connection_validated` /
  `placement_map_updated` audit row.
- The Timeline page renders all of these with humanized one-line
  summaries; the raw JSON payload is available behind a row
  expander.

## Troubleshooting

The error message you'll see in the wizard or on a recommendation
card is the humanized message; the `error_code` below appears in
the audit timeline and is what to grep on if you're debugging from
logs.

### Validate / Save errors

**`AuthFailed`** — The PAT is invalid, expired, or missing `repo`
scope.

- Fix: re-run §Step 1. Check the scope checkbox. Copy the token
  immediately — GitHub shows it once.
- If you copied the token correctly and it still fails, the most
  common cause is the PAT was created on a different GitHub account
  than the one that has access to the repo. Check the URL of the
  token-creation page when you create the token.

**`RepoNotFound`** — Squadron called GitHub with the PAT and got a
404 for `GET /repos/{owner}/{repo}`.

- Fix: check the spelling of `owner/repo` in step 4. GitHub is
  case-sensitive in the URL but not in the API; the API returns 404
  for either typo class.
- If the spelling is right, the PAT does not have access to the
  repo. Private repos in an org require the PAT to be created by an
  account with read access to the repo. If you're testing against
  a personal sandbox, the PAT must belong to the account that owns
  the sandbox.

**`FileNotFound`** — A placement row pointed at a path that doesn't
exist on the default branch. Surfaces only on Validate; not a Save
blocker.

- Fix: pick a different path that exists today, or accept the row
  as-is and Squadron will skip Open PR for this kind until you fix
  the path later.

**`ConnectionConflict`** — There's already an IaC connection for
this `(provider, repo_full_name)` pair. Squadron enforces one
connection per repo.

- Fix: visit the connections list, delete the existing connection
  if it's stale, and retry. Or edit the existing connection's
  placement map from the list page if that's what you actually
  wanted.

### Open PR errors

**`NoPlacementMapping`** — You clicked Open PR for a
recommendation whose `resource_kind` has no row in the placement
map (or the row was skipped at save time).

- Fix: the card's tooltip will deep-link you to the placement
  step in the wizard, focused on the missing row. Configure it and
  click Open PR again.

**`DefaultBranchWriteRefused`** — This is the security invariant
firing. It means Squadron's branch-name resolution somehow produced
the default branch name, which should not happen.

- This is a Squadron bug — please file an issue with the audit
  payload (everything except the PAT) attached. The fact that the
  invariant fired and the write was blocked is working as designed.

**`AuthFailed` (on Open PR specifically)** — The PAT was valid at
Validate time but is no longer valid. Usually means the PAT
expired or was revoked between the Save step and the Open PR
click.

- Fix: re-run §Step 1 to generate a fresh PAT, delete the existing
  connection, walk the wizard again.

**`SquadronFileAlreadyExists`** — v0.89.11 (#626 Stream 27) slice-1.5
only. You clicked Open PR for a `new_file`-disposition kind, but a
prior Squadron PR for the same kind already created
`squadron_<resource_kind>.tf` in the placement file's directory.
The next Open PR for the same kind would collide because slice 1.5
does not update an existing Squadron sibling file — that's slice 2
territory.

- Fix: in GitHub, find the existing
  `squadron_<resource_kind>.tf` file. Either merge the open PR
  that created it (preferred — the new scan's snippet may already
  be redundant) or close that PR and delete the file from the
  default branch, then re-run the scan. Squadron's next Open PR
  will create a fresh file.

## Rotation and cleanup

### PAT rotation

Slice 1 does not support in-place PAT rotation. To rotate:

1. Create a new PAT (§Step 1).
2. From the connections list, **Delete** the existing connection.
   This removes the row and the sealed token; pending audit events
   are preserved.
3. Walk the wizard with the new PAT.

In-place rotation is a slice 1.5 follow-on. The current path is
manual but auditable: every connection deletion and creation lands
in the timeline.

### Deleting a connection

The connections list has a **Delete** button per row. Confirm in the
modal. Squadron removes the row from SQLite immediately. The
deletion does not delete any branches or PRs on GitHub — those are
your repository's history and Squadron leaves them alone. Open PRs
that Squadron created stay open; close them in GitHub if you don't
want them.

Deleting a connection is idempotent — re-deleting an already-deleted
connection returns HTTP 204 without an error. No audit event fires
in slice 1; this is a known gap (slice 1.5 should land the
`iac.github.connection_deleted` event for symmetry with the other
lifecycle events).

### Revoking a PAT on GitHub's side

If you suspect a Squadron deployment's host is compromised, revoke
the PAT on GitHub immediately — <https://github.com/settings/tokens>
→ Revoke. This invalidates the credential. Then delete the
connection in Squadron. Squadron's next attempt to use the token
will surface `AuthFailed`; the deletion cleans up the sealed
ciphertext.

## What this does NOT cover

- **GitHub App authentication.** Slice 2.
- **Webhooks from GitHub back to Squadron** for
  `recommendation.pr_merged` events. **SHIPPED in v0.89.23 — see
  [webhook-listener.md](./webhook-listener.md) for the operator
  runbook.** The receiver requires the operator's Squadron
  deployment to expose a public callback URL. `pr_closed`-without-
  merge events are recorded as ignored deliveries, not audit
  events. The wizard step for entering the secret + per-connection
  secrets remain slice 2.
- **Multiple repos per connection.** Slice 2. Today: one Squadron
  IaC connection per GitHub repository.
- **Editing the PR body or title after open.** Each Open PR click
  is a new branch and a new PR.
- **Auto-merge.** Outside of Squadron's scope — your operator's
  call, configured on GitHub's side.
- **GitLab, Bitbucket, Azure DevOps.** Roadmap, not slice 1.
- **HCL-aware merging.** Slice 1 appends; slice 1.5 (v0.89.11)
  routes new_file kinds through a sibling-file write to avoid the
  duplicate-resource problem entirely. Patch_existing kinds still
  use the slice-1 append-only path and are labeled `[needs manual
  merge]` so the operator knows hand integration is required.
  Slice 2 ([proposals/603-slice-2-hcl-aware-merging.md](proposals/603-slice-2-hcl-aware-merging.md))
  will land HCL-aware merging that closes out the patch_existing
  kinds.

## See also

- [discovery-aws-first-time-setup.md](discovery-aws-first-time-setup.md)
  — the AWS half of the setup. Connect AWS first; IaC builds on it.
- [universal-discovery-design.md](universal-discovery-design.md) —
  the broader design doc this runbook operationalizes.
- [proposals/603-connect-iac-repo.md](proposals/603-connect-iac-repo.md)
  — the design doc that scoped this slice 1, including the threat
  model and the slice-2 roadmap (GitHub App, webhooks, multi-repo).
- [webhook-listener.md](webhook-listener.md) — v0.89.23 operator
  runbook for the inbound webhook that records
  `recommendation.pr_merged` events when Squadron-opened PRs land.
- [discovery-proposer-learning.md](discovery-proposer-learning.md)
  — v0.89.28 operator runbook for the discovery proposer's
  feedback loop, which reads `recommendation.pr_merged` events
  and stops re-proposing accepted recommendations on the next
  scan. The connection's `LearnFromAcceptedRecommendations` flag
  controls the loop per connection.
