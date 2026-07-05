// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"strings"
	"sync"
)

// reservedTokenLabels is the process-wide set of token labels that the public
// token-create API (POST /api/v1/auth/tokens) refuses to mint. It is EMPTY in
// OSS — the seam is inert — so the OSS default behavior (any label allowed) is
// unchanged. The enterprise wire calls SetReservedTokenLabels at startup with
// its break-glass bootstrap labels (the same set fed to the RBAC authorizer's
// WithBootstrapLabels), so an `auth:write` holder can no longer mint a
// `bootstrap`-labeled token and self-escalate to cross-tenant admin (ADR 0013
// D1). Mirrors middleware.SetAuthorizer / sqlite.SetStrictTenantScoping.
//
// The reserved set gates the PUBLIC handler ONLY. The internal first-start
// bootstrapAuthToken calls AuthService.Issue directly (not through the HTTP
// handler), so it can still mint the bootstrap label — otherwise a freshly
// enabled enterprise binary would lock the operator out before any role exists.
var (
	reservedTokenLabelsMu sync.RWMutex
	reservedTokenLabels   = map[string]struct{}{}
)

// reservedTokenLabelPrefixes is the process-wide set of label PREFIXES the
// public token-create API refuses to mint (ADR 0014 D9). Exact whole-string
// matching (reservedTokenLabels) is not enough for the identity labels: the
// enterprise OIDC/SCIM mint encodes the subject in the label (`oidc:<sub>`,
// `scim:<externalId>`), so the reserved SPACE is a prefix, not a single string.
// Without this, an `auth:write` holder could POST a `oidc:foo` label through
// the public handler and forge an OIDC identity. EMPTY in OSS (inert); the
// enterprise wire registers `oidc:` / `scim:`. The internal OIDC/SCIM mint
// calls AuthService.Issue directly (bypasses the public handler, like the
// bootstrap label), so it is unaffected.
var (
	reservedTokenLabelPrefixesMu sync.RWMutex
	reservedTokenLabelPrefixes   []string
)

// SetReservedTokenLabelPrefixes installs the process-wide reserved-label-prefix
// set the public token-create handler consults. Prefixes are matched
// case-insensitively against the trimmed caller-supplied label. Called once
// from the enterprise wire at startup; OSS never calls it, leaving the set
// empty (inert). Not safe for concurrent use with in-flight requests — call it
// during startup, before serving traffic. A nil or empty argument clears the
// set.
func SetReservedTokenLabelPrefixes(prefixes []string) {
	reservedTokenLabelPrefixesMu.Lock()
	defer reservedTokenLabelPrefixesMu.Unlock()
	next := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		next = append(next, p)
	}
	reservedTokenLabelPrefixes = next
}

// SetReservedTokenLabels installs the process-wide reserved-token-label set the
// public token-create handler consults. Labels are matched case-insensitively
// against the trimmed caller-supplied label (so `Bootstrap` can't bypass
// `bootstrap`). Called once from the enterprise wire at startup; OSS never
// calls it, leaving the set empty (inert). Not safe for concurrent use with
// in-flight requests — call it during startup, before serving traffic. A nil or
// empty argument clears the set.
func SetReservedTokenLabels(labels []string) {
	reservedTokenLabelsMu.Lock()
	defer reservedTokenLabelsMu.Unlock()
	next := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" {
			continue
		}
		next[l] = struct{}{}
	}
	reservedTokenLabels = next
}

// IsReservedTokenLabel reports whether the given label (trimmed,
// case-insensitive) is in the reserved set. Returns false when the set is empty
// (OSS default), so the check is inert unless the enterprise wire populated it.
func IsReservedTokenLabel(label string) bool {
	key := strings.ToLower(strings.TrimSpace(label))
	if key == "" {
		return false
	}
	reservedTokenLabelsMu.RLock()
	_, ok := reservedTokenLabels[key]
	reservedTokenLabelsMu.RUnlock()
	if ok {
		return true
	}
	// ADR 0014 D9 — also reject any label carrying a reserved prefix
	// (`oidc:`, `scim:` enterprise-side) so the public handler can't be used
	// to forge an OIDC/SCIM identity label. Inert in OSS (empty prefix set).
	reservedTokenLabelPrefixesMu.RLock()
	defer reservedTokenLabelPrefixesMu.RUnlock()
	for _, p := range reservedTokenLabelPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}
