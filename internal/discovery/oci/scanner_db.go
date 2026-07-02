// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// scanDatabases walks the OCI Database surfaces (DB Systems +
// Autonomous Databases) across the supplied compartments. Mirrors
// the compute walk's per-compartment loop in Scan; called from Scan
// after the compute walk completes so the result is a single Result
// with both Compute AND Databases populated for the same scan.
//
// The walk visits each compartment for BOTH product families. A
// failure on one family for one compartment does not stop the
// remaining family from being walked — partial-failure accumulation
// follows the recordPartialFailure helper's pattern (PartialReason
// joined with "; " separators, FailedServices append-only). Service
// identifier "ocidb" (ServiceIDDatabase).
//
// Slice 2 design choice — compartment 404 mid-walk is non-fatal for
// the database surface too. Many tenancies have no OCI Database
// services in most compartments (they're concentrated in a "data"
// compartment); a 404 surfaces as a silent skip rather than a
// partial-failure to avoid producing one PartialReason entry per
// uninitialized compartment. The root-level 404 case is treated as
// a partial failure (the operator's policy genuinely doesn't grant
// read database-family at the tenancy scope, or the tenancy_ocid is
// wrong — same condition the compute path treats as hard at the
// initial compartment list). See classifyOCIDBError below.
func (s *Scanner) scanDatabases(ctx context.Context, sk *SigningKey, compartments []ociCompartment, result *scanner.Result) {
	for _, comp := range compartments {
		// DB Systems walk. Failures are partial — surface and keep
		// walking the autonomous family on the same compartment.
		systems, sysErr := s.listDBSystems(ctx, sk, comp.ID)
		if sysErr != nil {
			if reason := classifyOCIDBError(sysErr, false /*atRoot*/); reason != "" {
				recordPartialFailure(result, ServiceIDDatabase, reason)
			}
		} else {
			for _, dbs := range systems {
				if !isDBLifecycleAvailable(dbs.LifecycleState) {
					continue
				}
				result.Databases = append(result.Databases, projectDBSystem(dbs, s.Region))
			}
		}

		// Autonomous Database walk on the same compartment.
		adbs, adbErr := s.listAutonomousDatabases(ctx, sk, comp.ID)
		if adbErr != nil {
			if reason := classifyOCIDBError(adbErr, false /*atRoot*/); reason != "" {
				recordPartialFailure(result, ServiceIDDatabase, reason)
			}
			continue
		}
		for _, adb := range adbs {
			if !isDBLifecycleAvailable(adb.LifecycleState) {
				continue
			}
			result.Databases = append(result.Databases, projectAutonomousDatabase(adb, s.Region))
		}
	}
}

// listDBSystems walks the OCI Database /dbSystems endpoint for a
// single compartment. Single-page walk matches the compute path's
// slice 1 posture (no opc-next-page header following) — slice 3
// adds pagination uniformly.
func (s *Scanner) listDBSystems(ctx context.Context, sk *SigningKey, compartmentID string) ([]dbSystem, error) {
	endpoint := s.databaseEndpoint()
	url := fmt.Sprintf(
		"%s/%s/dbSystems?compartmentId=%s",
		strings.TrimRight(endpoint, "/"),
		databaseListAPIVersion,
		compartmentID,
	)

	body, callErr := s.doSignedGET(ctx, sk, url)
	if callErr != nil {
		return nil, callErr
	}

	var out dbSystemList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, &ociCallError{Wrapped: fmt.Errorf("dbSystems response parse: %w", jerr)}
	}
	return out, nil
}

// listAutonomousDatabases walks the OCI Database /autonomousDatabases
// endpoint for a single compartment. Same single-page slice-1
// posture as listDBSystems.
func (s *Scanner) listAutonomousDatabases(ctx context.Context, sk *SigningKey, compartmentID string) ([]autonomousDatabase, error) {
	endpoint := s.databaseEndpoint()
	url := fmt.Sprintf(
		"%s/%s/autonomousDatabases?compartmentId=%s",
		strings.TrimRight(endpoint, "/"),
		databaseListAPIVersion,
		compartmentID,
	)

	body, callErr := s.doSignedGET(ctx, sk, url)
	if callErr != nil {
		return nil, callErr
	}

	var out autonomousDatabaseList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, &ociCallError{Wrapped: fmt.Errorf("autonomousDatabases response parse: %w", jerr)}
	}
	return out, nil
}

// databaseEndpoint returns the OCI Database API base URL. When
// ociEndpoint is set (tests), it's used directly — the test mock
// dispatches /dbSystems and /autonomousDatabases on the same
// httptest server that already routes /compartments and
// /instances. In production the per-region database endpoint
// pattern is https://database.<region>.oraclecloud.com.
func (s *Scanner) databaseEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://database.%s.oraclecloud.com", s.Region)
}

// dbSystemHasManagement implements the slice-2 OCI detection rule
// for DB Systems: the row counts as instrumented iff its nested
// databaseManagementConfig.databaseManagementStatus equals "ENABLED"
// case-insensitively. The case-insensitive comparison mirrors the
// ECSClusterSnapshot.IsInstrumented pattern — defense-in-depth
// costs nothing and keeps the rule resilient to OCI API drift.
//
// Kept as a package helper so projectDBSystem and tests can both
// reference the same predicate without re-deriving the rule.
func dbSystemHasManagement(dbs dbSystem) bool {
	return strings.EqualFold(dbs.DatabaseManagementConfig.DatabaseManagementStatus, dbManagementEnabledStatus)
}

// autonomousHasManagement implements the slice-2 OCI detection rule
// for Autonomous Databases: the row counts as instrumented iff its
// top-level databaseManagementStatus equals "ENABLED"
// case-insensitively. Same canonical rule as dbSystemHasManagement
// — the API shape difference (nested config block on DB Systems vs
// flat top-level field on Autonomous Databases) is absorbed here so
// every downstream consumer sees a single boolean.
func autonomousHasManagement(adb autonomousDatabase) bool {
	return strings.EqualFold(adb.DatabaseManagementStatus, dbManagementEnabledStatus)
}

// isDBLifecycleAvailable returns true when the lifecycleState
// matches the "AVAILABLE" sentinel case-insensitively. OCI's
// lifecycle enum carries TERMINATING / PROVISIONING / FAILED /
// AVAILABLE values; only AVAILABLE rows have an observability
// surface the proposer can recommend on. Slice 2 skips the rest
// — surfacing them as inventory would confuse the operator
// reading the Inventory tab.
func isDBLifecycleAvailable(state string) bool {
	return strings.EqualFold(state, dbAvailableLifecycleState)
}

// projectDBSystem maps a dbSystem into the provider-agnostic
// DatabaseInstanceSnapshot. The slice-2 mapping is:
//
//   - ResourceID: DisplayName (operator-readable, stable per
//     tenancy — same convention as the compute path's
//     ComputeInstanceSnapshot.ResourceID).
//   - Engine: "oracle" — every OCI DB System runs Oracle Database
//     in slice 2 (MySQL HeatWave is a separate product family
//     with its own scanner extension arc).
//   - EngineVersion: dbSystem.Version, e.g. "19.0.0.0".
//   - InstanceClass: dbSystem.Shape, e.g. "VM.Standard2.4".
//   - Region: the scanner's configured Region — OCI's dbSystems
//     response does not echo region per-row, so we use the
//     per-scan region the connection was configured with.
//   - Tags: flattened FreeformTags + DefinedTags via the same
//     flattenTags helper the compute path uses (single-source
//     the tag-flattening rule across both surfaces).
//   - Provider: "oci" — the proposer reads Provider plus the
//     matching axis to decide which recommendation kind to emit.
//   - DatabaseManagementEnabled: dbSystemHasManagement(dbs) —
//     the slice-2 detection axis (see helper godoc).
func projectDBSystem(dbs dbSystem, fallbackRegion string) scanner.DatabaseInstanceSnapshot {
	return scanner.DatabaseInstanceSnapshot{
		ResourceID: dbs.DisplayName,
		// ImportID: oci_database_db_system imports by the DB System OCID.
		ImportID:                  dbs.ID,
		Engine:                    dbEngineOracle,
		EngineVersion:             dbs.Version,
		InstanceClass:             dbs.Shape,
		Region:                    fallbackRegion,
		Tags:                      flattenTags(dbs.FreeformTags, dbs.DefinedTags),
		Provider:                  dbProviderOCI,
		DatabaseManagementEnabled: dbSystemHasManagement(dbs),
	}
}

// projectAutonomousDatabase maps an autonomousDatabase into the
// provider-agnostic DatabaseInstanceSnapshot. The slice-2 mapping
// is the same canonical shape as projectDBSystem with two
// per-row differences:
//
//   - EngineVersion: "autonomous-<workload>" where workload is the
//     dbWorkload field lower-cased ("OLTP" -> "autonomous-oltp",
//     "DW" -> "autonomous-dw"). This composite string is the
//     slice-2 contract chosen so the proposer can route on the
//     full workload without a second field — the proposer prompt
//     reads the prefix "autonomous-" to recognize the family.
//   - InstanceClass: "ocpu-<n>" where n is cpuCoreCount. Autonomous
//     Databases are sized in OCPUs (Oracle CPUs) rather than the
//     shape strings DB Systems use; the "ocpu-" prefix gives the
//     proposer a stable token to anchor the per-row size on.
//
// The remaining fields (ResourceID, Engine, Region, Tags, Provider,
// DatabaseManagementEnabled) follow the DB System projection
// convention so every snapshot row looks symmetric to the
// proposer.
func projectAutonomousDatabase(adb autonomousDatabase, fallbackRegion string) scanner.DatabaseInstanceSnapshot {
	return scanner.DatabaseInstanceSnapshot{
		ResourceID: adb.DisplayName,
		// ImportID: oci_database_autonomous_database imports by the ADB OCID.
		ImportID:                  adb.ID,
		Engine:                    dbEngineOracle,
		EngineVersion:             fmt.Sprintf("autonomous-%s", strings.ToLower(adb.DbWorkload)),
		InstanceClass:             fmt.Sprintf("ocpu-%d", adb.CpuCoreCount),
		Region:                    fallbackRegion,
		Tags:                      flattenTags(adb.FreeformTags, adb.DefinedTags),
		Provider:                  dbProviderOCI,
		DatabaseManagementEnabled: autonomousHasManagement(adb),
	}
}

// classifyOCIDBError maps an OCI Database call failure into the
// operator-visible PartialReason string under the ocidb service
// identifier. Parallels classifyOCIError (which uses the
// ocicompute identifier) so the audit consumer sees identical
// structure across the two service surfaces.
//
// The atRoot flag distinguishes the (hypothetical) initial root
// list call from per-compartment calls. The slice-2 walk reuses
// the compute-path compartment list, so atRoot is always false
// here in production — but the parameter is kept symmetric for
// future direct-root scans (e.g. a database-only validate path)
// and tests that exercise the root-404 mapping.
//
// Error mappings (per the chunk-4 brief and design doc §3.3):
//
//   - HTTP 401 -> credentials_invalid (signature rejected — wrong
//     fingerprint, malformed key, or skewed clock; mirrors the
//     compute path).
//   - HTTP 403 -> permission_denied with hint pointing operators
//     at the new "read database-family in tenancy" policy
//     statement.
//   - HTTP 404 mid-walk -> empty string (silent skip — many
//     compartments have no database resources; surfacing 404s as
//     partial failures would be noise). Root-level 404 (atRoot
//     true) -> "database surface not found" partial-failure
//     reason.
//   - HTTP 429 -> rate_limit.
//   - Transport / network errors -> network-error with truncated
//     underlying error.
//   - Any other 4xx/5xx -> truncated message under the ocidb
//     identifier.
func classifyOCIDBError(err error, atRoot bool) string {
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
			return fmt.Sprintf("%s: network error: %s", ServiceIDDatabase, truncate(wrapped, 200))
		}
		if oce.StatusCode == http.StatusTooManyRequests || oce.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDDatabase)
		}
		switch oce.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check fingerprint, user_ocid, and private key)", ServiceIDDatabase)
		case http.StatusForbidden:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: permission denied (verify the policy grants 'read database-family in tenancy'): %s", ServiceIDDatabase, truncate(msg, 200))
		case http.StatusNotFound:
			if atRoot {
				return fmt.Sprintf("%s: database surface not found (verify tenancy_ocid and the database-family policy)", ServiceIDDatabase)
			}
			// Mid-walk 404 — compartment has no database services
			// available (or the operator's policy doesn't grant
			// read database-family on this compartment). Skip
			// silently; the caller branches on the empty return.
			return ""
		default:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: OCI call failed (HTTP %d): %s", ServiceIDDatabase, oce.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDDatabase, truncate(err.Error(), 200))
}
