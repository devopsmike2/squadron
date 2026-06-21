# #603 — Connect IaC repo, slice 2: HCL-aware merging

**Status:** proposal stub, design landing in parallel at Stream 28.
**See also:** [603-connect-iac-repo.md](603-connect-iac-repo.md)
(slice 1 contract), [universal-discovery-design.md
§"IaC sub-arc"](../universal-discovery-design.md#iac-sub-arc-connect-repo--pr-disposition),
[discovery-iac-first-time-setup.md §"PR disposition"](../discovery-iac-first-time-setup.md#pr-disposition--new_file-vs-patch_existing).

This is a placeholder so cross-links from v0.89.11 (slice 1.5)
land somewhere meaningful. The full design lands separately at
Stream 28.

## Problem this slice closes

Slice 1.5 (v0.89.11 #626 Stream 27) routes recommendation kinds
through one of two dispositions based on Terraform shape:

- 4 of 9 kinds map to `new_file` — Squadron writes a sibling file
  and `terraform plan` passes on first try. Merge-clean.
- 5 of 9 kinds map to `patch_existing` — Squadron appends the
  snippet to the placement file and labels the PR
  `[needs manual merge]`. The operator hand-integrates the
  highlighted attributes into their existing resource block
  before merging. The label, the title prefix, and the audit
  payload's `manual_merge_required: true` flag all tell the
  operator the slice-1.5 friction is real but bounded.

Slice 2 closes out the `patch_existing` friction by making
Squadron HCL-aware:

- Parse the placement file's HCL into a typed AST.
- Locate the existing top-level resource block the recommendation
  targets (e.g. the specific `aws_lambda_function` whose `layers`
  attribute needs the OTel layer).
- Splice the proposer's attributes into the existing block at the
  right syntactic position.
- Write the merged file back.
- The PR title drops the `[needs manual merge]` prefix and the
  `squadron/needs-manual-merge` label.

Both the per-kind disposition map
(`internal/iac.KindDispositions`) and the proposer prompt remain
the source of truth for which kinds are net-new vs which kinds
patch. Slice 2 does not change that classification — it changes
HOW patch_existing kinds land.

## Scope (placeholder)

In scope:
- HCL parser dependency (`hashicorp/hcl/v2`) wired into the
  Open-PR handler.
- Per-kind splice rules — one per `patch_existing` kind, mirroring
  the per-kind classification in slice 1.5.
- Conflict detection (resource block named in the proposer's
  snippet but missing from the placement file → fall back to the
  slice-1.5 append behavior with a clear "block not found" note).
- Tests against fixture Terraform modules for each
  `patch_existing` kind.

Out of scope:
- Multi-file refactoring (snippets that span more than one
  resource block).
- HCL block reordering or comment preservation beyond what the
  parser supports.
- CDK / Pulumi / CloudFormation — slice 7 of the broader IaC
  format arc.

## Open questions

- How does slice 2 handle `dynamic` blocks and `for_each` loops
  in the targeted resource?
- Does the slice-2 PR call out the exact diff in the resource
  block, or just point the operator at the file?
- Does the slice-2 disposition output change shape (e.g. add a
  third `clean_merge` value) or stay binary?

These resolve when Stream 28's design lands.
