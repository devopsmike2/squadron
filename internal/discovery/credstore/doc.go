// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package credstore is the encrypted-at-rest credential substrate for
// Squadron's universal cloud discovery. It is the only place
// provider-typed connection metadata (account/project/subscription/
// site id, display name, regions, credentials) is persisted.
//
// The substrate is multi-cloud from day one per
// docs/universal-discovery-design.md "Decisions locked in this
// revision". Slice 1 ships the AWS scanner implementation; the
// substrate stores any provider's connection without schema changes.
//
// # What this package stores
//
//   - CloudConnection rows keyed by AccountID, with Provider /
//     ConnectionType discriminators.
//   - Per-provider authentication material as an opaque encrypted
//     blob plus its AEAD nonce. AWS uses RoleARN + ExternalID (see
//     aws.go); future providers store whatever their auth mechanism
//     requires.
//   - Operator-set metadata: display name, regions, timestamps.
//
// # What this package does NOT store
//
//   - Long-lived cloud access keys / secret keys. The substrate is
//     for trust-policy / federation material only — short-lived STS /
//     OAuth tokens live in memory and are dropped after each scan.
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
// The encryption primitive is exposed via the SecretsBackend interface
// so the Compliance Pack can plug in Vault / AWS Secrets Manager /
// GCP Secret Manager implementations without schema changes. The OSS
// edition's SQLiteSecretsBackend wraps SQUADRON_SECRETS_KEY.
//
// # Audit
//
// Every read (Get and List) emits a
// discovery.<provider>.connection_read event through the provided
// AuditRecorder. The payload carries account_id, provider,
// connection_type, display_name, and regions; the credentials bytes
// are never included in the payload and are never logged anywhere in
// this package.
//
// See docs/universal-discovery-design.md "Security architecture >
// Credential substrate" for the full design and threat model.
package credstore
