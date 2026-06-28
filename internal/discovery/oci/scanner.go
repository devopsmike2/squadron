// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Scanner walks Compute Instances in a single OCI tenancy. It is
// constructed per-scan with the API Signing Key private key bytes
// already unsealed by the caller; the scanner does not retain the
// PEM bytes beyond the Scan call's lifetime once the parsed RSA
// key is memoized.
//
// Slice 1's scope is single-region, single-tenancy, root +
// first-level-child compartments only. Slice 2 adds the full
// compartment subtree walk and multi-region orchestration.
type Scanner struct {
	// TenancyOCID is the OCI tenancy the scan targets
	// (ocid1.tenancy.oc1..<unique_id>). Required.
	TenancyOCID string

	// UserOCID is the OCI user whose API Signing Key authenticates
	// the scan (ocid1.user.oc1..<unique_id>). Required.
	UserOCID string

	// Fingerprint is the OCI API Signing Key fingerprint
	// (xx:xx:xx:...). Pairs with PrivateKey to identify which key
	// OCI should verify the request against. Required.
	Fingerprint string

	// PrivateKey is the unsealed RSA private key in PEM form.
	// Required. The caller MUST call credstore.UnsealOCIPrivateKey
	// before constructing the scanner; the scanner does not retain
	// the PEM bytes beyond the parse-on-first-use call (parsedKey
	// holds the memoized *rsa.PrivateKey).
	//
	// Substrate invariant: the PEM bytes NEVER appear in error
	// strings, log lines, audit payloads, or HTTP responses. The
	// classifyOCIError helper ensures error messages name failure
	// shapes ("signature rejected", "permission denied") without
	// echoing the credential.
	PrivateKey []byte

	// Region is the OCI region to scan (e.g. "us-phoenix-1").
	// REQUIRED for OCI — unlike AWS/GCP/Azure where empty Region
	// means "scan all", OCI's API endpoints are regional. The
	// scanner builds identity and compute endpoint URLs from this
	// value.
	Region string

	// httpClient is the transport for OCI REST API calls. Defaults
	// to http.DefaultClient when nil. Tests inject a custom client
	// pointing at an httptest server.
	httpClient *http.Client

	// ociEndpoint overrides the default identity + compute base
	// URLs. Empty in production; tests point this at their httptest
	// server which multiplexes both identity (/20160918/compartments)
	// and compute (/20160918/instances) routes via path-based
	// dispatch. When set, the scanner uses this single endpoint for
	// every OCI call regardless of which API surface the call
	// targets.
	ociEndpoint string

	// parsedKey memoizes the parsed RSA private key after the first
	// call to keyForSigning. Subsequent Scan invocations reuse the
	// parsed key without re-decoding the PEM bytes.
	parsedKey *rsa.PrivateKey

	// monitoringClient is the OCI Monitoring adapter the slice-2
	// chunk-3 MetricQuerier implementation uses (v0.89.118).
	// Nil-tolerant for backward compatibility with the chunk-1
	// skeleton path and the validation constructors that don't
	// need metric queries. When nil, QueryAggregate returns
	// scanner.ErrMetricNotImplemented mirroring the v0.89.113
	// chunk-1 surface. Tests inject a fake satisfying
	// MonitoringClient; production wires the signed REST client
	// via WithMonitoringClient + NewSignedMonitoringClient.
	monitoringClient MonitoringClient

	// metricsLimiter is the per-Scanner-instance rate limiter that
	// caps OCI Monitoring summarizeMetricsData TPS at
	// OCIMonitoringRateLimitTPS. Per-Scanner-instance is the
	// equivalent of per-tenancy in the slice 2 substrate (one
	// Scanner per CloudConnection per scan). Nil-tolerant:
	// QueryAggregate skips the Wait call when the limiter is nil.
	metricsLimiter *rate.Limiter

	// coldStartStore is the storage adapter for persisting cold-start
	// observations the chunk-4 detection branch produces. v0.89.118 —
	// nil-tolerant so a Scanner constructed via the validation-only
	// path doesn't have to wire a real store. When nil,
	// DetectColdStartRegression still runs the OCI Monitoring + ratio
	// math (so callers can inspect the detection result
	// programmatically) but skips the SaveColdStartObservation call.
	coldStartStore ColdStartStore

	// connectionID is the CloudConnection identifier that scopes
	// persisted cold-start observations. v0.89.118 — same rationale
	// as the AWS scanner's connectionID field.
	connectionID string

	// errorRateStore — Error rate correlation slice 1 chunk 3
	// (v0.89.129). Storage adapter for persisting
	// error_rate_observation rows the chunk-3 detection branch
	// produces. Nil-tolerant — same posture as coldStartStore.
	errorRateStore ErrorRateStore
}

// Provider satisfies the (future) scanner.Scanner interface. The
// chunk-3 API trampoline will wire the OCIConnection-based scanner
// onto the provider-agnostic surface; chunk-2 ships the concrete
// Scanner that the trampoline constructs.
func (s *Scanner) Provider() credstore.Provider {
	return credstore.ProviderOCI
}

// Scan walks Compute Instances in the configured tenancy + region,
// returning a scanner.Result with ComputeInstanceSnapshot entries.
// Partial failures (rate limits, permission denied mid-walk) are
// recorded in Result.PartialReason / Result.FailedServices per the
// shared partial-failure convention (see internal/discovery/aws/
// scanner.go::recordPartialFailure and internal/discovery/azure/
// scanner.go::recordPartialFailure for the canonical pattern; this
// scanner ships its own helper accumulating the same way — OCI
// service identifier "ocicompute").
//
// Returns nil error when the compartment + instance walk returned
// (even if an instance list call returned an error code mapped into
// a partial-failure reason). Returns a wrapped error only when the
// private key parse fails OR the initial compartment list call
// fails entirely — those are the substrate-level hard failures
// where zero instances could plausibly have been walked.
func (s *Scanner) Scan(ctx context.Context) (result scanner.Result, err error) {
	scanID := uuid.NewString()
	result = scanner.Result{
		ScanID:        scanID,
		ScanStartedAt: time.Now().UTC(),
		Provider:      credstore.ProviderOCI,
		AccountID:     s.TenancyOCID,
		Regions:       []string{s.Region},
	}
	// Named return: defer can mutate ScanCompletedAt after the
	// return statement copies the rest of the struct into the
	// caller's frame. Mirrors the AWS / GCP / Azure scanners'
	// pattern.
	defer func() {
		result.ScanCompletedAt = time.Now().UTC()
	}()

	// Field validation. The chunk-3 create handler validates these
	// on the way in; the scanner guards defensively at the entry
	// point. Each missing field surfaces a distinct error so the
	// caller's audit emit path names the misconfiguration shape.
	if s.TenancyOCID == "" {
		return result, errors.New("oci: TenancyOCID is required")
	}
	if s.UserOCID == "" {
		return result, errors.New("oci: UserOCID is required")
	}
	if s.Fingerprint == "" {
		return result, errors.New("oci: Fingerprint is required")
	}
	if len(s.PrivateKey) == 0 {
		return result, errors.New("oci: PrivateKey is required")
	}
	if s.Region == "" {
		return result, errors.New("oci: Region is required")
	}

	// Parse + memoize the private key. PEM parse failure is a
	// hard error — no OCI call can succeed without a valid key.
	signingKey, parseErr := s.signingKey()
	if parseErr != nil {
		return result, fmt.Errorf("oci: %s: signing failed: %w", ServiceIDCompute, parseErr)
	}

	// List first-level child compartments of the tenancy. Add the
	// tenancy itself as the root compartment so the walk covers
	// "tenancy + first-level children" per design doc §9. Failure
	// here is a hard error — zero compartments means zero possible
	// instances.
	compartments, listErr := s.listCompartments(ctx, signingKey)
	if listErr != nil {
		// The compartment list call is the substrate-level hard
		// failure surface, but the chunk-2 brief lists the
		// per-status mappings under partial-failure handling for
		// consistency with the per-compartment walks. We surface
		// the structured failure via recordPartialFailure AND
		// return a hard error so the caller's audit emit path
		// fires scan_failed rather than scan_completed-with-partial
		// when the entire compartment listing fails up-front.
		reason := classifyOCIError(listErr, true /*atRoot*/)
		// Root 404 is treated as "skip the entire scan" per design
		// — operator misconfigured the tenancy_ocid. The other
		// shapes (401, 403, 429, network) bubble up as hard errors
		// since they tell the operator their credentials or
		// connectivity are broken.
		recordPartialFailure(&result, ServiceIDCompute, reason)
		return result, fmt.Errorf("oci: %s: %s", ServiceIDCompute, reason)
	}

	// Walk root + first-level children. The root compartment IS
	// the tenancy OCID — OCI's compartment hierarchy roots at the
	// tenancy.
	allCompartments := append([]ociCompartment{
		{ID: s.TenancyOCID, Name: "root", LifecycleState: "ACTIVE"},
	}, compartments...)

	for _, comp := range allCompartments {
		instances, instErr := s.listInstances(ctx, signingKey, comp.ID)
		if instErr != nil {
			reason := classifyOCIError(instErr, false /*atRoot*/)
			// Compartment-missing (404 mid-walk) is non-fatal —
			// surface as partial but keep walking the remaining
			// compartments. The classifier returns "" for the
			// compartment-skip case so we can branch here.
			if reason == "" {
				continue
			}
			recordPartialFailure(&result, ServiceIDCompute, reason)
			continue
		}
		for _, inst := range instances {
			result.Compute = append(result.Compute, projectInstance(inst, s.Region))
		}
	}

	// Slice 2 (database tier): walk DB Systems + Autonomous
	// Databases across the same compartment set the compute walk
	// just visited. The walker is in scanner_db.go; it appends
	// rows to result.Databases and surfaces partial failures
	// under the ServiceIDDatabase identifier. Both walks (compute
	// + databases) share the same SigningKey + httpClient — no
	// per-surface auth duplication.
	s.scanDatabases(ctx, signingKey, allCompartments, &result)

	// Slice 2 (kubernetes tier): walk OKE clusters across the
	// same compartment set the compute and database walks just
	// visited. The walker is in scanner_oke.go; it appends rows
	// to result.Clusters and surfaces partial failures under the
	// ServiceIDKubernetes identifier ("oke"). All three walks
	// (compute + databases + clusters) share the same SigningKey
	// + httpClient — no per-surface auth duplication, per design
	// doc §5.3.
	s.scanOKEClusters(ctx, signingKey, allCompartments, &result)

	// Slice 1 instrumented rule (Compute): HasOTel == true.
	for _, c := range result.Compute {
		if c.HasOTel {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}

	return result, nil
}

// signingKey parses the PEM-encoded private key on first call and
// memoizes the result. Subsequent calls return the cached
// *rsa.PrivateKey without re-decoding.
func (s *Scanner) signingKey() (*SigningKey, error) {
	if s.parsedKey == nil {
		parsed, err := ParsePrivateKey(s.PrivateKey)
		if err != nil {
			return nil, err
		}
		s.parsedKey = parsed
	}
	return &SigningKey{
		TenancyOCID: s.TenancyOCID,
		UserOCID:    s.UserOCID,
		Fingerprint: s.Fingerprint,
		PrivateKey:  s.parsedKey,
	}, nil
}

// listCompartments walks the Identity /compartments endpoint for the
// tenancy root. Slice 1 ships compartmentIdInSubtree=false so only
// first-level children are returned; the tenancy itself is added by
// the caller (Scan) as the root compartment.
func (s *Scanner) listCompartments(ctx context.Context, sk *SigningKey) ([]ociCompartment, error) {
	endpoint := s.identityEndpoint()
	url := fmt.Sprintf(
		"%s/%s/compartments?compartmentId=%s&accessLevel=ANY&compartmentIdInSubtree=false",
		strings.TrimRight(endpoint, "/"),
		identityListAPIVersion,
		s.TenancyOCID,
	)

	body, callErr := s.doSignedGET(ctx, sk, url)
	if callErr != nil {
		return nil, callErr
	}

	var out ociCompartmentList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, &ociCallError{Wrapped: fmt.Errorf("compartments response parse: %w", jerr)}
	}
	return out, nil
}

// listInstances walks the Compute /instances endpoint for a single
// compartment. Slice 1 ships a single-page walk (no
// opc-next-page header following); slice 2 will add pagination.
func (s *Scanner) listInstances(ctx context.Context, sk *SigningKey, compartmentID string) ([]ociInstance, error) {
	endpoint := s.computeEndpoint()
	url := fmt.Sprintf(
		"%s/%s/instances?compartmentId=%s",
		strings.TrimRight(endpoint, "/"),
		computeListAPIVersion,
		compartmentID,
	)

	body, callErr := s.doSignedGET(ctx, sk, url)
	if callErr != nil {
		return nil, callErr
	}

	var out ociInstanceList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, &ociCallError{Wrapped: fmt.Errorf("instances response parse: %w", jerr)}
	}
	return out, nil
}

// doSignedGET signs and dispatches a single GET request, returning
// the response body on success or an *ociCallError on any non-2xx
// status / transport error. Centralizes the signing + request-issue
// + status-classification flow so listCompartments / listInstances
// don't duplicate it.
func (s *Scanner) doSignedGET(ctx context.Context, sk *SigningKey, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &ociCallError{Wrapped: err}
	}
	req.Header.Set("Accept", "application/json")

	if signErr := sk.SignRequest(req); signErr != nil {
		return nil, &ociCallError{Wrapped: signErr}
	}

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, &ociCallError{Wrapped: err, IsNetwork: true}
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}

	var oerr ociErrorBody
	_ = json.Unmarshal(body, &oerr)
	return nil, &ociCallError{
		StatusCode: resp.StatusCode,
		Code:       oerr.Code,
		Message:    oerr.Message,
		BodyHint:   truncate(string(body), 200),
		RetryAfter: resp.Header.Get("Retry-After"),
	}
}

// client returns the configured http.Client or http.DefaultClient
// when nil. Centralizes the nil-check.
func (s *Scanner) client() *http.Client {
	if s.httpClient != nil {
		return s.httpClient
	}
	return http.DefaultClient
}

// identityEndpoint returns the OCI Identity API base URL. When
// ociEndpoint is set (tests), it's used directly. In production the
// per-region identity endpoint pattern is
// https://identity.<region>.oci.oraclecloud.com.
func (s *Scanner) identityEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://identity.%s.oci.oraclecloud.com", s.Region)
}

// computeEndpoint returns the OCI Compute API base URL. When
// ociEndpoint is set (tests), it's used directly. In production the
// per-region compute endpoint pattern is
// https://iaas.<region>.oraclecloud.com.
func (s *Scanner) computeEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://iaas.%s.oraclecloud.com", s.Region)
}

// projectInstance maps an ociInstance into the provider-agnostic
// ComputeInstanceSnapshot. The mapping is the slice-1 contract:
//
//   - ResourceID: instance.DisplayName (operator-readable, stable
//     per tenancy per design doc §9).
//   - InstanceType: instance.Shape (e.g. "VM.Standard.E4.Flex").
//   - Tags: flattened FreeformTags + DefinedTags. DefinedTags
//     flatten by dropping the namespace prefix and keeping the
//     inner key=value pairs. Conflicts between freeform and defined
//     keys keep the freeform value (defensive default — freeform
//     tags are operator-chosen string→string and clearly readable;
//     defined tags carry typed values that the slice 1 flattener
//     stringifies).
//   - HasOTel: any flattened key starts with otel (case-insensitive).
//     Mirrors the AWS EC2 / GCP GCE / Azure VM slice-1 rule.
//   - OSFamily: "unknown" — OCI exposes OS via the Image relationship
//     which needs a secondary lookup. Slice 2 adds detection
//     (design doc §14).
//   - Region: instance.Region (or the scanner's configured Region
//     when the instance JSON does not carry it — slice 1 ships
//     single-region per connection).
func projectInstance(inst ociInstance, fallbackRegion string) scanner.ComputeInstanceSnapshot {
	region := inst.Region
	if region == "" {
		region = fallbackRegion
	}
	tags := flattenTags(inst.FreeformTags, inst.DefinedTags)
	return scanner.ComputeInstanceSnapshot{
		ResourceID:   inst.DisplayName,
		ImportID:     inst.ID, // OCID = oci_core_instance import id
		InstanceType: inst.Shape,
		Tags:         tags,
		HasOTel:      hasOTelTag(tags),
		OSFamily:     "unknown",
		Region:       region,
	}
}

// flattenTags merges OCI's two tag surfaces (freeform + defined) into
// the snapshot's single map[string]string. Freeform tags are
// string→string and copy through as-is. Defined tags are
// namespace→key→value where value carries a typed JSON value; the
// flattener uses fmt %v for the value side and DROPS the namespace
// (slice 1 simplicity per the chunk-2 brief). When a defined-tag key
// collides with a freeform-tag key, the freeform value wins (the
// freeform tag was set first, so the defined tag is the override
// that the operator added — but slice 1 prefers the freeform's
// clearly readable shape).
//
// Returns nil for an entirely empty input so the Tags field stays
// omit-empty-friendly. Mirrors the Azure scanner's copyTags pattern.
func flattenTags(freeform map[string]string, defined map[string]map[string]interface{}) map[string]string {
	if len(freeform) == 0 && len(defined) == 0 {
		return nil
	}
	out := make(map[string]string, len(freeform)+len(defined))
	// Defined first so freeform overrides.
	for _, kv := range defined {
		for k, v := range kv {
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	for k, v := range freeform {
		out[k] = v
	}
	return out
}

// hasOTelTag returns true if any tag key starts with the otel prefix
// case-insensitively. The case-insensitive rule mirrors the AWS EC2,
// GCP GCE, and Azure VM scanners' slice-1 implementations.
func hasOTelTag(tags map[string]string) bool {
	for k := range tags {
		if strings.HasPrefix(strings.ToLower(k), OTelTagPrefix) {
			return true
		}
	}
	return false
}

// recordPartialFailure marks the scan partial and appends both a
// service identifier to FailedServices AND a human-readable reason
// to PartialReason. Mirrors the AWS / GCP / Azure scanners'
// recordPartialFailure helper so the shared audit / proposer-side
// consumers see identical structure across providers. The
// accumulator joins multiple failures with "; "; single-failure
// scans are unchanged.
func recordPartialFailure(result *scanner.Result, service, reason string) {
	result.Partial = true
	if result.PartialReason == "" {
		result.PartialReason = reason
	} else {
		result.PartialReason = result.PartialReason + "; " + reason
	}
	result.FailedServices = append(result.FailedServices, service)
}

// classifyOCIError maps an OCI call failure into the
// operator-visible PartialReason string. The string is the audit
// payload's human-readable diagnostic; the structured FailedServices
// field carries the per-service identifier separately (see
// recordPartialFailure).
//
// The atRoot flag distinguishes the initial compartment-list call
// (where 404 maps to tenancy_not_found) from per-compartment
// instance-list calls (where 404 is non-fatal: the compartment was
// deleted between listCompartments and listInstances; skip it
// quietly). The classifier returns the empty string for the
// per-compartment 404 case so the caller can branch on it.
//
// Error mappings (per docs/proposals/oci-discovery-slice1.md §7.1
// and the chunk-2 brief):
//
//   - HTTP 401 -> credentials_invalid (signature rejected — typically
//     wrong fingerprint, malformed key, or skewed clock).
//   - HTTP 403 -> permission_denied with body excerpt (the user_ocid
//     does not have access to the compartment).
//   - HTTP 404 at root -> tenancy_not_found with remediation hint
//     pointing at tenancy_ocid.
//   - HTTP 404 mid-walk -> empty string (skip the compartment).
//   - HTTP 429 -> rate_limit.
//   - Transport / network errors -> network-error with the underlying
//     err.Error() truncated to keep audit payloads bounded.
//   - Any other 4xx/5xx -> truncated message under the ocicompute
//     identifier.
func classifyOCIError(err error, atRoot bool) string {
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
			return fmt.Sprintf("%s: network error: %s", ServiceIDCompute, truncate(wrapped, 200))
		}
		if oce.StatusCode == http.StatusTooManyRequests || oce.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDCompute)
		}
		switch oce.StatusCode {
		case http.StatusUnauthorized:
			// OCI returns 401 for any signature-layer rejection:
			// wrong fingerprint, malformed key, expired clock,
			// missing keyId. Surface as credentials_invalid so the
			// wizard's validate path treats it the same as the
			// generic signing failure surface.
			return fmt.Sprintf("%s: credentials invalid (re-check fingerprint, user_ocid, and private key)", ServiceIDCompute)
		case http.StatusForbidden:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: permission denied (verify the user has access to the tenancy / compartments): %s", ServiceIDCompute, truncate(msg, 200))
		case http.StatusNotFound:
			if atRoot {
				return fmt.Sprintf("%s: tenancy not found (verify tenancy_ocid is correct)", ServiceIDCompute)
			}
			// Mid-walk 404 — compartment vanished between list and
			// instance-list. Caller branches on the empty return.
			return ""
		default:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: OCI call failed (HTTP %d): %s", ServiceIDCompute, oce.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDCompute, truncate(err.Error(), 200))
}

// truncate caps a string at n bytes, appending an ellipsis when the
// cap fires. Used to keep audit payloads bounded — a misconfigured
// API endpoint can return a multi-kilobyte error body that bloats
// the audit row otherwise. Mirrors the Azure / GCP scanners' truncate
// helper.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// sprintfErr is a thin alias for fmt.Sprintf used in the ociCallError
// Error() method so types.go doesn't import fmt (keeps the types
// file focused on the JSON shapes).
func sprintfErr(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}
