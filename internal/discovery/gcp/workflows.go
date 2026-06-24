// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Orchestration tier slice 1 chunk 2 (v0.89.96, #729 Stream 127) GCP
// Workflows extension.
//
// This file ships the GCP Workflows API walk that the GCP scanner
// dispatches via ScanOrchestrations when the orchestration tier is
// in the request's tier list. Unlike the existing serverless/cloud-sql
// / compute walks, the orchestration walk is NOT folded into Scan() —
// chunk 1 of the arc landed an optional OrchestrationDiscoveryScanner
// interface on the handler side (internal/api/handlers/discovery.go)
// and the handler dispatches per-tier ScanOrchestrations after the
// main Scan returns. The walk surfaces independently in
// Result.Orchestrations with Provider="gcp" and Surface="workflows"
// so the proposer routes its findings to the workflows-trace-enable /
// workflows-logging-enable recommendation kinds (chunk 4).
//
// Library choice mirrors the cloudrun.go + cloudfunc.go + gke.go
// pattern: google.golang.org/api/workflows/v1 (the REST client). The
// httptest mock surface that shape-tests the earlier walks extends to
// the Workflows path with /v1/projects/.../locations/.../workflows
// handling in workflows_test.go's standalone fake.
//
// API surface used:
//   - GET https://workflows.googleapis.com/v1/projects/{project}/locations/{location}/workflows
//   - GET https://workflows.googleapis.com/v1/projects/{project}/locations
//     (used to enumerate locations when neither scope.Regions nor
//     s.Region pins a region).
//
// Pagination: standard nextPageToken; the walker loops on
// ListWorkflowsResponse.NextPageToken until empty. The slice 1 chunk
// 2 acceptance test "PaginationFollowsNextPageToken" pins the
// behavior.
//
// Location enumeration: GCP Workflows requires a concrete region in
// the parent path (no "-" wildcard, like Cloud Run). The walker
// follows scope.Regions when set, falls back to s.Region when set,
// and otherwise enumerates locations via Projects.Locations.List.
// This matches the design doc §3.2 posture: the operator's connection
// config can pin a single region, or scan every region the SA can
// see.
//
// OAuth scope: cloud-platform.read-only is the narrowest scope the
// generated workflows/v1 client library accepts for read-listing.
// There is no targeted workflows.readonly constant in the package
// (the same situation as cloudfunctions/v1 + container/v1beta1). The
// runbook (chunk 5) documents roles/workflows.viewer as the
// project-level IAM grant — the scope on the token and the role on
// the principal are independent axes; the role is the least-privilege
// ask either way.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	workflows "google.golang.org/api/workflows/v1"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// WorkflowsCallLogLevelLogAll is the GCP-defined value of
// callLogLevel that signals all calls within a Workflow are
// logged + traced. Slice 1 treats this as a soft proxy for
// HasTraceAxis per §3.2 of the design doc. GCP may expose a
// more granular trace toggle in future API versions; until then
// LOG_ALL_CALLS is the best signal available.
const WorkflowsCallLogLevelLogAll = "LOG_ALL_CALLS"

// WorkflowsCallLogLevelUnspecified is the GCP-defined value
// signaling no logging is configured. Any other value
// (LOG_ERRORS_ONLY, LOG_NONE, LOG_ALL_CALLS) satisfies
// HasLogAxis.
//
// Note on LOG_NONE: the GCP API publishes "LOG_NONE" as an explicit
// "disable logging" toggle distinct from the unspecified default. The
// detection rule documented in design doc §3.2 keys off
// "callLogLevel != CALL_LOG_LEVEL_UNSPECIFIED"; LOG_NONE thus flips
// HasLogAxis on by the letter of the rule. The slice 1 chunk 2
// behavior preserves that — the operator explicitly chose a logging
// posture (even if it's "log nothing"), which is more than the
// unspecified default. See test
// TestWorkflowsScanner_WorkflowWithLogNone_HasLogAxisFalse for the
// pinned interpretation: the spirit of the rule treats LOG_NONE as
// "logging disabled" (HasLogAxis=false), and the implementation
// honors that nuance over the bare-string check.
const WorkflowsCallLogLevelUnspecified = "CALL_LOG_LEVEL_UNSPECIFIED"

// WorkflowsCallLogLevelNone is the GCP-defined "explicitly disabled"
// log level. See WorkflowsCallLogLevelUnspecified godoc for why
// slice 1 treats LOG_NONE as HasLogAxis=false despite the bare
// "non-UNSPECIFIED" framing in the design doc — the operator's
// stated intent is "no logging", which Squadron's recommendation
// surface should treat as uninstrumented.
const WorkflowsCallLogLevelNone = "LOG_NONE"

// workflowsOrchestrationSurface is the Surface discriminator string
// for GCP Workflows snapshots. The proposer's recommendation-kind
// prefix routing switches on "stepfunc" → AWS, "workflows" → GCP,
// "logicapps" → Azure.
const workflowsOrchestrationSurface = "workflows"

// ServiceIDWorkflows is the orchestration-tier-slice1.md §3.2 service
// identifier the scanner reports when the Workflows walk produces a
// non-fatal error. Same unprefixed shape as ServiceIDComputeEngine /
// ServiceIDCloudSQL / ServiceIDGKE / ServiceIDCloudRun /
// ServiceIDCloudFunctions.
const ServiceIDWorkflows = "workflows"

// WorkflowsReadonlyScope is the OAuth scope the Workflows API walk is
// authorized against. The workflows/v1 client library does not expose
// a targeted workflows.readonly constant — only the platform-wide
// cloud-platform scope. We pin the read-only platform scope as the
// minimum-privilege fit at the OAuth layer (same posture as Cloud
// Functions + GKE). The runbook documents roles/workflows.viewer as
// the project-level IAM grant.
const WorkflowsReadonlyScope = "https://www.googleapis.com/auth/cloud-platform.read-only"

// ScanOrchestrations is the GCP scanner's orchestration-tier entry
// point. Slice 1 chunk 2 only covers Workflows; future slices may
// add other GCP orchestration primitives (Cloud Composer, Cloud
// Scheduler). The method is kept narrow so chunk-2 callers see a
// single dispatch point even as the per-surface coverage grows.
//
// Mirrors the AWS scanner's ScanOrchestrations / ScanStepFunctions
// layout: a standalone Scanner method that returns the slice of
// snapshots directly rather than threading them through the existing
// Scan() per-region loop. This keeps the chunk-2 wiring small and
// lets the handler dispatch orchestration scans on the tier filter
// alone.
//
// Scope semantics: scope.Regions (when non-empty) selects the target
// regions; when empty the scanner's configured s.Region is used; when
// both are empty the scanner enumerates locations via
// Projects.Locations.List. The scope's AccountID overrides the
// per-snapshot AccountID stamped on every row; empty falls back to
// the scanner's configured project id.
//
// Returns the snapshots verbatim; per-region list failures are
// swallowed inside the inner loop so a single failing region does not
// abort the whole scan. The behavior matches AWS's
// scanRegionStepFunctions posture: partial scans surface as
// short-by-one inventory rather than 500s.
//
// IAM contract per docs/proposals/orchestration-tier-slice1.md §3.2:
// workflows.workflows.list. Read-only; Squadron never executes a
// workflow mutation API.
func (s *Scanner) ScanOrchestrations(ctx context.Context, scope scanner.ScanScope) ([]scanner.OrchestrationInstanceSnapshot, error) {
	return s.ScanWorkflows(ctx, scope)
}

// ScanWorkflows walks the resolved region set's Workflows and returns
// the mapped orchestration snapshots. Slice 1 chunk 2 of the
// orchestration-tier arc (v0.89.96, #729 Stream 127).
//
// Detection per docs/proposals/orchestration-tier-slice1.md §3.2:
//
//   - HasTraceAxis ← workflow.callLogLevel == "LOG_ALL_CALLS". This is
//     a soft proxy: GCP Workflows does not expose a separate "trace"
//     toggle in slice 1; the call log level acts as the trace
//     primitive surface. LOG_ALL_CALLS instructs the workflow to log
//     every call step + return + exception, which the platform also
//     emits as Cloud Trace spans. Slice 2 may refine this if GCP
//     publishes a more granular trace toggle (see design doc §12 GCP
//     softness threat model).
//
//   - HasLogAxis  ← workflow.callLogLevel is set AND is not
//     CALL_LOG_LEVEL_UNSPECIFIED AND is not LOG_NONE. The
//     non-UNSPECIFIED check follows the design doc literally; the
//     LOG_NONE carve-out honors operator intent — an explicit
//     "log nothing" toggle should not count as a wired logging
//     destination. See WorkflowsCallLogLevelUnspecified godoc for the
//     full posture.
//
// IAM contract: workflows.workflows.list. Read-only.
func (s *Scanner) ScanWorkflows(ctx context.Context, scope scanner.ScanScope) ([]scanner.OrchestrationInstanceSnapshot, error) {
	if s.ProjectID == "" {
		return nil, errors.New("gcp: ProjectID is required")
	}
	if len(s.SAJSON) == 0 && s.httpClient == nil {
		return nil, errors.New("gcp: SAJSON is required")
	}
	oauthClient, err := s.buildOAuthHTTPClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: build oauth client: %w", err)
	}
	client, err := s.buildWorkflowsClient(ctx, oauthClient)
	if err != nil {
		return nil, fmt.Errorf("gcp: build workflows client: %w", err)
	}
	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.ProjectID
	}
	regions, err := s.workflowsRegions(ctx, client, scope)
	if err != nil {
		return nil, err
	}
	var out []scanner.OrchestrationInstanceSnapshot
	for _, region := range regions {
		regionOut, regionErr := s.walkWorkflowsRegion(ctx, client, region, accountID)
		if regionErr != nil {
			// Per-region list failure is non-fatal at the orchestration
			// dispatch layer — the next region's walk continues. Matches
			// the AWS scanner's per-machine describe-failure posture
			// (see scanRegionStepFunctions godoc): a single failing
			// region produces a short-by-one inventory rather than
			// aborting the whole scan. The handler-side dispatcher
			// (discovery.go::runAWSScan / runGCPScan) inspects the
			// returned slice and stamps the empty regions implicitly.
			continue
		}
		out = append(out, regionOut...)
	}
	return out, nil
}

// workflowsRegions resolves the list of regions to walk. Precedence:
// scope.Regions (when non-empty) → s.Region (when set) →
// Projects.Locations.List (enumerates every region the project's
// Workflows surface exposes). Matches the cloudRunRegions posture so
// the two GCP per-region walks share a discovery shape.
func (s *Scanner) workflowsRegions(ctx context.Context, client *workflows.Service, scope scanner.ScanScope) ([]string, error) {
	if len(scope.Regions) > 0 {
		// Defensive copy + empty-string skip so the caller can pass a
		// slice with trailing blanks (the AWS scope occasionally does)
		// without producing empty parent paths.
		out := make([]string, 0, len(scope.Regions))
		for _, r := range scope.Regions {
			if strings.TrimSpace(r) != "" {
				out = append(out, r)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	if s.Region != "" {
		return []string{s.Region}, nil
	}
	name := fmt.Sprintf("projects/%s", s.ProjectID)
	resp, err := client.Projects.Locations.List(name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("%s: %s", ServiceIDWorkflows, classifyWorkflowsListError(err))
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

// walkWorkflowsRegion lists workflows in a single region, paginating
// via the standard nextPageToken, and returns the projected snapshot
// list. Returns a non-nil error on the underlying List call's
// failure; the caller swallows the error and continues with the next
// region per the slice 1 partial-scan posture.
func (s *Scanner) walkWorkflowsRegion(ctx context.Context, client *workflows.Service, region, accountID string) ([]scanner.OrchestrationInstanceSnapshot, error) {
	parent := fmt.Sprintf("projects/%s/locations/%s", s.ProjectID, region)
	var out []scanner.OrchestrationInstanceSnapshot
	pageToken := ""
	for {
		call := client.Projects.Locations.Workflows.List(parent).Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return out, err
		}
		for _, wf := range resp.Workflows {
			if wf == nil {
				continue
			}
			out = append(out, projectWorkflow(wf, accountID, region))
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		pageToken = resp.NextPageToken
	}
}

// projectWorkflow maps a workflows.Workflow into the provider-agnostic
// OrchestrationInstanceSnapshot. The mapping is the slice 1 chunk 2
// contract per docs/proposals/orchestration-tier-slice1.md §3.2 +
// §5:
//
//   - Provider: "gcp"
//   - Surface:  "workflows"
//   - AccountID: the project id (the GCP connection's primary
//     identifier; the workflow is namespaced under the project).
//   - ResourceName: the trailing segment of wf.Name (the canonical
//     "projects/{p}/locations/{r}/workflows/{name}" shape). When
//     wf.Name is empty (defensive — the API populates it), the field
//     stays empty.
//   - ResourceARN: wf.Name verbatim. The fully-qualified resource path
//     is the canonical identifier; the proposer's evidence list and
//     the recommendation envelope's AffectedResources field both read
//     ResourceARN directly.
//   - Region: the region the walker called workflows.list against.
//     Workflows are per-region; the walker's region argument is the
//     denormalized projection. The Workflow.Name path encodes the
//     same region but the field is read-only to consumers; the
//     denormalized projection sidesteps a path-parse on every render.
//   - WorkflowType: empty. GCP Workflows has a single workflow type
//     in slice 1; see scanner.OrchestrationInstanceSnapshot.WorkflowType
//     godoc.
//
// Axis detection (per §3.2):
//
//   - HasTraceAxis  ← callLogLevel == "LOG_ALL_CALLS" (the soft proxy
//     for trace emission). Other values (LOG_ERRORS_ONLY, LOG_NONE,
//     CALL_LOG_LEVEL_UNSPECIFIED) leave the axis false.
//   - HasLogAxis    ← callLogLevel is set AND is not
//     CALL_LOG_LEVEL_UNSPECIFIED AND is not LOG_NONE. See type-level
//     godoc on WorkflowsCallLogLevelUnspecified for the LOG_NONE
//     carve-out rationale.
//
// Detail bag carries {"call_log_level": "..."} so the per-cloud
// Inventory tab's drilldown can render the raw value alongside the
// boolean axes.
func projectWorkflow(wf *workflows.Workflow, accountID, region string) scanner.OrchestrationInstanceSnapshot {
	snap := scanner.OrchestrationInstanceSnapshot{
		Provider:    string(credstore.ProviderGCP),
		Surface:     workflowsOrchestrationSurface,
		AccountID:   accountID,
		Region:      region,
		ResourceARN: wf.Name,
	}
	if wf.Name != "" {
		// Trailing path segment of "projects/{p}/locations/{r}/workflows/{n}".
		if i := strings.LastIndex(wf.Name, "/"); i >= 0 && i < len(wf.Name)-1 {
			snap.ResourceName = wf.Name[i+1:]
		} else {
			snap.ResourceName = wf.Name
		}
	}
	level := wf.CallLogLevel
	if level == WorkflowsCallLogLevelLogAll {
		snap.HasTraceAxis = true
	}
	if level != "" && level != WorkflowsCallLogLevelUnspecified && level != WorkflowsCallLogLevelNone {
		snap.HasLogAxis = true
	}
	snap.Detail = map[string]any{
		"call_log_level": level,
	}
	return snap
}

// buildWorkflowsClient constructs a workflows.Service using either
// the test-injected httpClient + endpoint (no auth) or the shared
// oauth-backed client built by buildOAuthHTTPClient. Production
// callers pass the shared client; tests pass nil and the function
// reads s.httpClient directly. Same shape as buildComputeClient +
// buildCloudSQLClient + buildContainerClient + buildRunClient +
// buildCloudFunctionsClient.
func (s *Scanner) buildWorkflowsClient(ctx context.Context, oauthClient *http.Client) (*workflows.Service, error) {
	if s.httpClient != nil {
		opts := []option.ClientOption{
			option.WithHTTPClient(s.httpClient),
			option.WithoutAuthentication(),
		}
		if s.endpoint != "" {
			opts = append(opts, option.WithEndpoint(s.endpoint))
		}
		return workflows.NewService(ctx, opts...)
	}
	return workflows.NewService(ctx, option.WithHTTPClient(oauthClient))
}

// classifyWorkflowsListError maps a workflows List failure into the
// operator-visible PartialReason string. Same shape as
// classifyCloudRunListError but scoped to the Workflows service so
// the error message points at the right IAM grant
// (roles/workflows.viewer per the design doc §12 threat model).
//
// Error mappings:
//
//   - 403 -> permission denied with remediation hint pointing at
//     roles/workflows.viewer.
//   - 404 -> project not found (verify project_id).
//   - 429 -> rate limit.
//   - Transport / network -> network-error with the underlying
//     err.Error() truncated.
//   - Other 4xx/5xx -> truncated message with the HTTP code surfaced.
//
// Exported indirectly via workflowsRegions which prefixes the
// service-id tag; the orchestration dispatch layer does not surface
// the per-region failure today but the helper is in place for the
// chunk-4 storage / dashboard wiring.
func classifyWorkflowsListError(err error) string {
	if err == nil {
		return ""
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusForbidden:
			return "permission denied (verify the service account has roles/workflows.viewer)"
		case http.StatusNotFound:
			return "project not found (verify project_id is correct)"
		case http.StatusTooManyRequests:
			return "rate limit exceeded mid-scan"
		default:
			return fmt.Sprintf("workflows list failed (HTTP %d): %s", ge.Code, truncate(ge.Message, 200))
		}
	}
	return fmt.Sprintf("network error: %s", truncate(err.Error(), 200))
}
