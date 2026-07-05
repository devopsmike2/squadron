// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"context"
	"time"
)

// Provider identifies the cloud (or on-prem) backing a CloudConnection.
// The architecture is multi-cloud from day one per the universal
// discovery design doc; slice 1's scanner implementation is AWS only,
// but the substrate stores any provider's connection without schema
// changes.
type Provider string

const (
	// ProviderAWS identifies an AWS account connected via IAM
	// assume-role. Credentials shape is AWSCredentials (see aws.go).
	ProviderAWS Provider = "aws"

	// ProviderGCP identifies a GCP project connected via workload
	// identity federation. Implementation lands in slice 4.
	ProviderGCP Provider = "gcp"

	// ProviderAzure identifies an Azure subscription. Implementation
	// lands in slice 5.
	ProviderAzure Provider = "azure"

	// ProviderOCI identifies an Oracle Cloud Infrastructure tenancy
	// connected via API Signing Key (RSA keypair). The scanner lives
	// in internal/discovery/oci; the connection rows live in the
	// ociconnstore substrate (see docs/proposals/oci-discovery-
	// slice1.md). v0.89.57 (#682 slice 1 chunk 2).
	ProviderOCI Provider = "oci"

	// ProviderOnPrem identifies an on-prem site whose inventory
	// arrives via the on-prem connector daemon. Implementation lands
	// in slice 6.
	ProviderOnPrem Provider = "onprem"
)

// ConnectionType discriminates how Squadron learns about a connection's
// inventory. The same substrate row carries cloud-API-discovered
// accounts and agent-polled on-prem sites; the discovery code branches
// on this value to pick the right scanner.
type ConnectionType string

const (
	// ConnectionAPIDiscovered means Squadron calls the provider's
	// cloud APIs (read-only) to enumerate inventory. The AWS, GCP,
	// and Azure providers use this connection type.
	ConnectionAPIDiscovered ConnectionType = "api_discovered"

	// ConnectionAgentPolled means an on-prem daemon reports inventory
	// to Squadron. Lands with slice 6.
	ConnectionAgentPolled ConnectionType = "agent_polled"

	// ConnectionManualImport means the operator supplied inventory
	// directly (CSV, JSON, etc.). Used for air-gapped or POC
	// onboarding paths.
	ConnectionManualImport ConnectionType = "manual_import"
)

// CloudConnection is the substrate row for one connected
// account / project / subscription / site. Provider-specific
// authentication material lives in the Credentials field as encrypted
// bytes — each provider's scanner unmarshals its own shape (see
// AWSCredentials in aws.go for the slice-1 example). The substrate
// itself never inspects the cleartext credentials, so adding a new
// provider does not require any change here.
//
// AccountID is the primary identifier across providers: account_id for
// AWS, project_id for GCP, subscription_id for Azure, site_id for
// on-prem. Treating the primary key uniformly across providers is the
// "multi-account from day one" decision from the design doc.
//
// Regions is multi-region native. Slice 1's scanner emits a single
// entry; slice 3's scheduled scans iterate the list. The column is a
// JSON-encoded string in SQLite so the schema does not change between
// slices.
type CloudConnection struct {
	// AccountID is the provider-native primary identifier:
	// account_id (aws), project_id (gcp), subscription_id (azure),
	// site_id (onprem). Unique per row.
	AccountID string `json:"account_id"`

	// Provider names the cloud or on-prem source for this row.
	Provider Provider `json:"provider"`

	// ConnectionType discriminates how inventory is fetched.
	ConnectionType ConnectionType `json:"connection_type"`

	// DisplayName is the operator-friendly label rendered in the UI
	// alongside the AccountID.
	DisplayName string `json:"display_name"`

	// Regions is the list of regions in scope for scans of this
	// connection. Multi-region native — slice 1 ships single-entry
	// lists, slice 3 iterates.
	Regions []string `json:"regions"`

	// Credentials is the provider-specific authentication material,
	// already-encrypted by the active SecretsBackend. The substrate
	// stores opaque bytes; each provider's scanner is responsible for
	// decrypting and parsing (see MarshalAWSCredentials /
	// UnmarshalAWSCredentials in aws.go).
	//
	// Storing pre-encrypted bytes here (rather than a typed struct)
	// is what lets the same substrate hold AWS role-ARN+ExternalID
	// today, GCP workload identity tomorrow, and an on-prem shared
	// secret in slice 6 without schema changes.
	Credentials []byte `json:"-"`

	// CredentialsNonce is the AEAD nonce used when sealing
	// Credentials. Stored alongside the ciphertext because the
	// backend cannot decrypt without it. Never logged.
	CredentialsNonce []byte `json:"-"`

	// TenantID is the Squadron tenant that owns this connection (ADR
	// 0013 §D6-b). Stamped at StoreConnection from the authenticated
	// actor's tenant so the discovery rescan scheduler can scope its
	// discovery_scans store writes to the owning tenant — a scheduled
	// rescan runs under WithSystemContext and carries no operator
	// identity. Resolves to identity.DefaultTenant ("default") in the
	// OSS single-tenant build — inert until the enterprise
	// TenantResolver derives a real tenant from the identity. The UPSERT
	// preserves this column on update (ownership is immutable); it is
	// only written on the initial INSERT.
	TenantID string `json:"tenant_id"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListFilter narrows a ListConnections result. An empty filter returns
// every row across providers. Provider filtering is the first knob;
// further filters (connection_type, region) can be added when a slice
// needs them without changing existing call sites.
type ListFilter struct {
	// Provider, when non-empty, restricts the result to rows whose
	// Provider field matches. Empty means "all providers."
	Provider Provider
}

// Store is the credential substrate interface. Implementations encrypt
// Credentials at rest through the SecretsBackend they were configured
// with, and emit a discovery.<provider>.connection_read audit event on
// every read.
//
// All methods are safe for concurrent use; the SQLite-backed default
// serializes writes through the underlying database connection pool.
type Store interface {
	// StoreConnection inserts or updates a connection record. The
	// Credentials field must already be encrypted (callers use the
	// per-provider Marshal helpers, e.g. MarshalAWSCredentials, to
	// produce the ciphertext). CreatedAt is preserved on update;
	// UpdatedAt is always stamped to now.
	StoreConnection(ctx context.Context, conn CloudConnection) error

	// GetConnection returns the connection record for the given
	// account ID. Credentials and CredentialsNonce are populated
	// with the on-disk ciphertext + nonce; the caller decrypts via
	// the per-provider Unmarshal helper. Returns (nil, nil) if no
	// row matches. Emits a discovery.<provider>.connection_read
	// audit event on every successful read.
	GetConnection(ctx context.Context, accountID string) (*CloudConnection, error)

	// ListConnections returns every connection record that matches
	// filter (or all rows when filter is the zero value). Each row's
	// Credentials field holds the on-disk ciphertext. Emits one
	// discovery.<provider>.connection_read audit event per row.
	ListConnections(ctx context.Context, filter ListFilter) ([]*CloudConnection, error)

	// DeleteConnection removes the row for the given account ID.
	// Idempotent: deleting a non-existent row is not an error.
	DeleteConnection(ctx context.Context, accountID string) error

	// Close releases the underlying database handle. Subsequent calls
	// to other methods return an error.
	Close() error
}
