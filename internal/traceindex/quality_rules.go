// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

// Span quality detection rule tables — see
// docs/proposals/span-quality-slice1.md §3.2 (required attributes)
// and §3.3 (placeholder values).
//
// Keeping the rules in a sibling file (rather than inline in
// quality.go) makes the chunk-2 proposer + the chunk-4 operator
// runbook easier to keep in sync: both reference the canonical tables
// and a single edit here ripples to the receiver hot path without a
// recompile of unrelated quality logic.

// requiredAttrsByTier names the attributes every span on a given tier
// MUST carry per §3.2. Compute has an additional "any one of"
// requirement (host.id / host.name / cloud.resource_id) handled by
// the firstMissingRequired host alternatives block — those keys are
// NOT in the per-tier list because the list is treated as
// "all-of-these-required".
var requiredAttrsByTier = map[string][]string{
	"compute": {"service.name", "cloud.provider", "cloud.account.id", "cloud.region"},
	"db":      {"service.name", "cloud.provider", "cloud.account.id", "db.system", "db.name"},
	"k8s":     {"service.name", "cloud.provider", "cloud.account.id", "k8s.cluster.name", "k8s.namespace.name", "k8s.pod.name"},
}

// computeHostAlternatives lists the host-identifying attributes any
// one of which satisfies the compute tier's host requirement.
// firstMissingRequired returns a special "|"-joined token when none
// of these are present so callers can render a useful error message.
var computeHostAlternatives = []string{"host.id", "host.name", "cloud.resource_id"}

// firstMissingRequired returns the first required attribute the
// supplied attrs map is missing for the given tier, or "" when every
// requirement is satisfied. The compute tier's host-any-of rule is
// applied AFTER the all-of rule so a span missing a base required
// attr reports that one first (more actionable than the host fan-out
// when neither base attrs nor host attrs are present).
//
// An unknown tier is treated as "no requirements" — returning "" so
// the caller does not count it as a missing-attrs span. Defensive:
// keeps a future tier addition from accidentally being marked as
// always-missing.
func firstMissingRequired(tier string, attrs map[string]string) string {
	required, ok := requiredAttrsByTier[tier]
	if !ok {
		return ""
	}
	for _, name := range required {
		if attrs[name] == "" {
			return name
		}
	}
	if tier == "compute" {
		hasAny := false
		for _, alt := range computeHostAlternatives {
			if attrs[alt] != "" {
				hasAny = true
				break
			}
		}
		if !hasAny {
			return "host.id|host.name|cloud.resource_id"
		}
	}
	return ""
}

// placeholdersByAttr is the §3.3 sentinel-value table. An attribute
// whose value matches one of these placeholders is counted as a
// mismatch. The empty-string case is handled by firstMissingRequired
// (a required attr that's empty counts as "missing" not "mismatch"),
// so the values here are all non-empty.
var placeholdersByAttr = map[string][]string{
	"host.name":        {"localhost", "127.0.0.1", "unknown_host"},
	"cloud.account.id": {"000000000000", "123456789012", "unknown"},
	"service.name":     {"unknown_service", "default-service"},
	"cloud.region":     {"unknown_region"},
}

// validCloudProviders is the §3.3 cloud.provider allowlist. A span
// with cloud.provider set to anything outside this set counts as a
// placeholder mismatch with attr="cloud.provider" and placeholder=the
// offending value. Empty cloud.provider is handled by the missing-
// attrs path (it's a required attr on every tier).
var validCloudProviders = map[string]struct{}{
	"aws":   {},
	"gcp":   {},
	"azure": {},
	"oci":   {},
}

// firstPlaceholder returns the first {attr, placeholder} pair the
// supplied attrs map matches against the §3.3 table, or two empty
// strings when no placeholder matches. The cloud.provider allowlist
// check runs AFTER the per-attr placeholder table so the more
// specific table-driven match wins when both apply.
//
// Map iteration order on placeholdersByAttr is non-deterministic by
// design — Observe counts mismatch per-span not per-attr, so the
// choice of "which placeholder we recorded into the detail surface"
// is non-load-bearing for the counter math. Tests that pin the
// recorded value use a placeholder-table with a single attr.
func firstPlaceholder(attrs map[string]string) (attr, placeholder string) {
	for attrName, placeholders := range placeholdersByAttr {
		v := attrs[attrName]
		if v == "" {
			continue
		}
		for _, p := range placeholders {
			if v == p {
				return attrName, p
			}
		}
	}
	if cp := attrs["cloud.provider"]; cp != "" {
		if _, ok := validCloudProviders[cp]; !ok {
			return "cloud.provider", cp
		}
	}
	return "", ""
}
