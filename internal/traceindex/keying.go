// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"fmt"
	"strings"
)

// ComputeResourceKey runs the §3 six-tier fallback chain against the
// resource attribute map and returns:
//
//   - key: the projected resource_key string used as the
//     trace_resource_seen primary key
//   - provider: "aws" / "gcp" / "azure" / "oci" / "unknown",
//     inferred from cloud.provider when present (slice 1 returns
//     "unknown" liberally rather than guessing — design doc §3 calls
//     out that misclassification is worse than under-classification
//     because the dashboard's per-provider rollup must be exact)
//   - scopeID: cloud.account.id family — the AWS account / GCP
//     project / Azure subscription / OCI tenancy ID
//   - resourceIDHint: the raw cloud.resource_id when present,
//     otherwise the empty string (the storage row preserves this
//     verbatim so a future inventory join can compare ARNs directly)
//   - serviceName: the OTel service.name (always set in OTLP per
//     §3 of the design doc; the index preserves it on every tier so
//     the diagnostic UI can display "service.name=X matched to
//     host.id=Y")
//   - confidence: MatchConfidenceStrong for tiers 1-4 (cloud-native
//     identifier), MatchConfidenceWeak for tiers 5-6 (name-only)
//   - ok: false when none of the six tiers match — the caller
//     (Index.Observe) drops the observation silently per the §13 Q4
//     decision that slice 1 prefers a clean index over an "orphan
//     traces" indicator
//
// The precedence order is intentionally rigid: tier 1 wins over tier
// 2 wins over tier 3, etc. This matters when an SDK populates BOTH
// cloud.resource_id AND host.id — the resource_id projection is the
// canonical one and the host.id is treated as redundant. The
// keying_test.go TestComputeResourceKey_PrecedenceOrder case pins
// this.
func ComputeResourceKey(attrs map[string]string) (
	key, provider, scopeID, resourceIDHint, serviceName string,
	confidence MatchConfidence,
	ok bool,
) {
	if len(attrs) == 0 {
		return "", "unknown", "", "", "", MatchConfidenceWeak, false
	}

	// Provider inference: cloud.provider is the OTel-canonical attr.
	// Slice 1 normalizes to a lowercase token — Squadron's discovery
	// surface uses the same vocabulary so the dashboard rollup joins
	// cleanly.
	provider = normalizeProvider(firstNonEmpty(attrs, "cloud.provider"))
	scopeID = firstNonEmpty(attrs, "cloud.account.id")
	serviceName = firstNonEmpty(attrs, "service.name")
	resourceIDHint = firstNonEmpty(attrs, "cloud.resource_id")

	// Tier 1: cloud.resource_id (strong, key = verbatim).
	//
	// The cloud detector populates this when the SDK has been given
	// enough context to resolve the full ARN-shaped identifier. This
	// is the most reliable signal and short-circuits the rest of the
	// chain.
	if resourceIDHint != "" {
		return resourceIDHint, provider, scopeID, resourceIDHint, serviceName, MatchConfidenceStrong, true
	}

	// Tier 2: host.id + cloud.account.id (strong).
	//
	// host.id is the provider-native instance ID (i-0abc, GCE numeric
	// ID, etc.). Combined with the account scope it identifies the
	// resource uniquely across cloud tenancies. The key shape
	// "<provider>:<account>:<host_id>" matches what the discovery
	// side projects from its inventory snapshot.
	if hostID := firstNonEmpty(attrs, "host.id"); hostID != "" && scopeID != "" {
		return fmt.Sprintf("%s:%s:%s", provider, scopeID, hostID),
			provider, scopeID, "", serviceName, MatchConfidenceStrong, true
	}

	// Tier 3: k8s.cluster.name + cloud.account.id (strong).
	//
	// For K8s workloads the cluster name keyed against the account is
	// the right granularity for slice 1 — per-pod identity is too
	// fine for the coverage dashboard (a single GKE cluster could
	// have thousands of pods and emit a single coverage signal). The
	// "k8s" namespace token in the key keeps the projection distinct
	// from a host whose host.id happens to equal a cluster name.
	if cluster := firstNonEmpty(attrs, "k8s.cluster.name"); cluster != "" && scopeID != "" {
		return fmt.Sprintf("%s:%s:k8s:%s", provider, scopeID, cluster),
			provider, scopeID, "", serviceName, MatchConfidenceStrong, true
	}

	// Tier 4: db.system + db.name + cloud.account.id (strong).
	//
	// Database resources carry their own identity surface. The
	// "<provider>:<account>:db:<system>:<name>" projection matches
	// what the discovery scanner produces for RDS / Cloud SQL /
	// Azure SQL / Autonomous DB inventory rows.
	if dbSystem := firstNonEmpty(attrs, "db.system"); dbSystem != "" {
		if dbName := firstNonEmpty(attrs, "db.name"); dbName != "" && scopeID != "" {
			return fmt.Sprintf("%s:%s:db:%s:%s", provider, scopeID, dbSystem, dbName),
				provider, scopeID, "", serviceName, MatchConfidenceStrong, true
		}
	}

	// Tier 5: host.name alone (weak).
	//
	// The OS detector populates host.name on every host the SDK runs
	// on. Slice 1's design doc §3 names this as best-effort — a
	// host.name collision across two clouds would produce a single
	// row, but the dashboard surfaces the weak indicator so an
	// operator can investigate.
	if hostName := firstNonEmpty(attrs, "host.name"); hostName != "" {
		return "host:" + hostName, provider, scopeID, "", serviceName, MatchConfidenceWeak, true
	}

	// Tier 6: service.name alone (weak).
	//
	// service.name is operator-controlled and always set, but its
	// relationship to inventory is conventional rather than enforced.
	// The "service:" namespace token in the key keeps it from
	// colliding with a host.name that happens to equal a service.
	if serviceName != "" {
		return "service:" + serviceName, provider, scopeID, "", serviceName, MatchConfidenceWeak, true
	}

	return "", "unknown", "", "", "", MatchConfidenceWeak, false
}

// firstNonEmpty returns the first non-empty attribute value across
// the supplied key sequence. Keeps the keying chain readable when
// the OTel semantic-convention key has more than one valid spelling
// (currently single-key for every tier, but the helper is cheap and
// preserves the option of merging spellings in slice 2).
func firstNonEmpty(attrs map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := attrs[k]; ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return ""
}

// normalizeProvider folds the raw cloud.provider value into the
// vocabulary Squadron's discovery surface already uses ("aws",
// "gcp", "azure", "oci"). Unknown / empty values fall through to
// "unknown" — design doc §3 calls out the explicit liberal
// fall-through.
func normalizeProvider(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "aws":
		return "aws"
	case "gcp", "google_cloud_platform", "google":
		return "gcp"
	case "azure", "microsoft_azure":
		return "azure"
	case "oci", "oracle_cloud":
		return "oci"
	default:
		return "unknown"
	}
}
