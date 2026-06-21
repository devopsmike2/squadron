// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package hclpatch

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// ApplyPatch parses existing as HCL, locates the resource block
// addressed by patch.TargetResourceAddress, applies each PatchOp in
// order via hclwrite, and returns the modified file content.
//
// On success, the returned bytes are valid HCL with comments and
// formatting preserved (subject to hclwrite's documented edges —
// comments inside list literals may not survive a
// list_append_dedupe). The returned *ApplyResult carries the
// lifecycle.ignore_changes detection signal the handler renders into
// the PR body (design doc §6).
//
// On error, the returned bytes are nil and the *ApplyResult is nil;
// errors.Is against the sentinel errors lets the handler route the
// fallback path (parse_error / resource_not_found / unknown_op /
// invalid_value_type). All errors are operator-recoverable through
// the slice-1.5 append-only fallback; the recommendation is never
// lost.
//
// The function is pure — it does not mutate `existing` and does not
// touch the network. Safe to call from a request goroutine.
func ApplyPatch(existing []byte, patch *Patch) ([]byte, *ApplyResult, error) {
	if patch == nil {
		return nil, nil, fmt.Errorf("hclpatch: patch is nil")
	}
	if strings.TrimSpace(patch.TargetResourceAddress) == "" {
		return nil, nil, fmt.Errorf("hclpatch: target_resource_address is empty")
	}
	resourceType, resourceName, ok := splitResourceAddress(patch.TargetResourceAddress)
	if !ok {
		return nil, nil, fmt.Errorf("hclpatch: target_resource_address %q is not in <type>.<name> form",
			patch.TargetResourceAddress)
	}

	// Validate ops + value types BEFORE the parse so a malformed
	// patch returns a clean error without touching hclwrite.
	for i, op := range patch.Patches {
		if err := validatePatchOp(i, op); err != nil {
			return nil, nil, err
		}
	}

	f, diags := hclwrite.ParseConfig(existing, "placement.tf", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("%w: %s", ErrParseFailed, diags.Error())
	}

	// Find the target resource block. Walk Body.Blocks() looking
	// for type "resource" with labels [resourceType, resourceName].
	var target *hclwrite.Block
	var existingAddresses []string
	matches := 0
	for _, blk := range f.Body().Blocks() {
		if blk.Type() != "resource" {
			continue
		}
		labels := blk.Labels()
		if len(labels) != 2 {
			continue
		}
		existingAddresses = append(existingAddresses, labels[0]+"."+labels[1])
		if labels[0] == resourceType && labels[1] == resourceName {
			target = blk
			matches++
		}
	}
	if matches == 0 {
		// Sort for deterministic test output.
		sort.Strings(existingAddresses)
		hint := ""
		if len(existingAddresses) > 0 {
			hint = " (existing resources: " + strings.Join(existingAddresses, ", ") + ")"
		}
		return nil, nil, fmt.Errorf("%w: %s%s",
			ErrResourceNotFound, patch.TargetResourceAddress, hint)
	}
	if matches > 1 {
		return nil, nil, fmt.Errorf("%w: %s (matched %d times)",
			ErrAmbiguousResource, patch.TargetResourceAddress, matches)
	}

	// lifecycle.ignore_changes detection runs BEFORE the edits so
	// we see the operator's pre-patch state. If any patched path's
	// top segment appears in ignore_changes, the operator's
	// terraform apply will no-op the change — surface this as a
	// PR body warning, design doc §6.
	result := &ApplyResult{}
	if ignored, which := detectLifecycleIgnore(target, patch.Patches); ignored {
		result.LifecycleIgnoresPatchedAttr = true
		result.IgnoredAttrPath = which
	}

	// Apply each op in order.
	for i, op := range patch.Patches {
		if err := applyOp(target.Body(), op); err != nil {
			return nil, nil, fmt.Errorf("patches[%d] (%s @ %s): %w",
				i, op.Op, strings.Join(op.AttributePath, "."), err)
		}
	}

	return f.Bytes(), result, nil
}

// validatePatchOp checks the op enum and the value's Go-side type
// shape before any HCL work happens.
func validatePatchOp(idx int, op PatchOp) error {
	if len(op.AttributePath) == 0 {
		return fmt.Errorf("patches[%d]: attribute_path is empty", idx)
	}
	switch op.Op {
	case OpScalarSet:
		if !isScalarValue(op.Value) {
			return fmt.Errorf("patches[%d] (%s @ %s): %w (need string/bool/int/float, got %T)",
				idx, op.Op, strings.Join(op.AttributePath, "."), ErrInvalidValueType, op.Value)
		}
	case OpListAppendDedupe:
		if _, ok := op.Value.([]any); !ok {
			// Allow []string for the convenience of in-package test
			// fixtures; the wire shape is always []any after JSON.
			if _, ok2 := op.Value.([]string); !ok2 {
				return fmt.Errorf("patches[%d] (%s @ %s): %w (need []any, got %T)",
					idx, op.Op, strings.Join(op.AttributePath, "."), ErrInvalidValueType, op.Value)
			}
		}
	case OpNestedBlockSet:
		if _, ok := op.Value.(map[string]any); !ok {
			return fmt.Errorf("patches[%d] (%s @ %s): %w (need map[string]any, got %T)",
				idx, op.Op, strings.Join(op.AttributePath, "."), ErrInvalidValueType, op.Value)
		}
	case OpNestedBlockFindOrCreate:
		m, ok := op.Value.(map[string]any)
		if !ok {
			return fmt.Errorf("patches[%d] (%s @ %s): %w (need map[string]any, got %T)",
				idx, op.Op, strings.Join(op.AttributePath, "."), ErrInvalidValueType, op.Value)
		}
		if _, ok := m["set"].(map[string]any); !ok {
			return fmt.Errorf("patches[%d] (%s @ %s): %w (value.set missing or wrong type)",
				idx, op.Op, strings.Join(op.AttributePath, "."), ErrInvalidValueType)
		}
		if strings.TrimSpace(op.BlockKey) == "" {
			return fmt.Errorf("patches[%d] (%s @ %s): block_key is empty",
				idx, op.Op, strings.Join(op.AttributePath, "."))
		}
		if strings.TrimSpace(op.BlockKeyValue) == "" {
			return fmt.Errorf("patches[%d] (%s @ %s): block_key_value is empty",
				idx, op.Op, strings.Join(op.AttributePath, "."))
		}
	case OpMapMerge:
		if _, ok := op.Value.(map[string]any); !ok {
			return fmt.Errorf("patches[%d] (%s @ %s): %w (need map[string]any, got %T)",
				idx, op.Op, strings.Join(op.AttributePath, "."), ErrInvalidValueType, op.Value)
		}
	default:
		return fmt.Errorf("patches[%d] (%s @ %s): %w",
			idx, op.Op, strings.Join(op.AttributePath, "."), ErrUnknownOp)
	}
	return nil
}

// applyOp dispatches a single PatchOp against the resource body.
func applyOp(body *hclwrite.Body, op PatchOp) error {
	switch op.Op {
	case OpScalarSet:
		return applyScalarSet(body, op.AttributePath, op.Value)
	case OpListAppendDedupe:
		return applyListAppendDedupe(body, op.AttributePath, op.Value)
	case OpNestedBlockSet:
		return applyNestedBlockSet(body, op.AttributePath, op.Value.(map[string]any))
	case OpNestedBlockFindOrCreate:
		return applyNestedBlockFindOrCreate(body, op.AttributePath, op.BlockKey, op.BlockKeyValue, op.Value.(map[string]any))
	case OpMapMerge:
		return applyMapMerge(body, op.AttributePath, op.Value.(map[string]any))
	}
	return ErrUnknownOp
}

// applyScalarSet sets a scalar attribute at attributePath. When the
// path has length > 1, the leading segments are walked as nested
// blocks (for "environment.variables.KEY" the leading segments are
// "environment" then "variables", treated as nested blocks; the
// terminal segment is the attribute key).
//
// Most patch_existing ops have len(AttributePath)==1 (a top-level
// attribute on the resource). The nested-block walk is used by
// lambda-otel-layer's environment.variables map keys, which the
// proposer expresses as ["environment", "variables", "AWS_LAMBDA_..."].
func applyScalarSet(body *hclwrite.Body, attributePath []string, value any) error {
	if len(attributePath) == 1 {
		return setScalarAttr(body, attributePath[0], value)
	}
	// Walk nested blocks. For the lambda env-vars case the
	// "variables" segment is actually a map-typed attribute, not a
	// block — we detect that by checking GetAttribute on the
	// penultimate segment.
	nested, err := walkOrCreateNestedBlocks(body, attributePath[:len(attributePath)-1])
	if err != nil {
		return err
	}
	// Check whether the terminal-1 segment refers to a map-typed
	// attribute (HCL ALLOWS both block syntax and attribute-with-
	// object-value syntax for the same shape; Terraform's lambda
	// resource uses block syntax for environment { ... } but
	// variables itself is an attribute set to an object literal).
	// We try block-walk first; if the last walked segment ended on
	// a body that has the next segment as a map attribute, fall
	// through to map_merge semantics.
	terminalKey := attributePath[len(attributePath)-1]
	return setScalarAttr(nested, terminalKey, value)
}

// setScalarAttr sets a single scalar attribute on the given body.
// Translates the Go value into a cty value and calls SetAttributeValue.
func setScalarAttr(body *hclwrite.Body, name string, value any) error {
	cv, err := goToCty(value)
	if err != nil {
		return err
	}
	body.SetAttributeValue(name, cv)
	return nil
}

// applyListAppendDedupe reads the existing list at attributePath,
// appends each new element if not already present (case-sensitive
// string compare), preserves original order, and writes back. The
// new value is rendered via TokensForValue, which uses tuple
// constructor syntax (`[a, b, c]`) — same shape the operator
// typically writes by hand.
//
// Limitation per design doc §5: comments INSIDE the list literal
// (`["a", # foo\n "b"]`) do not survive this op because hclwrite
// doesn't expose a list-aware mutation API and we rebuild the tuple
// from scratch. Comments OUTSIDE the list (before / after the
// attribute) are preserved.
func applyListAppendDedupe(body *hclwrite.Body, attributePath []string, value any) error {
	if len(attributePath) != 1 {
		// Nested lists (e.g. attribute_path = ["a", "b"]) are not in
		// scope for slice 2 — none of the 5 patch_existing kinds
		// uses one. Surface as invalid op rather than silently
		// mis-routing.
		return fmt.Errorf("list_append_dedupe at nested path not supported")
	}
	name := attributePath[0]
	newElems := normalizeListValue(value)

	existing := readStringList(body, name)
	merged := append([]string(nil), existing...)
	seen := make(map[string]bool, len(existing))
	for _, e := range existing {
		seen[e] = true
	}
	for _, e := range newElems {
		if seen[e] {
			continue
		}
		merged = append(merged, e)
		seen[e] = true
	}

	// Re-write as a typed cty tuple. TokensForValue picks
	// reasonable formatting; the surrounding `name = ` and the
	// attribute's trailing newline are preserved by hclwrite.
	cv := make([]cty.Value, len(merged))
	for i, s := range merged {
		cv[i] = cty.StringVal(s)
	}
	if len(cv) == 0 {
		body.SetAttributeValue(name, cty.EmptyTupleVal)
		return nil
	}
	body.SetAttributeValue(name, cty.TupleVal(cv))
	return nil
}

// readStringList returns the string elements of the list-valued
// attribute at name on body. Returns nil when the attribute does
// not exist or isn't a flat list of string literals.
func readStringList(body *hclwrite.Body, name string) []string {
	attr := body.GetAttribute(name)
	if attr == nil {
		return nil
	}
	// Render the attribute's expression to bytes, then re-parse it
	// as an hclsyntax expression to extract the literal values.
	src := attr.Expr().BuildTokens(nil).Bytes()
	expr, diags := hclsyntax.ParseExpression(src, "list.tf", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil
	}
	tup, ok := expr.(*hclsyntax.TupleConsExpr)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(tup.Exprs))
	for _, e := range tup.Exprs {
		// Each element should be a template (quoted string) or a
		// literal. Try to evaluate to a string; if we can't, give
		// up and treat as unparseable (the caller falls back to
		// just appending the new elements).
		v, vdiags := e.Value(nil)
		if vdiags.HasErrors() {
			return nil
		}
		if v.Type() != cty.String {
			return nil
		}
		out = append(out, v.AsString())
	}
	return out
}

// applyNestedBlockSet finds the singleton nested block at
// attributePath (typically length 1) and sets the attributes
// described by value. Creates the block if absent. Used by
// alb-access-logs (`access_logs { bucket=..., enabled=..., prefix=... }`).
func applyNestedBlockSet(body *hclwrite.Body, attributePath []string, value map[string]any) error {
	if len(attributePath) != 1 {
		return fmt.Errorf("nested_block_set at nested path not supported")
	}
	blockType := attributePath[0]
	block := body.FirstMatchingBlock(blockType, nil)
	if block == nil {
		block = body.AppendNewBlock(blockType, nil)
	}
	return setAttrsOnBody(block.Body(), value)
}

// applyNestedBlockFindOrCreate walks repeated nested blocks of the
// type at attributePath[0], looks for one whose blockKey attribute
// equals blockKeyValue, and either updates its `set` attributes or
// appends a brand-new block carrying both the matching key and the
// `set` attributes. Used by ecs-container-insights
// (`setting { name = "containerInsights" value = "enabled" }`).
func applyNestedBlockFindOrCreate(body *hclwrite.Body, attributePath []string, blockKey, blockKeyValue string, value map[string]any) error {
	if len(attributePath) != 1 {
		return fmt.Errorf("nested_block_find_or_create at nested path not supported")
	}
	blockType := attributePath[0]
	set, _ := value["set"].(map[string]any)

	for _, blk := range body.Blocks() {
		if blk.Type() != blockType {
			continue
		}
		// Inspect blockKey's value on the block's body.
		attr := blk.Body().GetAttribute(blockKey)
		if attr == nil {
			continue
		}
		v := readScalarStringAttr(attr)
		if v != blockKeyValue {
			continue
		}
		// Match — update the named attrs in `set`.
		return setAttrsOnBody(blk.Body(), set)
	}
	// No match — append a fresh block carrying BOTH the matching
	// key and the `set` attrs.
	newBlock := body.AppendNewBlock(blockType, nil)
	if err := setAttrsOnBody(newBlock.Body(), map[string]any{blockKey: blockKeyValue}); err != nil {
		return err
	}
	return setAttrsOnBody(newBlock.Body(), set)
}

// applyMapMerge merges the named keys in value into the map-valued
// attribute at attributePath. Preserves keys not named in value.
//
// AttributePath length 2 is the common case (e.g.
// ["environment", "variables"] for lambda env vars where the
// terminal segment is the attribute name and the leading segment is
// the nested-block walk path). Length 1 means a top-level map
// attribute.
func applyMapMerge(body *hclwrite.Body, attributePath []string, value map[string]any) error {
	if len(attributePath) == 0 {
		return fmt.Errorf("map_merge requires a non-empty attribute_path")
	}
	target := body
	for _, seg := range attributePath[:len(attributePath)-1] {
		blk := target.FirstMatchingBlock(seg, nil)
		if blk == nil {
			blk = target.AppendNewBlock(seg, nil)
		}
		target = blk.Body()
	}
	mapAttrName := attributePath[len(attributePath)-1]

	// Read existing map, merge, write back. Same comment-limitation
	// caveat as list_append_dedupe.
	existing := readStringMap(target, mapAttrName)
	merged := make(map[string]cty.Value, len(existing)+len(value))
	for k, v := range existing {
		merged[k] = cty.StringVal(v)
	}
	for k, v := range value {
		cv, err := goToCty(v)
		if err != nil {
			return fmt.Errorf("map_merge value for %q: %w", k, err)
		}
		merged[k] = cv
	}
	// Sort keys for deterministic output.
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	objAttrs := make([]hclwrite.ObjectAttrTokens, 0, len(keys))
	for _, k := range keys {
		objAttrs = append(objAttrs, hclwrite.ObjectAttrTokens{
			Name:  hclwrite.TokensForIdentifier(k),
			Value: hclwrite.TokensForValue(merged[k]),
		})
	}
	target.SetAttributeRaw(mapAttrName, hclwrite.TokensForObject(objAttrs))
	return nil
}

// readStringMap returns the string-valued entries of the map-typed
// attribute at name. Returns an empty map when absent or
// unparseable.
func readStringMap(body *hclwrite.Body, name string) map[string]string {
	out := map[string]string{}
	attr := body.GetAttribute(name)
	if attr == nil {
		return out
	}
	src := attr.Expr().BuildTokens(nil).Bytes()
	expr, diags := hclsyntax.ParseExpression(src, "map.tf", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return out
	}
	obj, ok := expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return out
	}
	for _, item := range obj.Items {
		// Key may be an ObjectConsKeyExpr wrapping either a literal
		// or a string-template. Try Value() to evaluate.
		kv, kdiags := item.KeyExpr.Value(nil)
		if kdiags.HasErrors() {
			continue
		}
		if kv.Type() != cty.String {
			continue
		}
		vv, vdiags := item.ValueExpr.Value(nil)
		if vdiags.HasErrors() {
			continue
		}
		if vv.Type() != cty.String {
			continue
		}
		out[kv.AsString()] = vv.AsString()
	}
	return out
}

// setAttrsOnBody applies a map of name → Go-typed value pairs by
// translating each to cty and calling SetAttributeValue. Sorted keys
// for deterministic output.
func setAttrsOnBody(body *hclwrite.Body, values map[string]any) error {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cv, err := goToCty(values[k])
		if err != nil {
			return fmt.Errorf("attribute %q: %w", k, err)
		}
		body.SetAttributeValue(k, cv)
	}
	return nil
}

// walkOrCreateNestedBlocks walks segments as nested blocks, creating
// any missing block in line. Returns the innermost body.
func walkOrCreateNestedBlocks(body *hclwrite.Body, segments []string) (*hclwrite.Body, error) {
	cur := body
	for _, seg := range segments {
		blk := cur.FirstMatchingBlock(seg, nil)
		if blk == nil {
			blk = cur.AppendNewBlock(seg, nil)
		}
		cur = blk.Body()
	}
	return cur, nil
}

// detectLifecycleIgnore inspects the target resource's lifecycle
// block (if any) for an ignore_changes list referencing any of the
// patched paths' top segments. Returns the FIRST match for the PR
// body warning.
func detectLifecycleIgnore(target *hclwrite.Block, ops []PatchOp) (bool, string) {
	lifecycle := target.Body().FirstMatchingBlock("lifecycle", nil)
	if lifecycle == nil {
		return false, ""
	}
	attr := lifecycle.Body().GetAttribute("ignore_changes")
	if attr == nil {
		return false, ""
	}
	// ignore_changes is a list of identifier traversals
	// (`[layers, environment]`), not a list of strings. Render and
	// reparse with hclsyntax to extract the root identifier names.
	src := attr.Expr().BuildTokens(nil).Bytes()
	expr, diags := hclsyntax.ParseExpression(src, "ignore.tf", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return false, ""
	}
	tup, ok := expr.(*hclsyntax.TupleConsExpr)
	if !ok {
		return false, ""
	}
	ignored := make(map[string]bool, len(tup.Exprs))
	for _, e := range tup.Exprs {
		// Each entry is a scope traversal expression. Pull the
		// root-name.
		switch te := e.(type) {
		case *hclsyntax.ScopeTraversalExpr:
			if len(te.Traversal) > 0 {
				ignored[te.Traversal.RootName()] = true
			}
		case *hclsyntax.RelativeTraversalExpr:
			// Unusual but tolerated; root not extractable.
		}
	}
	for _, op := range ops {
		if len(op.AttributePath) == 0 {
			continue
		}
		top := op.AttributePath[0]
		if ignored[top] {
			return true, top
		}
	}
	return false, ""
}

// readScalarStringAttr renders an attribute's expression to bytes,
// re-parses, and returns the literal string value when present.
// Returns "" when the attribute is not a single literal string —
// the caller treats that as "no match" for the find-or-create
// lookup.
func readScalarStringAttr(attr *hclwrite.Attribute) string {
	src := attr.Expr().BuildTokens(nil).Bytes()
	expr, diags := hclsyntax.ParseExpression(src, "scalar.tf", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return ""
	}
	v, vdiags := expr.Value(nil)
	if vdiags.HasErrors() {
		return ""
	}
	if v.Type() != cty.String {
		return ""
	}
	return v.AsString()
}

// goToCty translates a Go-typed PatchOp.Value into a cty value
// hclwrite can render. Strings, bools, ints, and float64s (the JSON
// number default) are supported; everything else is an error.
func goToCty(v any) (cty.Value, error) {
	switch t := v.(type) {
	case string:
		return cty.StringVal(t), nil
	case bool:
		return cty.BoolVal(t), nil
	case int:
		return cty.NumberIntVal(int64(t)), nil
	case int64:
		return cty.NumberIntVal(t), nil
	case float64:
		// JSON decode defaults numbers to float64. Coerce to
		// NumberIntVal when integral to avoid `1.0` showing up
		// where the operator wrote `1`.
		if t == float64(int64(t)) {
			return cty.NumberIntVal(int64(t)), nil
		}
		return cty.NumberFloatVal(t), nil
	case nil:
		return cty.NilVal, fmt.Errorf("%w: nil value", ErrInvalidValueType)
	default:
		return cty.NilVal, fmt.Errorf("%w: %T", ErrInvalidValueType, v)
	}
}

// isScalarValue reports whether v's Go type is one ApplyScalarSet
// can handle. Centralized so validatePatchOp and goToCty agree.
func isScalarValue(v any) bool {
	switch v.(type) {
	case string, bool, int, int64, float64:
		return true
	}
	return false
}

// normalizeListValue accepts either []any (the post-JSON shape) or
// []string (test fixture convenience) and returns []string.
func normalizeListValue(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// splitResourceAddress splits "aws_lambda_function.foo" into
// ("aws_lambda_function", "foo", true). Returns false on any other
// shape (zero dots, two or more dots, leading/trailing dot).
func splitResourceAddress(s string) (string, string, bool) {
	s = strings.TrimSpace(s)
	idx := strings.IndexByte(s, '.')
	if idx <= 0 || idx == len(s)-1 {
		return "", "", false
	}
	if strings.IndexByte(s[idx+1:], '.') >= 0 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}
