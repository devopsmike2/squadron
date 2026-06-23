// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package azureconnstore is the encrypted-at-rest substrate for Azure
// subscription connections. It persists the metadata Squadron needs
// to scan an Azure subscription for observability gaps and draft IaC
// PRs that close them.
//
// The package is the storage half of slice 1 chunk 1 of #674
// (docs/proposals/azure-discovery-slice1.md). It is modeled on the
// gcpconnstore package (internal/discovery/gcpconnstore) — the same
// SQLite+memory backend split, the same UUID-stamping Create
// convention, the same defensive copy posture on the in-memory
// implementation. The three substrates (iacconnstore, gcpconnstore,
// azureconnstore) feel like one design so reviews don't have to swap
// context.
//
// # What this package stores
//
//   - AzureConnection rows keyed by ID (a UUID), one per connected
//     Azure subscription. The substrate enforces no uniqueness on
//     subscription_id — operators may legitimately connect the same
//     subscription twice with different Service Principals (different
//     role scopes for different audiences).
//   - The Service Principal client_secret as an opaque sealed blob.
//     Sealing and unsealing happens through dedicated helpers in the
//     credstore package (SealAzureClientSecret /
//     UnsealAzureClientSecret) with a domain-tagged AAD that prevents
//     a sealed PAT, webhook secret, or GCP SA JSON blob from ever
//     being mis-unsealed here.
//
// # What this package does NOT store
//
//   - The scanner's output. Scan results live in the discovery
//     application store (snapshots, recommendations); this substrate
//     holds connection metadata only.
//   - The plaintext SP client_secret. The substrate sees opaque
//     sealed bytes in and opaque sealed bytes out; only
//     credstore.UnsealAzureClientSecret in the scanner ever touches
//     plaintext.
//
// # Encryption
//
// The sealed client_secret blob is produced by
// credstore.SealAzureClientSecret, which uses the same AES-256-GCM
// Key (SQUADRON_SECRETS_KEY) as the PAT, webhook-secret, and GCP SA
// paths but pins a distinct AAD "squadron.azure_client_secret.v1".
// The domain separator means a sealed PAT, webhook secret, or GCP SA
// blob can NEVER be mis-unsealed as an Azure SP client_secret and
// vice versa — the GCM auth tag fails because it was computed under
// a different AAD.
//
// # Own database file
//
// Like gcpconnstore, this substrate owns its own SQLite file
// (azureconnstore.db) so its migrations are independent of credstore,
// iacconnstore, gcpconnstore, and the application store. Operators
// can wipe Azure connection state without touching any other
// substrate.
//
// See docs/proposals/azure-discovery-slice1.md §3 (credential model
// option A), §5 (storage), §12 (threat model) for the design and
// threat model.
package azureconnstore
