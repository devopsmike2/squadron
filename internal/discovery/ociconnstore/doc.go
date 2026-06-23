// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package ociconnstore is the encrypted-at-rest substrate for OCI
// (Oracle Cloud) tenancy connections. It persists the metadata
// Squadron needs to scan an OCI tenancy for observability gaps and
// draft IaC PRs that close them.
//
// The package is the storage half of slice 1 chunk 1 of #681
// (docs/proposals/oci-discovery-slice1.md). It is modeled on the
// azureconnstore package (internal/discovery/azureconnstore) — the
// same SQLite+memory backend split, the same UUID-stamping Create
// convention, the same defensive copy posture on the in-memory
// implementation. The four substrates (iacconnstore, gcpconnstore,
// azureconnstore, ociconnstore) feel like one design so reviews
// don't have to swap context.
//
// # What this package stores
//
//   - OCIConnection rows keyed by ID (a UUID), one per connected OCI
//     tenancy. The substrate enforces no uniqueness on tenancy_ocid
//     — operators may legitimately connect the same tenancy twice
//     with different users (different role scopes for different
//     audiences).
//   - The API Signing Key private key (RSA, PEM-encoded) as an
//     opaque sealed blob. Sealing and unsealing happens through
//     dedicated helpers in the credstore package
//     (SealOCIPrivateKey / UnsealOCIPrivateKey) with a domain-tagged
//     AAD that prevents a sealed PAT, webhook secret, GCP SA JSON,
//     or Azure SP client_secret blob from ever being mis-unsealed
//     here.
//
// # What this package does NOT store
//
//   - The scanner's output. Scan results live in the discovery
//     application store (snapshots, recommendations); this substrate
//     holds connection metadata only.
//   - The plaintext RSA private key. The substrate sees opaque
//     sealed bytes in and opaque sealed bytes out; only
//     credstore.UnsealOCIPrivateKey in the scanner ever touches
//     plaintext. Private key bytes are the strongest credential type
//     Squadron handles (full asymmetric authentication material);
//     the never-log / never-embed-in-audit / never-echo invariants
//     are non-negotiable.
//
// # Encryption
//
// The sealed private key blob is produced by
// credstore.SealOCIPrivateKey, which uses the same AES-256-GCM Key
// (SQUADRON_SECRETS_KEY) as the PAT, webhook-secret, GCP SA, and
// Azure SP paths but pins a distinct AAD
// "squadron.oci_signing_key.v1". The domain separator means a
// sealed PAT, webhook secret, GCP SA JSON, or Azure SP client_secret
// blob can NEVER be mis-unsealed as an OCI private key and vice
// versa — the GCM auth tag fails because it was computed under a
// different AAD.
//
// # Own database file
//
// Like azureconnstore, this substrate owns its own SQLite file
// (ociconnstore.db) so its migrations are independent of credstore,
// iacconnstore, gcpconnstore, azureconnstore, and the application
// store. Operators can wipe OCI connection state without touching
// any other substrate.
//
// See docs/proposals/oci-discovery-slice1.md §3 (credential model
// option A — API Signing Key), §5 (storage), §12 (threat model) for
// the design and threat model.
package ociconnstore
