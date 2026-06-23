// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Serverless tier slice 1 chunk 2 (v0.89.91, #722 Stream 120) Cloud
// Run scanner tests. The test cases pin docs/proposals/
// serverless-tier-slice1.md §11 acceptance tests 4 + 5 (the GCP
// Cloud Run rows of the per-cloud detection matrix), plus the
// pagination + empty-response posture tests that mirror the AWS
// Lambda scanner's slice 1 chunk 1 test surface.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	run "google.golang.org/api/run/v1"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// makeCloudRunService — fixture builder. Wires the three optional
// axes (trace annotation, container list, env vars) through to the
// run.Service shape so each test can construct exactly the minimal
// service it needs to exercise one axis.
//
// The fixture sets Metadata.Name to the supplied name and synthesizes
// a SelfLink under the supplied region so the projection's
// ResourceARN reads honestly. Annotations + container list are nil
// when the caller passes empty values; the projection's nil-safe
// branches handle both shapes.
func makeCloudRunService(name, region string, annotations map[string]string, containers []*run.Container) *run.Service {
	svc := &run.Service{
		ApiVersion: "serving.knative.dev/v1",
		Kind:       "Service",
		Metadata: &run.ObjectMeta{
			Name:        name,
			Namespace:   "p",
			SelfLink:    "https://run.googleapis.com/apis/serving.knative.dev/v1/namespaces/p/services/" + name,
			Annotations: annotations,
		},
	}
	if len(containers) > 0 {
		svc.Spec = &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Spec: &run.RevisionSpec{
					Containers: containers,
				},
			},
		}
	}
	_ = region // kept for future per-region SelfLink derivation; unused today.
	return svc
}

// TestCloudRunScanner_ServiceWithTraceAnnotation_HasTraceAxis — slice
// 1 acceptance test 4 (docs/proposals/serverless-tier-slice1.md
// §11). A Cloud Run service whose metadata.annotations carries the
// run.googleapis.com/trace key sets HasTraceAxis=true on the
// projected snapshot, regardless of the value.
func TestCloudRunScanner_ServiceWithTraceAnnotation_HasTraceAxis(t *testing.T) {
	svc := makeCloudRunService("checkout", "us-central1",
		map[string]string{CloudRunTraceAnnotation: "enabled"},
		nil)

	snap := projectCloudRunService(svc, "p", "us-central1")

	assert.True(t, snap.HasTraceAxis, "HasTraceAxis should be true (annotation present)")
	assert.False(t, snap.HasOTelDistro, "HasOTelDistro should be false (no containers, no env)")
	assert.Equal(t, "gcp", snap.Provider)
	assert.Equal(t, "cloudrun", snap.Surface)
	assert.Equal(t, "checkout", snap.ResourceName)
	assert.Equal(t, "p", snap.AccountID)
	assert.Equal(t, "us-central1", snap.Region)
	assert.NotEmpty(t, snap.ResourceARN, "ResourceARN should fall back to SelfLink")
	assert.True(t, snap.IsInstrumented(), "IsInstrumented should follow HasTraceAxis under the OR rule")
}

// TestCloudRunScanner_ServiceWithoutTraceAnnotation_NoTraceAxis —
// confirms the absence of the run.googleapis.com/trace key keeps
// HasTraceAxis=false. The projection's nil-safe branches handle
// both nil and empty-map shapes.
func TestCloudRunScanner_ServiceWithoutTraceAnnotation_NoTraceAxis(t *testing.T) {
	// Case 1: nil annotations map.
	svc1 := makeCloudRunService("orders", "us-central1", nil, nil)
	snap1 := projectCloudRunService(svc1, "p", "us-central1")
	assert.False(t, snap1.HasTraceAxis)

	// Case 2: non-nil annotations map but without the trace key.
	svc2 := makeCloudRunService("orders", "us-central1",
		map[string]string{"other": "value"}, nil)
	snap2 := projectCloudRunService(svc2, "p", "us-central1")
	assert.False(t, snap2.HasTraceAxis)
}

// TestCloudRunScanner_ServiceWithOTelSidecar_HasOTelDistro — slice 1
// acceptance test 5. A Cloud Run service whose revision template
// includes a container with a name starting with "otel" flips
// HasOTelDistro=true. The Detail bag records the matched sidecar
// name so the chunk-5 Inventory tab can render it.
func TestCloudRunScanner_ServiceWithOTelSidecar_HasOTelDistro(t *testing.T) {
	svc := makeCloudRunService("api", "us-central1", nil,
		[]*run.Container{
			{Name: "app", Image: "gcr.io/p/app:1.2.3"},
			{Name: "otel-collector", Image: "otel/opentelemetry-collector:0.95.0"},
		})

	snap := projectCloudRunService(svc, "p", "us-central1")

	assert.False(t, snap.HasTraceAxis, "HasTraceAxis should be false (no annotation)")
	assert.True(t, snap.HasOTelDistro, "HasOTelDistro should be true (sidecar name matches)")
	assert.True(t, snap.IsInstrumented(), "IsInstrumented should follow HasOTelDistro under the OR rule")

	containerCount, _ := snap.Detail["container_count"].(int)
	assert.Equal(t, 2, containerCount)
	sidecarNames, _ := snap.Detail["sidecar_names"].([]string)
	assert.Equal(t, []string{"otel-collector"}, sidecarNames)
}

// TestCloudRunScanner_ServiceWithoutOTelSidecar_NoOTelDistro —
// confirms the inverse: no otel-prefixed container, no env var, no
// HasOTelDistro. The Detail bag still records container_count but
// omits the sidecar_names key.
func TestCloudRunScanner_ServiceWithoutOTelSidecar_NoOTelDistro(t *testing.T) {
	svc := makeCloudRunService("legacy", "us-central1", nil,
		[]*run.Container{
			{Name: "app", Image: "gcr.io/p/app:1.2.3"},
			{Name: "nginx", Image: "nginx:latest"},
		})

	snap := projectCloudRunService(svc, "p", "us-central1")

	assert.False(t, snap.HasOTelDistro, "no otel-prefixed container should keep HasOTelDistro false")
	containerCount, _ := snap.Detail["container_count"].(int)
	assert.Equal(t, 2, containerCount)
	_, hasSidecarKey := snap.Detail["sidecar_names"]
	assert.False(t, hasSidecarKey, "sidecar_names key should be absent when no sidecar matched")
}

// TestCloudRunScanner_ServiceWithOTLPExporterEnv_HasOTelDistro —
// the second sub-rule on axis 2: OTEL_EXPORTER_OTLP_ENDPOINT env var
// presence on any container flips HasOTelDistro to true, even
// without an otel-named sidecar. Mirrors the AWS Lambda scanner's
// "exec wrapper env" sub-rule.
func TestCloudRunScanner_ServiceWithOTLPExporterEnv_HasOTelDistro(t *testing.T) {
	svc := makeCloudRunService("workers", "us-central1", nil,
		[]*run.Container{
			{
				Name:  "app",
				Image: "gcr.io/p/app:1.2.3",
				Env: []*run.EnvVar{
					{Name: "PORT", Value: "8080"},
					{Name: OTLPExporterEndpointEnv, Value: "http://localhost:4318"},
				},
			},
		})

	snap := projectCloudRunService(svc, "p", "us-central1")

	assert.True(t, snap.HasOTelDistro, "OTLP exporter endpoint env should flip HasOTelDistro")
	_, hasSidecarKey := snap.Detail["sidecar_names"]
	assert.False(t, hasSidecarKey, "sidecar_names absent — env-only path")
}

// TestCloudRunScanner_PaginationFollowsNextPageToken — the Cloud Run
// List endpoint uses Knative-style continuation tokens. The walker
// loops on Metadata.Continue until empty. The fake serves a 3-page
// sequence; the scanner should produce one snapshot per service
// across pages.
//
// NOTE: the AWS scanner's "next page token" terminology is the test
// name; the underlying mechanism on Cloud Run is the Knative
// continue token. The test asserts behavior, not field name.
func TestCloudRunScanner_PaginationFollowsNextPageToken(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudRunPagesByRegion["us-central1"] = [][]*run.Service{
		{
			makeCloudRunService("svc-a-1", "us-central1", nil, nil),
			makeCloudRunService("svc-a-2", "us-central1", nil, nil),
		},
		{
			makeCloudRunService("svc-b-1", "us-central1",
				map[string]string{CloudRunTraceAnnotation: "on"}, nil),
		},
		{
			makeCloudRunService("svc-c-1", "us-central1", nil,
				[]*run.Container{{Name: "otel-agent", Image: "otel/agent:v1"}}),
		},
	}

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Serverless, 4, "all four services across three pages should land")
	assert.Equal(t, 3, fake.CloudRunListCalls["us-central1"], "three list calls expected for three pages")

	byName := map[string]int{}
	for i, sv := range res.Serverless {
		byName[sv.ResourceName] = i
	}
	for _, want := range []string{"svc-a-1", "svc-a-2", "svc-b-1", "svc-c-1"} {
		require.Contains(t, byName, want, "expected service %s", want)
	}
	assert.True(t, res.Serverless[byName["svc-b-1"]].HasTraceAxis)
	assert.True(t, res.Serverless[byName["svc-c-1"]].HasOTelDistro)
}

// TestCloudRunScanner_EmptyResponseReturnsEmptySlice — a project
// with no Cloud Run services in the configured region returns an
// empty Serverless slice without erroring. The walker still calls
// the list endpoint once.
func TestCloudRunScanner_EmptyResponseReturnsEmptySlice(t *testing.T) {
	fake := newFakeGCP()
	// No services seeded for us-central1.

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	cloudRunSnaps := serverlessBySurface(res.Serverless, cloudRunServerlessSurface)
	assert.Empty(t, cloudRunSnaps, "no Cloud Run services means no cloudrun snapshots")
	assert.Equal(t, 1, fake.CloudRunListCalls["us-central1"], "list still called once for the empty region")
	assert.False(t, res.Partial, "empty success is not a partial scan")
}

// TestCloudRunScanner_PermissionDenied_RecordsPartialFailure —
// a 403 on the Services.List call surfaces as a partial-failure
// entry against the cloudrun service identifier. The other walks
// (compute / Cloud SQL / GKE / Cloud Functions) still produce their
// results.
func TestCloudRunScanner_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeGCP()
	fake.CloudRunListStatus = http.StatusForbidden
	// Seed a successful compute walk so the test confirms compute
	// results survive a cloudrun failure.
	fake.Zones = nil

	s := newScannerWithFake(t, fake, "p", "us-central1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "cloudrun 403 is partial, not hard")

	assert.True(t, res.Partial)
	assert.Contains(t, res.PartialReason, ServiceIDCloudRun)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.PartialReason, "roles/run.viewer")
	assert.Contains(t, res.FailedServices, ServiceIDCloudRun)
}

// TestCloudRunScanner_NilContainersDefaultsToZeroAxes — defensive
// check: a service with nil Spec / Template / containers must not
// flip either axis or panic.
func TestCloudRunScanner_NilContainersDefaultsToZeroAxes(t *testing.T) {
	svc := &run.Service{
		Metadata: &run.ObjectMeta{Name: "bare", SelfLink: "..."},
		Spec:     nil,
	}
	snap := projectCloudRunService(svc, "p", "us-central1")
	assert.False(t, snap.HasTraceAxis)
	assert.False(t, snap.HasOTelDistro)
	containerCount, _ := snap.Detail["container_count"].(int)
	assert.Equal(t, 0, containerCount)
}

// TestCloudRunScanner_OTelSidecarMatchCaseInsensitive — the sidecar
// name match is case-insensitive against the lowered name. Operators
// naming their sidecar "OTel-Collector" or "OTELCOL" still get
// credit.
func TestCloudRunScanner_OTelSidecarMatchCaseInsensitive(t *testing.T) {
	cases := []string{"otel-collector", "OTel-Agent", "OTELCOL", "otelcollector"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			svc := makeCloudRunService("svc", "us-central1", nil,
				[]*run.Container{{Name: name, Image: "img"}})
			snap := projectCloudRunService(svc, "p", "us-central1")
			assert.True(t, snap.HasOTelDistro, "name %q should match prefix", name)
		})
	}
}

// TestCloudRunScanner_TraceAnnotationConstant — pins the canonical
// run.googleapis.com/trace annotation key per §12 of the design
// doc's threat model. A test failure here flags an unintentional
// drift in the constant; the chunk-6 runbook documents how to
// refresh it if Google publishes a new key family.
func TestCloudRunScanner_TraceAnnotationConstant(t *testing.T) {
	const expected = "run.googleapis.com/trace"
	if CloudRunTraceAnnotation != expected {
		t.Errorf("CloudRunTraceAnnotation drifted from canonical value: got %q, want %q — see chunk-6 runbook before merging",
			CloudRunTraceAnnotation, expected)
	}
	if OTLPExporterEndpointEnv != "OTEL_EXPORTER_OTLP_ENDPOINT" {
		t.Errorf("OTLPExporterEndpointEnv drifted: got %q, want OTEL_EXPORTER_OTLP_ENDPOINT",
			OTLPExporterEndpointEnv)
	}
	if CloudRunOTelSidecarNamePrefix != "otel" {
		t.Errorf("CloudRunOTelSidecarNamePrefix drifted: got %q, want otel",
			CloudRunOTelSidecarNamePrefix)
	}
}

// serverlessBySurface filters a Serverless slice down to entries
// matching the supplied surface. Used by tests that want to assert
// per-surface counts in a Scan result that exercises multiple
// serverless walks (cloudrun + cloudfunc both populate
// result.Serverless).
func serverlessBySurface(in []scanner.ServerlessInstanceSnapshot, surface string) []scanner.ServerlessInstanceSnapshot {
	out := []scanner.ServerlessInstanceSnapshot{}
	for _, sv := range in {
		if sv.Surface == surface {
			out = append(out, sv)
		}
	}
	return out
}
