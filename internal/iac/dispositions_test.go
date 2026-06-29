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
	// The slice-1 kinds must stay correctly classified even as the map
	// grows (event-source + cross-cloud kinds were added in #182), so
	// assert the slice-1 subset rather than an exact total count.
	if len(KindDispositions) < len(want) {
		t.Fatalf("KindDispositions has %d entries, want at least the %d slice-1 kinds", len(KindDispositions), len(want))
	}
	for k, v := range want {
		got, ok := KindDispositions[k]
		if !ok {
			t.Errorf("KindDispositions missing slice-1 kind %q", k)
			continue
		}
		if got != v {
			t.Errorf("KindDispositions[%q] = %q, want %q", k, got, v)
		}
	}

	// Every entry — slice-1 or any kind added later — must carry a valid
	// disposition value (the property the exact counts used to guard).
	for k, v := range KindDispositions {
		if v != DispositionNewFile && v != DispositionPatchExisting {
			t.Errorf("KindDispositions[%q] = %q, not a valid disposition", k, v)
		}
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
