// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package tfimport

import (
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// ParseExistingImportIDs returns the set of import IDs already declared
// in an existing Terraform file (the `id = "..."` of every `import {}`
// block). Used to dedup by the underlying CLOUD resource: re-running
// import generation never re-emits a block for a resource already in a
// prior squadron_imports.tf, so the flow is idempotent.
//
// Parse failure (or a non-literal id) yields an empty/partial set
// rather than an error — the safe default is "treat as not-yet-imported"
// so a malformed existing file never silently drops a real import.
func ParseExistingImportIDs(src []byte) map[string]struct{} {
	out := map[string]struct{}{}
	if len(src) == 0 {
		return out
	}
	f, diags := hclsyntax.ParseConfig(src, "imports.tf", hcl.InitialPos)
	if diags.HasErrors() || f == nil {
		return out
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return out
	}
	for _, blk := range body.Blocks {
		if blk.Type != "import" {
			continue
		}
		attr, ok := blk.Body.Attributes["id"]
		if !ok {
			continue
		}
		v, vdiags := attr.Expr.Value(nil)
		if vdiags.HasErrors() || v.IsNull() || !v.Type().Equals(ctyString) {
			continue
		}
		id := strings.TrimSpace(v.AsString())
		if id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

// DedupByImportID drops blocks whose ImportID is already present in
// `existing`, returning the kept blocks plus the count removed. Order is
// preserved.
func DedupByImportID(blocks []ImportBlock, existing map[string]struct{}) (kept []ImportBlock, removed int) {
	if len(existing) == 0 {
		return blocks, 0
	}
	for _, b := range blocks {
		if _, dup := existing[b.ImportID]; dup {
			removed++
			continue
		}
		kept = append(kept, b)
	}
	return kept, removed
}

// RenderBlocksOnly emits just the import blocks (no header), for
// appending to an existing squadron_imports.tf that already carries the
// header + earlier blocks.
func RenderBlocksOnly(blocks []ImportBlock) string {
	var sb strings.Builder
	for _, blk := range blocks {
		if blk.Region != "" {
			sb.WriteString("# region: " + blk.Region + "\n")
		}
		sb.WriteString("import {\n")
		sb.WriteString("  to = " + blk.TFAddress + "\n")
		sb.WriteString("  id = " + quote(blk.ImportID) + "\n")
		sb.WriteString("}\n\n")
	}
	return sb.String()
}
