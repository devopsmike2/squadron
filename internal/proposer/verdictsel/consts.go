// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package verdictsel

import "time"

// Default* are the slice 2 selection policy constants. Both
// surfaces (cost-spike #531 and discovery #643) consume these
// verbatim; config-driven overrides are slice 3 candidates per
// docs/proposals/531-proposer-learning-slice2.md §6.
const (
	// DefaultWindow is the hard recency cliff. Verdicts older than
	// (Now - DefaultWindow) are dropped before any other policy
	// applies.
	DefaultWindow = 30 * 24 * time.Hour

	// DefaultHotWindow is the boundary inside DefaultWindow that
	// separates "hot" (emitted first) from "cold" (backfill)
	// verdicts within each bucket.
	DefaultHotWindow = 7 * 24 * time.Hour

	// DefaultMaxTotal caps the total number of verdicts that flow
	// into the prompt across both buckets.
	DefaultMaxTotal = 4

	// DefaultMaxPerKind caps how many verdicts of a single Kind
	// can appear inside a bucket. Forces shape diversity at high
	// cardinality.
	DefaultMaxPerKind = 2
)
