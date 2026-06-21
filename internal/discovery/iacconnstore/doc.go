// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package iacconnstore is the encrypted-at-rest substrate for IaC
// (Infrastructure-as-Code) repository connections. It persists the
// metadata Squadron needs to translate a recommendation card into a
// pull request against the operator's terraform repository.
//
// The package is the storage half of slice 1 of #603
// (docs/proposals/603-connect-iac-repo.md). It is modeled on the
// credstore package (internal/discovery/credstore) — the AES-GCM
// substrate, the SQLite+memory backend split, and the "opaque
// ciphertext in, opaque ciphertext out" contract are deliberately
// parallel so the two substrates feel like one design.
//
// # What this package stores
//
//   - IaCConnection rows keyed by ConnectionID (a UUID), one per
//     connected repo. A (provider, repo_full_name) unique index
//     enforces the slice-1 "one connection per deployment per repo"
//     rule at the database layer.
//   - The operator-declared PlacementMap, a list of
//     (provider, resource_kind, file_path) entries that tells the
//     PR builder where to append each kind of snippet.
//   - The authentication material as an opaque encrypted blob. Slice 1
//     ships GitHub PAT only (see MarshalGitHubPATCreds); slice 2 adds a
//     GitHub App path with the same shape.
//
// # What this package does NOT store
//
//   - The PR history. The operator's GitHub repo is the source of
//     truth for what PRs have been opened.
//   - The snippet content. Audit rows must not scale with snippet size
//     (same rule as discovery.aws.recommendations_generated).
//
// # Encryption
//
// The credential blob is sealed with the same AES-256-GCM Key as
// credstore. Callers obtain a *credstore.Key via the existing
// LoadKeyFromEnv path (SQUADRON_SECRETS_KEY) and pass it to
// MarshalGitHubPATCreds / UnmarshalGitHubPATCreds. The substrate
// itself never sees plaintext.
//
// One deviation from credstore: this package stores nonce + ciphertext
// as a single opaque CredCiphertext blob rather than two columns.
// The marshal helpers pack [12-byte nonce][ciphertext] and the
// unmarshal helpers split it back. The single-blob shape matches the
// spec for slice 1 and keeps room for slice 2's GitHub App path
// (which carries a different plaintext shape but the same sealed
// transport).
//
// See docs/proposals/603-connect-iac-repo.md §4 ("Trust model") and
// §6 ("Operator-supplied placement map") for the design and threat
// model.
package iacconnstore
