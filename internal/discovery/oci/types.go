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
