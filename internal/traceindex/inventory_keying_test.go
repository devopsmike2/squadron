// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import "testing"

// inventory_keying_test.go — trace integration slice 1 chunk 4
// projection helper unit tests. The three Project*Key helpers are
// the inverse of ComputeResourceKey's tier-2/3/4 fallback chain;
// these tests pin the shape so a future ComputeResourceKey change
// breaks both sides in the same PR rather than silently producing
// non-matching keys.

func TestProjectComputeKey_HappyPath(t *testing.T) {
	cases := []struct {
		name                       string
		provider, scope, resource  string
		want                       string
	}{
		{"aws", "aws", "123456789012", "i-0abc", "aws:123456789012:i-0abc"},
		{"gcp", "gcp", "sandbox-12345", "1234567890", "gcp:sandbox-12345:1234567890"},
		{"azure", "azure", "sub-uuid", "/subscriptions/.../vm-prod", "azure:sub-uuid:/subscriptions/.../vm-prod"},
		{"oci", "oci", "ocid1.tenancy.oc1..aaa", "ocid1.instance.oc1..bbb", "oci:ocid1.tenancy.oc1..aaa:ocid1.instance.oc1..bbb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ProjectComputeKey(c.provider, c.scope, c.resource)
			if got != c.want {
				t.Errorf("ProjectComputeKey(%q,%q,%q)=%q want %q", c.provider, c.scope, c.resource, got, c.want)
			}
		})
	}
}

func TestProjectComputeKey_EmptyInputsReturnEmpty(t *testing.T) {
	cases := []struct{ provider, scope, resource string }{
		{"", "scope", "res"},
		{"aws", "", "res"},
		{"aws", "scope", ""},
		{"   ", "scope", "res"}, // trimmed to empty
		{"aws", "  ", "res"},
		{"aws", "scope", "   "},
	}
	for _, c := range cases {
		if got := ProjectComputeKey(c.provider, c.scope, c.resource); got != "" {
			t.Errorf("ProjectComputeKey(%q,%q,%q)=%q want empty", c.provider, c.scope, c.resource, got)
		}
	}
}

func TestProjectDatabaseKey_HappyPath(t *testing.T) {
	got := ProjectDatabaseKey("aws", "123", "postgresql", "db-prod")
	want := "aws:123:db:postgresql:db-prod"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

func TestProjectDatabaseKey_EmptyInputsReturnEmpty(t *testing.T) {
	if got := ProjectDatabaseKey("aws", "123", "", "db"); got != "" {
		t.Errorf("expected empty when db_system missing; got %q", got)
	}
	if got := ProjectDatabaseKey("aws", "123", "postgresql", ""); got != "" {
		t.Errorf("expected empty when db_name missing; got %q", got)
	}
}

func TestProjectClusterKey_HappyPath(t *testing.T) {
	got := ProjectClusterKey("gcp", "sandbox-12345", "gke-prod")
	want := "gcp:sandbox-12345:k8s:gke-prod"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

func TestProjectClusterKey_EmptyInputsReturnEmpty(t *testing.T) {
	if got := ProjectClusterKey("gcp", "scope", ""); got != "" {
		t.Errorf("expected empty when cluster_name missing; got %q", got)
	}
}

// TestProjectKeys_MirrorComputeResourceKeyTier2 pins the
// roundtrip: when the receiver sees a span with host.id +
// cloud.account.id matching the discovery-side resource_id +
// scope_id, the two projections MUST produce the same key.
func TestProjectKeys_MirrorComputeResourceKeyTier2(t *testing.T) {
	receiverKey, _, _, _, _, _, ok := ComputeResourceKey(map[string]string{
		"cloud.provider":   "aws",
		"cloud.account.id": "123456789012",
		"host.id":          "i-0abc",
	})
	if !ok {
		t.Fatalf("ComputeResourceKey returned ok=false")
	}
	inventoryKey := ProjectComputeKey("aws", "123456789012", "i-0abc")
	if receiverKey != inventoryKey {
		t.Errorf("keys mismatch: receiver=%q inventory=%q", receiverKey, inventoryKey)
	}
}

// TestProjectKeys_MirrorComputeResourceKeyTier3 — same roundtrip
// for the cluster projection.
func TestProjectKeys_MirrorComputeResourceKeyTier3(t *testing.T) {
	receiverKey, _, _, _, _, _, ok := ComputeResourceKey(map[string]string{
		"cloud.provider":   "gcp",
		"cloud.account.id": "sandbox-12345",
		"k8s.cluster.name": "gke-prod",
	})
	if !ok {
		t.Fatalf("ComputeResourceKey returned ok=false")
	}
	inventoryKey := ProjectClusterKey("gcp", "sandbox-12345", "gke-prod")
	if receiverKey != inventoryKey {
		t.Errorf("keys mismatch: receiver=%q inventory=%q", receiverKey, inventoryKey)
	}
}

// TestProjectKeys_MirrorComputeResourceKeyTier4 — same roundtrip
// for the database projection.
func TestProjectKeys_MirrorComputeResourceKeyTier4(t *testing.T) {
	receiverKey, _, _, _, _, _, ok := ComputeResourceKey(map[string]string{
		"cloud.provider":   "aws",
		"cloud.account.id": "123",
		"db.system":        "postgresql",
		"db.name":          "db-prod",
	})
	if !ok {
		t.Fatalf("ComputeResourceKey returned ok=false")
	}
	inventoryKey := ProjectDatabaseKey("aws", "123", "postgresql", "db-prod")
	if receiverKey != inventoryKey {
		t.Errorf("keys mismatch: receiver=%q inventory=%q", receiverKey, inventoryKey)
	}
}
