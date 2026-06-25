// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Serverless tier slice 1 chunk 2 (v0.89.91, #722 Stream 120) Cloud
// Run extension.
//
// This file ships the Cloud Run Admin API walk that the GCP scanner
// runs after the existing slice-1 Compute Engine + slice-2 Cloud SQL
// + slice-2 GKE walks complete. All four walks share the same
// per-scan SA credential and the same partial-failure accumulator on
// the Result; Cloud Run surfaces independently in Result.Serverless
// with Provider="gcp" and Surface="cloudrun" so the proposer routes
// its findings to the cloudrun-otel-sidecar / cloudrun-trace-enable /
// cloudrun-otel-export-endpoint recommendation kinds (see
// docs/proposals/serverless-tier-slice1.md §3.2 + §8).
//
// Library choice mirrors the compute + Cloud SQL + GKE walks:
// google.golang.org/api/run/v1 (the REST client). The httptest mock
// surface that shape-tests the earlier walks extends to the Cloud Run
// path by adding /v1/projects/.../locations/.../services handling —
// see scanner_test.go::fakeGCP.handler.
//
// API surface used:
//   - GET https://run.googleapis.com/v1/projects/{project}/locations/{region}/services
//
// Pagination: the Cloud Run List endpoint uses Knative-style
// continuation tokens (not the standard nextPageToken; see the run/v1
// generated client). The walker follows ListMeta.Continue until
// empty, accumulating Items across pages. The slice 1 acceptance
// test 4 / test 5 (per §11 of the design doc) hits the multi-page
// fixture path.
//
// Location enumeration: the Cloud Run List API requires a concrete
// region in the parent path (no "-" wildcard, unlike GKE / Cloud
// Functions). When s.Region is set we call exactly that region. When
// s.Region is empty we first enumerate locations via the run/v1
// Projects.Locations.List call and then list services per location.
// This matches the design doc's slice 1 posture: the operator's
// connection config can pin a single region, or scan all regions
// the SA can see.
//
// OAuth scope: run.readonly is the narrowest scope the run/v1 client
// library exposes for read-listing services. The shared
// buildOAuthHTTPClient adds it to the per-scan token's scope union
// alongside the existing compute / Cloud SQL / GKE / Cloud Functions
// scopes (see consts.go RunReadonlyScope + buildOAuthHTTPClient
// godoc).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// CloudRunTraceAnnotation is the GCP-defined annotation that signals
// a Cloud Run service has Cloud Trace integration enabled. Presence
// in the service's metadata.annotations satisfies the HasTraceAxis
// axis per docs/proposals/serverless-tier-slice1.md §3.2.
//
// The annotation key is stable across Cloud Run regions and revisions
// because it is part of the Knative-on-GCP API contract (Cloud Run
// extends the Knative Service shape with run.googleapis.com/* keys).
// A miss on this constant would be a false negative — the runbook
// (chunk 6) documents how to refresh it if Google publishes a new
// trace-axis annotation key family.
const CloudRunTraceAnnotation = "run.googleapis.com/trace"

// CloudRunOTelSidecarNamePrefix is the canonical name prefix for an
// OpenTelemetry collector sidecar container running alongside the
// user's workload on Cloud Run. The slice 1 chunk 2 detection rule
// matches container names by prefix because operators name them
// "otel-collector" / "otel-agent" / "otelcol" depending on the OTel
// distribution they use; a strict equality check on a single name
// would force operators to rename their containers to get credit.
//
// The match is case-insensitive against the lowered container name.
// SLICE 1 THREAT MODEL (§12 of the design doc): teams that name
// their sidecar something different ("telemetry-agent", "obs-relay")
// will not match. A miss is a false negative — Squadron reports "no
// OTel sidecar" when one is actually present; the operator declines
// the cloudrun-otel-sidecar recommendation and the verdict-learning
// loop records the decline. Slice 2 may add a configurable matcher
// list.
const CloudRunOTelSidecarNamePrefix = "otel"

// OTLPExporterEndpointEnv is the env var name OTel SDKs read to
// discover the OTLP exporter endpoint. Presence in any of a service's
// container env entries flips the HasOTelDistro axis to true,
// independent of the sidecar-name match. The OR semantics on the
// HasOTelDistro axis mirror the AWS Lambda scanner's "either layer
// OR exec wrapper" framing — operators on either detection family
// get credit without forcing them to adopt both.
const OTLPExporterEndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// cloudRunServerlessSurface is the Surface discriminator string for
// Cloud Run snapshots. The proposer's recommendation-kind prefix
// routing switches on "cloudrun" → GCP Cloud Run.
const cloudRunServerlessSurface = "cloudrun"

// ServiceIDCloudRun is the serverless-tier-slice1.md §3.2 service
// identifier the scanner reports against Result.FailedServices when
// the Cloud Run walk produces a non-fatal error. Same unprefixed
// shape as ServiceIDComputeEngine + ServiceIDCloudSQL + ServiceIDGKE.
const ServiceIDCloudRun = "cloudrun"

// walkCloudRun lists Cloud Run services in the configured project,
// projects each into a ServerlessInstanceSnapshot, and appends the
// result to result.Serverless. Errors are surfaced to the caller for
// recording as a partial-failure entry against the cloudrun service
// identifier — same pattern as the Cloud SQL + GKE walks' error
// surfacing.
//
// Region handling: the Cloud Run List endpoint requires a concrete
// region (no "-" wildcard). When s.Region is set we list that region;
// when s.Region is empty we enumerate locations first via
// Projects.Locations.List and walk each region in turn. A per-region
// list failure is non-fatal — the walker continues to the next
// region; the caller records the partial-failure reason on the
// Result.
//
// Pagination: Cloud Run uses Knative continuation tokens. The walker
// loops on ListServicesResponse.Metadata.Continue (and the
// continue query param) until empty. The slice 1 acceptance tests 4
// and 5 pin pagination behavior.
func (s *Scanner) walkCloudRun(ctx context.Context, client *run.APIService, result *scanner.Result) error {
	regions, err := s.cloudRunRegions(ctx, client)
	if err != nil {
		return err
	}
	for _, region := range regions {
		if err := s.walkCloudRunRegion(ctx, client, region, result); err != nil {
			// Per-region failure is non-fatal at the walker layer —
			// surface it back to the caller with the region embedded
			// so the caller's classify path can include it in the
			// partial-failure reason.
			return fmt.Errorf("region %s: %w", region, err)
		}
	}
	return nil
}

// cloudRunRegions resolves the list of regions to walk. When
// s.Region is set, returns just that region. When empty, enumerates
// every location the project's Cloud Run surface exposes via the
// Locations.List call.
func (s *Scanner) cloudRunRegions(ctx context.Context, client *run.APIService) ([]string, error) {
	if s.Region != "" {
		return []string{s.Region}, nil
	}
	name := fmt.Sprintf("projects/%s", s.ProjectID)
	resp, err := client.Projects.Locations.List(name).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Locations))
	for _, loc := range resp.Locations {
		if loc == nil || loc.LocationId == "" {
			continue
		}
		out = append(out, loc.LocationId)
	}
	return out, nil
}

// walkCloudRunRegion lists services in a single region, paginating
// via Knative continuation tokens, and appends a projected snapshot
// per service to result.Serverless.
func (s *Scanner) walkCloudRunRegion(ctx context.Context, client *run.APIService, region string, result *scanner.Result) error {
	parent := fmt.Sprintf("projects/%s/locations/%s", s.ProjectID, region)
	continueToken := ""
	for {
		call := client.Projects.Locations.Services.List(parent).Context(ctx)
		if continueToken != "" {
			call = call.Continue(continueToken)
		}
		resp, err := call.Do()
		if err != nil {
			return err
		}
		for _, svc := range resp.Items {
			if svc == nil {
				continue
			}
			result.Serverless = append(result.Serverless,
				projectCloudRunService(svc, s.ProjectID, region))
		}
		if resp.Metadata == nil || resp.Metadata.Continue == "" {
			return nil
		}
		continueToken = resp.Metadata.Continue
	}
}

// projectCloudRunService maps a run.Service into the provider-
// agnostic ServerlessInstanceSnapshot. The mapping is the slice 1
// chunk 2 contract per docs/proposals/serverless-tier-slice1.md
// §3.2 + §5:
//
//   - Provider: "gcp"
//   - Surface:  "cloudrun"
//   - AccountID: the project id (Squadron's GCP connection's primary
//     identifier; the Cloud Run service is namespaced under the
//     project).
//   - ResourceName: svc.Metadata.Name (the operator-readable name).
//   - ResourceARN: svc.Metadata.SelfLink when set, else the
//     synthesized "projects/{p}/locations/{r}/services/{name}" form.
//     The proposer's evidence list reads ResourceARN verbatim.
//   - Region: the region the walker called Services.List against.
//     (Cloud Run services are per-region; the SelfLink encodes the
//     same region but the projection is denormalized for the
//     Inventory tab.)
//   - Runtime: empty — Cloud Run services run arbitrary container
//     images and don't have a managed-runtime identifier the way
//     Cloud Functions / Lambda do. The chunk-5 Inventory tab's
//     Runtime column renders empty for cloudrun rows.
//
// Axis detection (per §3.2):
//
//   - HasTraceAxis  ← metadata.annotations[CloudRunTraceAnnotation]
//     is present. The value semantically pins the trace integration
//     state (on / off / sampled); slice 1 treats presence as on,
//     mirroring how the GCP console renders the toggle.
//   - HasOTelDistro ← (any container has a name with the
//     CloudRunOTelSidecarNamePrefix prefix, case-insensitive) OR
//     (any container's env vars include OTLPExporterEndpointEnv).
//
// Surface-specific detail bag populates {container_count,
// sidecar_names} so the per-cloud Inventory tab's per-row drilldown
// can render the matched sidecar names alongside the universal
// columns. The bag stays without a sidecar_names key when no
// containers fire the sidecar match; the container_count is set
// unconditionally.
func projectCloudRunService(svc *run.Service, projectID, region string) scanner.ServerlessInstanceSnapshot {
	snap := scanner.ServerlessInstanceSnapshot{
		Provider:  string(credstore.ProviderGCP),
		Surface:   cloudRunServerlessSurface,
		AccountID: projectID,
		Region:    region,
	}
	if svc.Metadata != nil {
		snap.ResourceName = svc.Metadata.Name
		if svc.Metadata.SelfLink != "" {
			snap.ResourceARN = svc.Metadata.SelfLink
		}
		// Axis 1: Cloud Trace integration annotation.
		if _, ok := svc.Metadata.Annotations[CloudRunTraceAnnotation]; ok {
			snap.HasTraceAxis = true
		}
	}
	if snap.ResourceARN == "" && snap.ResourceName != "" {
		snap.ResourceARN = fmt.Sprintf(
			"projects/%s/locations/%s/services/%s",
			projectID, region, snap.ResourceName)
	}

	// Axis 2: scan the revision template's containers for either the
	// otel-prefixed sidecar name OR the OTLP exporter endpoint env
	// var. The OR semantics on HasOTelDistro mirror the AWS Lambda
	// scanner's two-sub-rule pattern.
	containerCount := 0
	var sidecarNames []string
	if svc.Spec != nil && svc.Spec.Template != nil && svc.Spec.Template.Spec != nil {
		for _, c := range svc.Spec.Template.Spec.Containers {
			if c == nil {
				continue
			}
			containerCount++
			if isOTelSidecarName(c.Name) {
				snap.HasOTelDistro = true
				sidecarNames = append(sidecarNames, c.Name)
			}
			if containerHasOTLPEndpointEnv(c) {
				snap.HasOTelDistro = true
			}
		}
	}

	detail := map[string]any{
		"container_count": containerCount,
	}
	if len(sidecarNames) > 0 {
		detail["sidecar_names"] = sidecarNames
	}
	snap.Detail = detail
	return snap
}

// isOTelSidecarName returns true when the container name's lowered
// form begins with the OTel sidecar prefix. The case-insensitive
// match mirrors the AWS EC2 / GCE label heuristic — operator naming
// conventions vary across teams and the prefix should land regardless
// of casing.
func isOTelSidecarName(name string) bool {
	if name == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(name), CloudRunOTelSidecarNamePrefix)
}

// containerHasOTLPEndpointEnv returns true when any env entry on the
// container declares the OTLP exporter endpoint var. The check is
// strict-equality on Name; OTel SDKs key off the exact variable name
// and a casing miss wouldn't propagate to the SDK either way.
func containerHasOTLPEndpointEnv(c *run.Container) bool {
	for _, ev := range c.Env {
		if ev == nil {
			continue
		}
		if ev.Name == OTLPExporterEndpointEnv {
			return true
		}
	}
	return false
}

// classifyCloudRunListError maps a Services.List failure into the
// operator-visible PartialReason string. Same shape as
// classifyCloudSQLListError + classifyGKEListError but scoped to the
// Cloud Run service so the error message points at the right IAM
// grant (roles/run.viewer per the design doc §12 threat model) and
// not at the compute / Cloud SQL / GKE ones.
//
// Error mappings:
//
//   - 403 -> permission denied with remediation hint pointing at
//     roles/run.viewer.
//   - 404 -> project not found (same remediation hint as the compute
//   - Cloud SQL + GKE paths — verify project_id).
//   - 429 -> rate limit (Cloud Run has its own quota separate from
//     the other surfaces; same recovery story).
//   - Transport / network -> network-error with the underlying
//     err.Error() truncated to keep audit payloads bounded.
//   - Other 4xx/5xx -> truncated message with the HTTP code surfaced
//     so support agents can pattern-match against the Cloud Run
//     Admin API documentation.
func classifyCloudRunListError(err error) string {
	if err == nil {
		return ""
	}
	// Unwrap the "region {r}: ..." wrapper walkCloudRun applies so
	// the classify switch keys off the underlying googleapi.Error.
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service account has roles/run.viewer)", ServiceIDCloudRun)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: project not found (verify project_id is correct)", ServiceIDCloudRun)
		case http.StatusTooManyRequests:
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDCloudRun)
		default:
			return fmt.Sprintf("%s: services list failed (HTTP %d): %s", ServiceIDCloudRun, ge.Code, truncate(ge.Message, 200))
		}
	}
	return fmt.Sprintf("%s: network error: %s", ServiceIDCloudRun, truncate(err.Error(), 200))
}

// buildRunClient constructs a run.APIService using either the test
// httpClient + endpoint (no auth) or the shared oauth-backed client
// built by buildOAuthHTTPClient. Production callers pass the shared
// client; tests pass nil and the function reads s.httpClient
// directly. Same shape as buildComputeClient + buildCloudSQLClient +
// buildContainerClient.
func (s *Scanner) buildRunClient(ctx context.Context, oauthClient *http.Client) (*run.APIService, error) {
	if s.httpClient != nil {
		opts := []option.ClientOption{
			option.WithHTTPClient(s.httpClient),
			option.WithoutAuthentication(),
		}
		if s.endpoint != "" {
			opts = append(opts, option.WithEndpoint(s.endpoint))
		}
		return run.NewService(ctx, opts...)
	}
	return run.NewService(ctx, option.WithHTTPClient(oauthClient))
}
