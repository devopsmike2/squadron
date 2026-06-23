// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Slice-2 (database-tier-slice2.md, v0.89.65) Cloud SQL extension.
//
// This file ships the Cloud SQL walk that the GCP scanner runs after
// the slice-1 Compute Engine walk completes. The walks share the same
// per-scan SA credential and the same partial-failure accumulator on
// the Result; Cloud SQL surfaces independently in Result.Databases
// with Provider="gcp" so the proposer routes its findings to the
// cloudsql-pi-enable recommendation kind (see proposal §3.1).
//
// Library choice mirrors the compute walk: google.golang.org/api/
// sqladmin/v1beta4 (the REST client). The httptest mock surface that
// already shape-tests the compute walk extends to the Cloud SQL
// path by adding /sql/v1beta4/projects/.../instances handling — see
// scanner_test.go::fakeGCP.handler.
//
// OAuth scope: the production credential path requests TWO specific
// scopes (compute.readonly + sqlservice.admin) via a single
// google.JWTConfigFromJSON call. We deliberately do NOT use
// cloud-platform (which would cover both) because slice 1's posture
// commitment is least-privilege: the SA's effective scope should be
// the union of the read-only scopes the scanner actually needs, not
// the platform-wide superset.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// walkCloudSQL lists Cloud SQL instances in the configured project,
// projects each into a DatabaseInstanceSnapshot, and appends the
// result to result.Databases. Errors are surfaced to the caller for
// recording as a partial-failure entry against the cloudsql service
// identifier — same pattern as the compute walk's per-zone error
// surfacing.
//
// Pagination: InstancesListCall.Pages walks every page; failures
// mid-walk surface as a single error (the Cloud SQL API doesn't
// split errors per-page in a way the slice 2 scanner needs to
// distinguish — a 429 on page 3 of 5 still means "stop and record
// partial," same as a 429 on page 1).
//
// Region filter: the scanner's s.Region field restricts the snapshot
// projection — instances whose region doesn't match are skipped. The
// Cloud SQL API does not support a server-side region filter on
// InstancesList, so the walk reads every instance in the project and
// the projection applies the filter client-side. This matches the
// per-zone filter pattern in the compute walk (where the zone list
// is filtered client-side after Zones.List returns everything).
func (s *Scanner) walkCloudSQL(ctx context.Context, client *sqladmin.Service, result *scanner.Result) error {
	call := client.Instances.List(s.ProjectID).Context(ctx)
	return call.Pages(ctx, func(resp *sqladmin.InstancesListResponse) error {
		for _, inst := range resp.Items {
			if inst == nil {
				continue
			}
			if s.Region != "" && inst.Region != s.Region {
				continue
			}
			result.Databases = append(result.Databases, projectCloudSQLInstance(inst))
		}
		return nil
	})
}

// projectCloudSQLInstance maps a sqladmin.DatabaseInstance into the
// provider-agnostic DatabaseInstanceSnapshot. The mapping is the
// slice-2 contract:
//
//   - ResourceID: instance.Name (operator-readable identifier; the
//     Cloud SQL list response uses Name as the per-project unique
//     handle, parallel to GCE instance.Name).
//   - Engine: normalizeEngine(inst.DatabaseVersion) — "POSTGRES_15"
//     → "postgres", "MYSQL_8_0" → "mysql", "SQLSERVER_2019_STANDARD"
//     → "sqlserver".
//   - EngineVersion: extractVersion(inst.DatabaseVersion) — the
//     trailing version portion (e.g. "15", "8_0", "2019_STANDARD").
//   - InstanceClass: inst.Settings.Tier (raw "db-custom-2-7680" /
//     "db-n1-standard-2" string — the proposer normalizes when
//     reasoning about cost).
//   - Region: inst.Region.
//   - Tags: inst.Settings.UserLabels — defensively copied.
//   - Provider: "gcp" — the discriminator the proposer reads to
//     route to cloudsql-pi-enable.
//   - QueryInsightsEnabled: the
//     settings.insightsConfig.queryInsightsEnabled boolean. When
//     InsightsConfig is nil entirely, treat as false (the design
//     doc §3.1 detection rule pins this).
func projectCloudSQLInstance(inst *sqladmin.DatabaseInstance) scanner.DatabaseInstanceSnapshot {
	snap := scanner.DatabaseInstanceSnapshot{
		ResourceID:    inst.Name,
		Engine:        normalizeEngine(inst.DatabaseVersion),
		EngineVersion: extractVersion(inst.DatabaseVersion),
		Region:        inst.Region,
		Provider:      ProviderGCP,
	}
	if inst.Settings != nil {
		snap.InstanceClass = inst.Settings.Tier
		snap.Tags = copyLabels(inst.Settings.UserLabels)
		if inst.Settings.InsightsConfig != nil {
			snap.QueryInsightsEnabled = inst.Settings.InsightsConfig.QueryInsightsEnabled
		}
		// Missing insightsConfig => QueryInsightsEnabled stays false
		// (zero value). The design doc §3.1 detection rule pins this
		// behavior explicitly so the proposer's negative-case branch
		// fires on absence, not just on an explicit false.
	}
	return snap
}

// normalizeEngine maps the Cloud SQL DatabaseVersion enum to the
// provider-agnostic Engine string the proposer keys off. The Cloud
// SQL enum names start with a family prefix ("POSTGRES_*", "MYSQL_*",
// "SQLSERVER_*") that maps cleanly onto the AWS RDS engine vocabulary
// ("postgres", "mysql", "sqlserver"). Unknown family prefixes fall
// through to lowercase passthrough — defensive against new families
// (the Cloud SQL surface has added MySQL 8.4, MySQL 9.7, and other
// variants recently; the scanner should not crash on a string it
// doesn't recognize).
func normalizeEngine(databaseVersion string) string {
	if databaseVersion == "" {
		return ""
	}
	upper := strings.ToUpper(databaseVersion)
	switch {
	case strings.HasPrefix(upper, "POSTGRES_") || upper == "POSTGRES":
		return "postgres"
	case strings.HasPrefix(upper, "MYSQL_") || upper == "MYSQL":
		return "mysql"
	case strings.HasPrefix(upper, "SQLSERVER_") || upper == "SQLSERVER":
		return "sqlserver"
	default:
		return strings.ToLower(databaseVersion)
	}
}

// extractVersion peels the family prefix off the Cloud SQL
// DatabaseVersion enum and returns just the version tail. Examples:
//
//	"POSTGRES_15"              -> "15"
//	"MYSQL_8_0"                -> "8_0"
//	"SQLSERVER_2019_STANDARD"  -> "2019_STANDARD"
//	"POSTGRES"                 -> ""
//	""                         -> ""
//
// Unknown prefixes fall through to the raw input — the proposer reads
// EngineVersion as a raw string, so passing through preserves the
// caller's ability to render whatever Cloud SQL returned.
func extractVersion(databaseVersion string) string {
	if databaseVersion == "" {
		return ""
	}
	idx := strings.Index(databaseVersion, "_")
	if idx < 0 || idx == len(databaseVersion)-1 {
		return ""
	}
	return databaseVersion[idx+1:]
}

// classifyCloudSQLListError maps an Instances.List failure into the
// operator-visible PartialReason string. Same shape as
// classifyZonesListError but scoped to the Cloud SQL service so the
// error message points at the right IAM grant (roles/cloudsql.viewer
// per the design doc §12 threat model) and not at the compute one.
//
// Error mappings (per brief Step 5):
//
//   - 403 -> permission denied with remediation hint pointing at
//     roles/cloudsql.viewer (the slice-2 new IAM grant the runbook
//     documents).
//   - 404 -> project not found (same remediation hint as the compute
//     path — verify project_id).
//   - 429 -> rate limit (Cloud SQL has its own quota separate from
//     compute; same recovery story).
//   - Transport / network -> network-error with the underlying
//     err.Error() truncated to keep audit payloads bounded.
//   - Other 4xx/5xx -> truncated message with the HTTP code surfaced
//     so support agents can pattern-match against the Cloud SQL
//     documentation.
func classifyCloudSQLListError(err error) string {
	if err == nil {
		return ""
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service account has roles/cloudsql.viewer)", ServiceIDCloudSQL)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: project not found (verify project_id is correct)", ServiceIDCloudSQL)
		case http.StatusTooManyRequests:
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDCloudSQL)
		default:
			return fmt.Sprintf("%s: instances list failed (HTTP %d): %s", ServiceIDCloudSQL, ge.Code, truncate(ge.Message, 200))
		}
	}
	return fmt.Sprintf("%s: network error: %s", ServiceIDCloudSQL, truncate(err.Error(), 200))
}

// buildCloudSQLClient constructs a sqladmin.Service using the test
// httpClient + endpoint (no auth) or the SA-JSON-backed oauth2 client.
// Production callers reach the latter path; tests the former.
//
// Note: the OAuth scope for the production path is set on the SHARED
// JWT config that buildComputeClient also reads. See
// buildOAuthHTTPClient for the scope union — Compute Engine
// readonly + Cloud SQL admin (the latter is the read-listing scope
// despite the name; see the design doc §12 threat model footnote).
func (s *Scanner) buildCloudSQLClient(ctx context.Context, oauthClient *http.Client) (*sqladmin.Service, error) {
	if s.httpClient != nil {
		// Test path. The httpClient already points at the test server;
		// option.WithoutAuthentication stops the sqladmin client from
		// wrapping the test transport in another oauth2 layer.
		opts := []option.ClientOption{
			option.WithHTTPClient(s.httpClient),
			option.WithoutAuthentication(),
		}
		if s.endpoint != "" {
			opts = append(opts, option.WithEndpoint(s.endpoint))
		}
		return sqladmin.NewService(ctx, opts...)
	}
	// Production path: reuse the shared oauth-backed client built by
	// buildOAuthHTTPClient so the SA JSON is parsed once per scan
	// regardless of how many APIs the scan walks.
	return sqladmin.NewService(ctx, option.WithHTTPClient(oauthClient))
}
