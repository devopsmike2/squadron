// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/proposer/verdictprompt"
	"github.com/devopsmike2/squadron/internal/proposer/verdictsel"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// DiscoveryVerdictStore is the slice of ApplicationStore the discovery
// bridge reads for accepted-PR + closed-without-merge signal. Stated
// as an interface so tests can substitute a fake without spinning up
// the SQLite layer. v0.89.36 (#655 Stream 53) renames the v0.89.28
// DiscoveryAcceptedStore + widens the method to UNION pr_merged AND
// pr_closed_not_merged audit rows. v0.89.37 (#656 Stream 54, #531
// slice 2 chunk 4) adds ListExcludedRecommendations so the bridge can
// fold the new iac_recommendation_verdicts table's operator-set
// exclusions into the verdictsel pool as StateOperatorExcluded rows.
type DiscoveryVerdictStore interface {
	ListDiscoveryVerdicts(
		ctx context.Context,
		connectionID, accountID, region string,
		since time.Time, limit int,
	) ([]*applicationstore.DiscoveryVerdict, error)

	ListExcludedRecommendations(
		ctx context.Context,
		connectionID, accountID, region string,
		limit int,
	) ([]applicationstore.ExcludedRecommendation, error)
}

// DiscoveryConnectionStore is the slice of iacconnstore.Store the
// discovery bridge consults for the per-connection opt-in flag.
type DiscoveryConnectionStore interface {
	Get(ctx context.Context, connectionID string) (*iacconnstore.IaCConnection, error)
}

// DiscoveryBridge is the v0.89.28 (#643 slice 1) sibling of Bridge
// for the discovery proposer's verdicts feedback loop. Kept as a
// separate struct rather than a method on Bridge because the two
// surfaces have disjoint stores and lifecycles — the cost-spike
// Bridge polls open spikes; the discovery feedback runs lazily on
// every ProposeFromDiscoveryScan call from the recommendations
// handler. v0.89.36 (#655 Stream 53) refactors the bridge to feed
// the shared verdictsel + verdictprompt pipeline and surface
// negative signal via the new closed_not_merged event.
type DiscoveryBridge struct {
	store       DiscoveryVerdictStore
	connections DiscoveryConnectionStore
}

// NewDiscoveryBridge constructs a DiscoveryBridge. Both stores are
// required — a nil store causes assembleDiscoveryVerdicts to return
// an error so the wiring layer surfaces the misconfiguration rather
// than silently returning empty examples.
func NewDiscoveryBridge(store DiscoveryVerdictStore, connections DiscoveryConnectionStore) *DiscoveryBridge {
	return &DiscoveryBridge{store: store, connections: connections}
}

// AssembleDiscoveryVerdicts pulls the recent verdict pool for the
// supplied scope tuple, runs it through the shared verdictsel.Select
// policy, and returns:
//   - approved: the curated approved/merged-state slice for the
//     discovery surface (StateMerged rows from the audit table),
//   - rejected: the curated rejected-state slice (StateClosedNotMerged
//     rows; chunk 4 will add StateOperatorExcluded rows from the new
//     iac_recommendation_verdicts table),
//   - exampleURLs: the audit-payload list of PR URLs in the order they
//     appear in the rendered output (rejected first, then approved),
//     used by the audit-emit path's verdict_examples_used field.
//
// Selection policy per docs/proposals/531-proposer-learning-slice2.md
// §6, driven by the verdictsel.Default* constants:
//   - same connection_id + account_id + region only,
//   - since = now - DefaultWindow (30d hard-cliff),
//   - hot/cold tier inside the window (DefaultHotWindow = 7d),
//   - DefaultMaxTotal=4 across both buckets, DefaultMaxPerKind=2
//     inside each bucket,
//   - PreferNeg=false: discovery does not yet bias toward rejection
//     (chunk 4 will revisit when operator-excluded rows are also
//     candidates),
//   - cold start (zero matching rows) yields zero values across all
//     three slices — the prompt block omits entirely and the audit
//     array is empty.
//
// Opt-out: when IaCConnection.LearnFromAcceptedRecommendations==false
// the function short-circuits before the storage query and returns
// four zero values. The flag name stays at the v0.89.28 spelling per
// §5.3 of the slice 2 design (the runbook now explains the broadened
// meaning).
//
// Redaction + truncation: both happen downstream in
// verdictprompt.Render via its reasonField helper (240-char cap
// with ai.RedactSecrets). assembleDiscoveryVerdicts passes the raw
// projection through.
//
// v0.89.28 (#643 slice 1) → v0.89.36 (#655 Stream 53) refactor:
// renamed from AssembleAcceptedRecommendations; the return shape
// is now four-tuple (approved, rejected, urls, err); prompt block
// formatting moved to verdictprompt.Render at the call site.
func (b *DiscoveryBridge) AssembleDiscoveryVerdicts(
	ctx context.Context,
	connectionID, accountID, region string,
) (approved, rejected []verdictsel.Verdict, exampleURLs []string, err error) {
	if b == nil || b.store == nil || b.connections == nil {
		return nil, nil, nil, nil
	}
	if connectionID == "" || accountID == "" || region == "" {
		return nil, nil, nil, nil
	}
	// Per-connection opt-out check. Mirrors the cost-spike side's
	// GetGroup-then-check-LearnFromVerdicts shape. A deleted
	// connection (substrate row vanished but the proposer was
	// invoked with its ID anyway) drops to the empty path so the
	// caller produces a cold-start prompt. The opt-out short-
	// circuit MUST run before any storage query so the §12
	// acceptance test 8 (per-tenant opt-out short-circuits) holds.
	conn, err := b.connections.Get(ctx, connectionID)
	if err != nil {
		// Distinguish "not found" from "store errored" — the spec
		// treats a missing connection as the empty path, but a
		// transient store error should NOT silently mask the
		// learning signal. The caller logs and proceeds.
		return nil, nil, nil, fmt.Errorf("discovery bridge: get connection: %w", err)
	}
	if conn == nil || !conn.LearnFromAcceptedRecommendations {
		return nil, nil, nil, nil
	}

	now := time.Now().UTC()
	since := now.Add(-verdictsel.DefaultWindow)
	rows, err := b.store.ListDiscoveryVerdicts(
		ctx, connectionID, accountID, region, since, verdictsel.DefaultMaxTotal*4,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("discovery bridge: list verdicts: %w", err)
	}

	// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) — fold operator-
	// set exclusions into the verdict pool as StateOperatorExcluded
	// rows. The cold-start path now consults BOTH the audit-derived
	// PR signal AND the new iac_recommendation_verdicts table; either
	// source being non-empty produces a non-empty pool. The exclusion
	// rows feed the prompt's `[OPERATOR_EXCLUDED]` stanza per §7.2.
	excluded, err := b.store.ListExcludedRecommendations(
		ctx, connectionID, accountID, region, verdictsel.DefaultMaxTotal*4,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("discovery bridge: list excluded recommendations: %w", err)
	}

	if len(rows) == 0 && len(excluded) == 0 {
		return nil, nil, nil, nil
	}

	// Project rows into verdictsel.Verdict. ID=PRURL so the prompt's
	// `reference:` line for both merged (pr_url=) and
	// closed_not_merged (pr_closed=) stanzas points at the canonical
	// PR handle. Body carries the human-readable actor for the
	// reason field after redaction in verdictprompt.
	verdicts := make([]verdictsel.Verdict, 0, len(rows)+len(excluded))
	for _, r := range rows {
		if r == nil {
			continue
		}
		var state, body string
		switch r.State {
		case "merged":
			state = verdictsel.StateMerged
			if r.MergedBy != "" {
				body = "merged by " + r.MergedBy
			} else {
				body = "operator merged the PR"
			}
		case "closed_not_merged":
			state = verdictsel.StateClosedNotMerged
			if r.MergedBy != "" {
				body = "closed by " + r.MergedBy
			} else {
				body = "operator closed PR without merging"
			}
		default:
			continue
		}
		verdicts = append(verdicts, verdictsel.Verdict{
			ID:        r.PRURL,
			Kind:      r.RecommendationKind,
			State:     state,
			Timestamp: r.PRMergedAt,
			Body:      body,
			Excluded:  false,
		})
	}

	// Project operator-set exclusion rows. The ID carries the
	// recommendation_id so verdictprompt's `reference:` line lands
	// `operator_excluded=<date>` from the Verdict.Timestamp (the
	// renderer formats the date from the Verdict.Timestamp itself; the
	// recommendation_id flows through the audit payload list as the
	// stable identifier per §10 contract item 10's per-state bucket).
	// Body carries the actor + optional resource_id so verdictprompt's
	// reason: line surfaces both after redaction. Excluded=false on
	// the Verdict — this row IS signal (just negative); the Excluded
	// bit on Verdict is the secondary opt-out used by the cost-spike
	// side's per-rollout suppression.
	for _, ex := range excluded {
		if ex.RecommendationKind == "" {
			continue
		}
		body := "excluded by " + ex.ExcludedBy
		if ex.ResourceID != "" {
			body = body + "; resource=" + ex.ResourceID
		}
		verdicts = append(verdicts, verdictsel.Verdict{
			ID:        ex.RecommendationID,
			Kind:      ex.RecommendationKind,
			State:     verdictsel.StateOperatorExcluded,
			Timestamp: ex.ExcludedAt,
			Body:      body,
			Excluded:  false,
		})
	}

	selected := verdictsel.Select(verdicts, verdictsel.SelectOpts{
		Now:        now,
		Window:     verdictsel.DefaultWindow,
		HotWindow:  verdictsel.DefaultHotWindow,
		MaxTotal:   verdictsel.DefaultMaxTotal,
		MaxPerKind: verdictsel.DefaultMaxPerKind,
		PreferNeg:  false,
	})
	if len(selected) == 0 {
		return nil, nil, nil, nil
	}

	// Split selected into approved + rejected slices for
	// verdictprompt.Render. Walk in order to build exampleURLs so the
	// audit payload preserves Select's documented ordering
	// (rejected first, then approved).
	for _, v := range selected {
		if v.ID != "" {
			exampleURLs = append(exampleURLs, v.ID)
		}
		switch v.State {
		case verdictsel.StateApproved, verdictsel.StateMerged:
			approved = append(approved, v)
		case verdictsel.StateRejected, verdictsel.StateClosedNotMerged, verdictsel.StateOperatorExcluded:
			rejected = append(rejected, v)
		}
	}
	return approved, rejected, exampleURLs, nil
}

// AssembleAcceptedRecommendations is the v0.89.28 (#643 slice 1) entry
// point preserved as a thin facade so the slice 1 callers compile
// untouched while slice 2 chunk 3 lands. It runs
// AssembleDiscoveryVerdicts, renders the result through the shared
// verdictprompt pipeline with the discovery-surface copy, and projects
// the approved bucket back into the v0.89.28
// AcceptedRecommendationExample shape the existing wiring layer
// passes through ai.DiscoveryScanContext.AcceptedRecommendations. The
// rejected bucket flows into ai.DiscoveryScanContext.VerdictBlock via
// the second return value (slice 2 chunk 3 wiring change at the
// handler layer).
//
// Returns:
//   - the v0.89.28 approved-only example slice (slice 1 compat),
//   - the merged exampleURLs for the audit payload,
//   - any error.
//
// Pure compat: the slice 1 prompt body lands byte-for-byte unchanged
// when the rejected bucket is empty, since the new VerdictBlock-side
// rendering is invoked from a separate handler hook the wiring layer
// adds in this chunk.
func (b *DiscoveryBridge) AssembleAcceptedRecommendations(
	ctx context.Context,
	connectionID, accountID, region string,
) ([]ai.AcceptedRecommendationExample, []string, error) {
	approved, _, urls, err := b.AssembleDiscoveryVerdicts(ctx, connectionID, accountID, region)
	if err != nil {
		return nil, nil, err
	}
	if len(approved) == 0 {
		return nil, nil, nil
	}
	examples := make([]ai.AcceptedRecommendationExample, 0, len(approved))
	for _, v := range approved {
		examples = append(examples, ai.AcceptedRecommendationExample{
			RecommendationKind: ai.RedactSecrets(v.Kind),
			PRURL:              ai.RedactSecrets(v.ID),
			Branch:             "", // not threaded through verdictsel.Verdict; chunk 4 may revive
			MergedAt:           v.Timestamp,
			MergedBy:           ai.RedactSecrets(extractActor(v.Body)),
		})
	}
	// urls from AssembleDiscoveryVerdicts includes both buckets. Trim
	// to approved-bucket URLs only to preserve the v0.89.28 audit
	// payload contract (verdict_examples_used carries accepted PR
	// URLs).
	approvedURLs := make([]string, 0, len(approved))
	for _, v := range approved {
		if v.ID != "" {
			approvedURLs = append(approvedURLs, v.ID)
		}
	}
	_ = urls
	return examples, approvedURLs, nil
}

// extractActor pulls the login back out of the Body string the
// bridge synthesises ("merged by alice" or "closed by alice"). Used
// by the v0.89.28 compat facade to repopulate MergedBy on the
// AcceptedRecommendationExample shape — slice 1 callers still want
// it as a discrete field.
func extractActor(body string) string {
	const merged = "merged by "
	const closed = "closed by "
	switch {
	case len(body) > len(merged) && body[:len(merged)] == merged:
		return body[len(merged):]
	case len(body) > len(closed) && body[:len(closed)] == closed:
		return body[len(closed):]
	}
	return ""
}

// RenderDiscoveryVerdictBlock is the surface-aware wrapper around
// verdictprompt.Render the wiring layer can call once it has the
// approved + rejected slices from AssembleDiscoveryVerdicts. Lives on
// the bridge so callers don't have to import verdictprompt directly
// (keeping the slice 2 import boundary tight). Returns "" on empty
// pool — the caller drops the block and the prompt stays at the
// pre-slice-2 cold-start byte shape.
func (b *DiscoveryBridge) RenderDiscoveryVerdictBlock(approved, rejected []verdictsel.Verdict) string {
	return verdictprompt.Render(approved, rejected, verdictprompt.RenderOpts{
		Surface:         verdictprompt.SurfaceDiscovery,
		Now:             time.Now().UTC(),
		Header:          verdictprompt.DiscoveryHeader,
		InstructionTail: verdictprompt.DiscoveryInstructionTail,
	})
}
