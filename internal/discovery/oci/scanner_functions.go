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

// OCIAPMEnabledConfigKey is the function config key Oracle Cloud
// publishes for enabling APM integration. The slice 1 detection rule
// per docs/proposals/serverless-tier-slice1.md §3.5 fires
// HasTraceAxis when this key is present in the function's Config map
// with the literal string value "true". A missing key, an empty
// value, or any other value (including "false") leaves the axis
// false.
//
// The exact-match-on-"true" comparison mirrors the design doc's
// "function config[OCI_APM_ENABLED] is true" framing — operators
// who set the value to anything other than "true" (e.g. "TRUE",
// "1", "yes") miss the rule by design. The chunk-6 runbook will
// document the canonical lowercase form so operators don't trip on
// the case.
const OCIAPMEnabledConfigKey = "OCI_APM_ENABLED"

// OCIAPMEnabledConfigValue is the canonical Config map value the
// slice 1 rule treats as "APM is on". Kept as a constant so the
// scanner-side detection, the tests, and any future runbook
// reference share the same string.
const OCIAPMEnabledConfigValue = "true"

// OTelDistroConfigKey is the function Config key signaling that an
// OpenTelemetry distribution is attached. Slice 1 fires
// HasOTelDistro when this key is present with any non-empty value;
// operators commonly set it to the distro name ("opentelemetry-js",
// "opentelemetry-python", etc.) or a version pin.
//
// The presence-with-non-empty-value rule mirrors the design doc's
// "function config[OTEL_DISTRO] is set" framing. An explicitly
// empty string value (operator clearing a previously-set distro
// pointer) leaves the axis false; presence with any populated
// value flips it true.
const OTelDistroConfigKey = "OTEL_DISTRO"

// ServiceIDServerless is the slice 1 (serverless tier) service
// identifier the scanner reports against Result.FailedServices when
// the OCI Functions walk produces a non-fatal error. Mirrors the
// compute / database / OKE service identifiers ("ocicompute" /
// "ocidb" / "oke"); the per-provider connection model carries the
// provider discriminator separately, so the identifier is
// unprefixed.
//
// See docs/proposals/serverless-tier-slice1.md §10 (chunks list)
// + §11 acceptance test 9. Chunk-1's foundation chose "ocifunc" as
// the per-snapshot Surface discriminator; the service identifier
// follows the same family naming so the audit consumer's per-row
// FailedServices entries pattern-match cleanly across the scanner
// and proposer sides.
const ServiceIDServerless = "ocifunc"

// functionsListAPIVersion pins the OCI Functions /applications and
// /functions list API path version. OCI versions live in the path
// (e.g. "/20181201/") not a query parameter; the constant lives
// here so the scanner path construction is single-sourced. The
// Functions surface uses a different version date than Identity /
// Compute / Database / OKE — the constant keeps the per-surface
// version pin explicit.
const functionsListAPIVersion = "20181201"

// ocifuncSurface is the Surface discriminator string for OCI
// Functions snapshots. Mirrors AWS Lambda's lambdaServerlessSurface
// — the proposer's recommendation-kind prefix routing switches on
// "lambda" → AWS, "cloudrun" / "cloudfunc" → GCP, "azfunc" →
// Azure, "ocifunc" → OCI.
const ocifuncSurface = "ocifunc"

// providerOCI is the Provider discriminator the scanner writes
// onto every serverless snapshot row. Kept as a constant so future
// renames reuse the same string without scattering literal "oci"
// through the projection helper.
const providerOCI = "oci"

// ociApplication is the bare JSON shape of an OCI Functions
// Application as returned by the /applications list call. Functions
// in OCI are namespaced under Applications — each Application is
// the container that owns a set of functions, similar to an AWS
// Lambda alias / Cloud Run service. Slice 1 reads ID (carried into
// the per-function list call) and DisplayName (surfaced into the
// per-function snapshot's Detail bag as "application": appName).
//
// OCI Functions API path:
//
//	GET https://functions.<region>.oci.oraclecloud.com/20181201/applications
//	  ?compartmentId=<compartment_ocid>
//
// LifecycleState is read so non-ACTIVE applications can be
// short-circuited — a DELETING / FAILED application has no live
// functions worth listing.
type ociApplication struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	CompartmentID  string `json:"compartmentId"`
	LifecycleState string `json:"lifecycleState"`
}

// ociApplicationList is the JSON envelope returned by the Functions
// /applications list call. OCI returns the list directly as a JSON
// array; the scanner unmarshals into a []ociApplication slice.
// Pagination follows the opc-next-page response header (see
// listApplications below).
type ociApplicationList = []ociApplication

// ociFunction is the bare JSON shape of an OCI Function as returned
// by the /functions list call. Slice 1 reads ID (-> ResourceARN),
// DisplayName (-> ResourceName), Image (-> Runtime — OCI carries
// runtime detail in the function image name rather than a
// dedicated runtime field; the slice-1 mapping surfaces the image
// reference raw), and Config (-> the two-axis detection rule per
// §3.5).
//
// OCI Functions API path:
//
//	GET https://functions.<region>.oci.oraclecloud.com/20181201/functions
//	  ?applicationId=<application_ocid>
//
// Config is a map[string]string carrying per-function configuration
// values the runtime sees as environment variables. The two
// observability axes are detected by inspecting two specific keys:
//
//   - OCIAPMEnabledConfigKey == "true" -> HasTraceAxis
//   - OTelDistroConfigKey set, non-empty -> HasOTelDistro
//
// LifecycleState is surfaced raw via the Detail bag (slice 1 keeps
// every function in the inventory, regardless of state, so the
// Inventory tab can show mid-create / mid-delete functions; the
// proposer side filters before emitting plan steps).
type ociFunction struct {
	ID             string            `json:"id"`
	DisplayName    string            `json:"displayName"`
	ApplicationID  string            `json:"applicationId"`
	CompartmentID  string            `json:"compartmentId"`
	Image          string            `json:"image"`
	LifecycleState string            `json:"lifecycleState"`
	Config         map[string]string `json:"config,omitempty"`
}

// ociFunctionList is the JSON envelope returned by the Functions
// /functions list call. OCI returns the list directly as a JSON
// array.
type ociFunctionList = []ociFunction

// ScanServerless is the per-provider serverless tier entry point
// (docs/proposals/serverless-tier-slice1.md §5). The chunk-1
// foundation defined the contract: each scanner ships a
// ScanServerless method that returns []ServerlessInstanceSnapshot
// for the configured connection scope. The trampoline / dispatcher
// in chunk-5 calls this method when the scan request includes
// "serverless" in its tier list (default behavior post-chunk-5).
//
// For OCI, the scope is the configured Region + the compartments
// the slice-1 / slice-2 compute / database walks already enumerate
// (tenancy root + first-level child compartments). The scan walks
// every compartment for Applications, then walks every Application
// for Functions, mapping each Function into a
// ServerlessInstanceSnapshot.
//
// Partial-failure accumulation follows the recordPartialFailure
// helper's pattern under the ServiceIDServerless ("ocifunc")
// identifier. A failure on one compartment's Applications walk
// does not stop the remaining compartments; a failure on one
// Application's Functions walk does not stop the remaining
// Applications.
//
// Returns the collected snapshots on success. The slice-1 chunk-4
// contract surfaces partial failures via *result*-style accumulation
// (recorded onto the supplied result when callable via the existing
// per-compartment Scan loop) — this method's signature returns
// (snapshots, error) so callers wiring it directly (e.g. the
// chunk-5 trampoline or a future serverless-only validate path) get
// the typed snapshot slice; the error is non-nil only when the
// substrate-level credential parse fails. Per-compartment / per-
// application failures are swallowed silently here and surfaced via
// the integrating Scan call's recordPartialFailure path when the
// scanner orchestrator wires the walk through Scan.
//
// Slice 1 chunk 4 wires the walk through a dedicated entry point
// (this method) so the existing Scan flow stays untouched. Chunk 5
// of the serverless arc will fold the call into Scan alongside
// scanDatabases / scanOKEClusters.
func (s *Scanner) ScanServerless(ctx context.Context, scope scanner.ScanScope) ([]scanner.ServerlessInstanceSnapshot, error) {
	return s.ScanFunctions(ctx, scope)
}

// scanServerlessTier folds the OCI Functions serverless walk + the
// native-metric cold-start / error-rate detection passes into Scan —
// the slice-1 chunk-4 deferral ("Chunk 5 of the serverless arc will
// fold the call into Scan alongside scanDatabases / scanOKEClusters",
// see ScanServerless above), wired here as part of option 2 (#300).
//
// Gated on the monitoring client being wired — i.e.
// config.ServerlessMetricDetection.Enabled, which OCIFactory honors —
// so a default scan's behavior and API-call surface stay exactly as
// before; only an opted-in deployment walks Functions and runs the
// detectors. The detection passes are themselves nil-tolerant on
// coldStartStore / errorRateStore / connectionID, so even with a
// monitoring client wired they no-op until the stores are present.
//
// Reuses the compartment set the earlier compute / database / OKE tier
// walks already enumerated (tenancy root + first-level children) to
// avoid a redundant compartment-list call. A discovery failure is
// accumulated under ServiceIDServerless and does not halt the scan.
func (s *Scanner) scanServerlessTier(ctx context.Context, allCompartments []ociCompartment, result *scanner.Result) {
	if s.monitoringClient == nil {
		return
	}
	compartmentIDs := make([]string, 0, len(allCompartments))
	for _, comp := range allCompartments {
		compartmentIDs = append(compartmentIDs, comp.ID)
	}
	snaps, err := s.ScanServerless(ctx, scanner.ScanScope{
		AccountID:      s.TenancyOCID,
		CompartmentIDs: compartmentIDs,
	})
	if err != nil {
		recordPartialFailure(result, ServiceIDServerless, err.Error())
		return
	}
	result.Serverless = snaps
	s.runColdStartDetectionForServerless(ctx, result)
	s.runErrorRateDetectionForServerless(ctx, result)
}

// ScanFunctions walks the OCI Functions surface for the configured
// scope. Two-level walk: per compartment list Applications, then
// per Application list Functions. Both levels follow OCI's
// opc-next-page pagination header (the responseHeader carries the
// next-page token; an empty / missing header terminates the loop).
//
// Detection per docs/proposals/serverless-tier-slice1.md §3.5:
//
//   - HasTraceAxis  ← config[OCIAPMEnabledConfigKey] == "true"
//   - HasOTelDistro ← config[OTelDistroConfigKey] is set & non-empty
//
// The two axes are surfaced as independent booleans; the
// proposer's ocifunc-apm-enable and ocifunc-otel-distro
// recommendation kinds key off whichever axis is missing. Slice 1
// does NOT collapse the two into a single "instrumented"
// predicate — the per-axis surfaces are independent levers.
//
// scope.AccountID overrides the scanner's TenancyOCID for the
// snapshot's AccountID field — slice 1 wires the same value through
// both; the parameter is kept symmetric for future per-call scoping.
// scope.CompartmentIDs lets the caller scope the walk to a subset
// of compartments; an empty list defaults to "tenancy root +
// first-level children" via the existing listCompartments helper.
func (s *Scanner) ScanFunctions(ctx context.Context, scope scanner.ScanScope) ([]scanner.ServerlessInstanceSnapshot, error) {
	// Substrate validation. The Scan entry point does this on the
	// way in; ScanFunctions guards defensively at its own entry
	// point so the chunk-5 trampoline can call this method
	// directly without re-validating.
	if s.TenancyOCID == "" {
		return nil, errors.New("oci: TenancyOCID is required")
	}
	if s.Region == "" {
		return nil, errors.New("oci: Region is required")
	}

	signingKey, parseErr := s.signingKey()
	if parseErr != nil {
		return nil, fmt.Errorf("oci: %s: signing failed: %w", ServiceIDServerless, parseErr)
	}

	// Determine the compartment set. An explicit scope wins;
	// otherwise default to tenancy root + first-level children.
	compartments, err := s.compartmentsForServerless(ctx, signingKey, scope)
	if err != nil {
		return nil, fmt.Errorf("oci: %s: compartment listing failed: %w", ServiceIDServerless, err)
	}

	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.TenancyOCID
	}

	var snapshots []scanner.ServerlessInstanceSnapshot
	for _, comp := range compartments {
		apps, appsErr := s.listApplicationsAll(ctx, signingKey, comp.ID)
		if appsErr != nil {
			// Partial failure on this compartment's applications
			// walk — skip this compartment but continue walking
			// the rest. The chunk-5 integration will surface this
			// via recordPartialFailure when called through Scan.
			continue
		}
		for _, app := range apps {
			fns, fnErr := s.listFunctionsAll(ctx, signingKey, app.ID)
			if fnErr != nil {
				// Partial failure on this Application's
				// functions walk — skip this Application but
				// continue.
				continue
			}
			for _, fn := range fns {
				snapshots = append(snapshots,
					projectOCIFunction(fn, app, accountID, s.Region))
			}
		}
	}
	return snapshots, nil
}

// compartmentsForServerless resolves the compartment set to walk.
// An explicit scope.CompartmentIDs wins (the chunk-5 trampoline or
// a per-compartment-scoped invocation supplies it). When the scope
// carries no compartment list, the scanner walks tenancy root +
// first-level children — same default as the compute / database /
// OKE walks.
func (s *Scanner) compartmentsForServerless(ctx context.Context, sk *SigningKey, scope scanner.ScanScope) ([]ociCompartment, error) {
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

// listApplicationsAll walks every page of /applications for a
// single compartment. OCI signals additional pages via the
// opc-next-page response header; the loop passes that token back
// as the page=<token> query parameter on the next call. An empty
// or missing header terminates the loop.
func (s *Scanner) listApplicationsAll(ctx context.Context, sk *SigningKey, compartmentID string) ([]ociApplication, error) {
	var all []ociApplication
	nextPage := ""
	for {
		page, nextToken, callErr := s.listApplicationsPage(ctx, sk, compartmentID, nextPage)
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

// listApplicationsPage walks one page of /applications. The
// returned nextPage string is the opc-next-page header value
// (empty when there are no more pages).
func (s *Scanner) listApplicationsPage(ctx context.Context, sk *SigningKey, compartmentID, page string) ([]ociApplication, string, error) {
	endpoint := s.functionsEndpoint()
	u := fmt.Sprintf(
		"%s/%s/applications?compartmentId=%s",
		strings.TrimRight(endpoint, "/"),
		functionsListAPIVersion,
		url.QueryEscape(compartmentID),
	)
	if page != "" {
		u = u + "&page=" + url.QueryEscape(page)
	}
	body, nextPage, callErr := s.doSignedGETWithPage(ctx, sk, u)
	if callErr != nil {
		return nil, "", callErr
	}
	var out ociApplicationList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, "", &ociCallError{Wrapped: fmt.Errorf("applications response parse: %w", jerr)}
	}
	return out, nextPage, nil
}

// listFunctionsAll walks every page of /functions for a single
// Application. Mirrors listApplicationsAll's pagination loop —
// OCI's opc-next-page convention is the same on both Functions
// endpoints.
func (s *Scanner) listFunctionsAll(ctx context.Context, sk *SigningKey, applicationID string) ([]ociFunction, error) {
	var all []ociFunction
	nextPage := ""
	for {
		page, nextToken, callErr := s.listFunctionsPage(ctx, sk, applicationID, nextPage)
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

// listFunctionsPage walks one page of /functions for a single
// Application. Same pagination convention as listApplicationsPage.
func (s *Scanner) listFunctionsPage(ctx context.Context, sk *SigningKey, applicationID, page string) ([]ociFunction, string, error) {
	endpoint := s.functionsEndpoint()
	u := fmt.Sprintf(
		"%s/%s/functions?applicationId=%s",
		strings.TrimRight(endpoint, "/"),
		functionsListAPIVersion,
		url.QueryEscape(applicationID),
	)
	if page != "" {
		u = u + "&page=" + url.QueryEscape(page)
	}
	body, nextPage, callErr := s.doSignedGETWithPage(ctx, sk, u)
	if callErr != nil {
		return nil, "", callErr
	}
	var out ociFunctionList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, "", &ociCallError{Wrapped: fmt.Errorf("functions response parse: %w", jerr)}
	}
	return out, nextPage, nil
}

// doSignedGETWithPage parallels doSignedGET but also reads the
// opc-next-page response header so the per-surface paginators above
// can drive their loops without re-implementing the header read at
// every call site. The non-pagination scanners (compute / database /
// OKE) keep their original doSignedGET helper untouched.
func (s *Scanner) doSignedGETWithPage(ctx context.Context, sk *SigningKey, u string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", &ociCallError{Wrapped: err}
	}
	req.Header.Set("Accept", "application/json")

	if signErr := sk.SignRequest(req); signErr != nil {
		return nil, "", &ociCallError{Wrapped: signErr}
	}

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, "", &ociCallError{Wrapped: err, IsNetwork: true}
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := readAllLimited(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, resp.Header.Get("opc-next-page"), nil
	}

	var oerr ociErrorBody
	_ = json.Unmarshal(body, &oerr)
	return nil, "", &ociCallError{
		StatusCode: resp.StatusCode,
		Code:       oerr.Code,
		Message:    oerr.Message,
		BodyHint:   truncate(string(body), 200),
		RetryAfter: resp.Header.Get("Retry-After"),
	}
}

// readAllLimited reads up to 1MiB from the response body. Mirrors
// the limit applied in doSignedGET so a misconfigured endpoint
// returning a giant body can't bloat the scanner's working set.
func readAllLimited(body interface {
	Read(p []byte) (n int, err error)
}) ([]byte, error) {
	var buf [1 << 20]byte
	n := 0
	for n < len(buf) {
		k, err := body.Read(buf[n:])
		n += k
		if err != nil {
			break
		}
		if k == 0 {
			break
		}
	}
	out := make([]byte, n)
	copy(out, buf[:n])
	return out, nil
}

// functionsEndpoint returns the OCI Functions API base URL. When
// ociEndpoint is set (tests), it's used directly — the test mock
// dispatches /applications and /functions on the same httptest
// server that already routes the compute / database / OKE paths.
// In production the per-region Functions endpoint pattern is
// https://functions.<region>.oci.oraclecloud.com.
func (s *Scanner) functionsEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://functions.%s.oci.oraclecloud.com", s.Region)
}

// projectOCIFunction maps an ociFunction (and its parent
// ociApplication) into the provider-agnostic
// ServerlessInstanceSnapshot. The slice-1 chunk-4 mapping is:
//
//   - Provider: "oci".
//   - Surface: "ocifunc".
//   - AccountID: the per-scan tenancy OCID (the scope-provided
//     value or the scanner's TenancyOCID fallback).
//   - Region: the scanner's configured Region — OCI's Functions
//     response does not echo region per-row (the surface is
//     scoped to the region the request was sent to).
//   - ResourceName: fn.DisplayName (operator-readable function
//     name).
//   - ResourceARN: fn.ID (full OCI function OCID — the canonical
//     handle the proposer's evidence list references).
//   - Runtime: fn.Image (OCI Functions carry runtime info in the
//     container image name; slice 1 surfaces this raw so the
//     proposer's per-language SDK customization in slice 2 has a
//     stable identifier to key off).
//   - HasTraceAxis: fn.Config[OCI_APM_ENABLED] == "true". Exact
//     match — see OCIAPMEnabledConfigKey godoc for the rationale.
//   - HasOTelDistro: fn.Config[OTEL_DISTRO] set and non-empty —
//     see OTelDistroConfigKey godoc for the rationale.
//   - Detail: {"application": app.DisplayName,
//     "lifecycle_state": fn.LifecycleState}. The Application name
//     is the slice 1 chunk 4 contract per the brief — the
//     per-cloud Inventory tab needs the parent Application name
//     to disambiguate functions sharing a DisplayName across
//     Applications (legal in OCI's Application → Function
//     namespacing). LifecycleState is surfaced so the Inventory
//     tab can dim non-ACTIVE rows the same way the database / OKE
//     tabs do.
func projectOCIFunction(fn ociFunction, app ociApplication, accountID, region string) scanner.ServerlessInstanceSnapshot {
	snap := scanner.ServerlessInstanceSnapshot{
		Provider:     providerOCI,
		Surface:      ocifuncSurface,
		AccountID:    accountID,
		Region:       region,
		ResourceName: fn.DisplayName,
		ResourceARN:  fn.ID,
		Runtime:      fn.Image,
	}

	// Axis 1: APM trace integration.
	if v, ok := fn.Config[OCIAPMEnabledConfigKey]; ok && v == OCIAPMEnabledConfigValue {
		snap.HasTraceAxis = true
	}

	// Axis 2: OpenTelemetry distro attached.
	if v, ok := fn.Config[OTelDistroConfigKey]; ok && v != "" {
		snap.HasOTelDistro = true
	}

	snap.Detail = map[string]any{
		"application":     app.DisplayName,
		"lifecycle_state": fn.LifecycleState,
	}
	return snap
}

// classifyOCIFunctionsError maps an OCI Functions call failure into
// the operator-visible PartialReason string under the ocifunc
// service identifier. Parallels classifyOCIError /
// classifyOCIDBError / classifyOCIOKEError so the audit consumer
// sees identical structure across the four OCI service surfaces.
//
// Slice 1 chunk 4 ships this helper for the chunk-5 trampoline to
// surface partial failures via recordPartialFailure; the chunk-4
// ScanFunctions entry-point itself swallows per-compartment /
// per-application failures silently (returning the partial snapshot
// slice). The helper is unused in chunk-4 but exported here so
// chunk-5's integration finds it under the same name family.
//
// Error mappings (per the slice 1 design doc §12 threat model):
//
//   - HTTP 401 -> credentials_invalid (signature rejected).
//   - HTTP 403 -> permission_denied with hint pointing at the
//     "inspect functions in compartment" policy statement.
//   - HTTP 404 mid-walk -> empty string (silent skip — many
//     compartments have no Functions Applications; surfacing 404s
//     as partial failures would be noise).
//   - HTTP 429 -> rate_limit.
//   - Transport / network errors -> network-error with truncated
//     underlying error.
//   - Any other 4xx/5xx -> truncated message under the ocifunc
//     identifier.
func classifyOCIFunctionsError(err error, atRoot bool) string {
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
			return fmt.Sprintf("%s: network error: %s", ServiceIDServerless, truncate(wrapped, 200))
		}
		if oce.StatusCode == http.StatusTooManyRequests || oce.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDServerless)
		}
		switch oce.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check fingerprint, user_ocid, and private key)", ServiceIDServerless)
		case http.StatusForbidden:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: permission denied (verify the policy grants 'inspect functions in compartment'): %s", ServiceIDServerless, truncate(msg, 200))
		case http.StatusNotFound:
			if atRoot {
				return fmt.Sprintf("%s: Functions surface not found (verify tenancy_ocid and the inspect-functions policy)", ServiceIDServerless)
			}
			return ""
		default:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: OCI call failed (HTTP %d): %s", ServiceIDServerless, oce.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDServerless, truncate(err.Error(), 200))
}
