// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Serverless tier slice 1 chunk 2 (v0.89.91, #722 Stream 120) Cloud
// Functions scanner tests. The test cases pin docs/proposals/
// serverless-tier-slice1.md §11 acceptance test 6 (the GCP Cloud
// Functions row of the per-cloud detection matrix), plus the
// runtime-population + nil-input defensive cases.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cloudfunctions "google.golang.org/api/cloudfunctions/v1"
)

// makeCloudFunction — fixture builder. Wires the runtime + env vars
// into a CloudFunction shape with the canonical
// "projects/{p}/locations/{r}/functions/{name}" resource name so the
// projection's region extraction reads honestly.
func makeCloudFunction(name, region, runtime string, env map[string]string) *cloudfunctions.CloudFunction {
	return &cloudfunctions.CloudFunction{
		Name:                 "projects/p/locations/" + region + "/functions/" + name,
		Runtime:              runtime,
		EnvironmentVariables: env,
	}
}

// TestCloudFunctionsScanner_FunctionWithCloudTraceEnv_HasTraceAxis —
// a Cloud Function whose EnvironmentVariables include
// GOOGLE_CLOUD_TRACE sets HasTraceAxis=true on the projected snapshot.
// The Cloud Trace integration env-var rule per §3.3 of the design
// doc.
func TestCloudFunctionsScanner_FunctionWithCloudTraceEnv_HasTraceAxis(t *testing.T) {
	fn := makeCloudFunction("checkout", "us-central1", "python311",
		map[string]string{GoogleCloudTraceEnv: "true"})

	snap := projectCloudFunction(fn, "p", "us-central1")

	assert.True(t, snap.HasTraceAxis, "HasTraceAxis should be true (GOOGLE_CLOUD_TRACE present)")
	// Native-runtime auto-instrumentation: python311 + trace-axis on
	// also flips HasOTelDistro per the secondary rule.
	assert.True(t, snap.HasOTelDistro, "HasOTelDistro should be true via native-runtime rule")
	assert.Equal(t, "gcp", snap.Provider)
	assert.Equal(t, "cloudfunc", snap.Surface)
	assert.Equal(t, "checkout", snap.ResourceName)
	assert.Equal(t, "us-central1", snap.Region)
	assert.True(t, snap.IsInstrumented())
}

// TestCloudFunctionsScanner_FunctionWithOTelLayerEnv_HasOTelDistro —
// slice 1 acceptance test 6 (docs/proposals/serverless-tier-
// slice1.md §11). A Cloud Function whose EnvironmentVariables include
// the OTEL_INSTRUMENTATION_AUTO_ENABLED key (the OTel layer marker)
// flips HasOTelDistro=true, independent of any Cloud Trace toggle.
func TestCloudFunctionsScanner_FunctionWithOTelLayerEnv_HasOTelDistro(t *testing.T) {
	fn := makeCloudFunction("orders", "us-central1", "go121",
		map[string]string{CloudFunctionsOTelLayerEnv: "true"})

	snap := projectCloudFunction(fn, "p", "us-central1")

	assert.False(t, snap.HasTraceAxis, "HasTraceAxis should be false (no GOOGLE_CLOUD_TRACE)")
	assert.True(t, snap.HasOTelDistro, "HasOTelDistro should be true (OTel layer env present)")
	assert.True(t, snap.IsInstrumented())
}

// TestCloudFunctionsScanner_FunctionWithNeither_BothFalse — a
// function with neither the Cloud Trace env nor the OTel layer env
// AND a runtime that doesn't satisfy the native-OTel branch reads as
// both axes off.
func TestCloudFunctionsScanner_FunctionWithNeither_BothFalse(t *testing.T) {
	fn := makeCloudFunction("legacy", "us-central1", "go118",
		map[string]string{"OTHER": "value"})

	snap := projectCloudFunction(fn, "p", "us-central1")

	assert.False(t, snap.HasTraceAxis)
	assert.False(t, snap.HasOTelDistro)
	assert.False(t, snap.IsInstrumented())
}

// TestCloudFunctionsScanner_RuntimeFieldPopulated — the projected
// snapshot's Runtime carries the API's raw runtime string so the
// chunk-5 Inventory tab's Runtime column can render it directly.
func TestCloudFunctionsScanner_RuntimeFieldPopulated(t *testing.T) {
	cases := []string{"python311", "nodejs20", "go121", "java17", "dotnet6"}
	for _, runtime := range cases {
		t.Run(runtime, func(t *testing.T) {
			fn := makeCloudFunction("svc", "us-central1", runtime, nil)
			snap := projectCloudFunction(fn, "p", "us-central1")
			assert.Equal(t, runtime, snap.Runtime)
			gotRuntime, _ := snap.Detail["runtime"].(string)
			assert.Equal(t, runtime, gotRuntime, "Detail[runtime] should mirror Runtime")
		})
	}
}

// TestCloudFunctionsScanner_NativeRuntimeAutoInstrumented — the
// secondary rule: native-OTel-supported runtimes (python3.10+,
// nodejs18+, java17+) with Cloud Trace enabled count as having an
// OTel distro even without the explicit layer env. Per §3.3 design
// doc.
func TestCloudFunctionsScanner_NativeRuntimeAutoInstrumented(t *testing.T) {
	cases := []struct {
		runtime           string
		wantOTelOnTraceOn bool
	}{
		{"python310", true},
		{"python311", true},
		{"python312", true},
		{"nodejs18", true},
		{"nodejs20", true},
		{"java17", true},
		{"java21", true},
		// Below the native-OTel threshold.
		{"python39", false},
		{"nodejs16", false},
		{"go121", false},
		{"dotnet6", false},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			fn := makeCloudFunction("svc", "us-central1", tc.runtime,
				map[string]string{GoogleCloudTraceEnv: "true"})
			snap := projectCloudFunction(fn, "p", "us-central1")
			assert.True(t, snap.HasTraceAxis)
			assert.Equal(t, tc.wantOTelOnTraceOn, snap.HasOTelDistro,
				"runtime %s: HasOTelDistro mismatch", tc.runtime)
		})
	}
}

// TestCloudFunctionsScanner_RegionExtractionFromName — the projection
// parses the region from the resource name's
// "projects/{p}/locations/{r}/functions/{name}" form rather than
// requiring the caller to pass it. Confirms the parser's stable
// behavior across regions and the s.Region client-side filter logic.
func TestCloudFunctionsScanner_RegionExtractionFromName(t *testing.T) {
	cases := []struct {
		name   string
		region string
	}{
		{"projects/p/locations/us-central1/functions/checkout", "us-central1"},
		{"projects/p/locations/europe-west1/functions/orders", "europe-west1"},
		{"projects/p/locations/asia-east1/functions/api", "asia-east1"},
		{"malformed", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.region, regionFromFunctionName(tc.name))
		})
	}
}

// TestCloudFunctionsScanner_EmptyResponseReturnsEmptySlice — a
// project with zero Cloud Functions returns an empty Serverless
// slice (or a slice without any cloudfunc-surface entries) without
// erroring.
func TestCloudFunctionsScanner_EmptyResponseReturnsEmptySlice(t *testing.T) {
	fake := newFakeGCP()
	// No CloudFunctions seeded.

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	cloudFuncSnaps := serverlessBySurface(res.Serverless, cloudFuncServerlessSurface)
	assert.Empty(t, cloudFuncSnaps)
	assert.Equal(t, 1, fake.CloudFunctionsListCalls)
}

// TestCloudFunctionsScanner_PermissionDenied_RecordsPartialFailure —
// a 403 on the Functions.List call surfaces as a partial-failure
// entry against the cloudfunc service identifier. The other walks
// still produce their results.
func TestCloudFunctionsScanner_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudFunctionsListStatus = http.StatusForbidden

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "cloudfunc 403 is partial, not hard")

	assert.True(t, res.Partial)
	assert.Contains(t, res.PartialReason, ServiceIDCloudFunctions)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.PartialReason, "roles/cloudfunctions.viewer")
	assert.Contains(t, res.FailedServices, ServiceIDCloudFunctions)
}

// TestCloudFunctionsScanner_RegionFilterClientSide — when s.Region
// is set, the projection skips functions that live in other regions
// (the API doesn't expose a server-side region filter on the "-"
// location wildcard call, so the walker applies it client-side).
func TestCloudFunctionsScanner_RegionFilterClientSide(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudFunctions = []*cloudfunctions.CloudFunction{
		makeCloudFunction("svc-a", "us-central1", "python311", nil),
		makeCloudFunction("svc-b", "europe-west1", "python311", nil),
		makeCloudFunction("svc-c", "asia-east1", "python311", nil),
	}

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	cloudFuncSnaps := serverlessBySurface(res.Serverless, cloudFuncServerlessSurface)
	require.Len(t, cloudFuncSnaps, 1, "only the us-central1 function should land")
	assert.Equal(t, "svc-a", cloudFuncSnaps[0].ResourceName)
}

// TestCloudFunctionsScanner_NilEnvVarsDefaultsToZeroAxes — defensive:
// a function with nil EnvironmentVariables maps to both axes off
// without panicking.
func TestCloudFunctionsScanner_NilEnvVarsDefaultsToZeroAxes(t *testing.T) {
	fn := &cloudfunctions.CloudFunction{
		Name:    "projects/p/locations/us-central1/functions/bare",
		Runtime: "python311",
	}
	snap := projectCloudFunction(fn, "p", "us-central1")
	assert.False(t, snap.HasTraceAxis)
	assert.False(t, snap.HasOTelDistro)
	assert.Equal(t, "bare", snap.ResourceName)
}

// TestCloudFunctionsScanner_ConstantPinning — pins the canonical env
// var names per §12 of the design doc's threat model. A test failure
// here flags an unintentional drift in the constants; the chunk-6
// runbook documents how to refresh them.
func TestCloudFunctionsScanner_ConstantPinning(t *testing.T) {
	if GoogleCloudTraceEnv != "GOOGLE_CLOUD_TRACE" {
		t.Errorf("GoogleCloudTraceEnv drifted: got %q, want GOOGLE_CLOUD_TRACE", GoogleCloudTraceEnv)
	}
	if CloudFunctionsOTelLayerEnv != "OTEL_INSTRUMENTATION_AUTO_ENABLED" {
		t.Errorf("CloudFunctionsOTelLayerEnv drifted: got %q, want OTEL_INSTRUMENTATION_AUTO_ENABLED", CloudFunctionsOTelLayerEnv)
	}
}
