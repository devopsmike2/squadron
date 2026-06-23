// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

// ociCompartment is the bare JSON shape of an OCI compartment as
// returned by the Identity /compartments list call. Slice 1 only
// reads ID and LifecycleState — Name is surfaced for diagnostic
// hints in error messages.
//
// OCI Identity API path:
//
//	GET https://identity.<region>.oci.oraclecloud.com/20160918/compartments
//	  ?compartmentId=<TenancyOCID>
//	  &accessLevel=ANY
//	  &compartmentIdInSubtree=false
//
// The slice 1 walker requests first-level children of the tenancy
// only (compartmentIdInSubtree=false). Slice 2 will lift this to the
// full subtree.
type ociCompartment struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	LifecycleState string `json:"lifecycleState"`
}

// ociCompartmentList is the JSON list envelope returned by the
// Identity /compartments list call. OCI returns the list directly as
// a JSON array (not wrapped in a top-level object); the scanner
// unmarshals into a []ociCompartment slice. This type alias is kept
// for clarity at call sites.
type ociCompartmentList = []ociCompartment

// ociInstance is the bare JSON shape of an OCI Compute Instance as
// returned by the /instances list call. Slice 1 reads DisplayName
// (-> ResourceID), Shape (-> InstanceType), Region (->
// ComputeInstanceSnapshot.Region), FreeformTags + DefinedTags (->
// flattened Tags map + OTel detection), and LifecycleState (for
// future filtering — slice 1 surfaces every state, slice 2 may add
// TERMINATING/TERMINATED filtering).
//
// OCI Compute API path:
//
//	GET https://iaas.<region>.oraclecloud.com/20160918/instances
//	  ?compartmentId=<compartment_ocid>
//
// FreeformTags is a plain map[string]string per the OCI shape.
// DefinedTags is a two-level map[namespace]map[key]value where
// value carries a typed JSON value (string, number, etc.); the
// slice 1 flattener treats every defined-tag value as its string
// representation since the OTel detection rule only inspects keys.
type ociInstance struct {
	ID                 string                            `json:"id"`
	DisplayName        string                            `json:"displayName"`
	Shape              string                            `json:"shape"`
	Region             string                            `json:"region"`
	AvailabilityDomain string                            `json:"availabilityDomain"`
	LifecycleState     string                            `json:"lifecycleState"`
	FreeformTags       map[string]string                 `json:"freeformTags,omitempty"`
	DefinedTags        map[string]map[string]interface{} `json:"definedTags,omitempty"`
}

// ociInstanceList is the JSON list envelope returned by the Compute
// /instances list call. OCI returns the list directly as a JSON
// array; the scanner unmarshals into a []ociInstance slice. Pagination
// (opc-next-page header) is slice 2 — slice 1 ships a single-page
// walk that captures the leading 100 instances per compartment, which
// is the OCI default page size and matches the slice 1 single-page
// posture of the GCP and Azure first-revision walkers.
type ociInstanceList = []ociInstance

// dbSystem is the bare JSON shape of an OCI Database DB System as
// returned by the /dbSystems list call. Slice 2 (database tier)
// reads DisplayName (-> ResourceID), Shape (-> InstanceClass),
// Version (-> EngineVersion), FreeformTags + DefinedTags (->
// flattened Tags map), and DatabaseManagementConfig (-> the OCI
// observability primitive used by the slice 2 detection rule).
// LifecycleState is read to filter out non-AVAILABLE rows (the
// proposer has no observability surface to recommend on for
// TERMINATING / PROVISIONING / FAILED instances).
//
// OCI Database API path:
//
//	GET https://database.<region>.oraclecloud.com/20160918/dbSystems
//	  ?compartmentId=<compartment_ocid>
//
// The DatabaseManagementConfig field is a NESTED object on DB
// Systems — distinct from the Autonomous Database surface where
// the same observability signal is flattened to a top-level
// status field. Both are reduced to a single boolean by the
// per-type Has-Management helpers (see scanner_db.go).
type dbSystem struct {
	ID                       string                            `json:"id"`
	DisplayName              string                            `json:"displayName"`
	Shape                    string                            `json:"shape"`
	Version                  string                            `json:"version"`
	LifecycleState           string                            `json:"lifecycleState"`
	FreeformTags             map[string]string                 `json:"freeformTags,omitempty"`
	DefinedTags              map[string]map[string]interface{} `json:"definedTags,omitempty"`
	DatabaseManagementConfig dbSystemManagementConfig          `json:"databaseManagementConfig"`
}

// dbSystemManagementConfig is the nested observability-status block
// on a DB System. OCI returns a small object whose only
// scanner-relevant field is databaseManagementStatus
// ("ENABLED" / "NOT_ENABLED"). Kept as a typed struct so the
// scanner doesn't have to chase a generic map[string]interface{}
// out of every list response.
type dbSystemManagementConfig struct {
	DatabaseManagementStatus string `json:"databaseManagementStatus"`
}

// dbSystemList is the JSON envelope returned by the Database
// /dbSystems list call. OCI returns the list directly as a JSON
// array; the scanner unmarshals into a []dbSystem slice. Single-
// page walk matches the compute path's slice 1 posture; pagination
// is slice 3.
type dbSystemList = []dbSystem

// autonomousDatabase is the bare JSON shape of an OCI Autonomous
// Database as returned by the /autonomousDatabases list call. The
// slice 2 mapping reads DisplayName (-> ResourceID), DbWorkload
// (-> EngineVersion via "autonomous-<workload>"), CpuCoreCount
// (-> InstanceClass via "ocpu-<n>"), FreeformTags + DefinedTags
// (-> flattened Tags map), and DatabaseManagementStatus (the
// flat top-level field that differs from the DB System shape).
//
// OCI Database API path:
//
//	GET https://database.<region>.oraclecloud.com/20160918/autonomousDatabases
//	  ?compartmentId=<compartment_ocid>
//
// Note on the API shape difference vs DB Systems: Autonomous
// Database surfaces databaseManagementStatus as a top-level
// string field rather than as a nested config object. The
// per-type Has-Management helper handles the difference; the
// scanner-side detection rule remains the same canonical
// "status == ENABLED" predicate across both shapes.
type autonomousDatabase struct {
	ID                       string                            `json:"id"`
	DisplayName              string                            `json:"displayName"`
	DbName                   string                            `json:"dbName"`
	DbWorkload               string                            `json:"dbWorkload"`
	CpuCoreCount             int                               `json:"cpuCoreCount"`
	LifecycleState           string                            `json:"lifecycleState"`
	FreeformTags             map[string]string                 `json:"freeformTags,omitempty"`
	DefinedTags              map[string]map[string]interface{} `json:"definedTags,omitempty"`
	DatabaseManagementStatus string                            `json:"databaseManagementStatus"`
}

// autonomousDatabaseList is the JSON envelope returned by the
// Database /autonomousDatabases list call. OCI returns the list
// directly as a JSON array.
type autonomousDatabaseList = []autonomousDatabase

// okeCluster is the bare JSON shape of an OCI OKE managed
// Kubernetes cluster as returned by the Container Engine
// /clusters list call. Slice 2 (kubernetes tier) reads ID
// (-> ResourceID), Name (-> Name), KubernetesVersion (->
// extractMajorMinor -> KubernetesVersion), LifecycleState
// (-> Status, also gates the skip-non-active filter), FreeformTags
// + DefinedTags (-> flattened Tags map + tag-based detection
// rule), and CompartmentID (carried for diagnostic hints only;
// the scanner does not project it onto the snapshot since the
// proposer reads Provider + Region for routing).
//
// OCI Container Engine API path:
//
//	GET https://containerengine.<region>.oraclecloud.com/20180222/clusters
//	  ?compartmentId=<compartment_ocid>
//
// The slice-2 detection rule (operations-insights-enabled=true
// freeform tag, case-insensitive on both key and value) is
// implemented by clusterHasOperationsInsights — see scanner_oke.go.
//
// Note on KubernetesVersion shape: OCI returns "v1.29.1" /
// "v1.30.0" style values with a leading "v" most of the time, but
// some older clusters / mocked responses surface the value without
// the leading "v" (e.g. "1.30.0"). The extractMajorMinor helper
// strips the optional leading "v" before taking the first two
// version components ("v1.29.1" -> "1.29", "1.30.0" -> "1.30")
// so the snapshot field carries a canonical normalized form for
// the proposer.
type okeCluster struct {
	ID                string                            `json:"id"`
	Name              string                            `json:"name"`
	CompartmentID     string                            `json:"compartmentId"`
	KubernetesVersion string                            `json:"kubernetesVersion"`
	LifecycleState    string                            `json:"lifecycleState"`
	FreeformTags      map[string]string                 `json:"freeformTags,omitempty"`
	DefinedTags       map[string]map[string]interface{} `json:"definedTags,omitempty"`
}

// okeClusterList is the JSON envelope returned by the Container
// Engine /clusters list call. OCI returns the list directly as a
// JSON array; the scanner unmarshals into a []okeCluster slice.
// Pagination (opc-next-page header) is slice 3 — slice 2 ships a
// single-page walk matching the compute / database per-surface
// posture.
type okeClusterList = []okeCluster

// ociErrorBody is the JSON shape OCI returns on a 4xx / 5xx error.
// The scanner reads .Code to disambiguate permission_denied
// ("NotAuthorizedOrNotFound") vs tenancy/compartment-not-found
// ("NotAuthorizedOrNotFound" + 404) vs the various rate-limit codes
// ("TooManyRequests", "RateLimitExceeded") vs everything else.
// Mirrors the Azure scanner's armErrorResponse and the GCP scanner's
// googleapi.Error pattern.
type ociErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ociCallError is the internal sentinel the listInstances /
// listCompartments paths return when an OCI call fails. It carries
// the HTTP status code, the OCI error code (parsed from the JSON
// body), the body hint, and the optional wrapped network/transport
// error so classifyOCIError can dispatch on the right field.
//
// Pattern parallels Azure's armCallError and AWS's classify helpers
// — keeps cross-scanner symmetry so audit consumers see identical
// structure across providers.
type ociCallError struct {
	StatusCode int
	Code       string
	Message    string
	BodyHint   string
	RetryAfter string
	Wrapped    error
	IsNetwork  bool
}

func (e *ociCallError) Error() string {
	if e == nil {
		return ""
	}
	if e.IsNetwork && e.Wrapped != nil {
		return "network error: " + truncate(e.Wrapped.Error(), 200)
	}
	if e.StatusCode != 0 {
		return sprintfErr("OCI call failed (HTTP %d, code=%s)", e.StatusCode, e.Code)
	}
	if e.Wrapped != nil {
		return e.Wrapped.Error()
	}
	return "OCI call failed"
}
