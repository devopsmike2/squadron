// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iac

import "testing"

// TestKindDispositions_AllSlice1KindsClassified asserts the locked
// classification: 4 new_file kinds, 5 patch_existing kinds, no
// kind silently missing. A regression that dropped a kind from the
// map would silently default to patch_existing — costlier for the
// new-file kinds — so this test is the structural lock.
func TestKindDispositions_AllSlice1KindsClassified(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		"ec2-otel-layer":                DispositionNewFile,
		"lambda-otel-layer":             DispositionPatchExisting,
		"rds-pi-em":                     DispositionPatchExisting,
		"s3-access-logging":             DispositionNewFile,
		"alb-access-logs":               DispositionPatchExisting,
		"eks-cluster-logging":           DispositionPatchExisting,
		"eks-observability-addon":       DispositionNewFile,
		"dynamodb-contributor-insights": DispositionNewFile,
		"ecs-container-insights":        DispositionPatchExisting,
	}
	if len(KindDispositions) != len(want) {
		t.Fatalf("KindDispositions has %d entries, want %d", len(KindDispositions), len(want))
	}
	for k, v := range want {
		got, ok := KindDispositions[k]
		if !ok {
			t.Errorf("KindDispositions missing %q", k)
			continue
		}
		if got != v {
			t.Errorf("KindDispositions[%q] = %q, want %q", k, got, v)
		}
	}

	// Cross-check the headline shape: 5 new_file + 4 patch_existing.
	var n, p int
	for _, v := range KindDispositions {
		switch v {
		case DispositionNewFile:
			n++
		case DispositionPatchExisting:
			p++
		default:
			t.Errorf("unknown disposition value %q in KindDispositions", v)
		}
	}
	if n != 4 {
		t.Errorf("new_file count = %d, want 4", n)
	}
	if p != 5 {
		t.Errorf("patch_existing count = %d, want 5", p)
	}
}

// TestDispositionFor_KnownKinds covers each known kind and asserts
// the helper returns the value the map carries.
func TestDispositionFor_KnownKinds(t *testing.T) {
	t.Parallel()
	for kind, want := range KindDispositions {
		got := DispositionFor(kind)
		if got != want {
			t.Errorf("DispositionFor(%q) = %q, want %q", kind, got, want)
		}
	}
}

// TestDispositionFor_UnknownKindDefaultsToPatchExisting asserts the
// SAFE default. An unknown kind silently defaulting to new_file
// could shadow an existing file in the operator's repo; the safer
// default is to take the append-with-manual-merge-warning posture.
func TestDispositionFor_UnknownKindDefaultsToPatchExisting(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"some-unknown-kind",
		"future-slice-kind",
		"gcp-cloud-run-otel", // future provider, not slice-1
	}
	for _, kind := range cases {
		got := DispositionFor(kind)
		if got != DispositionPatchExisting {
			t.Errorf("DispositionFor(%q) = %q, want %q (safe default)",
				kind, got, DispositionPatchExisting)
		}
	}
}
