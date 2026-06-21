# #603 — Connect IaC repo, slice 2: HCL-aware merging

**Status:** proposal, slice-2 scoping. Closes #627 Stream 28.
**See also:** [603-connect-iac-repo.md](603-connect-iac-repo.md)
(slice 1 contract, v0.89.3); slice 1.5 lives in code at
[`internal/iac/dispositions.go`](../../internal/iac/dispositions.go)
+ the disposition routing in
[`internal/api/handlers/iac_github.go:1025–1199`](../../internal/api/handlers/iac_github.go)
(v0.89.11 #626 Stream 27).

## 1. Problem

Slice 1.5 closed half the merge-friction problem. The 4 `new_file`
kinds (`ec2-otel-layer`, `s3-access-logging`,
`eks-observability-addon`, `dynamodb-contributor-insights`) land
as sibling `squadron_<resource_kind>.tf` files — merge-clean on
first try.

The 5 `patch_existing` kinds (`lambda-otel-layer`, `rds-pi-em`,
`alb-access-logs`, `eks-cluster-logging`, `ecs-container-insights`)
still go through `appendSnippetWithTrailingNewline`
([`iac_github.go:1129`](../../internal/api/handlers/iac_github.go))
with the `[needs manual merge]` title prefix and label
([`iac_github.go:1173–1199`](../../internal/api/handlers/iac_github.go)).
If the operator merges anyway, `terraform plan` fails with a
duplicate-resource error on the redeclared block.

Slice 2 parses the placement file as HCL, finds the existing
block by address, applies a structured patch, writes the result
back. The title prefix and the `needs-manual-merge` label drop.
The `KindDispositions` map at
[`dispositions.go:84`](../../internal/iac/dispositions.go)
does NOT change — slice 2 changes HOW `patch_existing` lands,
not WHICH kinds are classified that way.

## 2. Non-goals (slice 2)

- HCL parsing for GitLab, Bitbucket, Azure DevOps. GitHub only.
- HCL-aware merging on `new_file` kinds.
- JSON-encoded IaC (CDK, Pulumi, CloudFormation) — slice 7.
- Squadron auto-applying or auto-merging — slice 1 §5 invariant.
- `terraform fmt` enforcement. Squadron writes valid HCL but
  preserves the operator's existing style (hclwrite round-trip).
- Multi-file patches.

## 3. Structured patch schema

The proposer emits a `patches` array per step for `patch_existing`
kinds; the handler's HCL writer consumes it. Locked contract.

```json
{
  "kind": "lambda-otel-layer",
  "disposition": "patch_existing",
  "target_resource_address": "aws_lambda_function.squadron_test_function_node_2",
  "patches": [
    {"attribute_path": ["layers"], "op": "list_append_dedupe", "value": ["arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-nodejs-amd64-ver-1-18-1:4"]},
    {"attribute_path": ["environment", "variables", "AWS_LAMBDA_EXEC_WRAPPER"], "op": "scalar_set", "value": "/opt/otel-handler"}
  ]
}
```

`target_resource_address` is the two-segment
`<resource_type>.<name>` form. Module-prefixed addresses
(`module.foo.aws_lambda_function.bar`) are out of scope — the
placement-map row pins the file, which pins the module.

`op` enum (locked):

- **`scalar_set`** — set a scalar (string/bool/int/number) at
  `attribute_path`, replacing existing value. Used by
  `rds-pi-em`.
- **`list_append_dedupe`** — append `value` to an existing list;
  case-sensitive dedupe; original order preserved. Used by
  `lambda-otel-layer.layers` and
  `eks-cluster-logging.enabled_cluster_log_types`.
- **`nested_block_set`** — find the singleton nested block at
  `attribute_path`, set attributes from `value`, create if
  missing. Used by `alb-access-logs.access_logs`.
- **`nested_block_find_or_create`** — repeated nested block;
  `value.key_attribute` + `value.key_value` pin which one;
  `value.set` is the attributes to write. Update in place if
  found, append if no match. Used by
  `ecs-container-insights.setting`.
- **`map_merge`** — at a map-valued path, set the named key
  without disturbing siblings. Used by
  `lambda-otel-layer.environment.variables` (the example uses
  `scalar_set` against the terminal map key — same effect on a
  single key; `map_merge` is the explicit multi-key form).

Patch validation runs BEFORE any GitHub call — malformed `op`
or missing `attribute_path` returns `MalformedPatch`; no
branch.

## 4. PR handler workflow

`HandleIaCGitHubOpenPR` at
[`iac_github.go:840`](../../internal/api/handlers/iac_github.go)
gains a `patch_existing` branch before the existing
`appendSnippetWithTrailingNewline` call:

1. Fetch the placement file via `client.GetFileContent` (same
   call at
   [`iac_github.go:1090`](../../internal/api/handlers/iac_github.go)).
2. Parse via `hclwrite.ParseConfig`. Failure →
   `PatchFailed{reason: "parse_error"}` → fall through.
3. Walk `f.Body().Blocks()` for a `resource` block whose two
   labels match `target_resource_address`. Zero matches →
   `PatchFailed{reason: "resource_not_found"}` with the
   addresses that DO exist as a hint. Multiple matches →
   `PatchFailed{reason: "ambiguous_address"}`.
4. Apply each operation via hclwrite's typed setters
   (`SetAttributeValue`, `AppendNewBlock`). §6–§8 spec the
   per-op semantics.
5. Serialize via `f.Bytes()`. No external `terraform fmt`.
6. PUT the file via the existing `client.PutFileContent` call at
   [`iac_github.go:1134`](../../internal/api/handlers/iac_github.go).
7. Open the PR. Title drops `[needs manual merge]`; the
   `squadron/needs-manual-merge` label is NOT applied. PR body
   names the patched address + operations applied.

Any `PatchFailed` falls through to the slice 1.5 append-only path
with the manual-merge label. The recommendation is never lost —
the operator gets the slice 1.5 experience plus a one-line note
in the PR body naming the failure reason.

## 5. Formatting and comment preservation

`hclwrite` round-trips "most formatting and all comments" with
known edges (comments inside list literals, multi-line heredocs).
The test suite asserts roundtrip preservation across hclwrite's
own 50+ documented corner cases (vendored as Squadron fixtures)
plus a 50-file corpus of operator-written Terraform spanning
tabs/spaces, quote style, `=` alignment, trailing commas, and
comments around resource blocks. Per fixture: parse → no-op
patch → serialize → byte-equal the input. Divergence is a
slice 2 blocker.

## 6. lifecycle.ignore_changes detection

If the target resource carries
`lifecycle { ignore_changes = [layers] }` and the patch touches
`["layers"]`, the patch is a no-op at apply time though the file
change is real. Squadron can't tell whether the hint is
intentional (state-import staging, drift acceptance) or stale.
Detect at PR time, warn in PR body — NOT blocking, since the
operator may want the file change for state-import or future
ignore-changes removal. The `recommendation.pr_opened` audit
payload gains `patch_no_op_at_apply: true`.

## 7. List append vs replace semantics

For `enabled_cluster_log_types` and `layers` the op is
append-and-dedupe, NOT replace — replace would wipe the
operator's existing instrumentation. Dedupe by case-sensitive
string equality; preserve original order of existing entries;
append new entries at the end in patch order; preserve comments
adjacent to existing entries (hclwrite-handled; corpus asserts).

At a map-valued path, the op is `map_merge`: set the named key,
leave siblings untouched. `scalar_set` at a map root would
replace the entire map and is a malformed-patch error.

## 8. Nested-block update semantics

`aws_lb.access_logs` — singleton: `nested_block_set` calls
`Body().FirstMatchingBlock("access_logs", nil)`, sets the named
attributes, falls back to `Body().AppendNewBlock` if absent.

`aws_ecs_cluster.setting` — repeated, keyed: walk
`Body().Blocks()` filtering `block.Type() == "setting"`, match
where `name == value.key_value` (`"containerInsights"`), set
`value`. Append a new
`setting { name = "containerInsights" value = "enabled" }` if no
match. The proposer's schema distinguishes singleton vs
repeated-keyed by op name; the handler asserts op-vs-block-arity
at validation time.

## 9. Threat model

| Threat | Mitigation |
| ------ | ---------- |
| File has syntax errors today. | `PatchFailed{reason: "parse_error"}` → fall through to slice 1.5 append; PR opens with manual-merge label; parse error in PR body. |
| Target address doesn't exist (operator renamed). | `PatchFailed{reason: "resource_not_found"}` with existing addresses as a hint. Fall through. |
| Multiple resources match. | `PatchFailed{reason: "ambiguous_address"}` asks for `for_each`/`count` key disambiguation. Fall through. |
| Proposer emits a malformed patch (unknown `op`, missing path, op-arity mismatch). | Schema validation BEFORE any GitHub call. 422 `MalformedPatch`; no branch; audit `recommendation.pr_open_failed`. |
| `hclwrite` round-trip drops a comment. | §5 no-op corpus catches it pre-ship. Defense in depth: post-PUT the handler re-GETs the file, re-applies, byte-compares; divergence → `roundtrip_divergence` → fall through. |
| HCL writer corrupts the file silently. | Squadron never writes default (slice 1 §9, enforced at [`client.go:69`](../../internal/iac/github/client.go) + [`iac_github.go:979`](../../internal/api/handlers/iac_github.go)). Operator's PR review is the last gate. |
| `lifecycle.ignore_changes` makes the patch a no-op at apply. | §6: detect, warn in PR body, audit `patch_no_op_at_apply: true`. Not blocking. |

## 10. Slice 2 contract

**In:** `hashicorp/hcl/v2/hclwrite` dep wired into the Open-PR
handler; structured patch schema (§3) with pre-GitHub schema
validation; per-kind patch application for the 5
`patch_existing` kinds; `lifecycle.ignore_changes` detection +
PR-body warning + `patch_no_op_at_apply` audit field; roundtrip
test corpus (§5); fallback to slice 1.5 append on any
`PatchFailed`; drop the `[needs manual merge]` title prefix +
`squadron/needs-manual-merge` label on successful merge.

**Out:** auto-merge; multi-file patches; cross-resource
references (IAM role + DB `monitoring_role_arn` as one atomic
step); module-prefixed addresses; CDK / Pulumi / CloudFormation;
in-PR preview UI.

## 11. Open questions

1. **PR shape parity with slice 1.5.** Differentiate the slice 2
   PR title/body so operators see at a glance it's merge-clean,
   or keep the same shape (minus title prefix + label) so the
   PR-template checklist doesn't churn? Leaning same-shape —
   absence of the warning IS the signal.
2. **`squadronctl iac dry-run`.** Local CLI that previews the
   patched file before Open PR. Reduces "did Squadron really do
   the right thing" anxiety; adds a tool surface to maintain.
3. **Cross-resource patches.** "Add IAM role AND set
   `monitoring_role_arn`" is structurally a 2-resource change.
   Slice 2 keeps these as two proposer steps. Worth a
   single-step cross-resource schema, or does
   step-as-atomic-unit hold?
4. **Patch schema as public interface.** Internal v0 contract,
   or public surface for operators writing custom patches?
5. **Test corpus floor.** Is 50 operator-written files enough,
   or do we need broader public-repo sampling for the long tail
   of style choices?

## 12. Acceptance tests

Seven tests in `internal/api/handlers/iac_github_patch_test.go`,
each pinned to a `patch_existing` kind or a cross-cutting case.

1. **patch_lambda_otel_layer_appends_layer_and_merges_env.**
   Placement declares
   `aws_lambda_function.squadron_test_function_node_2` with
   `layers = ["arn:...:layer:existing-layer:1"]` and
   `environment { variables = { FOO = "bar" } }`. Apply the §3
   patch. Assert: `layers` becomes the existing arn + the OTel
   arn (in order); `FOO == "bar"` preserved;
   `AWS_LAMBDA_EXEC_WRAPPER == "/opt/otel-handler"`. PR title
   has NO `[needs manual merge]`. Labels: `squadron`,
   `squadron/lambda-otel-layer`; NOT
   `squadron/needs-manual-merge`. Audit:
   `manual_merge_required=false`.

2. **patch_rds_pi_em_sets_scalar_bundle.** Placement declares
   `aws_db_instance.main` with no PI/EM attributes. Apply four
   `scalar_set` patches (`performance_insights_enabled`,
   `performance_insights_retention_period`,
   `monitoring_interval`, `monitoring_role_arn`). Assert: all
   four appear with proposer values; no other attribute
   modified; trailing-comma + comment style on adjacent
   attributes byte-equal to input.

3. **patch_alb_access_logs_creates_singleton_nested_block.**
   Placement declares `aws_lb.edge` with no `access_logs`.
   Apply one `nested_block_set` with `value = { bucket:
   "my-bucket", enabled: true, prefix: "alb" }`. Assert: new
   `access_logs { ... }` block with all three attributes.
   Re-run against a file where `access_logs` already exists
   with different values: updated in place (NOT duplicated).

4. **patch_eks_cluster_logging_appends_and_dedupes.** Placement
   declares `aws_eks_cluster.prod` with
   `enabled_cluster_log_types = ["api", "audit"]`. Apply one
   `list_append_dedupe` with
   `value = ["audit", "authenticator", "controllerManager"]`.
   Assert: result is
   `["api", "audit", "authenticator", "controllerManager"]` —
   duplicate "audit" folded, original order first, new entries
   appended in order.

5. **patch_ecs_container_insights_finds_or_creates_setting_block.**
   Two sub-cases. (a) Existing
   `setting { name = "containerInsights" value = "disabled" }`
   → patch updates `value` to `"enabled"`. (b) No `setting`
   block → patch appends
   `setting { name = "containerInsights" value = "enabled" }`.
   Both assert: a sibling
   `setting { name = "executeCommandConfiguration" ... }` stays
   byte-identical.

6. **patch_lifecycle_ignore_changes_warns_in_pr_body.** Placement
   declares the test-1 lambda plus
   `lifecycle { ignore_changes = [layers] }`. Apply the same
   patch. Assert: patch IS applied; PR body carries §6 warning
   naming `layers`; audit carries `patch_no_op_at_apply: true`.

7. **patch_failed_parse_falls_through_to_slice_1_5_append.**
   Placement file is malformed HCL (missing closing brace on a
   prior block). Proposer emits a valid lambda-otel-layer patch.
   Assert: handler hits `PatchFailed{reason: "parse_error"}`,
   falls through to `appendSnippetWithTrailingNewline`; PR opens
   with `[needs manual merge]` title prefix AND
   `squadron/needs-manual-merge` label; audit carries
   `manual_merge_required=true` plus
   `patch_failed_reason: "parse_error"`.
