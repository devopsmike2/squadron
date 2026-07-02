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

// scanAzureSQL walks the subscription's Microsoft.Sql surface and
// appends DatabaseInstanceSnapshot entries to result.Databases. The
// walk is three sequential REST calls per scan:
//
//  1. List SQL Servers in the subscription.
//  2. For each server, list its databases (skipping the system
//     `master` database — there is no operator-controllable
//     observability surface there).
//  3. For each database, GET microsoft.insights/diagnosticSettings
//     and flip SQLInsightsDiagEnabled when any setting routes the
//     SQLInsights category with enabled=true to ANY destination.
//
// Partial-failure semantics mirror the slice 1 VM walk: a 4xx/5xx
// on any of the three calls records a partial failure under the
// "azuresql" service id and the walk returns — compute results
// from the upstream VM walk stay valid. The one exception is the
// per-database Diagnostic Settings GET returning 404 (Azure's
// "NoSettingsConfigured" shape): that is the normal "no diagnostic
// settings on this database" response, NOT a permission or
// reachability failure, and the projection just leaves
// SQLInsightsDiagEnabled=false. An empty SQL server list (the
// operator has no SQL Servers) is also a perfectly valid response
// — the walk appends zero database snapshots without recording a
// partial failure.
//
// The accessToken parameter is the same OAuth2 bearer the VM walk
// acquired (Azure Reader at the subscription scope covers both
// Microsoft.Compute and Microsoft.Sql reads, so the slice 2 path
// does not re-issue a token).
func (s *Scanner) scanAzureSQL(ctx context.Context, accessToken string, result *scanner.Result) {
	servers, listErr := s.listSQLServers(ctx, accessToken)
	if listErr != nil {
		recordPartialFailure(result, ServiceIDAzureSQL, classifyAzureSQLError(listErr))
		return
	}
	// Zero servers is a valid response — operator has no SQL surface
	// in this subscription. No partial failure; no databases
	// appended. Symmetric with the slice-1 VM walk returning zero
	// VMs without flagging the scan as partial.
	if len(servers) == 0 {
		return
	}

	for _, server := range servers {
		rg := parseRGFromARMID(server.ID)
		if rg == "" {
			// Defensive: a server whose ARM id doesn't carry a
			// resource group means the list response was malformed
			// in a way that would cause the per-server database
			// list URL to be unbuildable. Surface as a partial
			// failure rather than silently dropping the server —
			// the operator should see that something is wrong.
			recordPartialFailure(result, ServiceIDAzureSQL, fmt.Sprintf("%s: server %s missing resource group in ARM id", ServiceIDAzureSQL, server.Name))
			continue
		}
		dbs, listDBErr := s.listSQLDatabases(ctx, accessToken, rg, server.Name)
		if listDBErr != nil {
			recordPartialFailure(result, ServiceIDAzureSQL, classifyAzureSQLError(listDBErr))
			// One server's database listing failing does not
			// necessarily mean the rest of the subscription is
			// inaccessible; continue with the remaining servers so
			// the operator sees whatever inventory the SP can read.
			continue
		}
		for _, db := range dbs {
			if strings.EqualFold(db.Name, sqlMasterDatabase) {
				continue
			}
			hasInsights, diagErr := s.probeSQLInsightsEnabled(ctx, accessToken, db.ID)
			if diagErr != nil {
				recordPartialFailure(result, ServiceIDAzureSQL, classifyAzureSQLError(diagErr))
				// Mid-scan diagnostic-settings call failure: append
				// the snapshot with SQLInsightsDiagEnabled=false so
				// the inventory row is still surfaced (the operator
				// can fix the policy / retry). Squadron's invariant
				// is to err toward visibility on partial failures
				// rather than hide rows.
				result.Databases = append(result.Databases, projectSQLDatabase(server, db, false))
				continue
			}
			result.Databases = append(result.Databases, projectSQLDatabase(server, db, hasInsights))
		}
	}
}

// listSQLServers walks the subscription-scope Microsoft.Sql/servers
// list endpoint, following nextLink pagination, and returns the
// accumulated servers. Errors are returned as *armCallError so the
// caller can dispatch on StatusCode / IsNetwork in
// classifyAzureSQLError.
func (s *Scanner) listSQLServers(ctx context.Context, accessToken string) ([]armSQLServer, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.Sql/servers?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		armSQLAPIVersion,
	)

	var out []armSQLServer
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armSQLServerListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("sql server list parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, nil
}

// listSQLDatabases walks the per-server databases list endpoint,
// following nextLink pagination. The endpoint URL is constructed
// from the subscription, the resource group extracted from the
// server's ARM id, and the server name.
func (s *Scanner) listSQLDatabases(ctx context.Context, accessToken, resourceGroup, serverName string) ([]armSQLDatabase, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Sql/servers/%s/databases?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		resourceGroup,
		serverName,
		armSQLAPIVersion,
	)

	var out []armSQLDatabase
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armSQLDatabaseListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("sql database list parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, nil
}

// probeSQLInsightsEnabled issues a GET against the database's
// microsoft.insights/diagnosticSettings sub-resource and returns
// true iff any returned setting routes the SQLInsights log
// category with Enabled=true. A 404 response is treated as "no
// settings configured" (returns false, no error) — that is the
// normal Azure shape for a database without any Diagnostic
// Settings, NOT a permission or reachability failure.
//
// The databaseARMID is the full ARM resource id of the database
// (e.g. /subscriptions/<sub>/resourceGroups/<rg>/providers/
// Microsoft.Sql/servers/<server>/databases/<db>). The scanner
// composes the diagnostic settings URL by prepending the ARM
// endpoint root and appending the diagnostic settings sub-path.
func (s *Scanner) probeSQLInsightsEnabled(ctx context.Context, accessToken, databaseARMID string) (bool, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	// The ARM id from the database list response starts with a
	// leading slash; the diagnostic settings sub-path is appended
	// directly. Strip any accidental leading slash duplication.
	resourceID := strings.TrimPrefix(databaseARMID, "/")
	diagURL := fmt.Sprintf(
		"%s/%s/providers/microsoft.insights/diagnosticSettings?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		resourceID,
		armDiagSettingsAPIVersion,
	)

	body, callErr := s.doARMGet(ctx, accessToken, diagURL)
	if callErr != nil {
		var ace *armCallError
		if errors.As(callErr, &ace) && ace.StatusCode == http.StatusNotFound {
			// 404 on the diagnostic settings GET means "this
			// database has no Diagnostic Settings configured" —
			// the canonical Azure response shape. Not a failure;
			// just the absence of the signal the rule keys on.
			return false, nil
		}
		return false, callErr
	}

	var resp armDiagnosticSettingsResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return false, &armCallError{Wrapped: fmt.Errorf("diagnostic settings parse: %w", jerr)}
	}
	for _, ds := range resp.Value {
		for _, log := range ds.Properties.Logs {
			if log.Category == sqlInsightsCategory && log.Enabled {
				return true, nil
			}
		}
	}
	return false, nil
}

// doARMGet performs a single GET against the ARM API with the
// supplied bearer token, returning the response body bytes on 200
// or an *armCallError on any non-200 / transport failure. Mirrors
// the per-page logic the VM list walker uses, factored out so the
// three SQL endpoints don't each duplicate the boilerplate.
func (s *Scanner) doARMGet(ctx context.Context, accessToken, fullURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, &armCallError{Wrapped: err}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, &armCallError{Wrapped: err, IsNetwork: true}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

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

// projectSQLDatabase maps a (server, database, hasInsights) triple
// into the provider-agnostic DatabaseInstanceSnapshot. Mapping
// decisions per the slice-2 design doc §3.2 / §4 and the chunk-3
// brief:
//
//   - ResourceID: "<server>/<database>". The Azure SQL surface
//     does not have a single user-friendly identifier — the ARM id
//     is operator-hostile (long, slash-laden), the database name
//     alone is non-unique across servers in the same subscription,
//     so the slice-2 contract is server-qualified naming.
//   - Engine: hardcoded "sqlserver". Azure SQL Database is a
//     SQL Server-engine service; unlike GCP Cloud SQL, the engine
//     does not vary per database.
//   - EngineVersion: properties.currentServiceObjectiveName. This
//     is the most diagnostic version-like signal Azure exposes
//     per-database — it reflects scaling state (e.g.
//     "GP_S_Gen5_2") which sku.name might lag when auto-scaling.
//     Falls back to sku.name when the property is empty (the
//     fallback ensures the proposer always has SOME version signal
//     rather than projecting empty strings on freshly-created
//     databases where currentServiceObjectiveName has not yet been
//     populated).
//   - InstanceClass: sku.name. The operator-readable SKU shorthand.
//   - Region: database.Location. Falls back to server.Location
//     when the database's own Location is empty — Azure occasionally
//     elides location at the database scope when the database is
//     co-located with its server (the common case).
//   - Tags: defensive copy of database.Tags (Azure tag map is
//     string→string).
//   - Provider: "azure" (drives the proposer's recommendation-kind
//     dispatch — see DatabaseInstanceSnapshot.Provider godoc on
//     the empty=AWS backward-compat default).
//   - SQLInsightsDiagEnabled: the supplied hasInsights bool — the
//     §3.2 detection rule's result.
func projectSQLDatabase(server armSQLServer, db armSQLDatabase, hasInsights bool) scanner.DatabaseInstanceSnapshot {
	engineVersion := db.Properties.CurrentServiceObjectiveName
	if engineVersion == "" {
		engineVersion = db.Sku.Name
	}
	region := db.Location
	if region == "" {
		region = server.Location
	}
	return scanner.DatabaseInstanceSnapshot{
		ResourceID: fmt.Sprintf("%s/%s", server.Name, db.Name),
		// db.ID is the full ARM id (.../Microsoft.Sql/servers/<s>/databases/<db>),
		// which is exactly the azurerm_mssql_database terraform import id.
		ImportID:               db.ID,
		Engine:                 azureSQLEngine,
		EngineVersion:          engineVersion,
		InstanceClass:          db.Sku.Name,
		Region:                 region,
		Tags:                   copyTags(db.Tags),
		Provider:               azureProviderID,
		SQLInsightsDiagEnabled: hasInsights,
	}
}

// parseRGFromARMID extracts the resource group name from an Azure
// Resource Manager resource id. The ARM id format is:
//
//	/subscriptions/<sub>/resourceGroups/<rg>/providers/<provider>/<type>/<name>[/...]
//
// Case-insensitive on the "resourceGroups" segment because Azure's
// documentation uses camelCase but its responses occasionally
// surface variants (resourcegroups, RESOURCEGROUPS) under certain
// API versions. Returns the empty string when the id does not
// contain a resourceGroups segment — the caller treats this as a
// malformed id and records a partial failure rather than emitting a
// half-constructed URL against the database list endpoint.
func parseRGFromARMID(id string) string {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if strings.EqualFold(parts[i], "resourceGroups") {
			return parts[i+1]
		}
	}
	return ""
}

// classifyAzureSQLError maps a Microsoft.Sql / diagnosticSettings
// walk failure into the operator-visible PartialReason string under
// the azuresql service identifier. Mirrors the slice-1
// classifyARMError shape (network / rate-limit / permission_denied /
// subscription_not_found / credentials / generic-tail) so the
// proposer-side consumer sees identical structure across services
// in the same scan.
//
// Error mappings:
//
//   - Transport / network → "azuresql: network error: <err>".
//   - HTTP 429 OR Retry-After header on any status → rate_limit.
//   - HTTP 403 → permission_denied (Azure ARM SQL/Diagnostic
//     Settings 403s are surfaced under the same operator
//     remediation as the VM walk: re-check the Reader role).
//   - HTTP 404 → subscription_not_found. Rare on the slice-2 path
//     since the upstream VM walk would already have errored, but
//     defended for symmetry. The per-database Diagnostic Settings
//     404 is filtered out by probeSQLInsightsEnabled BEFORE it
//     reaches this classifier — the canonical 404 there is "no
//     settings configured", which is not a failure.
//   - HTTP 401 → credentials_invalid (the OAuth-acquired token was
//     rejected by ARM — typically a stale token; the wizard's
//     validate path treats this the same as token-failure).
//   - Any other 4xx/5xx → truncated message under the azuresql
//     identifier.
func classifyAzureSQLError(err error) string {
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
			return fmt.Sprintf("%s: network error: %s", ServiceIDAzureSQL, truncate(wrapped, 200))
		}
		if ace.StatusCode == http.StatusTooManyRequests || ace.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDAzureSQL)
		}
		switch ace.StatusCode {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service principal has Reader role on the subscription)", ServiceIDAzureSQL)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: subscription not found (verify subscription_id is correct)", ServiceIDAzureSQL)
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check tenant_id, client_id, client_secret)", ServiceIDAzureSQL)
		default:
			msg := ace.Message
			if msg == "" {
				msg = ace.BodyHint
			}
			return fmt.Sprintf("%s: SQL walk failed (HTTP %d): %s", ServiceIDAzureSQL, ace.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDAzureSQL, truncate(err.Error(), 200))
}
