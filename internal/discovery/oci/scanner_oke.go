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

// scanOKEClusters walks the OCI OKE Container Engine /clusters
// endpoint across the supplied compartments. Mirrors the compute
// and database walks' per-compartment loop in Scan; called from
// Scan after the compute walk + database walks complete so the
// result is a single Result with Compute, Databases, AND Clusters
// populated for the same scan.
//
// The walk visits each compartment exactly once. A failure on one
// compartment does not stop the remaining compartments from being
// walked — partial-failure accumulation follows the
// recordPartialFailure helper's pattern (PartialReason joined with
// "; " separators, FailedServices append-only). Service identifier
// "oke" (ServiceIDKubernetes).
//
// Slice 2 design choice — compartment 404 mid-walk is non-fatal for
// the OKE surface (parallel to the database walk's posture). Many
// tenancies have zero OKE clusters in most compartments (they are
// typically concentrated in a "platform" / "k8s" compartment); a
// 404 mid-walk surfaces as a silent skip rather than a
// partial-failure to avoid producing one PartialReason entry per
// uninitialized compartment. The classifier returns "" for the
// compartment-skip case so the caller can branch on it.
//
// Clusters with LifecycleState != "ACTIVE" are skipped: mid-create
// (CREATING), mid-delete (DELETING), and in-progress upgrades
// (UPDATING) have no observability surface the proposer can
// usefully recommend on. The skip filter mirrors the database
// walk's isDBLifecycleAvailable gate.
func (s *Scanner) scanOKEClusters(ctx context.Context, sk *SigningKey, compartments []ociCompartment, result *scanner.Result) {
	for _, comp := range compartments {
		clusters, listErr := s.listOKEClusters(ctx, sk, comp.ID)
		if listErr != nil {
			if reason := classifyOCIOKEError(listErr, false /*atRoot*/); reason != "" {
				recordPartialFailure(result, ServiceIDKubernetes, reason)
			}
			continue
		}
		for _, c := range clusters {
			if !isClusterActive(c.LifecycleState) {
				continue
			}
			result.Clusters = append(result.Clusters, projectOKECluster(c, s.Region))
		}
	}
}

// listOKEClusters walks the OCI Container Engine /clusters endpoint
// for a single compartment. Single-page walk matches the compute
// and database paths' slice 1/2 posture (no opc-next-page header
// following) — slice 3 adds pagination uniformly across all three
// per-cloud OCI surfaces.
func (s *Scanner) listOKEClusters(ctx context.Context, sk *SigningKey, compartmentID string) ([]okeCluster, error) {
	endpoint := s.okeEndpoint()
	url := fmt.Sprintf(
		"%s/%s/clusters?compartmentId=%s",
		strings.TrimRight(endpoint, "/"),
		okeListAPIVersion,
		compartmentID,
	)

	body, callErr := s.doSignedGET(ctx, sk, url)
	if callErr != nil {
		return nil, callErr
	}

	var out okeClusterList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, &ociCallError{Wrapped: fmt.Errorf("clusters response parse: %w", jerr)}
	}
	return out, nil
}

// okeEndpoint returns the OCI Container Engine API base URL. When
// ociEndpoint is set (tests), it's used directly — the test mock
// dispatches /clusters on the same httptest server that already
// routes /compartments, /instances, /dbSystems, and
// /autonomousDatabases. In production the per-region OKE endpoint
// pattern is https://containerengine.<region>.oraclecloud.com.
func (s *Scanner) okeEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://containerengine.%s.oraclecloud.com", s.Region)
}

// clusterHasOperationsInsights implements the slice-2 OKE
// detection rule: the cluster counts as instrumented iff its
// freeform tags contain a key matching opsInsightsEnabledTagKey
// (case-insensitive) with a trimmed value matching
// opsInsightsEnabledTagValue (case-insensitive). The
// case-insensitive comparison on both axes mirrors the
// kubernetes-tier-slice2.md §11 test 7 contract — operators may
// use any casing for the key ("operations-insights-enabled",
// "Operations-Insights-Enabled", "OPERATIONS-INSIGHTS-ENABLED")
// or the value ("true", "TRUE", "True") and the rule still fires.
//
// The value is trimmed before comparison to absorb operators who
// added surrounding whitespace via copy-paste from the OCI console
// (a small but observed footgun in the database tier's
// "ENABLED" detection).
//
// Kept as a package helper so projectOKECluster and tests can both
// reference the same predicate without re-deriving the rule.
func clusterHasOperationsInsights(tags map[string]string) bool {
	for k, v := range tags {
		if strings.EqualFold(k, opsInsightsEnabledTagKey) &&
			strings.EqualFold(strings.TrimSpace(v), opsInsightsEnabledTagValue) {
			return true
		}
	}
	return false
}

// isClusterActive returns true when the lifecycleState matches the
// "ACTIVE" sentinel case-insensitively. OCI's OKE lifecycle enum
// carries CREATING / ACTIVE / FAILED / DELETING / UPDATING /
// DELETED values; only ACTIVE clusters have an observability
// surface the proposer can recommend on. Slice 2 skips the rest —
// surfacing them as inventory would confuse the operator reading
// the Inventory tab while a cluster is mid-create or mid-delete.
//
// Mirrors the database tier's isDBLifecycleAvailable gate; the
// per-surface enum value differs ("AVAILABLE" on the database
// surface; "ACTIVE" on OKE) but the skip-non-active posture is
// identical across surfaces.
func isClusterActive(state string) bool {
	return strings.EqualFold(state, okeActiveLifecycleState)
}

// extractMajorMinor normalizes an OCI-reported Kubernetes version
// string into the canonical "<major>.<minor>" shape the proposer
// reads. OCI returns the version with a leading "v" most of the
// time ("v1.29.1") but some older clusters surface the value
// without the leading "v" ("1.30.0"); the helper strips the
// optional leading "v" and takes the first two dot-separated
// components.
//
// Examples:
//   - "v1.29.1"   -> "1.29"
//   - "1.30.0"    -> "1.30"
//   - "v1.28"     -> "1.28"
//   - "v1"        -> "1"
//   - ""          -> ""
//
// Slice 2 design choice — normalize to major.minor only. Patch
// versions (1.29.1 vs 1.29.7) carry no proposer-relevant signal
// for the Operations Insights enable rule; collapsing them into a
// single bucket keeps the Inventory tab compact and avoids
// per-patch-version row spam. Slice 3 may surface the full
// version string for per-patch observability guidance.
func extractMajorMinor(version string) string {
	v := strings.TrimPrefix(version, "v")
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// projectOKECluster maps an okeCluster into the provider-agnostic
// ClusterSnapshot. The slice-2 mapping is:
//
//   - ResourceID: cluster.ID (full OCID — the proposer's evidence
//     list and the recommendation envelope's AffectedResources
//     field both reference the canonical OCID identifier).
//   - Name: cluster.Name (operator-readable display name).
//   - KubernetesVersion: extractMajorMinor(cluster.KubernetesVersion)
//     (canonical "<major>.<minor>" shape; see helper godoc).
//   - Status: cluster.LifecycleState (raw — only ACTIVE clusters
//     reach this projection since scanOKEClusters skips the rest,
//     but the field is surfaced raw so the Inventory tab matches
//     the database tier's lifecycle posture).
//   - Region: the scanner's configured Region — OCI's clusters
//     response does not echo region per-row (the cluster is
//     scoped to the region the request was sent to), so we use
//     the per-scan region the connection was configured with.
//   - Tags: flattened FreeformTags + DefinedTags via the same
//     flattenTags helper the compute / database paths use
//     (single-source the tag-flattening rule across surfaces).
//   - Provider: "oci" — the proposer reads Provider plus the
//     matching axis (OperationsInsightsEnabled) to decide which
//     recommendation kind to emit (oke-ops-insights-enable when
//     the boolean is false).
//   - OperationsInsightsEnabled: clusterHasOperationsInsights(
//     cluster.FreeformTags) — the slice-2 detection axis. Only
//     freeform tags participate in the rule per design doc §3.3
//     (defined tags carry typed values that don't match the
//     simple "true" sentinel cleanly; slice 3 may broaden if
//     operators request it).
//
// The existing AWS EKS axes (ControlPlaneLogging, Addons,
// NodegroupCount, FargateProfileCount) are left at their zero
// values — the proposer reads Provider="oci" and routes to the
// OCI-specific detection axis, leaving the AWS-specific fields
// unread.
func projectOKECluster(c okeCluster, fallbackRegion string) scanner.ClusterSnapshot {
	return scanner.ClusterSnapshot{
		ResourceID:                c.ID,
		Name:                      c.Name,
		KubernetesVersion:         extractMajorMinor(c.KubernetesVersion),
		Status:                    c.LifecycleState,
		Region:                    fallbackRegion,
		Tags:                      flattenTags(c.FreeformTags, c.DefinedTags),
		Provider:                  clusterProviderOCI,
		OperationsInsightsEnabled: clusterHasOperationsInsights(c.FreeformTags),
	}
}

// classifyOCIOKEError maps an OCI Container Engine call failure
// into the operator-visible PartialReason string under the oke
// service identifier. Parallels classifyOCIError (which uses the
// ocicompute identifier) and classifyOCIDBError (which uses the
// ocidb identifier) so the audit consumer sees identical
// structure across the three OCI service surfaces.
//
// The atRoot flag distinguishes the (hypothetical) initial root
// list call from per-compartment calls. The slice-2 walk reuses
// the compute-path compartment list, so atRoot is always false
// here in production — but the parameter is kept symmetric for
// future direct-root scans (e.g. a kubernetes-only validate path)
// and tests that exercise the root-404 mapping.
//
// Error mappings (per the chunk-4 brief and design doc §3.3 +
// §12 threat model):
//
//   - HTTP 401 -> credentials_invalid (signature rejected — wrong
//     fingerprint, malformed key, or skewed clock; mirrors the
//     compute and database paths).
//   - HTTP 403 -> permission_denied with hint pointing operators
//     at the new "read cluster-family in tenancy" policy
//     statement.
//   - HTTP 404 mid-walk -> empty string (silent skip — many
//     compartments have zero OKE clusters; surfacing 404s as
//     partial failures would be noise). Root-level 404 (atRoot
//     true) -> "OKE surface not found" partial-failure reason.
//   - HTTP 429 -> rate_limit.
//   - Transport / network errors -> network-error with truncated
//     underlying error.
//   - Any other 4xx/5xx -> truncated message under the oke
//     identifier.
func classifyOCIOKEError(err error, atRoot bool) string {
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
			return fmt.Sprintf("%s: network error: %s", ServiceIDKubernetes, truncate(wrapped, 200))
		}
		if oce.StatusCode == http.StatusTooManyRequests || oce.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDKubernetes)
		}
		switch oce.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check fingerprint, user_ocid, and private key)", ServiceIDKubernetes)
		case http.StatusForbidden:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: permission denied (verify the policy grants 'read cluster-family in tenancy'): %s", ServiceIDKubernetes, truncate(msg, 200))
		case http.StatusNotFound:
			if atRoot {
				return fmt.Sprintf("%s: OKE surface not found (verify tenancy_ocid and the cluster-family policy)", ServiceIDKubernetes)
			}
			// Mid-walk 404 — compartment has no OKE resources
			// available (or the operator's policy doesn't grant
			// read cluster-family on this compartment). Skip
			// silently; the caller branches on the empty return.
			return ""
		default:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: OCI call failed (HTTP %d): %s", ServiceIDKubernetes, oce.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDKubernetes, truncate(err.Error(), 200))
}
