// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package aicredstore is the app-global sealed store for the single
// AI-assist provider API key an operator can set through the settings
// UI/API (PUT/DELETE /api/v1/ai/credential).
//
// Unlike credstore (per-cloud-connection secrets) and iacconnstore
// (per-repo connections), this substrate holds exactly ONE row: the
// deployment-wide AI provider credential. It is deliberately NOT part
// of the tenant-scoped applicationstore — ADR 0043 strict tenant
// scoping does not apply, because the AI key is a single app-level
// setting shared by the whole control plane, not tenant-owned data.
//
// The store is intentionally "dumb": it persists the SEALED ciphertext
// + nonce produced by the caller (which seals via the credstore.Key
// loaded from SQUADRON_SECRETS_KEY) and never performs crypto itself.
// The plaintext key never touches this package. Two dialects are
// provided — SQLite (the zero-dependency default) and Postgres (the
// opt-in HA backend, decisions/0033) — selected to match whichever
// backend the application store already uses.
package aicredstore
