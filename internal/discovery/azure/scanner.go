// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Scanner walks Virtual Machines in a single Azure subscription. It
// is constructed per-scan with the Service Principal client_secret
// already unsealed by the caller; the scanner does not retain the
// secret bytes beyond the Scan call's lifetime.
//
// Slice 1's scope is single-subscription, single-location-or-all.
// Slice 2 adds multi-subscription orchestration paralleling AWS
// v0.89.7a / GCP slice 2.
type Scanner struct {
	// TenantID is the Azure AD tenant the Service Principal lives in
	// (UUID format). Required.
	TenantID string

	// SubscriptionID is the Azure subscription to scan (UUID format).
	// Required.
	SubscriptionID string

	// ClientID is the Service Principal app registration ID (UUID
	// format). Required.
	ClientID string

	// ClientSecret is the unsealed Service Principal client_secret
	// bytes. Required. The caller MUST call
	// credstore.UnsealAzureClientSecret before constructing the
	// scanner; the scanner does not retain the bytes beyond the Scan
	// call's lifetime.
	//
	// Substrate invariant: the secret bytes NEVER appear in error
	// strings, log lines, audit payloads, or HTTP responses. The
	// classifyTokenError / classifyARMError helpers ensure error
	// messages name failure shapes ("token endpoint auth failed",
	// "permission denied") without echoing the credential.
	ClientSecret []byte

	// Location restricts the scan to a single Azure region. Empty
	// means "scan all locations visible to the SP".
	Location string

	// httpClient is the transport for token-endpoint and ARM API
	// calls. Defaults to http.DefaultClient when nil. Tests inject a
	// custom client pointing at an httptest server.
	httpClient *http.Client

	// armEndpoint overrides the default https://management.azure.com
	// base URL. Empty in production; tests point this at their
	// httptest server so the scanner exercises the real REST flow
	// against a mock.
	armEndpoint string

	// tokenEndpoint overrides the default
	// https://login.microsoftonline.com base URL. Empty in production;
	// tests point this at their httptest server's token route so the
	// OAuth flow is exercised against a mock that returns a fake
	// access token.
	tokenEndpoint string

	// accessToken is an OAuth2 bearer token wired by the chunk-2
	// MetricQuerier wiring (v0.89.118). Set externally via
	// WithAccessToken so the cold-start detection branch (which runs
	// outside a Scan() lifecycle, e.g. via the per-resource
	// cold_start API endpoint) can issue Azure Monitor calls without
	// re-acquiring a token. The Scan() path acquires its own token
	// internally and does NOT persist it on the Scanner — the
	// accessToken field is exclusively for callers that have already
	// acquired a token externally.
	//
	// Empty by default. QueryAggregate treats an empty accessToken
	// as the chunk-1 skeleton path (returns
	// scanner.ErrMetricNotImplemented) for backward compatibility
	// with the v0.89.113 surface.
	accessToken string

	// costGovernor caps cost-query usage and is the opt-in "cost
	// correlation enabled" signal. Cost-correlation substrate slice 6
	// chunk 4 (v0.89.186) — Azure Cost Management Query is free per
	// call, but QueryCost still requires a governor (authorizing the
	// $0 per-call cost) so cost correlation only runs when explicitly
	// wired, never by default. See scanner.CostBudgetGovernor +
	// azure/cost.go.
	costGovernor *scanner.CostBudgetGovernor

	// metricsLimiter is the per-Scanner-instance rate limiter the
	// chunk-2 MetricQuerier implementation consults before every
	// Azure Monitor /metrics call (v0.89.118). Caps the per-
	// subscription RPH at AzureMonitorRateLimitRPH (12,000 RPH =
	// 200 RPM = ~3.33 RPS). Per-Scanner-instance is the equivalent
	// of per-subscription in the slice 2 substrate (one Scanner per
	// CloudConnection per scan); the chunk-5 runbook documents the
	// contract.
	//
	// Nil-tolerant: QueryAggregate skips the Wait call when the
	// limiter is nil, which is the chunk-1 skeleton path (no real
	// Azure Monitor calls being made).
	metricsLimiter *rate.Limiter
}

// Provider satisfies the (future) scanner.Scanner interface. The
// chunk-3 API trampoline will wire the AzureConnection-based scanner
// onto the provider-agnostic surface; chunk-2 ships the concrete
// Scanner that the trampoline constructs.
func (s *Scanner) Provider() credstore.Provider {
	return credstore.ProviderAzure
}

// Scan walks Virtual Machines in the configured subscription,
// returning a scanner.Result with ComputeInstanceSnapshot entries.
// Partial failures (rate limits, transient errors mid-walk) are
// recorded in Result.PartialReason / Result.FailedServices per the
// shared partial-failure convention (see internal/discovery/aws/
// scanner.go::recordPartialFailure and internal/discovery/gcp/
// scanner.go::recordPartialFailure for the canonical pattern; this
// scanner ships its own helper accumulating the same way — Azure
// service identifier "azurevm").
//
// Returns nil error when the VM list call returned (even if it
// returned an error code that we mapped into a partial-failure
// reason). Returns a wrapped error only when the OAuth2 token
// exchange itself fails — that is the one substrate-level hard
// failure where zero VMs could possibly have been walked AND the
// signal is actionable as a misconfiguration (tenant / client / secret
// wrong), not an Azure-side condition like a 5xx that retries fix.
func (s *Scanner) Scan(ctx context.Context) (result scanner.Result, err error) {
	scanID := uuid.NewString()
	result = scanner.Result{
		ScanID:        scanID,
		ScanStartedAt: time.Now().UTC(),
		Provider:      credstore.ProviderAzure,
		AccountID:     s.SubscriptionID,
	}
	// Named return: defer can mutate ScanCompletedAt after the
	// return statement copies the rest of the struct into the
	// caller's frame. Mirrors the AWS / GCP scanners' pattern where
	// the completed-at timestamp is the last thing stamped
	// regardless of the success / partial branch the scan took.
	defer func() {
		result.ScanCompletedAt = time.Now().UTC()
	}()

	if s.SubscriptionID == "" {
		return result, errors.New("azure: SubscriptionID is required")
	}
	if s.TenantID == "" || s.ClientID == "" || len(s.ClientSecret) == 0 {
		// In production the SP triple (tenant_id, client_id,
		// client_secret) is the only sanctioned auth path. Any
		// missing piece is a caller bug — the create handler
		// (chunk 1) validates these on the way in; the scanner
		// guards defensively at the entry point.
		return result, errors.New("azure: TenantID, ClientID, and ClientSecret are required")
	}

	// Acquire an access token. The token endpoint is the one place
	// where a failure means zero VMs were walked AND the failure is
	// almost always credential misconfiguration (tenant_id wrong,
	// client_id wrong, secret expired). Surface as a hard error so
	// the caller's audit emit path fires scan_failed rather than
	// scan_completed-with-partial.
	token, tokenErr := s.acquireAccessToken(ctx)
	if tokenErr != nil {
		return result, fmt.Errorf("azure: %s: %w", ServiceIDVirtualMachines, tokenErr)
	}

	// List VMs across the subscription. The subscription-wide listing
	// endpoint returns every VM across every resource group in a
	// single call; pagination is followed via nextLink.
	vms, walked, listErr := s.listVirtualMachines(ctx, token)
	if listErr != nil {
		reason := classifyARMError(listErr)
		recordPartialFailure(&result, ServiceIDVirtualMachines, reason)
		// Even on partial, denormalize the location filter (if set)
		// into result.Regions so the audit payload's shape stays
		// consistent with success.
		if s.Location != "" {
			result.Regions = []string{s.Location}
		}
		// Slice 2 (chunk 3): the Azure SQL walk runs against the
		// same OAuth token and is independent of the Compute
		// surface. A VM listing failure does NOT preclude scanning
		// SQL — the SP may have Reader on Microsoft.Sql but not
		// Microsoft.Compute in unusual policy splits, and surfacing
		// the database inventory is still useful when the VM walk
		// has already been bucketed as partial. The SQL walk's own
		// partial failures accumulate independently.
		s.scanAzureSQL(ctx, token, &result)
		// Kubernetes-tier-slice-2 (chunk 3): the AKS walk runs
		// against the same OAuth token and is independent of both
		// the Compute and SQL surfaces. A VM listing failure does
		// NOT preclude scanning AKS — the SP may have Reader on
		// Microsoft.ContainerService but not Microsoft.Compute in
		// unusual policy splits, and surfacing the cluster
		// inventory is still useful when the VM walk has already
		// been bucketed as partial. The AKS walk's own partial
		// failures accumulate independently under the "aks" service
		// id.
		s.scanAKS(ctx, token, &result)
		// Serverless-tier-slice-1 (chunk 3, v0.89.91, #723 Stream 121):
		// the Azure Functions walk runs against the same OAuth token
		// and is independent of the Compute / SQL / AKS surfaces.
		// A VM listing failure does NOT preclude scanning Function
		// Apps — the SP may have Reader on Microsoft.Web but not
		// Microsoft.Compute in unusual policy splits. Partial
		// failures accumulate under the "azfunc" service id.
		s.scanAzureFunctions(ctx, token, &result)
		return result, nil
	}

	walkedRegions := map[string]struct{}{}

	for _, vm := range vms {
		region := vm.Location
		if s.Location != "" && region != s.Location {
			continue
		}
		walkedRegions[region] = struct{}{}
		result.Compute = append(result.Compute, projectVM(vm))
	}

	// If the walk surfaced VMs across N regions but the operator
	// configured a Location filter that excluded ALL of them, still
	// surface the configured location in Regions so the audit
	// payload reads "this scan targeted eastus" rather than empty.
	if s.Location != "" && len(walkedRegions) == 0 {
		walkedRegions[s.Location] = struct{}{}
	}

	// Denormalize the walked-regions set into Result.Regions. Order
	// is not stable across runs (map iteration); the field is
	// documented as "regions actually walked" — a set, not a
	// sequence.
	for r := range walkedRegions {
		result.Regions = append(result.Regions, r)
	}

	// If the listing returned successfully but produced zero VMs
	// AND the walked tally is also zero, walked counts that the
	// inner loop tracked are zero — preserve the Regions tally
	// driven by the location filter only. (walked is currently
	// unused beyond the dead-air guard above; keep the variable
	// for symmetry with the GCP scanner's walked-zones pattern.)
	_ = walked

	// Slice 1 instrumented rule (Compute): HasOTel == true.
	for _, c := range result.Compute {
		if c.HasOTel {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}

	// Slice 2 (chunk 3): walk the Azure SQL surface (servers +
	// databases + Diagnostic Settings) using the same OAuth token.
	// Partial failures accumulate under the "azuresql" service id
	// and do NOT invalidate the compute results above. The slice 2
	// instrumented-count tally is owned by chunk 5 (handler /
	// proposer wiring); this chunk only emits raw snapshot rows.
	s.scanAzureSQL(ctx, token, &result)

	// Kubernetes-tier-slice-2 (chunk 3): walk the AKS managed-
	// clusters surface using the same OAuth token. Partial failures
	// accumulate under the "aks" service id and do NOT invalidate
	// the compute / SQL results above. The slice-2 kubernetes
	// instrumented-count tally is owned by chunk 5 (handler /
	// proposer wiring); this chunk only emits raw ClusterSnapshot
	// rows with the three-way disjunction detection result.
	s.scanAKS(ctx, token, &result)

	// Serverless-tier-slice-1 (chunk 3, v0.89.91, #723 Stream 121):
	// walk the Microsoft.Web/sites Function Apps surface using the
	// same OAuth token. Partial failures accumulate under the
	// "azfunc" service id and do NOT invalidate the compute / SQL
	// / AKS results above. The serverless-tier instrumented-count
	// tally is owned by chunk 5 (handler / proposer wiring); this
	// chunk only emits raw ServerlessInstanceSnapshot rows with
	// the two-axis (HasTraceAxis + HasOTelDistro) detection result.
	s.scanAzureFunctions(ctx, token, &result)

	// Coverage-parity arc slice 3 — object-store (Storage Accounts) +
	// load-balancer tiers, same OAuth token, partial-failure isolated
	// under azurestorage / azurelb. Emits rows like the SQL/AKS/
	// serverless walks above.
	s.scanAzureStorage(ctx, token, &result)
	s.scanAzureLoadBalancers(ctx, token, &result)

	return result, nil
}

// acquireAccessToken runs the OAuth2 client_credentials grant against
// the configured tenant. Returns the bearer access token string on
// success or a wrapped error naming the failure shape (without ever
// echoing the client_secret bytes).
func (s *Scanner) acquireAccessToken(ctx context.Context) (string, error) {
	tokenEndpoint := s.tokenEndpoint
	if tokenEndpoint == "" {
		tokenEndpoint = loginMicrosoftEndpoint
	}
	tokenURL := fmt.Sprintf("%s/%s/oauth2/v2.0/token", strings.TrimRight(tokenEndpoint, "/"), s.TenantID)

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", s.ClientID)
	form.Set("client_secret", string(s.ClientSecret))
	form.Set("scope", armScope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		// http.NewRequest only fails on malformed URLs or bad
		// methods — neither is reachable with the inputs above —
		// but tag the branch defensively for completeness.
		return "", fmt.Errorf("token endpoint request build: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client().Do(req)
	if err != nil {
		// Network error reaching login.microsoftonline.com.
		return "", fmt.Errorf("network error: %s", truncate(err.Error(), 200))
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	if resp.StatusCode == http.StatusOK {
		var tok armTokenResponse
		if jerr := json.Unmarshal(body, &tok); jerr != nil {
			return "", fmt.Errorf("token endpoint returned 200 but body did not parse: %s", truncate(jerr.Error(), 200))
		}
		if tok.AccessToken == "" {
			return "", errors.New("token endpoint returned 200 but access_token was empty")
		}
		return tok.AccessToken, nil
	}

	// Non-200 — map into a humanized failure shape. The secret is
	// NEVER echoed; we identify the failure by the error code
	// Azure returns ("invalid_client", "unauthorized_client",
	// "invalid_request", etc.) plus the HTTP status.
	var terr armTokenError
	_ = json.Unmarshal(body, &terr)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "", fmt.Errorf("token endpoint auth failed (tenant_id or client credentials wrong)")
	case resp.StatusCode == http.StatusBadRequest && terr.Error != "":
		return "", fmt.Errorf("token endpoint rejected request (%s): %s", terr.Error, truncate(terr.ErrorDescription, 200))
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("token endpoint not found (verify tenant_id)")
	default:
		return "", fmt.Errorf("token endpoint failed (HTTP %d)", resp.StatusCode)
	}
}

// listVirtualMachines walks the subscription-scope VM listing
// endpoint, following nextLink pagination, and returns the
// accumulated VMs. The second return value is the number of pages
// walked (informational; mirrors the GCP scanner's walked-zones
// pattern for cross-scanner symmetry). Errors are returned with the
// HTTP status code embedded as a sentinel so the caller's
// classifyARMError helper can dispatch on it.
func (s *Scanner) listVirtualMachines(ctx context.Context, accessToken string) ([]armVirtualMachine, int, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	// First page URL: subscription-scope VM list.
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.Compute/virtualMachines?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		armVMListAPIVersion,
	)

	var out []armVirtualMachine
	pages := 0
	for pageURL != "" {
		pages++
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, pages, &armCallError{Wrapped: err}
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")

		resp, err := s.client().Do(req)
		if err != nil {
			return nil, pages, &armCallError{Wrapped: err, IsNetwork: true}
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			var aerr armErrorResponse
			_ = json.Unmarshal(body, &aerr)
			return nil, pages, &armCallError{
				StatusCode: resp.StatusCode,
				Code:       aerr.Error.Code,
				Message:    aerr.Error.Message,
				BodyHint:   truncate(string(body), 200),
				RetryAfter: resp.Header.Get("Retry-After"),
			}
		}

		var page armVMListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, pages, &armCallError{Wrapped: fmt.Errorf("response parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, pages, nil
}

// client returns the configured http.Client or http.DefaultClient
// when nil. Centralizes the nil-check so neither the token nor the
// ARM call path duplicates it.
func (s *Scanner) client() *http.Client {
	if s.httpClient != nil {
		return s.httpClient
	}
	return http.DefaultClient
}

// projectVM maps an armVirtualMachine into the provider-agnostic
// ComputeInstanceSnapshot. The mapping is the slice-1 contract:
//
//   - ResourceID: vm.Name (operator-readable, stable per subscription
//     per design doc §9).
//   - InstanceType: vm.Properties.HardwareProfile.VMSize (e.g.
//     "Standard_D4s_v3").
//   - Tags: defensive copy of vm.Tags (Azure tag map is string→string,
//     unlike GCP's labels-vs-network-tags split).
//   - HasOTel: true iff any tag key starts with "otel"
//     case-insensitive. Matches the AWS EC2 / GCP GCE slice-1
//     single-axis rule.
//   - OSFamily: normalized from
//     vm.Properties.StorageProfile.OsDisk.OSType ("Linux" / "Windows"
//     / missing) into "linux" / "windows" / "unknown". Azure exposes
//     this cleanly in the same response — design doc §9 calls out
//     that AWS and GCP slice 1 leave OSFamily="unknown" but Azure
//     does not.
//   - Region: vm.Location.
func projectVM(vm armVirtualMachine) scanner.ComputeInstanceSnapshot {
	return scanner.ComputeInstanceSnapshot{
		ResourceID:   vm.Name,
		ImportID:     vm.ID, // full ARM resource ID = azurerm_*_virtual_machine import id
		InstanceType: vm.Properties.HardwareProfile.VMSize,
		Tags:         copyTags(vm.Tags),
		HasOTel:      hasOTelTag(vm.Tags),
		OSFamily:     normalizeOSType(vm.Properties.StorageProfile.OSDisk.OSType),
		Region:       vm.Location,
	}
}

// hasOTelTag returns true if any tag key starts with the otel prefix
// case-insensitively. The case-insensitive rule mirrors the AWS EC2
// and GCP GCE scanners' slice-1 implementations.
func hasOTelTag(tags map[string]string) bool {
	for k := range tags {
		if strings.HasPrefix(strings.ToLower(k), OTelTagPrefix) {
			return true
		}
	}
	return false
}

// normalizeOSType maps the Azure osType enum into the
// ComputeInstanceSnapshot OSFamily field. Azure returns "Linux" or
// "Windows" (capitalized); slice 1 normalizes to lowercase. Missing
// or unrecognized values map to "unknown" so the proposer can branch
// safely without nil checks.
func normalizeOSType(s string) string {
	switch strings.ToLower(s) {
	case "linux":
		return "linux"
	case "windows":
		return "windows"
	default:
		return "unknown"
	}
}

// copyTags returns a defensive copy of the Azure tag map so the
// snapshot doesn't share backing memory with the API response
// (paginated walks may reuse buffers). Returns nil for an empty
// input so the Tags field stays omit-empty-friendly. Mirrors the
// GCP scanner's copyLabels helper.
func copyTags(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		out[k] = v
	}
	return out
}

// armCallError is the internal sentinel the listVirtualMachines path
// returns when the ARM call fails. It carries the HTTP status code,
// the Azure error code (parsed from the JSON body), the body hint,
// and the optional wrapped network/transport error so classifyARMError
// can dispatch on the right field.
type armCallError struct {
	StatusCode int
	Code       string
	Message    string
	BodyHint   string
	RetryAfter string
	Wrapped    error
	IsNetwork  bool
}

func (e *armCallError) Error() string {
	if e == nil {
		return ""
	}
	if e.IsNetwork && e.Wrapped != nil {
		return fmt.Sprintf("network error: %s", truncate(e.Wrapped.Error(), 200))
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("ARM call failed (HTTP %d, code=%s)", e.StatusCode, e.Code)
	}
	if e.Wrapped != nil {
		return e.Wrapped.Error()
	}
	return "ARM call failed"
}

// recordPartialFailure marks the scan partial and appends both a
// service identifier to FailedServices AND a human-readable reason to
// PartialReason. Mirrors the AWS / GCP scanners' recordPartialFailure
// helper so the shared audit / proposer-side consumers see identical
// structure across providers. The accumulator joins multiple
// failures with "; "; single-failure scans are unchanged.
func recordPartialFailure(result *scanner.Result, service, reason string) {
	result.Partial = true
	if result.PartialReason == "" {
		result.PartialReason = reason
	} else {
		result.PartialReason = result.PartialReason + "; " + reason
	}
	result.FailedServices = append(result.FailedServices, service)
}

// classifyARMError maps a Virtual Machines list failure into the
// operator-visible PartialReason string. The string is the audit
// payload's human-readable diagnostic; the structured FailedServices
// field carries the per-service identifier separately (see
// recordPartialFailure).
//
// Error mappings (per docs/proposals/azure-discovery-slice1.md §7.1
// and the chunk-2 brief):
//
//   - HTTP 403 + body code "AuthorizationFailed" -> permission_denied
//     with remediation hint pointing at the SP's Reader role.
//   - HTTP 404 -> subscription_not_found with remediation hint
//     pointing at subscription_id.
//   - HTTP 401 -> credentials_invalid (the token was accepted at the
//     OAuth layer but ARM rejected it — typically a stale token; the
//     wizard's validate path treats this the same as token-failure).
//   - HTTP 429 OR Retry-After header present -> rate_limit.
//   - Transport / network errors -> network-error with the underlying
//     err.Error() truncated to keep audit payloads bounded.
//   - Any other 4xx/5xx -> truncated message under the azurevm
//     identifier.
func classifyARMError(err error) string {
	if err == nil {
		return ""
	}
	var ace *armCallError
	if errors.As(err, &ace) {
		if ace.IsNetwork {
			wrapped := ""
			if ace.Wrapped != nil {
				wrapped = ace.Wrapped.Error()
			}
			return fmt.Sprintf("%s: network error: %s", ServiceIDVirtualMachines, truncate(wrapped, 200))
		}
		// Rate-limit signal: 429 OR a Retry-After header on any
		// status. ARM occasionally returns 503 + Retry-After under
		// throttling; treat both as the same operator-facing shape.
		if ace.StatusCode == http.StatusTooManyRequests || ace.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDVirtualMachines)
		}
		switch ace.StatusCode {
		case http.StatusForbidden:
			// AuthorizationFailed is the canonical Azure ARM code
			// for "the SP doesn't have access to this resource".
			// Other 403 codes exist (DisallowedOperation,
			// SubscriptionDisabled) — surface them under the same
			// permission_denied bucket since the operator
			// remediation (re-check the SP's role assignment) is
			// the same.
			return fmt.Sprintf("%s: permission denied (verify the service principal has Reader role on the subscription)", ServiceIDVirtualMachines)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: subscription not found (verify subscription_id is correct)", ServiceIDVirtualMachines)
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check tenant_id, client_id, client_secret)", ServiceIDVirtualMachines)
		default:
			msg := ace.Message
			if msg == "" {
				msg = ace.BodyHint
			}
			return fmt.Sprintf("%s: VM list failed (HTTP %d): %s", ServiceIDVirtualMachines, ace.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDVirtualMachines, truncate(err.Error(), 200))
}

// truncate caps a string at n bytes, appending an ellipsis when the
// cap fires. Used to keep audit payloads bounded — a misconfigured
// API endpoint can return a multi-kilobyte error body that bloats
// the audit row otherwise. Mirrors the GCP scanner's truncate helper.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
