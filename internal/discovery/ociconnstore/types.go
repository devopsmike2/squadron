// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ociconnstore

import (
	"time"
)

// Provider names the cloud the connection points at. Slice 1 of #681
// ships OCI only; the constant lives here so callers don't string-
// type the discriminator and so future provider extensions follow
// the same shape used in iacconnstore / gcpconnstore / azureconnstore.
const (
	// ProviderOCI is the only supported provider in slice 1 of the
	// OCI arc (see docs/proposals/oci-discovery-slice1.md §1, §10).
	ProviderOCI = "oci"
)

// OCIConnection represents a single OCI tenancy connection that
// Squadron can scan. The OCI credential model is API Signing Key:
// an RSA keypair where the public key is uploaded to OCI Console
// against a user_ocid and the private key (plus the OCID fields,
// fingerprint, and region) constitutes the full credential set.
// The private key is stored sealed via the credstore substrate with
// domain-tagged AAD "squadron.oci_signing_key.v1" to prevent cross-
// domain confusion with PAT, webhook secret, GCP SA, or Azure SP
// sealed bytes (parallel to v0.89.31's per-connection webhook secret
// posture, v0.89.46's GCP SA posture, and v0.89.51's Azure SP
// posture). OCI is the FOURTH sealed credential type — the defense-
// in-depth posture extends across the full cross-product.
//
// Slice 1 ships single-region per connection (OCI's API endpoints
// are regional so Region is REQUIRED, unlike AWS/GCP/Azure where
// empty=scan-all). Multi-region orchestration is slice 2.
//
// SealedPrivateKey carries the on-disk sealed bytes. It is NEVER
// serialized to JSON (the json:"-" tag suppresses every marshal
// path) so an accidental List response, audit payload, or log line
// cannot leak the encrypted credential blob. Decryption is the
// scanner's job via credstore.UnsealOCIPrivateKey — the only
// sanctioned access path. Private key bytes are the strongest
// credential type Squadron handles; the never-log / never-embed-in-
// audit / never-echo invariants are non-negotiable.
type OCIConnection struct {
	// ID is the substrate's primary key, a UUIDv4 stamped by Create.
	// Callers do not supply this on Create.
	ID string `json:"id"`

	// OwnerTenantID is the Squadron tenant that OWNS this connection
	// (ADR 0013 §D6-b). This is DISTINCT from the TenancyOCID field
	// below, which is the OCI tenancy OCID (a cloud-side identifier).
	// OwnerTenantID is stamped at Create from the authenticated actor's
	// tenant so the discovery rescan scheduler can scope its
	// discovery_scans store writes to the owning tenant — a scheduled
	// rescan runs under WithSystemContext and carries no operator
	// identity. Resolves to identity.DefaultTenant ("default") in the
	// OSS single-tenant build — inert until the enterprise
	// TenantResolver derives a real tenant from the identity.
	OwnerTenantID string `json:"squadron_tenant_id"`

	// DisplayName is the operator-supplied human-readable label for
	// the connection. Surfaces in the discovery UI's connection list
	// and audit timeline.
	DisplayName string `json:"display_name"`

	// TenancyOCID is the OCI tenancy OCID
	// (ocid1.tenancy.oc1..<unique_id>). The scanner uses this to
	// build the OCI ConfigurationProvider. NOTE: this is the OCI
	// cloud-side tenancy identifier, NOT the Squadron owner tenant
	// (see OwnerTenantID above).
	TenancyOCID string `json:"tenancy_ocid"`

	// UserOCID is the OCI user OCID
	// (ocid1.user.oc1..<unique_id>). Pairs with the API Signing Key
	// to authenticate OCI REST API calls — the public key is
	// registered against this user_ocid in the OCI Console.
	UserOCID string `json:"user_ocid"`

	// Fingerprint is the OCI API Signing Key fingerprint
	// (e.g. "xx:xx:xx:..."). The OCI Console returns this when the
	// operator uploads the public key. Pairs with SealedPrivateKey
	// to identify which key OCI should validate the request against.
	Fingerprint string `json:"fingerprint"`

	// SealedPrivateKey is the credstore-sealed RSA private key (PEM-
	// encoded). NEVER serialized to JSON, NEVER logged, NEVER
	// embedded in audit payloads, NEVER returned in HTTP responses.
	// The only sanctioned access path is credstore.UnsealOCIPrivateKey
	// called from the scanner. Mirrors the v0.89.31 webhook secret,
	// v0.89.46 GCP SA, and v0.89.51 Azure SP posture. Private key
	// bytes are the strongest credential type Squadron handles
	// (full asymmetric authentication material).
	SealedPrivateKey []byte `json:"-"`

	// Region is the OCI region (e.g. "us-phoenix-1"). REQUIRED for
	// OCI — unlike AWS/GCP/Azure where empty Region means "scan
	// all", OCI's API endpoints are regional so the scanner must
	// know which region to query. Slice 1 ships single-region per
	// connection; multi-region orchestrator is slice 2.
	Region string `json:"region"`

	// LearnFromAcceptedRecommendations is the v0.89.27/v0.89.28 IaC
	// flag mirrored onto the OCI shape (parallel to the v0.89.46
	// GCP and v0.89.51 Azure mirrors). Not load-bearing in slice 1
	// chunk 1 (no OCI-side PRs have merged yet), but the column
	// exists for consistency with the IaC-connected shape — when
	// chunk 5's proposer integration ships, this flag controls
	// whether OCI-side accepted-recommendation verdicts feed the
	// discovery proposer's prompt. Defaults to true on Create.
	LearnFromAcceptedRecommendations bool `json:"learn_from_accepted_recommendations"`

	// CreatedAt and UpdatedAt are stamped by the Store. Callers do
	// not supply these on Create / Update.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
