# Arc: Context-Aware, Merge-Ready Remediation PRs

Status: proposed (2026-06-28)
Owner: autonomous
Supersedes nothing; builds on the live-loop validation (PR #2 e2e) and #183 (repo scanning).

## Problem

The live remediation loop is proven end-to-end (scan -> propose -> open PR ->
merge -> verdict -> citation). But the PR Squadron opens is not yet something an
operator can trust to merge blind:

1. **The proposer never reads the operator's Terraform.** It generates from the
   scan inventory + general guidance only. So snippets (and `hcl_patch` resource
   addresses) are built blind to real resource names, variables, providers, and
   existing instrumentation. It can target the wrong block or duplicate one.

2. **`new_file` kinds drop a standalone file.** ec2-otel-layer, s3-access-logging,
   eks-observability-addon, dynamodb-contributor-insights write a sibling
   `squadron_<kind>.tf`. Valid Terraform in isolation (won't syntactically break
   the module) but zero awareness of what already exists -> no dedup, no shared
   variable/module references.

3. **No merge-ready guarantee.** Squadron never runs `terraform validate`/`plan`.
   The check-run posts Squadron's own status, not a real plan result. "Won't
   break existing code" is currently the operator's job.

4. **Comment bloat.** Every PR carries a Squadron header banner
   (`# Authored by Squadron ...`) plus the model's explanatory comments. Over many
   PRs that accumulates noise operators may not want committed.

## Goals

- PRs that consider the existing code and are safe to merge.
- Operator control over comment verbosity in the committed Terraform.
- Bounded, predictable AI token cost.

## Token-cost note

Reading the repo is **free** (GitHub API + local HCL parsing — no AI). Cost
accrues only for repo content placed *into* the proposer prompt. Reference: the
PR #2 run used ~29.7K input / 2.6K output tokens. A 100-300 line placement file
adds ~1-4K input tokens (fractions of a cent/run at Sonnet pricing). Mitigations:
parse HCL deterministically to extract addresses/vars/providers, send the model a
compact summary + only the relevant file (never the whole repo), and cap file
size with a byte budget.

## Slices

### Slice 1 — Comment-exclusion option (this PR)
Add `exclude_comments` to the open-PR request. A `stripHCLComments` helper using
`hclwrite` token scanning (drops comment tokens; safe against `#`/`//` inside
string literals; handles `#`, `//`, `/* */`). When set: suppress the
"Authored by Squadron" header banner and strip comments from the snippet across
all three content paths (new_file, append fallback, HCL-merge). UI: a checkbox in
the open-PR wizard step, defaulting to off (comments kept).

Interpretation of "exclude all comment code": strip ALL comments from the
committed `.tf` (both Squadron's banner and the model's inline explanations),
yielding clean code. The rationale still lives in the PR body, which is never
stripped.

### Slice 2 — Repo context to proposer
Fetch the placement file(s) via the connection PAT, parse HCL locally to extract
resource addresses/names, declared variables, provider blocks, and any existing
OTel/observability config. Inject a token-bounded summary + the relevant file
into the discovery proposer prompt so it generates against real config and can
emit accurate `hcl_patch` addresses.

### Slice 3 — terraform validate gate
Run `terraform init -backend=false && terraform validate` (ideally a plan) on the
PR branch — sandboxed in Squadron or as a required GitHub Action check. Surface
the result as the check-run signal; mark the PR ready only on pass; annotate
failures so the operator sees *why*.

### Slice 4 — HCL-aware merge coverage + dedup
Resolve real resource addresses from the parsed file; extend HCL-aware merge
beyond the 5 current patch_existing kinds; dedup so Squadron never re-adds
instrumentation already present.

## Sequencing

1 (independent, quick win) -> 2 (foundation for smarter snippets) ->
3 (the trust gate) -> 4 (depends on 2's parsing).

## Non-goals

- Executing Terraform apply (Squadron stays orchestrator, not executor).
- Multi-file refactors in a single PR (one placement file per PR for now).
