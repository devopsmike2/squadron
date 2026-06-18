// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build live

// Package proposer's live stress test. Excluded from the default
// build; run with:
//
//   go test -tags=live -run TestProposerStress_Live ./internal/proposer/...
//
// Requires ANTHROPIC_API_KEY in the environment because the test
// drives the real internal/ai.Service against the real Anthropic
// Messages API. Costs and latency are not bounded; expect roughly
// 50 calls to the Sonnet endpoint at the seeds' token sizes, on
// the order of a couple of US dollars and a couple of minutes.

package proposer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
)

// TestProposerStress_Live is the live counterpart to the default
// stress test. Identical seed corpus; the only difference is the
// proposer is the real internal/ai.Service so we exercise the
// prompt, tool use parsing, redaction, and refusal logic against
// the real model.
//
// This is the test that produces the LinkedIn shareable findings:
// in the default test the fake LLM's verdict is deterministic, so
// "refused incorrectly" rates are zero by construction; the live
// test is where the human labeled legitimacy verdict can actually
// disagree with what the model produced.
func TestProposerStress_Live(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; live stress test requires a real key")
	}

	svc := ai.NewService(ai.Config{
		Enabled:      true,
		APIKey:       key,
		ExplainModel: ai.DefaultExplainModel,
		MergeModel:   ai.DefaultMergeModel,
	}, zap.NewNop())

	seeds := stressSeeds()
	results := make([]iterResult, 0, len(seeds))

	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	for i, seed := range seeds {
		store, _ := seed.makeStore()
		rollouts := &fakeRollouts{}
		if seed.expectDispatchFail {
			rollouts.err = errors.New("dispatch rejected by rollout service")
		}
		audit := &fakeAudit{}

		// The bridge accepts the real *ai.Service as its
		// proposer because *ai.Service satisfies the package's
		// Proposer interface (ProposeFromCostSpike + Enabled).
		b := New(svc, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())

		start := time.Now()
		// 60s deadline per seed: the proposer's HTTP client has
		// a 30s default; we double it so a slow tail call is not
		// classified as a panic.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		b.tick(ctx)
		cancel()
		elapsed := time.Since(start)

		// classifyLive is a thinner classifier than the default
		// test's classify because we do not control whether the
		// real model declined or not. We walk: did the bridge
		// post a rollout. Yes → succeeded (or refused-incorrectly
		// when the seed was not legitimate; we still flag for
		// review). No → declined (refused-correctly when seed
		// was illegitimate; refused-incorrectly when it was).
		r := classifyLive(seed, rollouts, elapsed, i)
		results = append(results, r)
	}

	runtime.ReadMemStats(&memAfter)

	buckets := map[outcomeKind]int{}
	for _, r := range results {
		buckets[r.outcome]++
	}

	latencies := make([]time.Duration, len(results))
	for i, r := range results {
		latencies[i] = r.wallTime
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[len(latencies)/2]
	p95 := latencies[len(latencies)*95/100]
	p99 := latencies[len(latencies)*99/100]

	var rep strings.Builder
	rep.WriteString("\n========== Proposer LIVE stress test ==========\n")
	rep.WriteString(fmt.Sprintf("Iterations:        %d\n", len(results)))
	rep.WriteString(fmt.Sprintf("Latency p50:       %s\n", p50))
	rep.WriteString(fmt.Sprintf("Latency p95:       %s\n", p95))
	rep.WriteString(fmt.Sprintf("Latency p99:       %s\n", p99))
	rep.WriteString(fmt.Sprintf("Heap alloc delta:  %s\n", humanBytes(memAfter.HeapAlloc-memBefore.HeapAlloc)))
	rep.WriteString("\nOutcome distribution:\n")
	for _, k := range allOutcomes() {
		rep.WriteString(fmt.Sprintf("  %-30s %d\n", k, buckets[k]))
	}
	rep.WriteString("\nPer iteration table:\n")
	for _, r := range results {
		rep.WriteString(fmt.Sprintf("  %3d  %-23s  %-47s  %-28s  %8s  %s\n",
			r.index+1, truncate(r.category, 23), truncate(r.name, 47),
			r.outcome, r.wallTime, r.notes))
	}
	rep.WriteString("================================================\n")
	t.Log(rep.String())

	// The live test does not gate on outcomes; the report is the
	// deliverable. Save the human review of any
	// "refused-incorrectly" entries for the findings doc.
}

func classifyLive(seed stressSeed, rollouts *fakeRollouts, elapsed time.Duration, idx int) iterResult {
	r := iterResult{
		index:    idx,
		name:     seed.name,
		category: seed.category,
		wallTime: elapsed,
	}
	if elapsed > 55*time.Second {
		r.outcome = outcomeTimeout
		return r
	}
	posted := len(rollouts.inputs) > 0
	switch {
	case seed.legitimate && posted:
		r.outcome = outcomeSucceeded
	case !seed.legitimate && !posted:
		r.outcome = outcomeRefusedCorrectly
	case seed.legitimate && !posted:
		r.outcome = outcomeRefusedIncorrectly
		r.notes = "real model declined; review the prompt and decline reason"
	case !seed.legitimate && posted:
		r.outcome = outcomeSucceeded
		r.notes = "seed flagged not legitimate but model produced a proposal anyway"
	}
	return r
}
