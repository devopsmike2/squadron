// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package tfimport

import (
	"strings"
	"testing"
)

func TestParseExistingImportIDs(t *testing.T) {
	src := []byte(`# header
import {
  to = aws_instance.imported_i_0abc
  id = "i-0abc"
}

import {
  to = aws_s3_bucket.imported_logs
  id = "my-logs"
}
`)
	ids := ParseExistingImportIDs(src)
	if _, ok := ids["i-0abc"]; !ok {
		t.Errorf("missing i-0abc in %v", ids)
	}
	if _, ok := ids["my-logs"]; !ok {
		t.Errorf("missing my-logs in %v", ids)
	}
	if len(ids) != 2 {
		t.Errorf("want 2 ids, got %d: %v", len(ids), ids)
	}
}

func TestParseExistingImportIDs_MalformedSafe(t *testing.T) {
	if ids := ParseExistingImportIDs([]byte("not { valid ===")); len(ids) != 0 {
		t.Errorf("malformed should yield empty, got %v", ids)
	}
	if ids := ParseExistingImportIDs(nil); len(ids) != 0 {
		t.Errorf("nil should yield empty, got %v", ids)
	}
}

func TestDedupByImportID(t *testing.T) {
	blocks := []ImportBlock{
		{TFType: "aws_instance", TFAddress: "aws_instance.imported_a", ImportID: "i-a"},
		{TFType: "aws_instance", TFAddress: "aws_instance.imported_b", ImportID: "i-b"},
	}
	existing := map[string]struct{}{"i-a": {}}
	kept, removed := DedupByImportID(blocks, existing)
	if removed != 1 || len(kept) != 1 || kept[0].ImportID != "i-b" {
		t.Errorf("dedup wrong: kept=%v removed=%d", kept, removed)
	}
	// Round-trip: dedup against the IDs parsed from a rendered file is a no-op
	// on a second pass (idempotency).
	rendered := Render(kept, nil)
	ids := ParseExistingImportIDs([]byte(rendered))
	kept2, removed2 := DedupByImportID(kept, ids)
	if len(kept2) != 0 || removed2 != 1 {
		t.Errorf("idempotency failed: kept2=%v removed2=%d", kept2, removed2)
	}
}

func TestRenderBlocksOnly_NoHeader(t *testing.T) {
	out := RenderBlocksOnly([]ImportBlock{{TFType: "aws_instance", TFAddress: "aws_instance.imported_a", ImportID: "i-a"}})
	if strings.Contains(out, "generate-config-out") {
		t.Errorf("blocks-only should have no header, got:\n%s", out)
	}
	if !strings.Contains(out, `id = "i-a"`) {
		t.Errorf("missing block body:\n%s", out)
	}
}
