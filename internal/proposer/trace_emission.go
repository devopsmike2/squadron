// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/proposer/iacpicker"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// TraceEmissionInventoryRow is the per-row projection the trace-emission
// detection branch reads. Carries the four signals the §3 rule needs:
// the (provider, tier) routing pair, the primitive_enabled bit, the
// LastSeenAt annotation from trace integration slice 1 (v0.89.77), plus
// the recommendation_id used to consult the operator-set exclusion
// table.
//
// The shape is intentionally minimal — the wiring layer projects from
// scanner.ComputeInstanceSnapshot / DatabaseInstanceSnapshot /
// ClusterSnapshot at the API handler boundary so this package doesn't
// import the scanner.
type TraceEmissionInventoryRow struct {
	// RecommendationID is the deterministic identifier the discovery
	// proposer will assign when emitting the trace-emission
	// recommendation. Used to consult ExclusionStore so a row the
	// operator already said "don't propose this again" on stays
	// suppressed.
	RecommendationID string

	// Provider is one of "aws" / "gcp" / "azure" / "oci". Routes to
	// the matching kind via traceEmissionKindFor.
	Provider string

	// Tier is one of "compute" / "db" / "k8s". Same routing role as
	// Provider; combined they select one of the 12 trace-emission kinds.
	Tier string

	// PrimitiveEnabled is the slice 1 detection axis: HasOTel=true for
	// compute, PerformanceInsightsEnabled (et al) for db, the per-cloud
	// addon flag for k8s. The §3 rule fires only when this is true AND
	// LastSeenAt is stale; rows with the primitive off route to the
	// existing per-tier kinds via the discovery proposer.
	PrimitiveEnabled bool

	// LastSeenAt — slice 1 chunk 4 (v0.89.77) annotation. Nil means
	// "never observed"; non-nil is the most recent trace timestamp
	// from Squadron's traceindex. Stale = nil OR more than 24h ago.
	LastSeenAt *time.Time

	// ResourceTFName is the operator's best-effort Terraform resource
	// name (e.g. "prod_orders"). Empty when the IaC introspection
	// couldn't classify; the picker's snippet falls back to "<name>".
	ResourceTFName string

	// ResourceID is the canonical resource identifier the
	// recommendation surfaces in its UI. Threaded into the audit
	// payload so the operator sees which row Squadron flagged.
	ResourceID string

	// Region is the per-row region. Surfaced on the draft so the
	// downstream RecommendationDraft.Scope is well-formed.
	Region string
}

// TraceEmissionScope is the (connection, account/scope, region) tuple
// used to consult the exclusion store. Matches the §6 selection
// algorithm's scope-tuple shape from the verdict learning loop.
type TraceEmissionScope struct {
	ConnectionID string
	ScopeID      string // account_id / project_id / subscription_id / tenancy_ocid
	Region       string
}

// TraceEmissionRecommendationDraft is the per-row output of the
// detection branch. The wiring layer projects this into a
// recommendations.Recommendation (the wire shape the UI consumes); the
// fields here are the ones the chunk 2/3 conversion needs.
//
// Note: this struct is INTENTIONALLY separate from
// recommendations.Recommendation. Importing that package from
// internal/proposer would pull a circular dependency through the
// services layer; the boundary projection lives at the handler.
type TraceEmissionRecommendationDraft struct {
	Kind             string
	RecommendationID string
	Reasoning        string
	Terraform        string
	ScopeID          string
	Region           string
	ResourceID       string
	PickedPattern    iacpicker.PickedPattern
}

// TraceEmissionExclusionStore is the slice of ApplicationStore the
// detection branch consults to honor operator-set "Don't propose this
// again" verdicts. Mirrors DiscoveryVerdictStore's interface posture
// (and is satisfied by the same backing applicationstore.ApplicationStore
// implementation) so production wiring can pass the same store.
type TraceEmissionExclusionStore interface {
	ListExcludedRecommendations(
		ctx context.Context,
		connectionID, accountID, region string,
		limit int,
	) ([]applicationstore.ExcludedRecommendation, error)
}

// CheckTraceEmissionGap is the §3 detection branch the discovery
// proposer flow runs ALONGSIDE the existing primitive-enablement kinds.
// For each inventory row that has the primitive enabled but stale (or
// missing) trace emission, emits a draft for the matching
// trace-emission-* kind.
//
// Returns nil when the row is below threshold (primitive off, fresh
// emission, excluded by the operator, unrouted (provider, tier) pair).
//
// iacContentReader is a best-effort callback the caller threads in —
// when the IaC repo is available, it returns the raw HCL of the file
// the recommendation targets; when not, it returns "" and the picker
// falls back to the documented default pattern. The signature lets the
// production wiring inject an iacconnstore reader without forcing this
// package to import iacconnstore.
//
// Per §3:
//
//	inventory_row.primitive_enabled == true
//	  AND inventory_row.last_seen_at is null OR > 24h ago
//	  AND inventory_row.last_excluded_at is null
func CheckTraceEmissionGap(
	ctx context.Context,
	row TraceEmissionInventoryRow,
	scope TraceEmissionScope,
	exclusions TraceEmissionExclusionStore,
	iacContentReader func(row TraceEmissionInventoryRow) string,
	now time.Time,
) (*TraceEmissionRecommendationDraft, error) {
	if !row.PrimitiveEnabled {
		return nil, nil
	}
	if row.LastSeenAt != nil && now.Sub(*row.LastSeenAt) < 24*time.Hour {
		return nil, nil
	}
	kind := traceEmissionKindFor(row.Provider, row.Tier)
	if kind == "" {
		return nil, nil
	}
	if exclusions != nil && scope.ConnectionID != "" && scope.ScopeID != "" {
		excluded, err := exclusions.ListExcludedRecommendations(
			ctx, scope.ConnectionID, scope.ScopeID, scope.Region, 256,
		)
		if err != nil {
			return nil, fmt.Errorf("trace emission: list excluded recommendations: %w", err)
		}
		for _, ex := range excluded {
			// Per §3, the exclusion check is by RecommendationID. The
			// upstream "Don't propose this again" affordance stores the
			// row-level recommendation_id when the operator wants the
			// suppression scoped to one row, OR a kind-only marker
			// (RecommendationID="" + RecommendationKind set) when the
			// operator wants the suppression across the whole scope.
			if ex.RecommendationID != "" && ex.RecommendationID == row.RecommendationID {
				return nil, nil
			}
			if ex.RecommendationID == "" && ex.RecommendationKind == kind {
				return nil, nil
			}
		}
	}
	var iacContent string
	if iacContentReader != nil {
		iacContent = iacContentReader(row)
	}
	picked := iacpicker.Pick(iacpicker.RecommendationContext{
		Provider:       row.Provider,
		Tier:           row.Tier,
		ResourceTFName: row.ResourceTFName,
	}, iacContent)
	return &TraceEmissionRecommendationDraft{
		Kind:             kind,
		RecommendationID: row.RecommendationID,
		Reasoning:        formatTraceEmissionReasoning(row, picked),
		Terraform:        picked.PrimaryTerraform,
		ScopeID:          scope.ScopeID,
		Region:           scope.Region,
		ResourceID:       row.ResourceID,
		PickedPattern:    picked,
	}, nil
}

// CheckTraceEmissionGapBatch is the convenience wrapper that runs
// CheckTraceEmissionGap over a slice of rows and accumulates the
// non-nil drafts. Errors are accumulated per row and returned via the
// second return value so a transient exclusion-store failure on one
// row doesn't drop the whole pass.
func CheckTraceEmissionGapBatch(
	ctx context.Context,
	rows []TraceEmissionInventoryRow,
	scope TraceEmissionScope,
	exclusions TraceEmissionExclusionStore,
	iacContentReader func(row TraceEmissionInventoryRow) string,
	now time.Time,
) ([]TraceEmissionRecommendationDraft, []error) {
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]TraceEmissionRecommendationDraft, 0, len(rows))
	var errs []error
	for _, r := range rows {
		draft, err := CheckTraceEmissionGap(ctx, r, scope, exclusions, iacContentReader, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if draft != nil {
			out = append(out, *draft)
		}
	}
	return out, errs
}

// traceEmissionKindFor returns the trace-emission-* kind for the
// (provider, tier) pair. Unknown pairs return "" — the caller skips
// the row.
//
// Static lookup table; the 12 valid pairs are documented in
// docs/proposals/trace-integration-slice2.md §4. The exhaustive table
// is pinned by TestTraceEmissionDetection_AllTwelveKinds_KindLookupCorrect.
func traceEmissionKindFor(provider, tier string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	tier = strings.ToLower(strings.TrimSpace(tier))
	switch provider {
	case "aws":
		switch tier {
		case "compute":
			return "trace-emission-aws-compute"
		case "db":
			return "trace-emission-aws-db"
		case "k8s":
			return "trace-emission-aws-k8s"
		}
	case "gcp":
		switch tier {
		case "compute":
			return "trace-emission-gcp-compute"
		case "db":
			return "trace-emission-gcp-db"
		case "k8s":
			return "trace-emission-gcp-k8s"
		}
	case "azure":
		switch tier {
		case "compute":
			return "trace-emission-azure-compute"
		case "db":
			return "trace-emission-azure-db"
		case "k8s":
			return "trace-emission-azure-k8s"
		}
	case "oci":
		switch tier {
		case "compute":
			return "trace-emission-oci-compute"
		case "db":
			return "trace-emission-oci-db"
		case "k8s":
			return "trace-emission-oci-k8s"
		}
	}
	return ""
}

// formatTraceEmissionReasoning composes the operator-facing reasoning
// the recommendation surfaces. Pairs the picker's per-cloud reasoning
// with the §11 three-failure-mode hedge so a reviewer can decline the
// PR when case (2) or (3) applies.
func formatTraceEmissionReasoning(row TraceEmissionInventoryRow, picked iacpicker.PickedPattern) string {
	staleness := "never"
	if row.LastSeenAt != nil {
		staleness = fmt.Sprintf("last seen %s", row.LastSeenAt.UTC().Format(time.RFC3339))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Resource %s (provider=%s, tier=%s) has the observability primitive enabled but Squadron's traceindex has received no spans from it in the last 24 hours (%s).\n\n",
		row.ResourceID, row.Provider, row.Tier, staleness)
	b.WriteString("Three failure modes are possible:\n")
	b.WriteString("1. SDK not deployed: the most common cause. This Terraform PR targets this case by installing the cloud-native auto-instrumentation agent.\n")
	b.WriteString("2. SDK deployed but exporter misconfigured: less common. Check the agent's endpoint configuration.\n")
	b.WriteString("3. Resource attribute mismatch: the agent is emitting but with identifiers that don't match Squadron's expectation.\n\n")
	b.WriteString("If case (2) or (3) applies, decline this PR and note the actual case in the decline reason — the verdict learning loop will record it for future recommendations.\n\n")
	if picked.Reasoning != "" {
		b.WriteString("Picker note: ")
		b.WriteString(picked.Reasoning)
		b.WriteString("\n")
	}
	if picked.FallbackUsed {
		b.WriteString("(iacpicker fell back to the documented default because the operator's IaC content could not be classified.)\n")
	}
	return b.String()
}
