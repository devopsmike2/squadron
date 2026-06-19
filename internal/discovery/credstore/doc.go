// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package credstore is the encrypted-at-rest credential substrate for
// Squadron's universal cloud discovery. It is the only place trust-policy
// metadata for connected AWS accounts (role ARN, ExternalId, display
// name, region) is persisted.
//
// What this package stores
//
//   - The customer's AWS role ARN (not a secret per se, but identifies
//     the target account).
//   - The deployment's per-account ExternalId (effectively a secret —
//     must not leak; encrypted at rest, never logged).
//   - Per-account metadata: display name, primary region, timestamps.
//
// What this package does NOT store
//
//   - AWS access keys / secret keys (Squadron never receives them).
//   - STS tokens (those live in memory only, dropped after each scan).
//   - Customer telemetry or business data.
//
// # Encryption
//
// AES-256-GCM at rest with the data-encryption key sourced from the
// SQUADRON_SECRETS_KEY environment variable (base64-encoded 32 bytes).
// On Store construction the key is validated; a missing or wrong-length
// key returns a hard error. There is no fallback to a default key, a
// zero key, or plaintext — failing loud is the entire point of the
// substrate.
//
// # Audit
//
// Every read (Get and List) emits a `discovery.role_assumed` audit
// event through the provided AuditRecorder. The payload carries
// account_id and role_arn; the ExternalId is never included in the
// payload and is never logged anywhere in this package.
//
// See docs/universal-discovery-design.md "Security architecture >
// Credential substrate" for the full design and threat model.
package credstore
