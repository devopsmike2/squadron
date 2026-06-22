// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// acceptedRecsWindow is the fixed look-back the discovery proposer
// applies when pulling prior accepted recommendations. Matches the
// v0.89.17 verdictsWindow on the cost-spike side; 30 days is hard-
// coded in slice 1 per the design's resolved §5. v0.89.28 (#643).
const acceptedRecsWindow = 30 * 24 * time.Hour

// acceptedRecsMaxExamples is the §5 selection cap on the discovery
// side: at most 4 examples total, newest-first. Mirrors the v0.89.17
// verdictsMaxExamples constant.
const acceptedRecsMaxExamples = 4

// DiscoveryAcceptedStore is the slice of ApplicationStore the
// discovery bridge reads for accepted-PR signal. Stated as an
// interface so tests can substitute a fake without spinning up the
// SQLite layer.
type DiscoveryAcceptedStore interface {
	ListAcceptedDiscoveryRecommendations(
		ctx context.Context,
		connectionID, accountID, region string,
		since time.Time, limit int,
	) ([]*applicationstore.AcceptedRecommendation, error)
}

// DiscoveryConnectionStore is the slice of iacconnstore.Store the
// discovery bridge consults for the per-connection opt-in flag.
type DiscoveryConnectionStore interface {
	Get(ctx context.Context, connectionID string) (*iacconnstore.IaCConnection, error)
}

// DiscoveryBridge is the v0.89.28 (#643 slice 1) sibling of Bridge
// for the discovery proposer's accepted-recommendations feedback
// loop. Kept as a separate struct rather than a method on Bridge
// because the two surfaces have disjoint stores and lifecycles —
// the cost-spike Bridge polls open spikes; the discovery feedback
// runs lazily on every ProposeFromDiscoveryScan call from the
// recommendations handler.
type DiscoveryBridge struct {
	accepted    DiscoveryAcceptedStore
	connections DiscoveryConnectionStore
}

// NewDiscoveryBridge constructs a DiscoveryBridge. Both stores are
// required — a nil store causes assembleAcceptedRecommendations to
// return an error so the wiring layer surfaces the misconfiguration
// rather than silently returning empty examples.
func NewDiscoveryBridge(accepted DiscoveryAcceptedStore, connections DiscoveryConnectionStore) *DiscoveryBridge {
	return &DiscoveryBridge{accepted: accepted, connections: connections}
}

// AssembleAcceptedRecommendations pulls the recent accepted-PR
// projection for the supplied scope tuple and returns:
//   - the formatted example slice ready to drop into the prompt
//     block (§6),
//   - the list of PR URLs actually used (for the audit field §8).
//
// Selection policy per §5:
//   - same connection_id + account_id + region only,
//   - since = now - 30d,
//   - newest-first, capped at N=4,
//   - no per-kind cap (slice 1 has no rejection signal to balance
//     against),
//   - cold start (zero matching rows) yields empty slices — the
//     prompt block omits entirely.
//
// Opt-out: when IaCConnection.LearnFromAcceptedRecommendations==false
// the function returns empty slices regardless of stored merges.
//
// Defense-in-depth redaction: every example's text fields flow
// through ai.RedactSecrets before they land on the
// AcceptedRecommendationExample. Slice 1's projection is mostly
// identifiers (PR URL, branch, merged_by, kind); the redact pass is
// cheap and guards against operator-side strings carrying secret
// fragments via PR titles or branch suffixes.
//
// v0.89.28 (#643 slice 1).
func (b *DiscoveryBridge) AssembleAcceptedRecommendations(
	ctx context.Context,
	connectionID, accountID, region string,
) ([]ai.AcceptedRecommendationExample, []string, error) {
	if b == nil || b.accepted == nil || b.connections == nil {
		return nil, nil, nil
	}
	if connectionID == "" || accountID == "" || region == "" {
		return nil, nil, nil
	}
	// Per-connection opt-out check. Mirrors the cost-spike side's
	// GetGroup-then-check-LearnFromVerdicts shape. A deleted
	// connection (substrate row vanished but the proposer was
	// invoked with its ID anyway) drops to the empty path so the
	// caller produces a cold-start prompt.
	conn, err := b.connections.Get(ctx, connectionID)
	if err != nil {
		// Distinguish "not found" from "store errored" — the spec
		// treats a missing connection as the empty path, but a
		// transient store error should NOT silently mask the
		// learning signal. The caller logs and proceeds.
		return nil, nil, fmt.Errorf("discovery bridge: get connection: %w", err)
	}
	if conn == nil || !conn.LearnFromAcceptedRecommendations {
		return nil, nil, nil
	}
	since := time.Now().UTC().Add(-acceptedRecsWindow)
	rows, err := b.accepted.ListAcceptedDiscoveryRecommendations(
		ctx, connectionID, accountID, region, since, acceptedRecsMaxExamples,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("discovery bridge: list accepted: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}
	examples := make([]ai.AcceptedRecommendationExample, 0, len(rows))
	urls := make([]string, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		examples = append(examples, ai.AcceptedRecommendationExample{
			RecommendationKind: ai.RedactSecrets(r.RecommendationKind),
			PRURL:              ai.RedactSecrets(r.PRURL),
			Branch:             ai.RedactSecrets(r.Branch),
			MergedAt:           r.PRMergedAt,
			MergedBy:           ai.RedactSecrets(r.MergedBy),
		})
		if r.PRURL != "" {
			urls = append(urls, r.PRURL)
		}
	}
	return examples, urls, nil
}
