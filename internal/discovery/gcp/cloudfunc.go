// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Serverless tier slice 1 chunk 2 (v0.89.91, #722 Stream 120) Cloud
// Functions extension.
//
// This file ships the Cloud Functions API walk that the GCP scanner
// runs after the Cloud Run walk completes. Both serverless walks
// share the per-scan SA credential and the partial-failure
// accumulator on the Result; Cloud Functions surfaces independently
// in Result.Serverless with Provider="gcp" and Surface="cloudfunc"
// so the proposer routes its findings to the cloudfunc-trace-enable /
// cloudfunc-otel-layer recommendation kinds (see
// docs/proposals/serverless-tier-slice1.md §3.3 + §8).
//
// Library choice mirrors the earlier walks:
// google.golang.org/api/cloudfunctions/v1 (the REST client). The
// httptest mock surface extends to the Cloud Functions path by
// adding /v1/projects/.../locations/.../functions handling — see
// scanner_test.go::fakeGCP.handler.
//
// API surface used:
//   - GET https://cloudfunctions.googleapis.com/v1/projects/{project}/locations/-/functions
//
// Location wildcard: the Cloud Functions List API supports the
// "projects/{p}/locations/-" parent, fanning across every region the
// project owns functions in. A single call covers the project; the
// walker applies the s.Region filter client-side after projection.
// This matches the GKE walk's posture (the GKE list also uses the
// "-" location wildcard).
//
// Pagination: standard nextPageToken; ProjectsLocationsFunctionsListCall.Pages
// loops the walker callback through every page. A list failure on
// page N surfaces as a single error, surfaced as the cloudfunc
// service identifier on result.FailedServices.
//
// OAuth scope: cloudfunctions/v1 only exposes the platform-wide
// cloud-platform scope as a Go constant. The shared
// buildOAuthHTTPClient adds it to the per-scan token's scope union.
// The runbook documents roles/cloudfunctions.viewer as the IAM
// grant — least-privilege at the role layer.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	cloudfunctions "google.golang.org/api/cloudfunctions/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// GoogleCloudTraceEnv is the env var name the Cloud Functions auto-
// instrumentation reads to enable Cloud Trace integration. Per
// docs/proposals/serverless-tier-slice1.md §3.3, a non-empty value
// in function.environmentVariables[GoogleCloudTraceEnv] satisfies
// the HasTraceAxis axis.
const GoogleCloudTraceEnv = "GOOGLE_CLOUD_TRACE"

// CloudFunctionsOTelLayerEnv is the env var name signaling that an
// OpenTelemetry distribution layer has been attached to a Cloud
// Function. Operators set it via `gcloud functions deploy
// --set-env-vars OTEL_INSTRUMENTATION_AUTO_ENABLED=true` when they
// add the OTel distro. Presence flips HasOTelDistro to true,
// regardless of the Cloud Trace toggle.
const CloudFunctionsOTelLayerEnv = "OTEL_INSTRUMENTATION_AUTO_ENABLED"

// cloudFuncServerlessSurface is the Surface discriminator string for
// Cloud Functions snapshots. The proposer's recommendation-kind prefix
// routing switches on "cloudfunc" → GCP Cloud Functions.
const cloudFuncServerlessSurface = "cloudfunc"

// ServiceIDCloudFunctions is the serverless-tier-slice1.md §3.3
// service identifier the scanner reports against
// Result.FailedServices when the Cloud Functions walk produces a
// non-fatal error. Same unprefixed shape as
// ServiceIDComputeEngine / ServiceIDCloudSQL / ServiceIDGKE /
// ServiceIDCloudRun.
const ServiceIDCloudFunctions = "cloudfunc"

// otelNativeRuntimePrefixes enumerates the Cloud Functions runtimes
// whose execution environment natively bundles an OpenTelemetry
// distribution when Cloud Trace is enabled. Per §3.3 of the design
// doc, "the Cloud Functions auto-instrumentation runtimes
// (python3.10+, nodejs18+, java17+) — if the runtime supports OTel
// native AND HasTraceAxis is true, also set HasOTelDistro."
//
// The prefixes match Google's runtime identifier scheme — the API
// returns values like "python310", "python311", "nodejs18",
// "nodejs20", "java17", "java21". We match by prefix so newer
// runtimes within the same family (e.g. python312) still satisfy
// the rule without code edits.
//
// Slice 1 keeps the list conservative — only the runtimes Google's
// auto-instrumentation docs name explicitly. Slice 2 may expand to
// the .NET / Go runtimes once Google ships matching auto-distros.
var otelNativeRuntimePrefixes = []string{
	"python310", "python311", "python312", "python313",
	"nodejs18", "nodejs20", "nodejs22",
	"java17", "java21",
}

// walkCloudFunctions lists Cloud Functions in the configured project,
// projects each into a ServerlessInstanceSnapshot, and appends the
// result to result.Serverless. Errors are surfaced to the caller for
// recording as a partial-failure entry against the cloudfunc service
// identifier — same pattern as the Cloud Run + GKE + Cloud SQL walks'
// error surfacing.
//
// Location wildcard: the parent path uses "projects/{p}/locations/-"
// so a single call fans across every region. The s.Region filter is
// applied client-side after projection (mirrors the GKE walk's
// posture; see gke.go::walkGKE godoc).
//
// Pagination: standard nextPageToken via the generated client's
// Pages helper. The callback returns nil to advance; any list failure
// surfaces back to the caller as the underlying error.
func (s *Scanner) walkCloudFunctions(ctx context.Context, client *cloudfunctions.Service, result *scanner.Result) error {
	parent := fmt.Sprintf("projects/%s/locations/-", s.ProjectID)
	call := client.Projects.Locations.Functions.List(parent).Context(ctx)
	return call.Pages(ctx, func(resp *cloudfunctions.ListFunctionsResponse) error {
		for _, fn := range resp.Functions {
			if fn == nil {
				continue
			}
			region := regionFromFunctionName(fn.Name)
			if s.Region != "" && region != s.Region {
				continue
			}
			result.Serverless = append(result.Serverless,
				projectCloudFunction(fn, s.ProjectID, region))
		}
		return nil
	})
}

// regionFromFunctionName extracts the region segment from a Cloud
// Functions resource name. Names arrive as
// "projects/{p}/locations/{r}/functions/{name}"; the projection
// reads the region between "/locations/" and the next "/" so the
// snapshot's Region matches what the Inventory tab renders.
//
// Defensive fallthrough: returns "" when the shape doesn't parse.
// The API guarantees the structure but slice 1 chunk 2 shouldn't
// crash on an unexpected value.
func regionFromFunctionName(name string) string {
	const marker = "/locations/"
	idx := strings.Index(name, marker)
	if idx < 0 {
		return ""
	}
	rest := name[idx+len(marker):]
	end := strings.Index(rest, "/")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// projectCloudFunction maps a cloudfunctions.CloudFunction into the
// provider-agnostic ServerlessInstanceSnapshot. The mapping is the
// slice 1 chunk 2 contract per
// docs/proposals/serverless-tier-slice1.md §3.3 + §5:
//
//   - Provider: "gcp"
//   - Surface:  "cloudfunc"
//   - AccountID: the project id.
//   - ResourceName: the trailing "/functions/{name}" segment.
//   - ResourceARN: fn.Name (the fully-qualified resource path the
//     Cloud Functions API uses canonically).
//   - Region: parsed from fn.Name via regionFromFunctionName.
//   - Runtime: fn.Runtime verbatim (e.g. "python311", "nodejs20").
//
// Axis detection (per §3.3):
//
//   - HasTraceAxis  ← fn.EnvironmentVariables[GoogleCloudTraceEnv]
//     is set to any non-empty value.
//   - HasOTelDistro ← fn.EnvironmentVariables[CloudFunctionsOTelLayerEnv]
//     is set. SECONDARY RULE: if HasTraceAxis is true AND the runtime
//     supports OTel natively (any of the otelNativeRuntimePrefixes
//     matches as a prefix), HasOTelDistro is also true — the
//     execution environment auto-instruments those runtimes when
//     Cloud Trace is on.
//
// Surface-specific detail bag populates {runtime, env_keys_count}
// for the per-cloud Inventory tab's per-row drilldown. Slice 1 keeps
// the bag deliberately narrow — chunk 5's UI lands the columns the
// operator actually reads on the row; deeper drill-down lives in the
// proposer's evidence pane.
func projectCloudFunction(fn *cloudfunctions.CloudFunction, projectID, region string) scanner.ServerlessInstanceSnapshot {
	snap := scanner.ServerlessInstanceSnapshot{
		Provider:     string(credstore.ProviderGCP),
		Surface:      cloudFuncServerlessSurface,
		AccountID:    projectID,
		Region:       region,
		ResourceName: shortFunctionName(fn.Name),
		ResourceARN:  fn.Name,
		Runtime:      fn.Runtime,
	}

	// Axis 1: GOOGLE_CLOUD_TRACE env var.
	if _, ok := fn.EnvironmentVariables[GoogleCloudTraceEnv]; ok {
		snap.HasTraceAxis = true
	}

	// Axis 2a: explicit OTel auto-instrumentation env var.
	if _, ok := fn.EnvironmentVariables[CloudFunctionsOTelLayerEnv]; ok {
		snap.HasOTelDistro = true
	}
	// Axis 2b: native auto-instrumentation runtime + trace axis on.
	if !snap.HasOTelDistro && snap.HasTraceAxis && runtimeSupportsNativeOTel(fn.Runtime) {
		snap.HasOTelDistro = true
	}

	snap.Detail = map[string]any{
		"runtime":         fn.Runtime,
		"env_keys_count":  len(fn.EnvironmentVariables),
	}
	return snap
}

// shortFunctionName extracts the operator-readable function name from
// the fully-qualified resource path. Returns the trailing
// "/functions/{name}" segment, or the raw input if the prefix isn't
// present. Empty input returns empty output.
func shortFunctionName(name string) string {
	const marker = "/functions/"
	idx := strings.LastIndex(name, marker)
	if idx < 0 {
		return name
	}
	return name[idx+len(marker):]
}

// runtimeSupportsNativeOTel returns true when the function's runtime
// string starts with any of the otelNativeRuntimePrefixes. The match
// is case-insensitive on the runtime — Google's API canonicalizes
// these to lowercase but slice 1 chunk 2 is defensive against any
// future drift.
func runtimeSupportsNativeOTel(runtime string) bool {
	if runtime == "" {
		return false
	}
	lower := strings.ToLower(runtime)
	for _, prefix := range otelNativeRuntimePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// classifyCloudFunctionsListError maps a Functions.List failure into
// the operator-visible PartialReason string. Same shape as the other
// per-surface classify helpers but scoped to Cloud Functions so the
// error message points at the right IAM grant
// (roles/cloudfunctions.viewer per the design doc §12 threat model).
//
// Error mappings:
//
//   - 403 -> permission denied with remediation hint pointing at
//     roles/cloudfunctions.viewer.
//   - 404 -> project not found.
//   - 429 -> rate limit.
//   - Transport / network -> network-error with the underlying
//     err.Error() truncated.
//   - Other 4xx/5xx -> truncated message with the HTTP code.
func classifyCloudFunctionsListError(err error) string {
	if err == nil {
		return ""
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service account has roles/cloudfunctions.viewer)", ServiceIDCloudFunctions)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: project not found (verify project_id is correct)", ServiceIDCloudFunctions)
		case http.StatusTooManyRequests:
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDCloudFunctions)
		default:
			return fmt.Sprintf("%s: functions list failed (HTTP %d): %s", ServiceIDCloudFunctions, ge.Code, truncate(ge.Message, 200))
		}
	}
	return fmt.Sprintf("%s: network error: %s", ServiceIDCloudFunctions, truncate(err.Error(), 200))
}

// buildCloudFunctionsClient constructs a cloudfunctions.Service using
// either the test httpClient + endpoint (no auth) or the shared
// oauth-backed client built by buildOAuthHTTPClient. Same shape as
// the other per-API builders.
func (s *Scanner) buildCloudFunctionsClient(ctx context.Context, oauthClient *http.Client) (*cloudfunctions.Service, error) {
	if s.httpClient != nil {
		opts := []option.ClientOption{
			option.WithHTTPClient(s.httpClient),
			option.WithoutAuthentication(),
		}
		if s.endpoint != "" {
			opts = append(opts, option.WithEndpoint(s.endpoint))
		}
		return cloudfunctions.NewService(ctx, opts...)
	}
	return cloudfunctions.NewService(ctx, option.WithHTTPClient(oauthClient))
}
