// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package discoverydrift

import "testing"

func TestBetween_AddedRemovedAndInstrumentationFlip(t *testing.T) {
	older := `{
		"compute":[{"resource_id":"i-keep","has_otel":false},{"resource_id":"i-gone","has_otel":true}],
		"functions":[{"resource_id":"fn-keep","has_otel_layer":false}],
		"databases":[{"resource_id":"db-1"}],
		"clusters":[{"resource_id":"c-1"}]
	}`
	newer := `{
		"compute":[{"resource_id":"i-keep","has_otel":true},{"resource_id":"i-new","has_otel":false}],
		"functions":[{"resource_id":"fn-keep","has_otel_layer":false},{"resource_id":"fn-new","has_otel_layer":true}],
		"databases":[{"resource_id":"db-1"},{"resource_id":"db-2"}],
		"clusters":[]
	}`
	d, err := Between(older, newer)
	if err != nil {
		t.Fatalf("Between: %v", err)
	}
	// Compute: i-new added, i-gone removed, i-keep flipped false->true.
	if len(d.Compute.Added) != 1 || d.Compute.Added[0] != "i-new" {
		t.Errorf("compute added = %v, want [i-new]", d.Compute.Added)
	}
	if len(d.Compute.Removed) != 1 || d.Compute.Removed[0] != "i-gone" {
		t.Errorf("compute removed = %v, want [i-gone]", d.Compute.Removed)
	}
	if len(d.Compute.InstrumentationChanged) != 1 ||
		d.Compute.InstrumentationChanged[0].ResourceID != "i-keep" ||
		d.Compute.InstrumentationChanged[0].Was != false || d.Compute.InstrumentationChanged[0].Now != true {
		t.Errorf("compute flips = %+v, want i-keep false->true", d.Compute.InstrumentationChanged)
	}
	// Functions: fn-new added, no flip on fn-keep.
	if len(d.Functions.Added) != 1 || d.Functions.Added[0] != "fn-new" {
		t.Errorf("functions added = %v", d.Functions.Added)
	}
	if len(d.Functions.InstrumentationChanged) != 0 {
		t.Errorf("functions flips = %v, want none", d.Functions.InstrumentationChanged)
	}
	// Databases: db-2 added. Clusters: c-1 removed.
	if len(d.Databases.Added) != 1 || d.Databases.Added[0] != "db-2" {
		t.Errorf("databases added = %v", d.Databases.Added)
	}
	if len(d.Clusters.Removed) != 1 || d.Clusters.Removed[0] != "c-1" {
		t.Errorf("clusters removed = %v", d.Clusters.Removed)
	}
	// Totals: added = i-new+fn-new+db-2 = 3; removed = i-gone+c-1 = 2; flips = 1.
	if d.TotalAdded != 3 || d.TotalRemoved != 2 || d.TotalInstrumentationChanged != 1 {
		t.Errorf("totals = %d/%d/%d, want 3/2/1", d.TotalAdded, d.TotalRemoved, d.TotalInstrumentationChanged)
	}
}

func TestBetween_FirstScanVsEmpty_AllAdded(t *testing.T) {
	newer := `{"compute":[{"resource_id":"i-1","has_otel":false}]}`
	d, err := Between("", newer)
	if err != nil {
		t.Fatalf("Between: %v", err)
	}
	if len(d.Compute.Added) != 1 || d.TotalAdded != 1 || d.TotalRemoved != 0 {
		t.Errorf("first-scan-vs-empty: %+v", d)
	}
}

func TestBetween_NoChange_EmptyDiff(t *testing.T) {
	inv := `{"compute":[{"resource_id":"i-1","has_otel":true}],"databases":[{"resource_id":"db-1"}]}`
	d, err := Between(inv, inv)
	if err != nil {
		t.Fatalf("Between: %v", err)
	}
	if d.TotalAdded != 0 || d.TotalRemoved != 0 || d.TotalInstrumentationChanged != 0 {
		t.Errorf("identical scans should diff empty, got %+v", d)
	}
	// Added/Removed are non-nil empty slices (clean JSON [] not null).
	if d.Compute.Added == nil || d.Compute.Removed == nil {
		t.Errorf("added/removed should be empty slices, not nil")
	}
}

func TestBetween_MalformedJSON_Errors(t *testing.T) {
	if _, err := Between("{not json", `{}`); err == nil {
		t.Error("expected parse error on malformed older blob")
	}
}
