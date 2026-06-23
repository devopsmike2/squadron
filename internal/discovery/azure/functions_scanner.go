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
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// AppInsightsConnectionStringAppSetting is the standard Azure-defined
// app_setting key for an Application Insights connection string.
// Presence on a Function App's app settings flips
// ServerlessInstanceSnapshot.HasTraceAxis to true per
// docs/proposals/serverless-tier-slice1.md §3.4 — Application Insights
// is the cloud-native trace primitive for Azure Functions, paralleling
// X-Ray active tracing on AWS Lambda.
//
// The detection rule is presence-based: a non-empty value flips the
// axis. Empty-string values are treated as "not set" — operators
// occasionally blank the value to disable the integration without
// removing the key entirely, and the slice 1 contract reports based
// on a workable connection-string presence, not just key existence.
const AppInsightsConnectionStringAppSetting = "APPLICATIONINSIGHTS_CONNECTION_STRING"

// OTelDotNetAutoHomeAppSetting and OTelPythonDistroAppSetting are the
// two app_setting keys signaling that an OpenTelemetry distribution
// has been attached to the Function App. Either present (with a
// non-empty value) flips ServerlessInstanceSnapshot.HasOTelDistro to
// true.
//
// The two-key disjunction mirrors AWS Lambda's "ADOT layer ARN OR
// AWS_LAMBDA_EXEC_WRAPPER env var" pattern — Azure operators on
// either the .NET or Python distro family get credit without forcing
// them to adopt both. Future runtime distros (Java auto-instrumentation,
// Node.js distro) can extend this list without breaking the wire shape.
const (
	OTelDotNetAutoHomeAppSetting = "OTEL_DOTNET_AUTO_HOME"
	OTelPythonDistroAppSetting   = "OTEL_PYTHON_DISTRO"
)

// FunctionsWorkerRuntimeAppSetting is the canonical Azure Functions
// app_setting key carrying the worker runtime identifier
// ("python" / "node" / "dotnet" / "dotnet-isolated" / "java" /
// "powershell"). Used as a Windows-side fallback when neither
// LinuxFxVersion nor WindowsFxVersion is populated — older Function
// Apps and Consumption-plan Windows sites can elide both Fx version
// fields, leaving FUNCTIONS_WORKER_RUNTIME as the only runtime
// signal. Slice 1 surfaces the bare worker identifier when this
// fallback fires; slice 2 can pull in FUNCTIONS_EXTENSION_VERSION
// for a sharper signal if operators request it.
const FunctionsWorkerRuntimeAppSetting = "FUNCTIONS_WORKER_RUNTIME"

// azureFunctionsServerlessSurface is the Surface discriminator string
// for Azure Functions snapshots. Drives the proposer's
// recommendation-kind prefix routing (azfunc-* → Azure) per
// docs/proposals/serverless-tier-slice1.md §10.
const azureFunctionsServerlessSurface = "azfunc"

// ServiceIDAzureFunctions is the serverless-tier slice 1 chunk 3
// service identifier the scanner reports against
// Result.FailedServices when the Azure Functions walk produces a
// non-fatal error. Parallel to the "azurevm" / "azuresql" / "aks"
// identifiers — the connection model carries the provider
// discriminator separately so the per-service id stays unprefixed.
//
// See docs/proposals/serverless-tier-slice1.md §3.4 ("Service
// identifier for partial failures: azfunc").
const ServiceIDAzureFunctions = "azfunc"

// armWebAppListAPIVersion pins the Microsoft.Web/sites
// list-by-subscription API version. 2023-12-01 returns the site shape
// fields chunk 3 needs — top-level kind discriminator, siteConfig with
// linuxFxVersion / windowsFxVersion, and the list_application_settings
// POST sub-resource works at stable JSON paths.
const armWebAppListAPIVersion = "2023-12-01"

// functionAppKindPrefix is the Microsoft.Web/sites kind discriminator
// prefix that identifies a Function App (as distinct from a regular
// Web App). Azure stores the kind as one of:
//
//   - "functionapp"           (Windows-hosted, no plan modifier)
//   - "functionapp,linux"     (Linux-hosted)
//   - "functionapp,workflowapp" (Logic Apps standard hosted on
//     Functions runtime)
//   - "functionapp,linux,container" (Linux container deployment)
//
// All begin with "functionapp"; the prefix match keeps the rule
// resilient to additional comma-suffixed flavors Azure publishes
// later. Web Apps ("app", "app,linux") are filtered out — they are
// not in scope for the serverless tier (they fall under the existing
// compute tier conceptually, though slice 1 does NOT extend
// ComputeInstanceSnapshot to cover them).
const functionAppKindPrefix = "functionapp"

// scanAzureFunctions walks the subscription's Microsoft.Web/sites
// surface, filters for Function Apps (kind starting with
// "functionapp"), and for each one issues a POST against the
// list_application_settings sub-resource to pull the app_settings
// the two slice 1 detection axes key on. Appends
// ServerlessInstanceSnapshot entries to result.Serverless.
//
// Partial-failure semantics mirror the slice 1 VM walk + slice 2
// SQL/AKS walks: a 4xx/5xx on the subscription-wide list call
// records a partial failure under the "azfunc" service id and the
// walk returns. A per-site application-settings call failing
// records a partial failure but does not block the rest of the
// walk — the snapshot for that site is appended with both axes
// false so the inventory row is still surfaced. An empty Function
// App list (operator has no Azure Functions surface) is a valid
// response — no partial failure, no snapshots appended.
//
// The accessToken parameter is the same OAuth2 bearer the VM walk
// acquired. Azure Reader at the subscription scope already covers
// Microsoft.Web reads + the list_application_settings sub-resource
// action; the serverless-tier slice 1 chunk 3 path does not
// re-issue a token. See docs/proposals/serverless-tier-slice1.md
// §3.4 (required Azure RBAC: existing Reader role suffices).
//
// Detection per docs/proposals/serverless-tier-slice1.md §3.4:
//
//   - HasTraceAxis  ← app_settings[APPLICATIONINSIGHTS_CONNECTION_STRING]
//     is set to a non-empty value.
//   - HasOTelDistro ← app_settings[OTEL_DOTNET_AUTO_HOME] OR
//     app_settings[OTEL_PYTHON_DISTRO] is set to a non-empty value.
func (s *Scanner) scanAzureFunctions(ctx context.Context, accessToken string, result *scanner.Result) {
	sites, listErr := s.listFunctionApps(ctx, accessToken)
	if listErr != nil {
		recordPartialFailure(result, ServiceIDAzureFunctions, classifyAzureFunctionsError(listErr))
		return
	}
	if len(sites) == 0 {
		// Zero Function Apps is a valid response — operator has no
		// serverless surface in this subscription. No partial failure;
		// no snapshots appended. Symmetric with the AKS/SQL empty
		// branches.
		return
	}
	for _, site := range sites {
		// Defensive: a site whose ARM id doesn't carry a resource
		// group cannot have its list_application_settings URL built.
		// Surface as a partial failure rather than silently dropping
		// — the operator should see something is wrong.
		rg := parseRGFromARMID(site.ID)
		if rg == "" {
			recordPartialFailure(result, ServiceIDAzureFunctions, fmt.Sprintf("%s: site %s missing resource group in ARM id", ServiceIDAzureFunctions, site.Name))
			continue
		}
		settings, settingsErr := s.listFunctionAppSettings(ctx, accessToken, rg, site.Name)
		if settingsErr != nil {
			recordPartialFailure(result, ServiceIDAzureFunctions, classifyAzureFunctionsError(settingsErr))
			// Append the snapshot with both axes false so the
			// inventory row is still surfaced. Squadron's invariant
			// is to err toward visibility on partial failures rather
			// than hide rows.
			result.Serverless = append(result.Serverless, projectFunctionAppNoSettings(site, result.AccountID))
			continue
		}
		result.Serverless = append(result.Serverless, projectFunctionApp(site, settings, result.AccountID))
	}
}

// listFunctionApps walks the subscription-scope Microsoft.Web/sites
// list endpoint, following nextLink pagination, filters down to
// Function Apps via the kind prefix, and returns the accumulated
// sites. Errors are returned as *armCallError so the caller can
// dispatch on StatusCode / IsNetwork in classifyAzureFunctionsError.
//
// Filtering is performed client-side rather than via a
// ?$filter=kind+eq+'functionapp' server-side query for two reasons.
// First, the kind value carries comma-separated flavors
// ("functionapp,linux", "functionapp,linux,container") that the
// equality filter would miss without enumerating every known
// suffix combination — the prefix match in code is more resilient.
// Second, the Microsoft.Web list endpoint's $filter support has
// historically been inconsistent across API versions; the
// client-side filter sidesteps that fragility.
func (s *Scanner) listFunctionApps(ctx context.Context, accessToken string) ([]armWebSite, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.Web/sites?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		armWebAppListAPIVersion,
	)

	var out []armWebSite
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armWebSiteListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("web sites list parse: %w", jerr)}
		}
		for _, site := range page.Value {
			if isFunctionApp(site) {
				out = append(out, site)
			}
		}
		pageURL = page.NextLink
	}
	return out, nil
}

// isFunctionApp reports whether a Microsoft.Web/sites entry is a
// Function App (as opposed to a regular Web App). The kind field
// carries one of "functionapp" / "functionapp,linux" /
// "functionapp,workflowapp" / "functionapp,linux,container" — all
// begin with the canonical prefix. Web Apps surface as "app" /
// "app,linux" and fail the prefix match.
//
// The check is case-sensitive because Azure publishes kind values
// in lowercase; defending against an upper-case future would invite
// false positives on unrelated kinds. Empty kind values fail the
// match too, mapping to the safe "not in scope" branch.
func isFunctionApp(site armWebSite) bool {
	return strings.HasPrefix(site.Kind, functionAppKindPrefix)
}

// listFunctionAppSettings issues a POST against the
// list_application_settings sub-resource of the Function App. The
// Microsoft.Web API requires POST (not GET) here because the
// response carries secrets in the spec — Squadron only reads
// presence and (for runtime fallback) the FUNCTIONS_WORKER_RUNTIME
// value; it never logs or persists the raw secret values.
//
// Errors are returned as *armCallError. A 404 is treated as a real
// failure here (unlike the SQL Diagnostic Settings 404 case) —
// every Function App has app_settings even when empty; a 404 means
// the URL is wrong or the site is in an unusual lifecycle state,
// not a "no settings configured" success.
func (s *Scanner) listFunctionAppSettings(ctx context.Context, accessToken, resourceGroup, siteName string) (map[string]string, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	settingsURL := fmt.Sprintf(
		"%s/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Web/sites/%s/config/appsettings/list?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		resourceGroup,
		siteName,
		armWebAppListAPIVersion,
	)
	body, callErr := s.doARMPost(ctx, accessToken, settingsURL)
	if callErr != nil {
		return nil, callErr
	}
	var resp armAppSettingsResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return nil, &armCallError{Wrapped: fmt.Errorf("app settings parse: %w", jerr)}
	}
	if resp.Properties == nil {
		return map[string]string{}, nil
	}
	return resp.Properties, nil
}

// doARMPost performs a single POST against the ARM API with the
// supplied bearer token, returning the response body bytes on 200
// or an *armCallError on any non-200 / transport failure. The
// list_application_settings sub-resource requires POST (with an
// empty body) per the Microsoft.Web API contract — this helper
// parallels doARMGet on the SQL scanner so the
// app-settings-specific call doesn't duplicate the boilerplate.
func (s *Scanner) doARMPost(ctx context.Context, accessToken, fullURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(""))
	if err != nil {
		return nil, &armCallError{Wrapped: err}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Length", "0")

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, &armCallError{Wrapped: err, IsNetwork: true}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		var aerr armErrorResponse
		_ = json.Unmarshal(body, &aerr)
		return nil, &armCallError{
			StatusCode: resp.StatusCode,
			Code:       aerr.Error.Code,
			Message:    aerr.Error.Message,
			BodyHint:   truncate(string(body), 200),
			RetryAfter: resp.Header.Get("Retry-After"),
		}
	}
	return body, nil
}

// projectFunctionApp maps a (site, app_settings) pair into a
// ServerlessInstanceSnapshot. Extracted as a standalone helper so
// the per-axis detection logic is independently testable — the
// slice 1 chunk 3 acceptance tests (7-8) hit this helper directly
// with fixture site values, asserting the HasTraceAxis /
// HasOTelDistro outcome without spinning up a full scanner.
//
// Mapping decisions per the slice 1 chunk 3 brief:
//
//   - Surface:      "azfunc" (drives proposer recommendation routing).
//   - Provider:     "azure".
//   - ResourceName: site.Name.
//   - ResourceARN:  site.ID (full ARM resource id — operator-hostile
//     but the canonical identifier the proposer's evidence list
//     and recommendation envelope's AffectedResources field
//     reference).
//   - Region:       site.Location.
//   - Runtime:      normalized from LinuxFxVersion / WindowsFxVersion
//     into "python3.11" / "node18" / "dotnet6.0" / etc. via
//     normalizeFunctionAppRuntime. Empty when no signal available.
//   - HasTraceAxis: APPLICATIONINSIGHTS_CONNECTION_STRING is present
//     with a non-empty value.
//   - HasOTelDistro: OTEL_DOTNET_AUTO_HOME OR OTEL_PYTHON_DISTRO is
//     present with a non-empty value.
//   - Detail:       slim view of the load-bearing app_setting keys
//     so the per-cloud Inventory tab can render which axis fired.
func projectFunctionApp(site armWebSite, settings map[string]string, accountID string) scanner.ServerlessInstanceSnapshot {
	snap := scanner.ServerlessInstanceSnapshot{
		Provider:     azureProviderID,
		Surface:      azureFunctionsServerlessSurface,
		AccountID:    accountID,
		Region:       site.Location,
		ResourceName: site.Name,
		ResourceARN:  site.ID,
	}

	linuxFx := ""
	windowsFx := ""
	if site.Properties != nil {
		if site.Properties.SiteConfig != nil {
			linuxFx = site.Properties.SiteConfig.LinuxFxVersion
			windowsFx = site.Properties.SiteConfig.WindowsFxVersion
		}
	}
	snap.Runtime = normalizeFunctionAppRuntime(linuxFx, windowsFx, settings[FunctionsWorkerRuntimeAppSetting])

	// Axis 1: Application Insights connection string. Presence with a
	// non-empty value flips HasTraceAxis. Empty-string values are
	// treated as "not set" — operators occasionally blank the value
	// to disable the integration without removing the key, and the
	// slice 1 contract reports on workable connection-string
	// presence, not just key existence.
	if v, ok := settings[AppInsightsConnectionStringAppSetting]; ok && v != "" {
		snap.HasTraceAxis = true
	}

	// Axis 2: OTel distro app_settings. Either key present (with a
	// non-empty value) flips HasOTelDistro. The disjunction mirrors
	// AWS Lambda's "ADOT layer OR exec wrapper" pattern — operators
	// on either runtime family get credit.
	if v, ok := settings[OTelDotNetAutoHomeAppSetting]; ok && v != "" {
		snap.HasOTelDistro = true
	}
	if v, ok := settings[OTelPythonDistroAppSetting]; ok && v != "" {
		snap.HasOTelDistro = true
	}

	snap.Detail = map[string]any{
		"kind":               site.Kind,
		"linux_fx_version":   linuxFx,
		"windows_fx_version": windowsFx,
		"has_app_insights":   snap.HasTraceAxis,
		"has_otel_dotnet":    settings[OTelDotNetAutoHomeAppSetting] != "",
		"has_otel_python":    settings[OTelPythonDistroAppSetting] != "",
	}
	return snap
}

// projectFunctionAppNoSettings is the helper the partial-failure
// branch uses when the list_application_settings call failed — the
// inventory row is still surfaced with both axes false so the
// operator sees the Function App exists and can fix the policy.
// Runtime is still populated from the site shape (LinuxFxVersion /
// WindowsFxVersion are on the list response itself; only the
// app_settings disjunction requires the failed sub-call).
func projectFunctionAppNoSettings(site armWebSite, accountID string) scanner.ServerlessInstanceSnapshot {
	snap := scanner.ServerlessInstanceSnapshot{
		Provider:     azureProviderID,
		Surface:      azureFunctionsServerlessSurface,
		AccountID:    accountID,
		Region:       site.Location,
		ResourceName: site.Name,
		ResourceARN:  site.ID,
	}
	linuxFx := ""
	windowsFx := ""
	if site.Properties != nil && site.Properties.SiteConfig != nil {
		linuxFx = site.Properties.SiteConfig.LinuxFxVersion
		windowsFx = site.Properties.SiteConfig.WindowsFxVersion
	}
	snap.Runtime = normalizeFunctionAppRuntime(linuxFx, windowsFx, "")
	snap.Detail = map[string]any{
		"kind":               site.Kind,
		"linux_fx_version":   linuxFx,
		"windows_fx_version": windowsFx,
		"settings_unread":    true,
	}
	return snap
}

// normalizeFunctionAppRuntime turns the Azure runtime signals into
// the friendly normalized form the Inventory tab + proposer surface
// ("python3.11" / "node18" / "dotnet6.0" / etc.). The three input
// signals, in order of preference:
//
//  1. LinuxFxVersion ("Python|3.11" / "Node|18" / "DotNet|6.0" /
//     "DOTNET-ISOLATED|6.0") — Linux Function Apps carry the most
//     diagnostic signal here.
//  2. WindowsFxVersion ("dotnet:6.0" / etc.) — Windows containers.
//     Sparse coverage in practice; Windows Function Apps usually
//     leave this blank and rely on FUNCTIONS_WORKER_RUNTIME instead.
//  3. FUNCTIONS_WORKER_RUNTIME ("python" / "node" / "dotnet" /
//     "dotnet-isolated" / "java") — the canonical Windows-side
//     fallback. Carries only the worker family, not the version,
//     so the output is the bare worker identifier without a
//     version suffix.
//
// The parser is intentionally forgiving — unrecognized shapes pass
// through as a lowercased best-effort string so the field is
// never blank when the operator actually has a runtime configured.
// Empty across all three inputs projects to empty so the Inventory
// tab can render "unknown" without ambiguity.
func normalizeFunctionAppRuntime(linuxFx, windowsFx, workerRuntime string) string {
	if r := normalizeFxVersion(linuxFx); r != "" {
		return r
	}
	if r := normalizeFxVersion(windowsFx); r != "" {
		return r
	}
	if workerRuntime != "" {
		return strings.ToLower(strings.TrimSpace(workerRuntime))
	}
	return ""
}

// normalizeFxVersion parses an Azure Fx version string. Linux uses
// "Family|Version" (e.g. "Python|3.11", "Node|18", "DotNet|6.0",
// "DOTNET-ISOLATED|6.0"); Windows occasionally uses "family:version"
// (e.g. "dotnet:6.0"). The family component is lowercased and
// normalized:
//
//   - "python"            → "python"
//   - "node"              → "node"
//   - "dotnet"            → "dotnet"
//   - "dotnet-isolated"   → "dotnet"  (the proposer treats both
//     hosting modes identically for instrumentation guidance)
//   - "dotnetcore"        → "dotnet"  (older naming)
//   - "java"              → "java"
//   - "powershell"        → "powershell"
//   - anything else       → lowercased verbatim
//
// The version component passes through verbatim (no semver
// normalization) — operators see the same version string they
// configured.
//
// Empty input returns empty. Inputs without a delimiter ("Python"
// alone, "Node" alone) project to the lowercased family with no
// version suffix — covers the rare lifecycle-edge case where the
// version segment hasn't been populated yet on a freshly-created
// site.
func normalizeFxVersion(fxVersion string) string {
	if fxVersion == "" {
		return ""
	}
	// Pick the delimiter — Linux uses "|", Windows occasionally
	// uses ":". The first delimiter wins; the rest of the string
	// is treated as version.
	delim := ""
	for _, d := range []string{"|", ":"} {
		if strings.Contains(fxVersion, d) {
			delim = d
			break
		}
	}
	family := fxVersion
	version := ""
	if delim != "" {
		parts := strings.SplitN(fxVersion, delim, 2)
		family = parts[0]
		if len(parts) == 2 {
			version = parts[1]
		}
	}
	normFamily := normalizeRuntimeFamily(family)
	if version == "" {
		return normFamily
	}
	return normFamily + version
}

// normalizeRuntimeFamily collapses the runtime family aliases into
// the canonical lowercased label the Inventory tab + proposer
// surface. See normalizeFxVersion godoc for the mapping table.
func normalizeRuntimeFamily(family string) string {
	switch strings.ToLower(strings.TrimSpace(family)) {
	case "python":
		return "python"
	case "node":
		return "node"
	case "dotnet", "dotnet-isolated", "dotnetcore":
		return "dotnet"
	case "java":
		return "java"
	case "powershell":
		return "powershell"
	default:
		return strings.ToLower(strings.TrimSpace(family))
	}
}

// classifyAzureFunctionsError maps a Microsoft.Web walk failure into
// the operator-visible PartialReason string under the "azfunc"
// service identifier. Mirrors classifyARMError / classifyAzureSQLError
// / classifyAKSError shapes so the proposer-side consumer sees
// identical structure across services in the same scan.
//
// Mappings: network → "network error"; 429 OR Retry-After →
// rate_limit; 403 → permission_denied; 404 → subscription_not_found;
// 401 → credentials_invalid; other 4xx/5xx → generic-tail.
func classifyAzureFunctionsError(err error) string {
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
			return fmt.Sprintf("%s: network error: %s", ServiceIDAzureFunctions, truncate(wrapped, 200))
		}
		if ace.StatusCode == http.StatusTooManyRequests || ace.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDAzureFunctions)
		}
		switch ace.StatusCode {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service principal has Reader role on the subscription)", ServiceIDAzureFunctions)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: subscription not found (verify subscription_id is correct)", ServiceIDAzureFunctions)
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check tenant_id, client_id, client_secret)", ServiceIDAzureFunctions)
		default:
			msg := ace.Message
			if msg == "" {
				msg = ace.BodyHint
			}
			return fmt.Sprintf("%s: Function Apps list failed (HTTP %d): %s", ServiceIDAzureFunctions, ace.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDAzureFunctions, truncate(err.Error(), 200))
}
