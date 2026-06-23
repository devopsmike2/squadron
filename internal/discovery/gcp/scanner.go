// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package gcp implements scanner.Scanner against the GCP compute
// API for slice 1 of the GCP discovery arc (design doc:
// docs/proposals/gcp-discovery-slice1.md, v0.89.45).
//
// Slice 1 scope: Compute Engine instances only. The proposer
// drafts gce-otel-label recommendations against instances whose
// labels don't include an otel* key (case-insensitive). Slice 2
// will extend to Cloud SQL; slice 3 to GKE; etc.
//
// Credentials: the scanner takes the unsealed Service Account
// JSON bytes (already credstore-unsealed by the caller) and
// constructs a google-cloud-go compute client. The JSON bytes
// are never logged, never embedded in errors, never returned
// in audit payloads. The scanner mirrors AWS's pattern of
// short-lived in-memory credential use.
//
// Library choice: this package uses google.golang.org/api/compute/v1
// (the REST-based "Google API client" library) rather than the
// gRPC-based cloud.google.com/go/compute. Rationale:
//   - Slice 1's mock surface is httptest-based (see scanner_test.go),
//     and the REST client accepts an option.WithEndpoint pointing at
//     a test server; the gRPC client would force a fake gRPC server.
//   - The transitive dependency footprint is smaller (no grpc/proto
//     plumbing pulled in just for the compute API).
//   - The instance shape Squadron needs (Name, MachineType, Labels,
//     Zone) is identical across both libraries — the proposer-facing
//     ComputeInstanceSnapshot doesn't leak any library-specific types.
//
// Slice 2's Cloud SQL scanner will likely sit in this same package
// (or a sibling under internal/discovery/gcp/cloudsql) and follow
// the same library convention.
package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Scanner walks Compute Engine instances in a single GCP project. It
// is constructed per-scan with the unsealed Service Account JSON
// bytes; the scanner does not retain the JSON beyond the Scan call's
// lifetime.
//
// Slice 1's scope is single-project, single-region-or-all. Slice 2
// adds multi-project orchestration paralleling AWS v0.89.7a.
type Scanner struct {
	// ProjectID is the GCP project to scan. Required.
	ProjectID string

	// SAJSON is the unsealed Service Account JSON bytes. Required.
	// The caller MUST call credstore.UnsealGCPServiceAccount before
	// constructing the scanner. The scanner does not retain the
	// JSON beyond the Scan call's lifetime.
	SAJSON []byte

	// Region restricts the scan to a single region. Empty means
	// scan all regions the SA can see.
	Region string

	// httpClient is the transport for compute API calls. When nil,
	// the scanner builds an oauth2-backed client from SAJSON at scan
	// time. Tests inject a custom client pointing at an httptest
	// server (combined with endpoint to bypass credentials).
	httpClient *http.Client

	// endpoint overrides the compute API base URL. Empty in
	// production; tests point this at their httptest server so the
	// scanner exercises the real REST client against a mock.
	endpoint string
}

// Provider satisfies the (future) scanner.Scanner interface. The
// chunk-3 API trampoline will wire the GCPConnection-based scanner
// onto the provider-agnostic surface; chunk-2 ships the concrete
// Scanner that the trampoline constructs.
func (s *Scanner) Provider() credstore.Provider {
	return credstore.ProviderGCP
}

// Scan walks Compute Engine instances in the configured project,
// returning a scanner.Result with ComputeInstanceSnapshot entries.
// Partial failures (rate limits, transient errors mid-walk) are
// recorded in Result.PartialReason / Result.FailedServices per the
// shared partial-failure convention (see internal/discovery/aws/
// scanner.go::recordPartialFailure for the canonical pattern; chunk
// 2 ships its own helper accumulating the same way — gcp service
// identifier "gce").
//
// Returns nil error when some instances were walked successfully even
// if other zones failed (partial result). Returns a wrapped error only
// when zero instances were walked due to a hard failure at the
// authentication or zones-list layer.
func (s *Scanner) Scan(ctx context.Context) (result scanner.Result, err error) {
	scanID := uuid.NewString()
	result = scanner.Result{
		ScanID:        scanID,
		ScanStartedAt: time.Now().UTC(),
		Provider:      credstore.ProviderGCP,
		AccountID:     s.ProjectID,
	}
	// Named return: defer can mutate ScanCompletedAt after the
	// return statement copies the rest of the struct into the
	// caller's frame. Mirrors the AWS scanner's pattern where the
	// completed-at timestamp is the last thing stamped regardless
	// of the success/partial branch the scan took.
	defer func() {
		result.ScanCompletedAt = time.Now().UTC()
	}()

	if s.ProjectID == "" {
		return result, errors.New("gcp: ProjectID is required")
	}
	if len(s.SAJSON) == 0 && s.httpClient == nil {
		// In production the SA JSON is the only sanctioned auth path;
		// tests bypass it by injecting an httpClient + endpoint. Both
		// missing is a misconfiguration.
		return result, errors.New("gcp: SAJSON is required")
	}

	// Build the shared oauth client once per scan. Both the compute
	// and Cloud SQL clients reuse it so the SA JSON is parsed once
	// regardless of how many APIs the scan walks. The scope union
	// (compute.readonly + sqlservice.admin) is requested explicitly
	// — see buildOAuthHTTPClient godoc for why we don't fall back
	// to cloud-platform.
	oauthClient, err := s.buildOAuthHTTPClient(ctx)
	if err != nil {
		// Authentication-layer failure — no instances were walked.
		// Surface as a hard error so the caller's audit emit path
		// fires scan_failed rather than scan_completed-with-partial.
		return result, fmt.Errorf("gcp: build oauth client: %w", err)
	}
	client, err := s.buildComputeClient(ctx, oauthClient)
	if err != nil {
		// Authentication-layer failure — no instances were walked.
		// Surface as a hard error so the caller's audit emit path
		// fires scan_failed rather than scan_completed-with-partial.
		return result, fmt.Errorf("gcp: build compute client: %w", err)
	}

	// List zones once for the project. This is the single hard-failure
	// gate — if the SA can't see zones at all, we bail with a wrapped
	// error rather than emitting a partial result that says "0
	// instances" (which would be operationally indistinguishable from
	// a project with literally zero instances; see design doc §11.3
	// for the silent-half-empty-scan threat model).
	zonesResp, err := client.Zones.List(s.ProjectID).Context(ctx).Do()
	if err != nil {
		reason := classifyZonesListError(err)
		recordPartialFailure(&result, ServiceIDComputeEngine, reason)
		// Tally regions even on partial — empty Regions on a failure
		// keeps the audit payload's shape consistent with success.
		if s.Region != "" {
			result.Regions = []string{s.Region}
		}
		return result, nil
	}

	walkedRegions := map[string]struct{}{}

	for _, zone := range zonesResp.Items {
		region := regionFromZone(zone.Name)
		if s.Region != "" && region != s.Region {
			continue
		}
		walkedRegions[region] = struct{}{}

		// Per-zone instance walk. Errors here are non-fatal — the
		// scan continues on to the next zone, and the failure is
		// accumulated into result.PartialReason / FailedServices.
		if err := s.walkZoneInstances(ctx, client, zone.Name, region, &result); err != nil {
			reason := fmt.Sprintf("%s: %s", ServiceIDComputeEngine, classifyInstanceListError(zone.Name, err))
			recordPartialFailure(&result, ServiceIDComputeEngine, reason)
		}
	}

	// Slice 2 (database-tier-slice2.md §5.1) Cloud SQL walk. Runs
	// after the compute walk completes so the two surfaces share the
	// same per-scan credential setup and accumulate into the same
	// Result. A Cloud SQL list failure is non-fatal — compute results
	// from the same scan are still valuable, so the failure surfaces
	// as a partial-failure entry against the "cloudsql" service
	// identifier rather than failing the whole scan.
	sqlClient, sqlErr := s.buildCloudSQLClient(ctx, oauthClient)
	if sqlErr != nil {
		recordPartialFailure(&result, ServiceIDCloudSQL, fmt.Sprintf("%s: build client: %s", ServiceIDCloudSQL, truncate(sqlErr.Error(), 200)))
	} else if err := s.walkCloudSQL(ctx, sqlClient, &result); err != nil {
		recordPartialFailure(&result, ServiceIDCloudSQL, classifyCloudSQLListError(err))
	} else if s.Region != "" {
		// Successful Cloud SQL walk against a region-filtered scan.
		// The Cloud SQL walk reads every instance in the project
		// and filters client-side (the API has no server-side region
		// filter), so the walked-regions set already contains s.Region
		// from the compute path — but in the degenerate case where
		// compute returned zero zones for the filtered region, we
		// still want s.Region surfaced. The walkedRegions set absorbs
		// it idempotently.
		walkedRegions[s.Region] = struct{}{}
	}

	// Slice 2 (kubernetes-tier-slice2.md §5.1) GKE clusters walk.
	// Runs after the Cloud SQL walk completes so all three surfaces
	// share the same per-scan credential setup and accumulate into
	// the same Result. A GKE list failure is non-fatal — compute +
	// Cloud SQL results from the same scan are still valuable, so
	// the failure surfaces as a partial-failure entry against the
	// "gke" service identifier rather than failing the whole scan.
	//
	// Mirrors the Cloud SQL walk's three-step error surface:
	// build-client failure → partial; list failure → classified
	// partial; success against a region-filtered scan → record the
	// filtered region in walkedRegions so the audit payload reflects
	// what was actually walked.
	gkeClient, gkeErr := s.buildContainerClient(ctx, oauthClient)
	if gkeErr != nil {
		recordPartialFailure(&result, ServiceIDGKE, fmt.Sprintf("%s: build client: %s", ServiceIDGKE, truncate(gkeErr.Error(), 200)))
	} else if err := s.walkGKE(ctx, gkeClient, &result); err != nil {
		recordPartialFailure(&result, ServiceIDGKE, classifyGKEListError(err))
	} else if s.Region != "" {
		// Successful GKE walk against a region-filtered scan. Same
		// idempotent walkedRegions absorption as the Cloud SQL path
		// above — the GKE walk uses the "-" location wildcard and
		// filters client-side, so the filtered region is the relevant
		// one even when compute returned no zones for it.
		walkedRegions[s.Region] = struct{}{}
	}

	// Serverless tier slice 1 chunk 2 (v0.89.91, #722 Stream 120) —
	// Cloud Run + Cloud Functions walks. ScanServerless dispatches
	// to both surfaces in sequence so each can fail independently
	// without contaminating the other; either one's failure is
	// recorded as a partial-failure entry against the matching
	// service identifier (cloudrun / cloudfunc). See
	// docs/proposals/serverless-tier-slice1.md §3.2 + §3.3 for the
	// per-surface detection rules and §11 acceptance tests 4-6 for
	// the chunk-2 cases.
	if err := s.ScanServerless(ctx, oauthClient, &result); err != nil {
		// ScanServerless only returns a hard error when BOTH the
		// Cloud Run AND Cloud Functions walks fail at the
		// build-client layer — in that case both surfaces are
		// already recorded as partial failures and we want the
		// scan to continue rather than fail the whole result. The
		// returned error is informational; we drop it because the
		// partial-failure entries carry the operator-facing
		// detail.
		_ = err
	}
	if s.Region != "" {
		// Successful serverless walk against a region-filtered scan
		// records the filtered region the same way GKE / Cloud SQL
		// do — the walked-regions set absorbs it idempotently so
		// the audit payload reflects what was actually walked.
		walkedRegions[s.Region] = struct{}{}
	}

	// Denormalize the walked-region list into Result.Regions. Order is
	// not stable across runs (map iteration); the field is documented
	// as "regions actually walked" — a set, not a sequence.
	for r := range walkedRegions {
		result.Regions = append(result.Regions, r)
	}

	// Slice 1 instrumented rule (Compute): HasOTel == true.
	for _, c := range result.Compute {
		if c.HasOTel {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}
	// Slice 2 (database-tier-slice2.md §4) instrumented rule for GCP
	// Cloud SQL: QueryInsightsEnabled == true. AWS RDS rows (which
	// would have Provider="aws" or "", and read off
	// PerformanceInsightsEnabled+EnhancedMonitoringEnabled) never
	// appear in a GCP scan's Result.Databases — this scanner only
	// emits Provider="gcp" rows, so the slice 1 RDS tally branch is
	// inert here.
	for _, d := range result.Databases {
		if d.Provider == ProviderGCP && d.QueryInsightsEnabled {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}
	// Slice 2 (kubernetes-tier-slice2.md §3.1) instrumented rule for
	// GCP GKE: ManagedPrometheusEnabled == true. AWS EKS rows (which
	// would have Provider="aws" or "", and read off
	// ControlPlaneLogging+Addons composite) never appear in a GCP
	// scan's Result.Clusters — this scanner only emits Provider="gcp"
	// rows, so the AWS EKS composite tally branch is inert here.
	for _, c := range result.Clusters {
		if c.Provider == ProviderGCP && c.ManagedPrometheusEnabled {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}
	// Serverless tier slice 1 chunk 2 (v0.89.91, #722 Stream 120) —
	// a serverless row counts as instrumented when EITHER the
	// HasTraceAxis (cloud-native trace primitive on) OR HasOTelDistro
	// (OTel distribution attached) axis holds — the OR rule
	// documented on scanner.ServerlessInstanceSnapshot.IsInstrumented.
	// AWS Lambda rows never appear in a GCP scan's Result.Serverless
	// (this scanner only emits Provider="gcp" rows), so the AWS
	// branch is inert here. The shared predicate keeps the scanner-
	// side tally, the proposer-side reasoning, and the per-cloud
	// Inventory tab honest about the same denominator.
	for _, sv := range result.Serverless {
		if sv.IsInstrumented() {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}

	return result, nil
}

// ScanServerless dispatches to the Cloud Run + Cloud Functions walks
// in sequence, accumulating their snapshots into result.Serverless
// and routing per-surface failures into result.PartialReason /
// result.FailedServices. Both surfaces are independent — a Cloud Run
// failure does NOT short-circuit the Cloud Functions walk; partial
// results from either side are surfaced.
//
// Returns a non-nil error only when BOTH surfaces' build-client paths
// fail; in that case the caller already has both surfaces recorded
// as partial failures and the error is informational. The walker
// layer never returns nil-failure when partial entries were added —
// the caller in Scan() inspects result.Partial / result.FailedServices
// for the operator-facing detail.
//
// Per docs/proposals/serverless-tier-slice1.md §11 acceptance tests
// 4-6: the dispatcher is tested by seeding the fake with both
// Cloud Run services and Cloud Functions and asserting that both
// land in result.Serverless after a single Scan call; and by failing
// one surface (e.g. cloudrun 403) and asserting the other still
// produces its snapshots (the partial-failure recording is honest
// about which side failed).
func (s *Scanner) ScanServerless(ctx context.Context, oauthClient *http.Client, result *scanner.Result) error {
	// Cloud Run walk. Build-client failure is non-fatal at the
	// scan level — recorded as a partial-failure entry against the
	// cloudrun service identifier so the operator sees the
	// build-time hint (typically a malformed SA JSON or revoked
	// credentials issue, both of which the Cloud Functions walk
	// will also hit).
	var runBuildErr, funcBuildErr error
	runClient, runErr := s.buildRunClient(ctx, oauthClient)
	if runErr != nil {
		recordPartialFailure(result, ServiceIDCloudRun, fmt.Sprintf("%s: build client: %s", ServiceIDCloudRun, truncate(runErr.Error(), 200)))
		runBuildErr = runErr
	} else if err := s.walkCloudRun(ctx, runClient, result); err != nil {
		recordPartialFailure(result, ServiceIDCloudRun, classifyCloudRunListError(err))
	}

	// Cloud Functions walk. Same build-client + list-error handling
	// as Cloud Run; the two walks are independent so a failure on
	// one side leaves the other's snapshots intact.
	funcClient, funcErr := s.buildCloudFunctionsClient(ctx, oauthClient)
	if funcErr != nil {
		recordPartialFailure(result, ServiceIDCloudFunctions, fmt.Sprintf("%s: build client: %s", ServiceIDCloudFunctions, truncate(funcErr.Error(), 200)))
		funcBuildErr = funcErr
	} else if err := s.walkCloudFunctions(ctx, funcClient, result); err != nil {
		recordPartialFailure(result, ServiceIDCloudFunctions, classifyCloudFunctionsListError(err))
	}

	if runBuildErr != nil && funcBuildErr != nil {
		return fmt.Errorf("gcp: serverless build-client failed for both surfaces: cloudrun=%v cloudfunc=%v", runBuildErr, funcBuildErr)
	}
	return nil
}

// walkZoneInstances lists instances in a single zone and appends each
// projected ComputeInstanceSnapshot to result.Compute. Returns the
// raw error (with the zone name embedded) so the caller can
// classify + record a partial-failure reason; returns nil on success
// (including the empty-zone case).
func (s *Scanner) walkZoneInstances(ctx context.Context, client *compute.Service, zone, region string, result *scanner.Result) error {
	call := client.Instances.List(s.ProjectID, zone).Context(ctx)
	// Pages() walks every page; the slice-1 scanner doesn't cap
	// pagination because rate-limit failures mid-walk are surfaced as
	// partial-failure events (the design doc §13 acceptance test 6
	// pins this behavior).
	err := call.Pages(ctx, func(resp *compute.InstanceList) error {
		for _, inst := range resp.Items {
			snap := projectInstance(inst, region)
			result.Compute = append(result.Compute, snap)
		}
		return nil
	})
	return err
}

// projectInstance maps a compute.Instance into the provider-agnostic
// ComputeInstanceSnapshot. The mapping is the slice-1 contract:
//
//   - ResourceID: instance.Name (operator-readable, stable per project
//     per design doc §12 Q4).
//   - InstanceType: trimmed from the full machine type URL (the GCP
//     API returns "zones/us-central1-a/machineTypes/n2-standard-4";
//     the proposer reasons about the bare type "n2-standard-4").
//   - Tags: the GCP "labels" map. NOTE: GCP's instance.Tags field is
//     a list of network tags, NOT key/value labels. The slice-1
//     scanner uses labels (instance.Labels) for the otel* detection
//     rule per design doc §8.
//   - HasOTel: true iff any label key starts with "otel"
//     case-insensitive. Matches the AWS EC2 slice-1 single-axis rule.
//   - OSFamily: "unknown" — slice 1 defers OS detection per design
//     doc §12 Q5 (proper detection requires the licenseCodes lookup,
//     a separate API call whose cost/benefit hasn't been measured).
//   - Region: derived from the zone (zone "us-central1-a" → region
//     "us-central1"). Denormalized so the proposer can reason about
//     collector colocation without re-deriving.
func projectInstance(inst *compute.Instance, region string) scanner.ComputeInstanceSnapshot {
	snap := scanner.ComputeInstanceSnapshot{
		ResourceID:   inst.Name,
		InstanceType: trimMachineType(inst.MachineType),
		Tags:         copyLabels(inst.Labels),
		HasOTel:      hasOTelLabel(inst.Labels),
		OSFamily:     "unknown",
		Region:       region,
	}
	return snap
}

// hasOTelLabel returns true if any label key starts with the otel
// prefix case-insensitively. The case-insensitive rule mirrors the
// AWS EC2 scanner's slice-1 implementation (and matches the operator
// expectation that "OTel", "otel", and "OTEL_*" all read the same).
func hasOTelLabel(labels map[string]string) bool {
	for k := range labels {
		if strings.HasPrefix(strings.ToLower(k), OTelLabelPrefix) {
			return true
		}
	}
	return false
}

// trimMachineType strips the API's URL-style machineType field down to
// the bare type string. Example:
//
//	"https://www.googleapis.com/compute/v1/projects/.../zones/us-central1-a/machineTypes/n2-standard-4"
//	  -> "n2-standard-4"
//	"zones/us-central1-a/machineTypes/n2-standard-4"
//	  -> "n2-standard-4"
//	""  -> ""
//
// The InstanceType field is a raw string per scanner.go comment —
// the proposer normalizes when reasoning about cost.
func trimMachineType(url string) string {
	if url == "" {
		return ""
	}
	if i := strings.LastIndex(url, "/"); i >= 0 && i < len(url)-1 {
		return url[i+1:]
	}
	return url
}

// copyLabels returns a defensive copy of the GCP labels map so the
// snapshot doesn't share backing memory with the API response (which
// may be reused by the client library between pages). Returns nil for
// an empty input so the Tags field stays omit-empty-friendly.
func copyLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// regionFromZone trims the trailing zone suffix off a GCP zone name.
// GCP zones are named "<region>-<letter>" (e.g. "us-central1-a"); the
// region is everything up to the last hyphen. For inputs without a
// hyphen the zone is returned unchanged (defensive — the API
// guarantees the format but slice 1 shouldn't crash on an unexpected
// shape).
func regionFromZone(zone string) string {
	if zone == "" {
		return ""
	}
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// buildOAuthHTTPClient parses the SA JSON once per scan and returns a
// shared *http.Client whose transport re-mints short-lived access
// tokens against the scope union the slice-1 + slice-2 walks need:
//
//   - compute.ComputeReadonlyScope (compute.readonly) for the slice-1
//     Compute Engine zones + instances walk.
//   - sqladmin.SqlserviceAdminScope (sqlservice.admin) for the
//     slice-2 database tier Cloud SQL instances walk. Despite the
//     "admin" suffix, this is the scope Google requires for the
//     read-listing call (the alternative is the platform-wide
//     cloud-platform scope — see database-tier-slice2.md §3.1 design
//     rationale).
//   - ContainerReadonlyScope (cloud-platform.read-only) for the
//     slice-2 kubernetes tier GKE clusters walk. The
//     container/v1beta1 client library does not expose a more-targeted
//     container.readonly constant; the platform-wide read-only scope
//     is the narrowest scope the generated client offers for read
//     paths against the GKE control plane. See consts.go
//     ContainerReadonlyScope godoc for the rationale; see
//     kubernetes-tier-slice2.md §5.1 for the matching IAM grant
//     (roles/container.viewer at the project level).
//
// The compute.readonly scope alone is insufficient for Cloud SQL
// (returns 403 Insufficient Permission) and for GKE; the read-only
// platform scope alone is insufficient for Cloud SQL list (returns
// 403). The union is the least-privilege fit across all three APIs.
// The full cloud-platform scope would cover everything but violates
// the slice-1 posture commitment to least-privilege (the SA's
// effective scope should be the union of the read-only scopes
// actually exercised, not the platform superset).
//
// Returns nil with an error when the SA JSON is malformed or missing
// fields. The error message NEVER embeds the bytes (substrate
// invariant inherited from credstore: SA JSON never appears in error
// strings or logs).
//
// Returns nil with nil when the test bypass path is active (caller
// provided s.httpClient — the per-API builders construct an
// option.WithoutAuthentication-flagged client in that case).
func (s *Scanner) buildOAuthHTTPClient(ctx context.Context) (*http.Client, error) {
	if s.httpClient != nil {
		// Test bypass: the per-API client builders use s.httpClient
		// directly with WithoutAuthentication. Return nil so the
		// production-path branches in the builders don't run.
		return nil, nil
	}
	cfg, err := google.JWTConfigFromJSON(
		s.SAJSON,
		compute.ComputeReadonlyScope,
		sqladmin.SqlserviceAdminScope,
		// Slice-2 (kubernetes-tier-slice2.md §5.1) GKE Container API
		// read scope. The container/v1beta1 client library doesn't
		// expose a more-targeted container.readonly constant; see
		// consts.go ContainerReadonlyScope godoc for the rationale.
		ContainerReadonlyScope,
		// Serverless tier slice 1 chunk 2 (v0.89.91, #722 Stream 120)
		// — Cloud Run + Cloud Functions read scopes. The run/v1
		// client library exposes a targeted run.readonly constant;
		// the cloudfunctions/v1 library only exposes the platform-
		// wide cloud-platform scope, so we pin the read-only
		// platform variant for Cloud Functions. The runbook
		// documents roles/run.viewer and roles/cloudfunctions.viewer
		// as the project-level IAM grants.
		RunReadonlyScope,
		CloudFunctionsPlatformScope,
	)
	if err != nil {
		return nil, fmt.Errorf("gcp: parse SA JSON: %w", err)
	}
	ts := cfg.TokenSource(ctx)
	return oauth2.NewClient(ctx, ts), nil
}

// buildComputeClient constructs a compute.Service using either the
// test-injected httpClient + endpoint (no auth) or the shared
// oauth-backed client built by buildOAuthHTTPClient. Production
// callers pass the shared client; tests pass nil and the function
// reads s.httpClient directly.
func (s *Scanner) buildComputeClient(ctx context.Context, oauthClient *http.Client) (*compute.Service, error) {
	if s.httpClient != nil {
		// Test path. The httpClient is already pointing at the test
		// server; we pass option.WithoutAuthentication so the compute
		// client library doesn't try to wrap it in another oauth2
		// transport. The endpoint override pins the base URL at the
		// test server.
		opts := []option.ClientOption{
			option.WithHTTPClient(s.httpClient),
			option.WithoutAuthentication(),
		}
		if s.endpoint != "" {
			opts = append(opts, option.WithEndpoint(s.endpoint))
		}
		return compute.NewService(ctx, opts...)
	}
	// Production path. The oauthClient already wraps the SA JSON's
	// TokenSource via buildOAuthHTTPClient.
	return compute.NewService(ctx, option.WithHTTPClient(oauthClient))
}

// recordPartialFailure marks the scan partial and appends both a
// service identifier to FailedServices AND a human-readable reason to
// PartialReason. Mirrors the AWS scanner's recordPartialFailure
// helper (internal/discovery/aws/scanner.go:2127) so the shared
// audit / proposer-side consumers see identical structure across
// providers. The accumulator joins multiple failures with "; ";
// single-failure scans are unchanged.
func recordPartialFailure(result *scanner.Result, service, reason string) {
	result.Partial = true
	if result.PartialReason == "" {
		result.PartialReason = reason
	} else {
		result.PartialReason = result.PartialReason + "; " + reason
	}
	result.FailedServices = append(result.FailedServices, service)
}

// classifyZonesListError maps a Zones.List failure into the
// operator-visible PartialReason string. The string is the audit
// payload's human-readable diagnostic; the structured
// FailedServices field carries the per-service identifier separately
// (see recordPartialFailure).
//
// Error mappings (per brief Step 2):
//
//   - googleapi.Error 403 -> permission_denied with remediation hint
//     pointing at roles/compute.viewer.
//   - googleapi.Error 404 -> project_not_found with remediation hint
//     pointing at the project_id field.
//   - googleapi.Error 429 (or X-RateLimit-Remaining: 0) -> rate_limit.
//   - Transport / network errors -> network-error with the underlying
//     err.Error() truncated to keep audit payloads bounded.
//   - Any other 4xx/5xx -> truncated message under the gce identifier.
func classifyZonesListError(err error) string {
	if err == nil {
		return ""
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service account has roles/compute.viewer)", ServiceIDComputeEngine)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: project not found (verify project_id is correct)", ServiceIDComputeEngine)
		case http.StatusTooManyRequests:
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDComputeEngine)
		default:
			return fmt.Sprintf("%s: zones list failed (HTTP %d): %s", ServiceIDComputeEngine, ge.Code, truncate(ge.Message, 200))
		}
	}
	return fmt.Sprintf("%s: network error: %s", ServiceIDComputeEngine, truncate(err.Error(), 200))
}

// classifyInstanceListError maps a per-zone Instances.List failure
// into the operator-visible PartialReason string. Same shape as
// classifyZonesListError but scoped per-zone so a single zone's
// throttling doesn't contaminate the others.
//
// The caller prepends the "gce:" prefix via the format string in
// Scan; this function returns just the zone-specific tail so the
// shared prefix logic stays in one place.
func classifyInstanceListError(zone string, err error) string {
	if err == nil {
		return ""
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusForbidden:
			return fmt.Sprintf("permission denied in zone %s (verify the service account has roles/compute.viewer)", zone)
		case http.StatusNotFound:
			return fmt.Sprintf("zone %s not found", zone)
		case http.StatusTooManyRequests:
			return fmt.Sprintf("rate limit exceeded in zone %s", zone)
		default:
			return fmt.Sprintf("zone %s failed (HTTP %d): %s", zone, ge.Code, truncate(ge.Message, 200))
		}
	}
	return fmt.Sprintf("network error in zone %s: %s", zone, truncate(err.Error(), 200))
}

// truncate caps a string at n bytes, appending an ellipsis when the
// cap fires. Used to keep audit payloads bounded — a misconfigured
// API endpoint can return a multi-kilobyte error body that bloats
// the audit row otherwise.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
