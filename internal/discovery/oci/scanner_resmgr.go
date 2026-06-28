// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// OCIResourceManagerLogSourceService is the OCI Logging service
// name identifying Resource Manager as a log source. Per design
// doc §3.4, the slice 2 detection rule fires when a log resource
// in the same compartment as the Stack carries
// Configuration.Source.Service == "resourcemanager". See
// docs/proposals/orchestration-tier-slice2.md §3.4.
const OCIResourceManagerLogSourceService = "resourcemanager"

// OCIResourceManagerSurface identifies Resource Manager Stacks in
// the OrchestrationInstanceSnapshot.Surface field. The "resmgr-"
// prefix on recommendation kinds matches the chunk 2 webhook
// routing extension per design doc §8.
const OCIResourceManagerSurface = "resmgr"

// OCIResourceManagerWorkflowType is the per-snapshot WorkflowType
// label for Resource Manager Stacks. RM has a single workflow
// shape (Stack), so the label is fixed.
const OCIResourceManagerWorkflowType = "Stack"

// ServiceIDOrchestration is the slice 2 service identifier the
// scanner reports against Result.FailedServices when the OCI
// Resource Manager walk produces a non-fatal error. Mirrors the
// compute / database / OKE / functions / streaming identifiers.
// See docs/proposals/orchestration-tier-slice2.md §10.
const ServiceIDOrchestration = "resmgr"

// resourceManagerListAPIVersion pins the OCI Resource Manager
// /stacks list API path version. OCI versions live in the path.
const resourceManagerListAPIVersion = "20180917"

// providerOCIOrchestration is the Provider discriminator the
// scanner writes onto every orchestration snapshot row.
const providerOCIOrchestration = "oci"

// resourceManagerStack is the wire shape for an OCI Resource Manager
// Stack returned by ListStacks. Slice 2 chunk 1 reads ID
// (-> ResourceARN), DisplayName (-> ResourceName), CompartmentID
// (carried through to the per-Stack Logging detection), and
// LifecycleState + TimeCreated (surfaced raw via Detail).
//
// OCI Resource Manager API path:
//
//	GET https://resourcemanager.<region>.oci.oraclecloud.com/20180917/stacks
//	  ?compartmentId=<compartment_ocid>
//
// Pagination follows the opc-next-page response header.
// Required OCI policy: "inspect orm-stacks in compartment". Read-only.
type resourceManagerStack struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	LifecycleState string `json:"lifecycleState"`
	CompartmentID  string `json:"compartmentId"`
	TimeCreated    string `json:"timeCreated"`
}

// resourceManagerStackList is the JSON envelope returned by the
// Resource Manager /stacks list call. OCI returns the list directly
// as a JSON array; the scanner unmarshals into a
// []resourceManagerStack slice.
type resourceManagerStackList = []resourceManagerStack

// resourceManagerLogSource captures the slice 2 chunk 1
// per-compartment Logging detection result for the Resource Manager
// surface. Slice 2 chunk 1 uses compartment-level detection per
// the §3.4 honest framing: a Stack with Logging configured at the
// compartment level but NOT specifically routed for RM sources
// will still get has_log_axis = true. Slice 3 may add
// per-source-mapping inspection for tighter correlation.
type resourceManagerLogSource struct {
	LogGroupID    string
	LogID         string
	CompartmentID string
	Service       string // "resourcemanager"
	SourceType    string // "OCISERVICE"
}

// ScanOrchestrations is the OCI scanner's orchestration-tier entry
// point. Slice 2 chunk 1 only covers Resource Manager Stacks;
// Process Automation is a slice 3 candidate per design doc §1.
// Mirrors the AWS / GCP / Azure scanners' ScanOrchestrations
// layout — a standalone Scanner method returning the snapshot slice.
//
// See docs/proposals/orchestration-tier-slice2.md §5. Replaces the
// slice 1 nil-returning posture (OCI did not satisfy
// OrchestrationDiscoveryScanner in slice 1 — slice 2 chunk 1 adds
// the method for the first time).
func (s *Scanner) ScanOrchestrations(ctx context.Context, scope scanner.ScanScope) ([]scanner.OrchestrationInstanceSnapshot, error) {
	return s.ScanResourceManagerStacks(ctx, scope)
}

// ScanResourceManagerStacks lists OCI Resource Manager Stacks in
// the compartment(s) within scope, then for each Stack determines
// whether OCI Logging is configured with Resource Manager as a
// log source for the Stack's compartment.
//
// Two-pass walk per compartment:
//
//  1. List Stacks via:
//     GET https://resourcemanager.<region>.oci.oraclecloud.com/20180917/stacks
//     ?compartmentId=<compartment_ocid>  — paginated via opc-next-page.
//  2. List OCI Logging log resources for the compartment and
//     filter to those whose configuration.source.service ==
//     "resourcemanager". The detection result applies to every
//     Stack in the compartment per §3.4 compartment-level framing.
//
// Detection per docs/proposals/orchestration-tier-slice2.md §3.4:
//   - HasLogAxis ← compartment has at least one log resource with
//     Configuration.Source.Service == "resourcemanager".
//   - HasTraceAxis ← always false. OCI does not expose a direct
//     OTel integration for Resource Manager; slice 3 may add.
//
// Per-compartment / per-stack / per-logging-call failures are
// swallowed inside the inner loops — a single failing /logs call
// must NOT abort the whole scan. The Stack row still surfaces;
// the axes default to false when the Logging call fails
// (partial-scan posture; mirrors the OCI Streaming chunk).
//
// scope.CompartmentIDs lets the caller scope the walk to a subset;
// empty defaults to "tenancy root + first-level children".
// scope.AccountID overrides the per-snapshot AccountID; empty
// falls back to s.TenancyOCID.
//
// IAM contract per design doc §3.4 + §12: "inspect orm-stacks in
// compartment" + the existing Logging read policy. Read-only.
func (s *Scanner) ScanResourceManagerStacks(ctx context.Context, scope scanner.ScanScope) ([]scanner.OrchestrationInstanceSnapshot, error) {
	// Substrate validation. The Scan entry point does this on the
	// way in; ScanResourceManagerStacks guards defensively at its
	// own entry point so the chunk 2 trampoline / handler dispatch
	// can call this method directly without re-validating. Match
	// the existing OCI scanner posture: missing required fields
	// return an error rather than nil, nil.
	if s.TenancyOCID == "" {
		return nil, errors.New("oci: TenancyOCID is required")
	}
	if s.Region == "" {
		return nil, errors.New("oci: Region is required")
	}

	signingKey, parseErr := s.signingKey()
	if parseErr != nil {
		return nil, fmt.Errorf("oci: %s: signing failed: %w", ServiceIDOrchestration, parseErr)
	}

	// Determine the compartment set. An explicit scope wins;
	// otherwise default to tenancy root + first-level children.
	compartments, err := s.compartmentsForOrchestration(ctx, signingKey, scope)
	if err != nil {
		return nil, fmt.Errorf("oci: %s: compartment listing failed: %w", ServiceIDOrchestration, err)
	}

	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.TenancyOCID
	}

	var snapshots []scanner.OrchestrationInstanceSnapshot
	for _, comp := range compartments {
		stacks, stacksErr := s.listResourceManagerStacksAll(ctx, signingKey, comp.ID)
		if stacksErr != nil {
			// Partial failure on this compartment's stacks walk —
			// skip this compartment but continue walking the rest.
			// The chunk 2 integration will surface this via
			// recordPartialFailure when called through Scan.
			continue
		}

		// §3.4: per-Stack Logging axis detection — query OCI
		// Logging service once per compartment for log resources
		// with RM source service, then apply per-Stack. A failure
		// here defaults all Stacks in the compartment to
		// has_log_axis = false (partial-scan posture); the Stacks
		// still surface.
		rmSources, sourcesErr := s.listResourceManagerLogSources(ctx, signingKey, comp.ID)
		if sourcesErr != nil {
			// Defensive: proceed with empty source set; all
			// Stacks in this compartment will get has_log_axis =
			// false. The operator can re-scan once the Logging
			// permission is granted.
			rmSources = nil
		}

		for _, stack := range stacks {
			snap := s.projectResourceManagerStack(stack, comp.ID, rmSources, accountID)
			snapshots = append(snapshots, snap)
		}
	}
	return snapshots, nil
}

// compartmentsForOrchestration resolves the compartment set. An
// explicit scope.CompartmentIDs wins; otherwise default to
// tenancy root + first-level children. Mirrors
// compartmentsForServerless / compartmentsForEventSource.
func (s *Scanner) compartmentsForOrchestration(ctx context.Context, sk *SigningKey, scope scanner.ScanScope) ([]ociCompartment, error) {
	if len(scope.CompartmentIDs) > 0 {
		out := make([]ociCompartment, 0, len(scope.CompartmentIDs))
		for _, id := range scope.CompartmentIDs {
			out = append(out, ociCompartment{ID: id, LifecycleState: "ACTIVE"})
		}
		return out, nil
	}
	children, listErr := s.listCompartments(ctx, sk)
	if listErr != nil {
		return nil, listErr
	}
	all := append([]ociCompartment{
		{ID: s.TenancyOCID, Name: "root", LifecycleState: "ACTIVE"},
	}, children...)
	return all, nil
}

// listResourceManagerStacksAll walks every page of /stacks for a
// single compartment via the opc-next-page header. Mirrors
// listStreamsAll / listApplicationsAll — same pagination convention
// across every OCI surface.
func (s *Scanner) listResourceManagerStacksAll(ctx context.Context, sk *SigningKey, compartmentID string) ([]resourceManagerStack, error) {
	var all []resourceManagerStack
	nextPage := ""
	for {
		page, nextToken, callErr := s.listResourceManagerStacksPage(ctx, sk, compartmentID, nextPage)
		if callErr != nil {
			return nil, callErr
		}
		all = append(all, page...)
		if nextToken == "" {
			break
		}
		nextPage = nextToken
	}
	return all, nil
}

// listResourceManagerStacksPage walks one page of /stacks. The
// returned nextPage string is the opc-next-page header value
// (empty when there are no more pages).
func (s *Scanner) listResourceManagerStacksPage(ctx context.Context, sk *SigningKey, compartmentID, page string) ([]resourceManagerStack, string, error) {
	endpoint := s.resourceManagerEndpoint()
	u := fmt.Sprintf(
		"%s/%s/stacks?compartmentId=%s",
		strings.TrimRight(endpoint, "/"),
		resourceManagerListAPIVersion,
		url.QueryEscape(compartmentID),
	)
	if page != "" {
		u = u + "&page=" + url.QueryEscape(page)
	}
	body, nextPage, callErr := s.doSignedGETWithPage(ctx, sk, u)
	if callErr != nil {
		return nil, "", callErr
	}
	var out resourceManagerStackList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, "", &ociCallError{Wrapped: fmt.Errorf("stacks response parse: %w", jerr)}
	}
	return out, nextPage, nil
}

// listResourceManagerLogSources walks the OCI Logging /logs
// endpoint for the compartment, filters to entries whose
// configuration.source.service == "resourcemanager", and returns
// the matching source mappings. Slice 2 chunk 1 uses
// compartment-level detection per §3.4: every Stack in the
// compartment shares the compartment's detection result. Slice 3
// may add per-Stack-resource source mapping inspection.
//
//	GET https://logging.<region>.oci.oraclecloud.com/20200531/logs
//	  ?compartmentId=<compartment_ocid>
//
// Paginated via opc-next-page. A 4xx/5xx returns an error; the
// caller falls back to has_log_axis = false for all Stacks in the
// compartment (partial-scan posture).
func (s *Scanner) listResourceManagerLogSources(ctx context.Context, sk *SigningKey, compartmentID string) ([]resourceManagerLogSource, error) {
	// OCI Logging has no flat compartment-level /logs list (the prior
	// "/logs?compartmentId=" call 404'd — found live during slice-6
	// validation). Enumerate log groups in the compartment, then logs
	// within each group, filtering to source.service == resourcemanager.
	groups, gErr := s.listLoggingLogGroups(ctx, sk, compartmentID)
	if gErr != nil {
		return nil, gErr
	}
	base := strings.TrimRight(s.loggingEndpoint(), "/")
	var all []resourceManagerLogSource
	for _, g := range groups {
		u := fmt.Sprintf("%s/%s/logGroups/%s/logs", base, loggingListAPIVersion, url.PathEscape(g.ID))
		body, _, callErr := s.doSignedGETWithPage(ctx, sk, u)
		if callErr != nil {
			return nil, callErr
		}
		var out []resourceManagerLogResource
		if jerr := json.Unmarshal(body, &out); jerr != nil {
			return nil, &ociCallError{Wrapped: fmt.Errorf("logs response parse: %w", jerr)}
		}
		for _, lg := range out {
			src := lg.Configuration.Source
			if strings.EqualFold(src.Service, OCIResourceManagerLogSourceService) {
				lgID := lg.LogGroupID
				if lgID == "" {
					lgID = g.ID
				}
				all = append(all, resourceManagerLogSource{
					LogGroupID:    lgID,
					LogID:         lg.ID,
					CompartmentID: compartmentID,
					Service:       src.Service,
					SourceType:    src.SourceType,
				})
			}
		}
	}
	return all, nil
}

// resourceManagerLogResource is the bare JSON shape of an OCI
// Logging log resource for Resource Manager detection. Slice 2
// chunk 1 reads the configuration.source.service field. Distinct
// from ociLogResource (scanner_streaming.go) because the RM
// detection rule reads .source.service rather than .source.resource.
type resourceManagerLogResource struct {
	ID            string                          `json:"id"`
	LogGroupID    string                          `json:"logGroupId"`
	DisplayName   string                          `json:"displayName"`
	Configuration resourceManagerLogConfiguration `json:"configuration"`
}

// resourceManagerLogConfiguration is the nested config block on
// an OCI Logging resource. Slice 2 chunk 1 reads Source.Service.
type resourceManagerLogConfiguration struct {
	Source resourceManagerLogSourceWire `json:"source"`
}

// resourceManagerLogSourceWire is the inner source block on an OCI
// Logging configuration. Service carries the source service
// identifier (e.g. "resourcemanager"); SourceType is typically
// "OCISERVICE" for service-emitted logs.
type resourceManagerLogSourceWire struct {
	Service    string `json:"service"`
	SourceType string `json:"sourceType"`
}

// stackHasLoggingSource returns true when the OCI Logging service
// has at least one log resource in the same compartment as the
// Stack with a source mapping to Resource Manager. Slice 2 chunk 1
// uses compartment-level detection per §3.4 caveat: a Stack with
// Logging configured at the compartment level but NOT specifically
// routed for RM sources still gets has_log_axis = true. Slice 3
// may add per-source-mapping inspection for tighter correlation.
func stackHasLoggingSource(stack resourceManagerStack, rmSources []resourceManagerLogSource) bool {
	for _, src := range rmSources {
		if src.CompartmentID == stack.CompartmentID &&
			strings.EqualFold(src.Service, OCIResourceManagerLogSourceService) {
			return true
		}
	}
	return false
}

// resourceManagerEndpoint returns the OCI Resource Manager
// control-plane API base URL. Tests inject ociEndpoint to route
// to a shared httptest server; production builds the per-region
// URL pattern.
func (s *Scanner) resourceManagerEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://resourcemanager.%s.oci.oraclecloud.com", s.Region)
}

// projectResourceManagerStack maps a resourceManagerStack into the
// provider-agnostic OrchestrationInstanceSnapshot per slice 2
// chunk 1. Provider="oci", Surface="resmgr", WorkflowType="Stack".
// HasLogAxis is the result of stackHasLoggingSource against the
// compartment's pre-fetched log source list; HasTraceAxis is
// always false per design doc §3.4 (OCI does not expose a direct
// OTel integration for Resource Manager in slice 2). Detail
// carries lifecycle_state / compartment_id / time_created for the
// Inventory tab and proposer reasoning.
func (s *Scanner) projectResourceManagerStack(stack resourceManagerStack, compartmentID string, rmSources []resourceManagerLogSource, accountID string) scanner.OrchestrationInstanceSnapshot {
	snap := scanner.OrchestrationInstanceSnapshot{
		Provider:     providerOCIOrchestration,
		Surface:      OCIResourceManagerSurface,
		AccountID:    accountID,
		Region:       s.Region,
		ResourceName: stack.DisplayName,
		ResourceARN:  stack.ID,
		WorkflowType: OCIResourceManagerWorkflowType,
		HasTraceAxis: false,
		HasLogAxis:   stackHasLoggingSource(stack, rmSources),
		Detail: map[string]any{
			"lifecycle_state": stack.LifecycleState,
			"compartment_id":  compartmentID,
			"time_created":    stack.TimeCreated,
		},
	}
	return snap
}

// classifyOCIResourceManagerError maps an OCI Resource Manager /
// Logging call failure into the operator-visible PartialReason
// string under the orchestration service identifier. Parallels
// classifyOCIError / classifyOCIStreamingError. Ships for the
// chunk 2 trampoline; the chunk 1 ScanResourceManagerStacks entry
// point itself swallows per-compartment failures silently.
//
// 401 -> credentials_invalid, 403 -> permission_denied (hint at
// "inspect orm-stacks in compartment"), 404 mid-walk -> empty
// (silent skip), 404 at root -> tenancy/policy hint, 429 -> rate_limit.
func classifyOCIResourceManagerError(err error, atRoot bool) string {
	if err == nil {
		return ""
	}
	var oce *ociCallError
	if errors.As(err, &oce) {
		if oce.IsNetwork {
			wrapped := ""
			if oce.Wrapped != nil {
				wrapped = oce.Wrapped.Error()
			}
			return fmt.Sprintf("%s: network error: %s", ServiceIDOrchestration, truncate(wrapped, 200))
		}
		if oce.StatusCode == http.StatusTooManyRequests || oce.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDOrchestration)
		}
		switch oce.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check fingerprint, user_ocid, and private key)", ServiceIDOrchestration)
		case http.StatusForbidden:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: permission denied (verify the policy grants 'inspect orm-stacks in compartment'): %s", ServiceIDOrchestration, truncate(msg, 200))
		case http.StatusNotFound:
			if atRoot {
				return fmt.Sprintf("%s: Resource Manager surface not found (verify tenancy_ocid and the inspect-orm-stacks policy)", ServiceIDOrchestration)
			}
			return ""
		default:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: OCI call failed (HTTP %d): %s", ServiceIDOrchestration, oce.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDOrchestration, truncate(err.Error(), 200))
}
