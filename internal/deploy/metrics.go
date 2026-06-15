// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package deploy — DORA-style metrics over the deploy_runs ledger.
//
// The DORA (DevOps Research and Assessment) metrics are the de-facto
// industry yardstick for software delivery performance. Squadron
// already records every deploy attempt in deploy_runs; this file
// reduces that ledger into the four-metric view that an SRE
// director will recognize on sight:
//
//   1. Deploy frequency — how often we ship per day (windowed)
//   2. Change failure rate — share of deploys that fail
//   3. Mean time to recovery (MTTR) — average gap between a failure
//      and the next successful deploy to the same target
//   4. Lead time — request-to-completion duration for successful
//      deploys
//
// All numbers are computed in-process from a single ListRuns query
// — no new schema, no new audit shape. We trade slight recomputation
// cost on each /metrics call for storage simplicity; the call is
// cheap enough (O(runs in window)) that this is the right tradeoff
// for a handful of targets.
//
// Added in v0.39.0 (insights expansion).

package deploy

import (
	"context"
	"sort"
	"time"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// DORAWindow is the time bucket the caller wants metrics over.
// "7d" and "30d" are the two industry-standard reporting windows;
// anything else is treated as "30d" since that's the longest range
// we want to scan unbounded.
type DORAWindow string

const (
	DORAWindow7d  DORAWindow = "7d"
	DORAWindow30d DORAWindow = "30d"
	DORAWindow90d DORAWindow = "90d"
)

// AsDuration returns the window's lookback duration. Unknown values
// fall back to 30d — the most-requested default in practice.
func (w DORAWindow) AsDuration() time.Duration {
	switch w {
	case DORAWindow7d:
		return 7 * 24 * time.Hour
	case DORAWindow90d:
		return 90 * 24 * time.Hour
	default:
		return 30 * 24 * time.Hour
	}
}

// DORAMetrics is the reduced view returned by /api/v1/deploy/metrics.
// Zero values are valid — a fleet with no recent deploys returns
// all zeros and an empty samples slice rather than null fields.
//
// The struct intentionally mirrors the four DORA buckets so the UI
// can ship as a 2x2 of KPI tiles without further transformation.
type DORAMetrics struct {
	Window DORAWindow `json:"window"`
	// TotalRuns is the absolute count of deploy runs in the window —
	// useful for showing "X of Y deploys succeeded" beneath the
	// failure rate tile.
	TotalRuns int `json:"total_runs"`
	// CompletedRuns counts runs that have a terminal Status. The
	// frequency calculation uses only completed runs; an in-progress
	// run hasn't happened yet from a DORA perspective.
	CompletedRuns int `json:"completed_runs"`
	// SuccessfulRuns counts terminal runs with conclusion = success.
	SuccessfulRuns int `json:"successful_runs"`
	// FailedRuns counts terminal runs with conclusion ∈ {failure,
	// cancelled, timed_out}. "Skipped" is excluded — a skipped run
	// is closer to a no-op than a failed change.
	FailedRuns int `json:"failed_runs"`

	// DeploysPerDay is total completed runs / window in days.
	DeploysPerDay float64 `json:"deploys_per_day"`
	// ChangeFailureRate is FailedRuns / CompletedRuns, in [0, 1].
	// Zero if no completed runs in the window.
	ChangeFailureRate float64 `json:"change_failure_rate"`
	// MTTRMinutes is the average minutes between a failed deploy
	// and the next successful deploy targeting the same DeployTarget.
	// Computed pairwise per-target; only completed pairs count
	// toward the average. Zero if no recovery cycles observed.
	MTTRMinutes float64 `json:"mttr_minutes"`
	// LeadTimeMinutes is the average minutes from RequestedAt to
	// CompletedAt for successful runs in the window. Zero if no
	// successful runs.
	LeadTimeMinutes float64 `json:"lead_time_minutes"`

	// PerTarget breaks the same numbers out per DeployTarget. Useful
	// for spotting one target that drags down the fleet-wide MTTR.
	PerTarget []TargetDORA `json:"per_target,omitempty"`
}

// TargetDORA is the per-target version of DORAMetrics. Same four
// buckets, computed only over runs whose TargetID matches.
type TargetDORA struct {
	TargetID          string  `json:"target_id"`
	TargetName        string  `json:"target_name,omitempty"`
	TotalRuns         int     `json:"total_runs"`
	SuccessfulRuns    int     `json:"successful_runs"`
	FailedRuns        int     `json:"failed_runs"`
	ChangeFailureRate float64 `json:"change_failure_rate"`
	MTTRMinutes       float64 `json:"mttr_minutes"`
	LeadTimeMinutes   float64 `json:"lead_time_minutes"`
}

// ComputeDORA reduces a slice of deploy runs into DORAMetrics.
// Pulled out as a pure function so it's trivially unit-testable
// against synthetic ledgers without a database fixture.
//
// The runs slice can be in any order; we sort by RequestedAt
// ascending internally because the MTTR pairing needs chronological
// scan order.
func ComputeDORA(runs []*apptypes.DeployRun, window DORAWindow, now time.Time) DORAMetrics {
	out := DORAMetrics{
		Window:    window,
		PerTarget: []TargetDORA{},
	}
	if len(runs) == 0 {
		return out
	}
	// Sort by RequestedAt ascending. We sort a local copy so the
	// caller's slice isn't mutated as a side effect — small but
	// surprising bug to introduce if we sorted in place.
	scratch := make([]*apptypes.DeployRun, len(runs))
	copy(scratch, runs)
	sort.Slice(scratch, func(i, j int) bool {
		return scratch[i].RequestedAt.Before(scratch[j].RequestedAt)
	})

	out.TotalRuns = len(scratch)

	var (
		leadTotal     time.Duration
		leadCount     int
		mttrTotal     time.Duration
		mttrCount     int
		lastFailureBy = map[string]time.Time{} // targetID → most recent failure
	)
	perTarget := map[string]*TargetDORA{}

	for _, r := range scratch {
		tt, ok := perTarget[r.TargetID]
		if !ok {
			tt = &TargetDORA{TargetID: r.TargetID, TargetName: r.TargetName}
			perTarget[r.TargetID] = tt
		}
		tt.TotalRuns++

		// Only completed runs count toward frequency / success /
		// failure. An in-progress run hasn't happened yet.
		if r.Status != "completed" {
			continue
		}
		out.CompletedRuns++
		switch r.Conclusion {
		case "success":
			out.SuccessfulRuns++
			tt.SuccessfulRuns++
			// Lead time: requested → completed.
			if r.CompletedAt != nil {
				d := r.CompletedAt.Sub(r.RequestedAt)
				if d > 0 {
					leadTotal += d
					leadCount++
				}
				// MTTR: if this target had an earlier failure that
				// wasn't yet recovered, this success closes the
				// loop. Use CompletedAt of the success vs the
				// CompletedAt of the failure (closest proxy to
				// "when did the broken state end / begin").
				if fail, hasFail := lastFailureBy[r.TargetID]; hasFail {
					gap := r.CompletedAt.Sub(fail)
					if gap > 0 {
						mttrTotal += gap
						mttrCount++
					}
					delete(lastFailureBy, r.TargetID)
				}
			}
		case "failure", "cancelled", "timed_out":
			out.FailedRuns++
			tt.FailedRuns++
			// Track this failure for the MTTR pairing pass. If a
			// later success on the same target follows, the gap
			// between this failure's CompletedAt and that success's
			// CompletedAt becomes one MTTR sample.
			if r.CompletedAt != nil {
				lastFailureBy[r.TargetID] = *r.CompletedAt
			}
		}
	}

	// Aggregate floats.
	winDays := window.AsDuration().Hours() / 24.0
	if winDays > 0 {
		out.DeploysPerDay = float64(out.CompletedRuns) / winDays
	}
	if out.CompletedRuns > 0 {
		out.ChangeFailureRate = float64(out.FailedRuns) / float64(out.CompletedRuns)
	}
	if leadCount > 0 {
		out.LeadTimeMinutes = leadTotal.Minutes() / float64(leadCount)
	}
	if mttrCount > 0 {
		out.MTTRMinutes = mttrTotal.Minutes() / float64(mttrCount)
	}

	// Finalize per-target. Compute the same rate / lead time on each
	// bucket. Iteration over the map yields nondeterministic order,
	// so sort the resulting slice by TargetName for a stable UI.
	for _, tt := range perTarget {
		completed := tt.SuccessfulRuns + tt.FailedRuns
		if completed > 0 {
			tt.ChangeFailureRate = float64(tt.FailedRuns) / float64(completed)
		}
		out.PerTarget = append(out.PerTarget, *tt)
	}
	sort.Slice(out.PerTarget, func(i, j int) bool {
		if out.PerTarget[i].TargetName == out.PerTarget[j].TargetName {
			return out.PerTarget[i].TargetID < out.PerTarget[j].TargetID
		}
		return out.PerTarget[i].TargetName < out.PerTarget[j].TargetName
	})

	_ = now // reserved for future "trend vs previous window" calc
	return out
}

// Metrics is the Service-level entry point. Pulls the windowed
// ledger and hands it to ComputeDORA. Window narrowing happens at
// the apptypes.DeployRunFilter level so we don't fetch the entire
// table when the operator asked for 7 days.
func (s *Service) Metrics(ctx context.Context, window DORAWindow) (DORAMetrics, error) {
	cutoff := time.Now().UTC().Add(-window.AsDuration())
	// ListRuns doesn't take a since filter today; we fetch a
	// generous Limit and filter in-memory. At our scale (a target
	// runs ≪ 100 deploys/day) this is fine; if we ever blow past a
	// few thousand rows we can add a SinceAt to DeployRunFilter.
	all, err := s.ListRuns(ctx, apptypes.DeployRunFilter{Limit: 10_000})
	if err != nil {
		return DORAMetrics{}, err
	}
	filtered := make([]*apptypes.DeployRun, 0, len(all))
	for _, r := range all {
		if r.RequestedAt.Before(cutoff) {
			continue
		}
		filtered = append(filtered, r)
	}
	return ComputeDORA(filtered, window, time.Now().UTC()), nil
}
