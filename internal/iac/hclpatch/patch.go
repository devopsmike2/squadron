// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package hclpatch is the slice-2 (v0.89.12 #628 Stream 29) HCL-aware
// merge engine. Given an existing Terraform placement file and a
// structured Patch describing the per-attribute / per-nested-block
// edits the proposer wants applied to a specific resource block, it
// returns the modified file content — comments and formatting
// preserved — via hashicorp/hcl/v2/hclwrite.
//
// Five operations are locked (per design doc #603-slice-2 §3):
//
//   - scalar_set                 — overwrite a scalar attribute
//   - list_append_dedupe         — append to a list, case-sensitive dedupe
//   - nested_block_set           — set attrs on a singleton nested block (create if absent)
//   - nested_block_find_or_create — find a keyed-repeated nested block, update or append
//   - map_merge                  — set named keys on an object/map attribute without disturbing siblings
//
// The handler at internal/api/handlers/iac_github.go consumes ApplyPatch.
// On any error (parse, unknown resource, unknown op, invalid value
// type) the handler falls back to the slice-1.5 append-only path —
// the operator never loses a recommendation.
package hclpatch

import "errors"

// Patch is the proposer-emitted structured edit for a single
// patch_existing recommendation step. The handler decodes it from the
// Open-PR request body's optional hcl_patch field and hands it to
// ApplyPatch.
//
// Disposition is "patch_existing" in all current callers; the field is
// retained on the wire so a future slice 3 disposition (e.g. a third
// hybrid path) can be distinguished without re-shaping the request.
type Patch struct {
	// Kind names the recommendation kind (lambda-otel-layer,
	// rds-pi-em, ...). Carried for audit + cross-correlation; not
	// consumed by ApplyPatch itself, which routes on the per-op
	// type rather than the kind.
	Kind string `json:"kind"`

	// Disposition is "patch_existing" for this package. The
	// handler validates the kind→disposition mapping before
	// invoking ApplyPatch.
	Disposition string `json:"disposition"`

	// TargetResourceAddress is the two-segment
	// <resource_type>.<name> form (e.g.
	// "aws_lambda_function.squadron_test_function_node_2").
	// Module-prefixed addresses (module.foo.aws_lambda_function.bar)
	// are out of scope per design doc §3 — the placement-map row
	// pins the file, which pins the module.
	TargetResourceAddress string `json:"target_resource_address"`

	// Patches is the ordered list of per-attribute / per-nested-block
	// edits to apply. ApplyPatch runs them in order; a later op may
	// reference (and overwrite) state a previous op wrote.
	Patches []PatchOp `json:"patches"`
}

// PatchOp is one structured edit. The five Op values are locked
// (design doc §3).
type PatchOp struct {
	// AttributePath is the dotted path into the resource block.
	// Examples:
	//   ["layers"]                            — top-level list attribute
	//   ["environment", "variables", "AWS_LAMBDA_EXEC_WRAPPER"]
	//                                          — nested map key
	//   ["access_logs"]                       — nested block at resource level
	//   ["setting"]                           — repeated nested block
	AttributePath []string `json:"attribute_path"`

	// Op is one of: "scalar_set", "list_append_dedupe",
	// "nested_block_set", "nested_block_find_or_create", "map_merge".
	// Unknown values surface as ErrUnknownOp at apply time.
	Op string `json:"op"`

	// Value is op-specific:
	//   - scalar_set: string | bool | number
	//   - list_append_dedupe: []any (each element appended after dedupe)
	//   - nested_block_set: map[string]any (attrs to set on the block)
	//   - nested_block_find_or_create: map[string]any (must include
	//     the matching key attribute named in BlockKey, plus a "set"
	//     object naming the attrs to write)
	//   - map_merge: map[string]any (named keys merged without
	//     disturbing existing siblings)
	Value any `json:"value"`

	// BlockKey is the attribute name used to match repeated nested
	// blocks (e.g. "name" for aws_ecs_cluster.setting). Used only
	// by nested_block_find_or_create.
	BlockKey string `json:"block_key,omitempty"`

	// BlockKeyValue is the value to match on the BlockKey attribute
	// (e.g. "containerInsights"). Used only by
	// nested_block_find_or_create.
	BlockKeyValue string `json:"block_key_value,omitempty"`
}

// Op enum, locked per design doc §3.
const (
	OpScalarSet                = "scalar_set"
	OpListAppendDedupe         = "list_append_dedupe"
	OpNestedBlockSet           = "nested_block_set"
	OpNestedBlockFindOrCreate  = "nested_block_find_or_create"
	OpMapMerge                 = "map_merge"
)

// Sentinel errors. Callers errors.Is against these; the Open-PR
// handler maps each to a `hcl_patch_failure_reason` audit payload
// field and falls back to the slice-1.5 append-only path.
var (
	// ErrParseFailed — the existing file content was not valid HCL.
	// Wrapped error carries the hclwrite parse diagnostic.
	ErrParseFailed = errors.New("hclpatch: existing HCL did not parse")

	// ErrResourceNotFound — the Patch's TargetResourceAddress did
	// not match any resource block in the file. The wrapped error
	// carries the addresses that DO exist as a hint, so a future
	// CLI dry-run can render them.
	ErrResourceNotFound = errors.New("hclpatch: target_resource_address not found")

	// ErrAmbiguousResource — defensively returned when more than
	// one resource block matches TargetResourceAddress. Should not
	// happen since Terraform itself rejects duplicate resource
	// addresses at apply time, but the check is cheap and catches
	// pathological author input.
	ErrAmbiguousResource = errors.New("hclpatch: multiple resources match target_resource_address")

	// ErrUnknownOp — the PatchOp.Op was not one of the locked enum
	// values. The proposer prompt teaches the model the five-op
	// vocabulary; an unknown op surfaces as a fallback to the
	// slice-1.5 path with a clear hcl_patch_failure_reason.
	ErrUnknownOp = errors.New("hclpatch: unknown patch op")

	// ErrInvalidValueType — the PatchOp.Value's Go type didn't
	// match what the op expects (e.g. scalar_set with a slice
	// value, list_append_dedupe with a string value). The wrapped
	// error names the op and the offending path so a future
	// proposer-eval surface can index on it.
	ErrInvalidValueType = errors.New("hclpatch: patch value has wrong type for op")
)

// ApplyResult is the side-channel hclpatch returns alongside the
// modified file content. Today it carries only the
// lifecycle.ignore_changes detection signal (design doc §6); future
// fields will land here without breaking the ApplyPatch signature.
type ApplyResult struct {
	// LifecycleIgnoresPatchedAttr is true when the target resource
	// carries `lifecycle { ignore_changes = [...] }` AND one of the
	// patched attribute paths' top segment appears in the
	// ignore_changes list. The patch is still applied — the file
	// change is real — but the operator's terraform apply will
	// no-op the corresponding attribute. The handler surfaces this
	// as a warning section in the PR body so the operator can
	// decide whether to remove the ignore_changes entry or accept
	// the staged file change.
	LifecycleIgnoresPatchedAttr bool

	// IgnoredAttrPath names the FIRST ignored path detected. Used
	// in the PR body warning. Empty when
	// LifecycleIgnoresPatchedAttr is false.
	IgnoredAttrPath string
}
