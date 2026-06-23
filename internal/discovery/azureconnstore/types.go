// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azureconnstore

import (
	"time"
)

// Provider names the cloud the connection points at. Slice 1 of #674
// ships Azure only; the constant lives here so callers don't
// string-type the discriminator and so future provider extensions
// follow the same shape used in iacconnstore / gcpconnstore.
const (
	// ProviderAzure is the only supported provider in slice 1 of the
	// Azure arc (see docs/proposals/azure-discovery-slice1.md §1, §10).
	ProviderAzure = "azure"
)

// AzureConnection represents a single Azure subscription connection
// that Squadron can scan. Service Principal client_secret is stored
// sealed via the credstore substrate with domain-tagged AAD
// "squadron.azure_client_secret.v1" to prevent cross-domain confusion
// with PAT, webhook secret, or GCP SA sealed bytes (parallel to
// v0.89.31's per-connection webhook secret posture and v0.89.46's
// GCP SA posture). Azure is the THIRD sealed credential type — the
// defense-in-depth posture extends across the full cross-product.
//
// Slice 1 ships single-subscription per connection (mirrors GCP
// single-project). Multi-subscription orchestration is slice 2.
//
// SealedSecret carries the on-disk sealed bytes. It is NEVER
// serialized to JSON (the json:"-" tag suppresses every marshal path)
// so an accidental List response, audit payload, or log line cannot
// leak the encrypted credential blob. Decryption is the scanner's
// job via credstore.UnsealAzureClientSecret — the only sanctioned
// access path.
type AzureConnection struct {
	// ID is the substrate's primary key, a UUIDv4 stamped by Create.
	// Callers do not supply this on Create.
	ID string `json:"id"`

	// DisplayName is the operator-supplied human-readable label for
	// the connection. Surfaces in the discovery UI's connection list
	// and audit timeline.
	DisplayName string `json:"display_name"`

	// TenantID is the Azure AD tenant the Service Principal lives in
	// (UUID format). The scanner uses this to build the
	// ClientSecretCredential.
	TenantID string `json:"tenant_id"`

	// SubscriptionID is the Azure subscription the connection scans
	// (UUID format). Slice 1 ships single-subscription per connection;
	// multi-subscription orchestration is slice 2.
	SubscriptionID string `json:"subscription_id"`

	// ClientID is the Service Principal app registration ID (UUID
	// format). Pairs with SealedSecret to authenticate Azure REST API
	// calls.
	ClientID string `json:"client_id"`

	// SealedSecret is the credstore-sealed SP client_secret. NEVER
	// serialized to JSON, NEVER logged, NEVER embedded in audit
	// payloads, NEVER returned in HTTP responses. The only sanctioned
	// access path is credstore.UnsealAzureClientSecret called from
	// the scanner. Mirrors the v0.89.31 webhook secret and v0.89.46
	// GCP SA posture.
	SealedSecret []byte `json:"-"`

	// Location is the Azure region filter. Empty string means "scan
	// all locations visible to the SP"; a non-empty value like
	// "eastus" scopes the scan. Slice 1 ships this as single-value;
	// slice 2 adds multi-location orchestration.
	Location string `json:"location"`

	// LearnFromAcceptedRecommendations is the v0.89.27/v0.89.28 IaC
	// flag mirrored onto the Azure shape (parallel to the v0.89.46
	// GCP mirror). Not load-bearing in slice 1 (no Azure-side PRs
	// have merged yet), but the column exists for consistency with
	// the IaC-connected shape — when chunk 5's proposer integration
	// ships, this flag controls whether Azure-side accepted-
	// recommendation verdicts feed the discovery proposer's prompt.
	// Defaults to true on Create.
	LearnFromAcceptedRecommendations bool `json:"learn_from_accepted_recommendations"`

	// CreatedAt and UpdatedAt are stamped by the Store. Callers do
	// not supply these on Create / Update.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
