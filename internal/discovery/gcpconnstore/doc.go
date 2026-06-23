// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package gcpconnstore is the encrypted-at-rest substrate for GCP
// project connections. It persists the metadata Squadron needs to
// scan a GCP project for observability gaps and draft IaC PRs that
// close them.
//
// The package is the storage half of slice 1 chunk 1 of #667
// (docs/proposals/gcp-discovery-slice1.md). It is modeled on the
// iacconnstore package (internal/discovery/iacconnstore) — the same
// SQLite+memory backend split, the same UUID-stamping Create
// convention, the same defensive copy posture on the in-memory
// implementation. The two substrates feel like one design so reviews
// don't have to swap context.
//
// # What this package stores
//
//   - GCPConnection rows keyed by ID (a UUID), one per connected GCP
//     project. The substrate enforces no uniqueness on project_id —
//     operators may legitimately connect the same project twice with
//     different SAs (different role scopes for different audiences).
//   - The Service Account JSON key as an opaque sealed blob. Sealing
//     and unsealing happens through dedicated helpers in the
//     credstore package (SealGCPServiceAccount /
//     UnsealGCPServiceAccount) with a domain-tagged AAD that prevents
//     a sealed PAT or webhook secret blob from ever being mis-unsealed
//     here.
//
// # What this package does NOT store
//
//   - The scanner's output. Scan results live in the discovery
//     application store (snapshots, recommendations); this substrate
//     holds connection metadata only.
//   - The plaintext SA JSON. The substrate sees opaque sealed bytes
//     in and opaque sealed bytes out; only credstore.UnsealGCPServiceAccount
//     in the scanner ever touches plaintext.
//
// # Encryption
//
// The sealed SA JSON blob is produced by credstore.SealGCPServiceAccount,
// which uses the same AES-256-GCM Key (SQUADRON_SECRETS_KEY) as the
// PAT and webhook-secret paths but pins a distinct AAD
// "squadron.gcp_sa.v1". The domain separator means a sealed PAT can
// NEVER be mis-unsealed as an SA and vice versa — the GCM auth tag
// fails because it was computed under a different AAD.
//
// See docs/proposals/gcp-discovery-slice1.md §3 (credential model
// option A), §5 (storage), §11 (threat model) for the design and
// threat model.
package gcpconnstore
