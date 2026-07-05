// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcpconnstore

import (
	"time"
)

// Provider names the cloud the connection points at. Slice 1 of #667
// ships GCP only; the constant lives here so callers don't string-type
// the discriminator and so future provider extensions (Azure, etc.)
// follow the same shape.
const (
	// ProviderGCP is the only supported provider in slice 1 of the
	// universal-observability arc beyond AWS (see
	// docs/proposals/gcp-discovery-slice1.md §1, §10).
	ProviderGCP = "gcp"
)

// GCPConnection represents a single GCP project connection that
// Squadron can scan. Service Account JSON is stored sealed via the
// credstore substrate with domain-tagged AAD "squadron.gcp_sa.v1"
// to prevent cross-domain confusion with PAT or webhook secret
// sealed bytes (parallel to v0.89.31's per-connection webhook
// secret posture).
//
// Slice 1 ships single-region (or empty=scan-all). Multi-region
// orchestration is slice 2.
//
// SealedSA carries the on-disk sealed bytes. It is NEVER serialized
// to JSON (the json:"-" tag suppresses every marshal path) so an
// accidental List response, audit payload, or log line cannot leak
// the encrypted credential blob. Decryption is the scanner's job
// via credstore.UnsealGCPServiceAccount — the only sanctioned
// access path.
type GCPConnection struct {
	// ID is the substrate's primary key, a UUIDv4 stamped by Create.
	// Callers do not supply this on Create.
	ID string `json:"id"`

	// DisplayName is the operator-supplied human-readable label for
	// the connection. Surfaces in the discovery UI's connection list
	// and audit timeline.
	DisplayName string `json:"display_name"`

	// ProjectID is the GCP project the connection scans. Per design
	// doc §12 Q4, slice 1 uses project ID (operator-readable, stable)
	// exclusively rather than the numeric project number.
	ProjectID string `json:"project_id"`

	// SealedSA is the credstore-sealed Service Account JSON. NEVER
	// serialized to JSON, NEVER logged, NEVER embedded in audit
	// payloads, NEVER returned in HTTP responses. The only sanctioned
	// access path is credstore.UnsealGCPServiceAccount called from
	// the scanner. Mirrors the v0.89.31 webhook secret posture.
	SealedSA []byte `json:"-"`

	// Region is the GCP region filter. Empty string means "scan all
	// regions visible to the SA"; a non-empty value like "us-central1"
	// scopes the scan. Slice 1 ships this as single-value; slice 2
	// adds multi-region orchestration.
	Region string `json:"region"`

	// LearnFromAcceptedRecommendations is the v0.89.27/v0.89.28 IaC
	// flag mirrored onto the GCP shape. Not load-bearing in slice 1
	// (no GCP-side PRs have merged yet), but the column exists for
	// consistency with the IaC-connected shape — when chunk 5's
	// proposer integration ships, this flag controls whether GCP-side
	// accepted-recommendation verdicts feed the discovery proposer's
	// prompt. Defaults to true on Create.
	LearnFromAcceptedRecommendations bool `json:"learn_from_accepted_recommendations"`

	// TenantID is the Squadron tenant that owns this connection (ADR
	// 0013 §D6-b). Stamped at Create from the authenticated actor's
	// tenant so the discovery rescan scheduler can scope its
	// discovery_scans store writes to the owning tenant — a scheduled
	// rescan runs under WithSystemContext and carries no operator
	// identity. Resolves to identity.DefaultTenant ("default") in the
	// OSS single-tenant build — inert until the enterprise
	// TenantResolver derives a real tenant from the identity.
	TenantID string `json:"tenant_id"`

	// CreatedAt and UpdatedAt are stamped by the Store. Callers do
	// not supply these on Create / Update.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
