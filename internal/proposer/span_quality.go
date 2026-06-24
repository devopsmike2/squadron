// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"fmt"
	"strings"

	"github.com/devopsmike2/squadron/internal/proposer/iacpicker"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// SpanQualityInventoryRow is the per-row projection the span-quality
// detection branch reads. It's INTENTIONALLY separate from
// TraceEmissionInventoryRow even though they share much of the same
// shape — the two detection branches have disjoint trigger semantics
// (trace-emission fires on stale LastSeenAt, span-quality fires on
// pathology percentages) and the wiring layer projects them from
// disjoint sources (inventory store vs. traceindex Quality observer).
// Keeping them separate makes the branches independently testable and
// keeps a future divergence (per-tier window size, etc.) from
// requiring a refactor.
type SpanQualityInventoryRow struct {
	// RecommendationID is the deterministic identifier the discovery
	// proposer will assign when emitting the span-quality
	// recommendation. The detection branch concatenates per-kind
	// suffixes (".orphan" / ".missing" / ".mismatch") onto this to
	// consult ExclusionStore — operators may exclude one pathology
	// kind without excluding the others.
	RecommendationID string

	// Provider is one of "aws" / "gcp" / "azure" / "oci". Threaded
	// onto the picker context so the per-cloud Terraform shape is
	// selected.
	Provider string

	// Tier is one of "compute" / "db" / "k8s". Same routing role as
	// Provider; combined they select the iacpicker dispatch.
	Tier string

	// ResourceTFName is the operator's best-effort Terraform resource
	// name. Empty when the IaC introspection couldn't classify; the
	// picker's snippet falls back to "<name>".
	ResourceTFName string

	// ResourceID is the canonical resource identifier the
	// recommendation surfaces in its UI. Threaded into the reasoning
	// text so the operator sees which row Squadron flagged.
	ResourceID string

	// Region is the per-row region. Surfaced on the draft so the
	// downstream RecommendationDraft.Scope is well-formed.
	Region string
}

// SpanQualityScope is the (connection, account/scope, region) tuple
// used to consult the exclusion store. Mirrors TraceEmissionScope's
// shape so the wiring layer threads one scope tuple through both
// detection branches without re-projecting.
type SpanQualityScope struct {
	ConnectionID string
	ScopeID      string
	Region       string
}

// SpanQualityRecommendationDraft is the per-row output of the
// span-quality detection branch. Same posture as
// TraceEmissionRecommendationDraft — projected at the handler
// boundary into recommendations.Recommendation, kept separate here so
// the proposer package doesn't pull the recommendations package
// (circular dependency through services).
//
// One row may produce up to three drafts (orphan + missing + mismatch
// all firing simultaneously); the batch wrapper accumulates them in
// order.
type SpanQualityRecommendationDraft struct {
	Kind             string
	RecommendationID string
	Reasoning        string
	Terraform        string
	ScopeID          string
	Region           string
	ResourceID       string
	PickedPattern    iacpicker.PickedPattern
}

// SpanQualityExclusionStore is the slice of ApplicationStore the
// detection branch consults. Same posture as
// TraceEmissionExclusionStore — both interfaces are satisfied by the
// same applicationstore.ApplicationStore so production wiring passes
// one store to both branches.
type SpanQualityExclusionStore interface {
	ListExcludedRecommendations(
		ctx context.Context,
		connectionID, accountID, region string,
		limit int,
	) ([]applicationstore.ExcludedRecommendation, error)
}

// Minimum-sample-size guard. Per span-quality-slice1.md §3 the
// thresholds (10% orphan / 25% missing / 5% mismatch) only carry
// meaning when the rolling window has accumulated enough span volume
// to render the percentages stable. Below this floor, the
// percentages are too noisy — a 1-of-3-spans orphan rate would
// trigger the 25% threshold despite being a single statistical fluke.
//
// 100 spans was chosen via the design-doc §13 "minimum sample size
// before drafting" note: rates of change at 1/100 (1%) are below the
// lowest threshold (5% for mismatch), so a fully clean resource that
// later starts emitting placeholders won't trip the threshold on the
// FIRST batch.
//
// Slice 2 candidate: tune per-tier (db tier sees fewer spans on
// average; the floor may need to drop to 50 to avoid suppressing
// real signal there).
const SpanQualityMinimumSpansThreshold = 100

// Per-pathology threshold constants per design doc §3. Lifted into
// named constants so the percentages aren't sprinkled through the
// detection code and are pinned by the threshold-edge-case tests.
const (
	SpanQualityOrphanThresholdPct   = 10.0
	SpanQualityMissingThresholdPct  = 25.0
	SpanQualityMismatchThresholdPct = 5.0

	// Slice 2 (v0.89.110) — W3C trace context thresholds per
	// span-quality-slice2.md §3. The 1% malformed threshold is
	// intentionally low because ANY malformed traceparent is unusual;
	// the 5% missing-on-child threshold sits between the slice 1
	// thresholds because SDK propagation is mostly an "always or
	// never" pattern.
	SpanQualityMalformedTraceparentThresholdPct      = 1.0
	SpanQualityMissingTraceparentOnChildThresholdPct = 5.0

	// Minimum denominators for the two new traceparent thresholds.
	// Below these floors the percentages are too noisy to act on —
	// a 1-of-10 malformed rate (10%) would trigger the 1% threshold
	// despite being a single statistical fluke. 50 matches the
	// slice 1 SpanQualityMinimumSpansThreshold halving — the two
	// new axes carry their own denominators so we can't reuse
	// TotalSpans here.
	SpanQualityMinimumSpansWithTraceparent = 50
	SpanQualityMinimumChildSpans           = 50
)

// CheckSpanQualityIssues is the §3 detection branch the discovery
// proposer runs ALONGSIDE the existing trace-emission detection (and
// the per-cloud primitive-enablement kinds). For each inventory row
// with a non-nil Quality snapshot and TotalSpans >= the minimum-
// sample-size floor, the branch evaluates the three thresholds
// independently and emits zero, one, two, or three drafts.
//
// Returns:
//   - nil drafts + nil error when no thresholds are crossed (or the
//     row's sample size is below the floor),
//   - one or more drafts when one or more thresholds trip,
//   - non-nil error when the exclusion store fails (the partial
//     drafts collected before the error are still returned; the
//     batch wrapper accumulates per-row errors so one transient
//     failure can't drop the whole pass).
//
// Per-kind drafts pass through iacpicker.Pick{Orphan,MissingAttrs,
// Mismatch}Pattern for the Terraform pattern; the per-kind reasoning
// template lives in formatSpanQuality*Reasoning below.
func CheckSpanQualityIssues(
	ctx context.Context,
	row SpanQualityInventoryRow,
	qual *traceindex.QualityCountersSnapshot,
	scope SpanQualityScope,
	exclusions SpanQualityExclusionStore,
) ([]SpanQualityRecommendationDraft, error) {
	if qual == nil {
		return nil, nil
	}
	if qual.TotalSpans < SpanQualityMinimumSpansThreshold {
		// Below sample-size floor; emit nothing. The dashboard's
		// per-resource quality indicator still renders the
		// percentages (and the per-row drill-down shows them) — the
		// detection branch only suppresses the proposer-side draft
		// drafting, not the read surface.
		return nil, nil
	}
	pickCtx := iacpicker.RecommendationContext{
		Provider:       row.Provider,
		Tier:           row.Tier,
		ResourceTFName: row.ResourceTFName,
	}

	excluded, err := loadSpanQualityExclusions(ctx, scope, exclusions)
	if err != nil {
		return nil, err
	}

	var drafts []SpanQualityRecommendationDraft
	// Orphan trace — §3.1 + §4.1.
	if qual.OrphanPct >= SpanQualityOrphanThresholdPct {
		const kind = "span-quality-orphan-trace"
		recID := row.RecommendationID + ".orphan"
		if !isSpanQualityExcluded(excluded, recID, kind) {
			picked := iacpicker.PickOrphanTracePattern(pickCtx)
			drafts = append(drafts, SpanQualityRecommendationDraft{
				Kind:             kind,
				RecommendationID: recID,
				Reasoning:        formatSpanQualityOrphanReasoning(row, qual, picked),
				Terraform:        picked.PrimaryTerraform,
				ScopeID:          scope.ScopeID,
				Region:           scope.Region,
				ResourceID:       row.ResourceID,
				PickedPattern:    picked,
			})
		}
	}
	// Missing required resource attributes — §3.2 + §4.2.
	if qual.MissingAttrPct >= SpanQualityMissingThresholdPct {
		const kind = "span-quality-missing-resource-attrs"
		recID := row.RecommendationID + ".missing"
		if !isSpanQualityExcluded(excluded, recID, kind) {
			picked := iacpicker.PickMissingAttrsPattern(pickCtx)
			drafts = append(drafts, SpanQualityRecommendationDraft{
				Kind:             kind,
				RecommendationID: recID,
				Reasoning:        formatSpanQualityMissingReasoning(row, qual, picked),
				Terraform:        picked.PrimaryTerraform,
				ScopeID:          scope.ScopeID,
				Region:           scope.Region,
				ResourceID:       row.ResourceID,
				PickedPattern:    picked,
			})
		}
	}
	// Placeholder / attribute mismatch — §3.3 + §4.3.
	if qual.AttrMismatchPct >= SpanQualityMismatchThresholdPct {
		const kind = "span-quality-attribute-mismatch"
		recID := row.RecommendationID + ".mismatch"
		if !isSpanQualityExcluded(excluded, recID, kind) {
			picked := iacpicker.PickMismatchPattern(pickCtx)
			drafts = append(drafts, SpanQualityRecommendationDraft{
				Kind:             kind,
				RecommendationID: recID,
				Reasoning:        formatSpanQualityMismatchReasoning(row, qual, picked),
				Terraform:        picked.PrimaryTerraform,
				ScopeID:          scope.ScopeID,
				Region:           scope.Region,
				ResourceID:       row.ResourceID,
				PickedPattern:    picked,
			})
		}
	}
	// Slice 2 (v0.89.110) — malformed traceparent. Honest denominator:
	// only fire when SpansWithTraceparent is at or above the
	// minimum-sample-size floor for the denominator that actually
	// gates this rate. A resource with 0 spans-carrying-traceparent
	// can't meaningfully be "malformed at X%" — the snapshot's
	// MalformedTraceparentPct is 0 anyway via the snapshot zero-safety
	// fallback, but the explicit denominator check pins the intent.
	if qual.MalformedTraceparentPct >= SpanQualityMalformedTraceparentThresholdPct &&
		qual.SpansWithTraceparent >= SpanQualityMinimumSpansWithTraceparent {
		const kind = "span-quality-traceparent-malformed"
		recID := row.RecommendationID + ".traceparent_malformed"
		if !isSpanQualityExcluded(excluded, recID, kind) {
			picked := iacpicker.PickMalformedTraceparentPattern(pickCtx)
			drafts = append(drafts, SpanQualityRecommendationDraft{
				Kind:             kind,
				RecommendationID: recID,
				Reasoning:        formatSpanQualityMalformedTraceparentReasoning(row, qual, picked),
				Terraform:        picked.PrimaryTerraform,
				ScopeID:          scope.ScopeID,
				Region:           scope.Region,
				ResourceID:       row.ResourceID,
				PickedPattern:    picked,
			})
		}
	}
	// Slice 2 — missing traceparent on child spans. Honest denominator:
	// only fire when ChildSpans is at or above the minimum. A resource
	// with 0 child spans can't be missing-on-child.
	if qual.MissingTraceparentOnChildPct >= SpanQualityMissingTraceparentOnChildThresholdPct &&
		qual.ChildSpans >= SpanQualityMinimumChildSpans {
		const kind = "span-quality-traceparent-missing"
		recID := row.RecommendationID + ".traceparent_missing"
		if !isSpanQualityExcluded(excluded, recID, kind) {
			picked := iacpicker.PickMissingTraceparentPattern(pickCtx)
			drafts = append(drafts, SpanQualityRecommendationDraft{
				Kind:             kind,
				RecommendationID: recID,
				Reasoning:        formatSpanQualityMissingTraceparentReasoning(row, qual, picked),
				Terraform:        picked.PrimaryTerraform,
				ScopeID:          scope.ScopeID,
				Region:           scope.Region,
				ResourceID:       row.ResourceID,
				PickedPattern:    picked,
			})
		}
	}
	return drafts, nil
}

// CheckSpanQualityIssuesBatch is the convenience wrapper that runs
// CheckSpanQualityIssues over a slice of rows + per-row snapshots and
// accumulates the non-nil drafts. Errors are accumulated per row so a
// transient exclusion-store failure on one row doesn't drop the rest.
//
// snapshots[i] is paired with rows[i] by index; a nil snapshot entry
// is treated the same as "no Quality data for this row" (no drafts).
// The caller (the wiring layer) is responsible for the row->snapshot
// projection.
func CheckSpanQualityIssuesBatch(
	ctx context.Context,
	rows []SpanQualityInventoryRow,
	snapshots []*traceindex.QualityCountersSnapshot,
	scope SpanQualityScope,
	exclusions SpanQualityExclusionStore,
) ([]SpanQualityRecommendationDraft, []error) {
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]SpanQualityRecommendationDraft, 0, len(rows))
	var errs []error
	for i, row := range rows {
		var snap *traceindex.QualityCountersSnapshot
		if i < len(snapshots) {
			snap = snapshots[i]
		}
		drafts, err := CheckSpanQualityIssues(ctx, row, snap, scope, exclusions)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, drafts...)
	}
	return out, errs
}

// loadSpanQualityExclusions runs the exclusion-store lookup once per
// row rather than once per pathology kind — the three thresholds can
// fire independently but they share the same exclusion scope, so a
// single store call is enough to filter all three.
//
// Returns nil + nil error when the store is nil OR the scope is
// incomplete (the same "no learning context, don't try to use it"
// posture the trace-emission detection follows).
func loadSpanQualityExclusions(
	ctx context.Context,
	scope SpanQualityScope,
	exclusions SpanQualityExclusionStore,
) ([]applicationstore.ExcludedRecommendation, error) {
	if exclusions == nil || scope.ConnectionID == "" || scope.ScopeID == "" {
		return nil, nil
	}
	excluded, err := exclusions.ListExcludedRecommendations(
		ctx, scope.ConnectionID, scope.ScopeID, scope.Region, 256,
	)
	if err != nil {
		return nil, fmt.Errorf("span quality: list excluded recommendations: %w", err)
	}
	return excluded, nil
}

// isSpanQualityExcluded matches the operator's per-row "Don't propose
// this again" affordance against either the row-specific
// recommendation_id OR the kind-only marker (RecommendationID="" +
// RecommendationKind set). Mirrors the trace-emission detection's
// exclusion check so operators see consistent suppression semantics
// across the two surfaces.
func isSpanQualityExcluded(excluded []applicationstore.ExcludedRecommendation, recID, kind string) bool {
	for _, ex := range excluded {
		if ex.RecommendationID != "" && ex.RecommendationID == recID {
			return true
		}
		if ex.RecommendationID == "" && ex.RecommendationKind == kind {
			return true
		}
	}
	return false
}

// formatSpanQualityOrphanReasoning composes the operator-facing
// reasoning for the orphan-trace kind. Pairs the percentages with the
// §11-style "if the actual cause is different, decline" hedge so a
// reviewer can decline the PR when the propagator config isn't the
// real fix.
func formatSpanQualityOrphanReasoning(row SpanQualityInventoryRow, qual *traceindex.QualityCountersSnapshot, picked iacpicker.PickedPattern) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"Squadron's traceindex Quality observer has observed %.1f%% of spans from this resource (%s, provider=%s, tier=%s) in the last hour with parent_span_id values that don't resolve to any span in the same trace (over %d total spans).\n\n",
		qual.OrphanPct, row.ResourceID, row.Provider, row.Tier, qual.TotalSpans,
	)
	b.WriteString("The most common cause is broken W3C trace context propagation across an HTTP or queue boundary — the caller emitted a span, but the called service's library didn't read the W3C traceparent header.\n\n")
	b.WriteString("This Terraform PR targets that case by enabling the tracecontext + baggage propagators on the SDK config.\n\n")
	b.WriteString("If the actual cause is different — exporter restarts mid-flush, misconfigured queue headers, etc. — decline this PR. The verdict learning loop will record the decline.\n")
	appendPickerNote(&b, picked)
	return b.String()
}

// formatSpanQualityMissingReasoning composes the operator-facing
// reasoning for the missing-resource-attrs kind.
func formatSpanQualityMissingReasoning(row SpanQualityInventoryRow, qual *traceindex.QualityCountersSnapshot, picked iacpicker.PickedPattern) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"Squadron's traceindex Quality observer has observed %.1f%% of spans from this resource (%s, provider=%s, tier=%s) in the last hour missing one or more required resource attributes (over %d total spans).\n\n",
		qual.MissingAttrPct, row.ResourceID, row.Provider, row.Tier, qual.TotalSpans,
	)
	b.WriteString("The most common cause is the OTel SDK's resource detector running with insufficient permissions, or running before the cloud metadata service was reachable.\n\n")
	b.WriteString("This Terraform PR targets that case by adjusting IAM permissions so the resource detector can populate the missing attributes.\n\n")
	b.WriteString("If the actual cause is different — the SDK is configured with explicit attribute overrides that omit the required ones, etc. — decline this PR. The verdict learning loop will record the decline.\n")
	appendPickerNote(&b, picked)
	return b.String()
}

// formatSpanQualityMismatchReasoning composes the operator-facing
// reasoning for the attribute-mismatch kind. Includes the per-resource
// placeholder list verbatim from the snapshot so the operator can see
// which sentinel the SDK fell back to.
func formatSpanQualityMismatchReasoning(row SpanQualityInventoryRow, qual *traceindex.QualityCountersSnapshot, picked iacpicker.PickedPattern) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"Squadron's traceindex Quality observer has observed %.1f%% of spans from this resource (%s, provider=%s, tier=%s) in the last hour with placeholder values in required attributes (over %d total spans).\n\n",
		qual.AttrMismatchPct, row.ResourceID, row.Provider, row.Tier, qual.TotalSpans,
	)
	if len(qual.Placeholders) > 0 {
		b.WriteString("Recent placeholder observations:\n")
		for _, p := range qual.Placeholders {
			fmt.Fprintf(&b, "  - %s = %q\n", p.Attribute, p.Placeholder)
		}
		b.WriteString("\n")
	}
	b.WriteString("The most common cause is the OTel SDK falling back to default values when the resource detector failed silently — host.name=localhost, cloud.account.id=000000000000, service.name=unknown_service, etc.\n\n")
	b.WriteString("This Terraform PR targets that case by injecting an explicit OTEL_RESOURCE_ATTRIBUTES env var hardcoding the correct values from the inventory row Squadron already has.\n\n")
	b.WriteString("If the actual cause is different, decline this PR. The verdict learning loop will record the decline.\n")
	appendPickerNote(&b, picked)
	return b.String()
}

// formatSpanQualityMalformedTraceparentReasoning composes the
// operator-facing reasoning for the span-quality-traceparent-malformed
// kind. Pairs the per-resource percentage with the slice 2 §1
// "three failure modes" framing so the operator can decline cleanly
// when their case is one of the alternatives.
func formatSpanQualityMalformedTraceparentReasoning(row SpanQualityInventoryRow, qual *traceindex.QualityCountersSnapshot, picked iacpicker.PickedPattern) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"Squadron's Quality observer has observed %.1f%% of this resource's spans (%s, provider=%s, tier=%s) carrying a traceparent attribute that doesn't conform to the W3C spec (over %d spans with traceparent in the last hour).\n\n",
		qual.MalformedTraceparentPct, row.ResourceID, row.Provider, row.Tier, qual.SpansWithTraceparent,
	)
	b.WriteString("The most common causes are: (1) an upstream service emitting a CUSTOM trace ID format that doesn't fit W3C constraints (some legacy SDKs); (2) an SDK version mismatch — upstream emits a 'next-version' (01) traceparent and the downstream rejects it; (3) the header being rewritten by a proxy / load balancer in transit (rare).\n\n")
	b.WriteString("This Terraform PR targets the SDK-side fix by pinning the upstream SDK to the latest W3C-compliant release.\n\n")
	b.WriteString("If your actual case is different (the runbook describes the three failure modes), decline this PR. The verdict learning loop will record the decline.\n")
	appendPickerNote(&b, picked)
	return b.String()
}

// formatSpanQualityMissingTraceparentReasoning composes the
// operator-facing reasoning for the span-quality-traceparent-missing
// kind. Notes the CHILD-spans denominator explicitly because that
// matters for the worker/background-job decline branch.
func formatSpanQualityMissingTraceparentReasoning(row SpanQualityInventoryRow, qual *traceindex.QualityCountersSnapshot, picked iacpicker.PickedPattern) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"Squadron's Quality observer has observed %.1f%% of this resource's child spans (%s, provider=%s, tier=%s) arriving without a traceparent attribute (over %d child spans in the last hour).\n\n",
		qual.MissingTraceparentOnChildPct, row.ResourceID, row.Provider, row.Tier, qual.ChildSpans,
	)
	b.WriteString("The most common cause is the SDK's HTTP server instrumentation not extracting the W3C context propagator on the inbound request. Possible causes: (1) SDK deployed but the context propagator middleware wasn't enabled in the application's HTTP server config; (2) custom middleware in front of the SDK consumes the traceparent header before the SDK reads it; (3) the resource is a worker / background-job pod (no inbound HTTP) and child spans here are intra-process.\n\n")
	b.WriteString("This Terraform PR adds the OpenTelemetry context propagator via the OTEL_PROPAGATORS=tracecontext,baggage env var injection (per-cloud pattern same as span-quality-orphan-trace from v0.89.86).\n\n")
	b.WriteString("If your actual case is the worker/background-job pattern (no inbound HTTP), decline this PR. The verdict learning loop will record the decline.\n")
	appendPickerNote(&b, picked)
	return b.String()
}

// appendPickerNote tacks the picker's per-cloud reasoning + fallback
// flag onto the reasoning. Factored out so all three reasoning
// builders share one consistent footer.
func appendPickerNote(b *strings.Builder, picked iacpicker.PickedPattern) {
	if picked.Reasoning != "" {
		b.WriteString("\nPicker note: ")
		b.WriteString(picked.Reasoning)
		b.WriteString("\n")
	}
	if picked.FallbackUsed {
		b.WriteString("(iacpicker fell back to a redirect / default because no per-cloud Terraform pattern applies cleanly here.)\n")
	}
}
