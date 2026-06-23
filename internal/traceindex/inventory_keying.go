// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"fmt"
	"strings"
)

// Inventory-side projection helpers — trace integration slice 1
// chunk 4 (docs/proposals/trace-integration-slice1.md §3 + §6).
//
// The receiver side already projects a resource_key from incoming
// OTel resource attributes via ComputeResourceKey's six-tier
// fallback chain. The discovery side needs to project the SAME key
// from its own provider-native inventory snapshots so the per-scan
// last_seen_at annotation can join cleanly against the index.
//
// Slice 1 mirrors tiers 2-4 of ComputeResourceKey in REVERSE: the
// discovery scanner does not necessarily emit an ARN-shaped tier-1
// cloud.resource_id identifier, so the inventory-side projection
// builds the strong key from {provider, scope_id, resource_id}
// directly and relies on the OTel SDK's host detector to populate
// the matching identifier on emit-time. When the SDK emits with
// cloud.resource_id set (tier 1) the receiver-side key wins the
// precedence race and the inventory-side projection's tier-2 key
// will NOT match — slice 2 (see §13 Q2 of the design doc) adds an
// ARN-shaped projection so the discovery side can join against
// tier-1 receiver-side rows. Slice 1's posture is honest:
// last_seen_at populates for the tier-2/3/4 emitters and stays nil
// for tier-1 emitters until slice 2 lands.
//
// All three helpers return the empty string when any required
// component is missing — the annotation helper treats an empty
// projection as "no lookup possible" and leaves LastSeenAt nil
// rather than calling the index with a malformed key.

// ProjectComputeKey computes the resource_key for a discovered
// compute instance, mirroring ComputeResourceKey's tier-2 shape:
// "<provider>:<scope_id>:<resource_id>".
//
// Provider mappings (each per-cloud scanner pre-populates these
// before calling the annotation helper):
//   - AWS:   provider="aws",   scope_id=account_id,       resource_id=EC2 instance ID (i-...)
//   - GCP:   provider="gcp",   scope_id=project_id,       resource_id=GCE numeric id or display name
//   - Azure: provider="azure", scope_id=subscription_id,  resource_id=VM ARM ID
//   - OCI:   provider="oci",   scope_id=tenancy_ocid,     resource_id=instance OCID
//
// Returns "" when provider, scopeID, or resourceID is empty — the
// caller treats an empty projection as "skip this row."
func ProjectComputeKey(provider, scopeID, resourceID string) string {
	provider = strings.TrimSpace(provider)
	scopeID = strings.TrimSpace(scopeID)
	resourceID = strings.TrimSpace(resourceID)
	if provider == "" || scopeID == "" || resourceID == "" {
		return ""
	}
	return fmt.Sprintf("%s:%s:%s", provider, scopeID, resourceID)
}

// ProjectDatabaseKey computes the resource_key for a discovered
// managed-database row, mirroring ComputeResourceKey's tier-4
// shape: "<provider>:<scope_id>:db:<db_system>:<db_name>".
//
// db_system is the OTel db.system value the SDK on the DB workload
// will populate (e.g. "postgresql", "mysql", "mssql"); the discovery
// scanner pre-normalizes its provider-native engine string to the
// matching OTel token before calling here. db_name is the
// operator-controlled database identifier (RDS DB instance
// identifier / Cloud SQL instance name / etc.).
//
// Returns "" when any required component is missing.
func ProjectDatabaseKey(provider, scopeID, dbSystem, dbName string) string {
	provider = strings.TrimSpace(provider)
	scopeID = strings.TrimSpace(scopeID)
	dbSystem = strings.TrimSpace(dbSystem)
	dbName = strings.TrimSpace(dbName)
	if provider == "" || scopeID == "" || dbSystem == "" || dbName == "" {
		return ""
	}
	return fmt.Sprintf("%s:%s:db:%s:%s", provider, scopeID, dbSystem, dbName)
}

// ProjectClusterKey computes the resource_key for a discovered
// managed-Kubernetes cluster row, mirroring ComputeResourceKey's
// tier-3 shape: "<provider>:<scope_id>:k8s:<cluster_name>".
//
// cluster_name is the OTel k8s.cluster.name value the SDK on a
// workload running in the cluster will populate. The discovery
// scanner passes its provider-native cluster name verbatim — EKS,
// GKE, AKS, and OKE all expose a single operator-visible name that
// the K8s detector picks up.
//
// Returns "" when any required component is missing.
func ProjectClusterKey(provider, scopeID, clusterName string) string {
	provider = strings.TrimSpace(provider)
	scopeID = strings.TrimSpace(scopeID)
	clusterName = strings.TrimSpace(clusterName)
	if provider == "" || scopeID == "" || clusterName == "" {
		return ""
	}
	return fmt.Sprintf("%s:%s:k8s:%s", provider, scopeID, clusterName)
}
