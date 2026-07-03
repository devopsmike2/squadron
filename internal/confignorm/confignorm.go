// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package confignorm is the single source of truth for canonicalizing
// OpenTelemetry collector config content before it is compared, hashed, or
// diffed anywhere in Squadron.
//
// Before this package existed the same normalization was reimplemented at
// three sites that MUST agree, and two of them silently diverged:
//
//   - the rollout preview diff (internal/configdiff) trimmed only trailing
//     newlines, so a target that differed from the current config by nothing
//     but leading/trailing whitespace rendered as a real change;
//   - drift detection (internal/services) trimmed ALL surrounding whitespace,
//     so the very same pair was reported as "synced" once delivered.
//
// The result: an operator saw "N lines changed" in the rollout preview, ran
// it, and drift immediately reported the agent as in-sync — a change that was
// a semantic no-op. The preview's own comment even claimed it "matches the
// drift-detection normalization"; it did not. Routing every site through the
// functions here makes that guarantee structural rather than a stale comment.
package confignorm

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Normalize canonicalizes config content for content-equality comparison:
// surrounding whitespace is trimmed and CRLF line endings are folded to LF, so
// two configs that differ only in incidental formatting (a pasted Windows
// snippet, a stray leading blank line, trailing spaces) compare equal. It does
// NOT touch interior indentation, which is semantically significant in YAML.
func Normalize(content string) string {
	normalized := strings.TrimSpace(content)
	normalized = strings.ReplaceAll(normalized, "\r\n", "\n")
	return normalized
}

// Hash returns the canonical content hash used as a config fingerprint (the
// ConfigHash persisted on configs/rollout plans and the effective-config hash
// computed during drift detection). It is the lowercase hex SHA-256 of the
// normalized content. Empty (or whitespace-only) content hashes to the empty
// string sentinel: an agent with no effective config is handled explicitly by
// callers as "no effective config", never as a content match against a real
// hash, so the sentinel keeps those two states from ever colliding.
func Hash(content string) string {
	normalized := Normalize(content)
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}
