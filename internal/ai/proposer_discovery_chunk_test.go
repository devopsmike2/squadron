package ai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunDiscoveryChunks_RespectsDeadline pins the async-recommendations
// hang fix (fix/discovery-recommendations-job-timeout). Before it, a
// stalled upstream model call held a tier chunk open until the HTTP
// client's 180s timeout, and six serial tiers pushed the job past the
// operator's patience — the job sat in "running" for minutes. Now each
// chunk runs under a per-call deadline derived from the job context, so a
// stalled call must fail the job PROMPTLY instead of hanging. A blocking
// fake server holds every /v1/messages request open until its request
// context is cancelled; a short-deadline parent context must drive
// runDiscoveryChunks to a prompt error, not a hang.
func TestRunDiscoveryChunks_RespectsDeadline(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// Stall this request so the caller's per-chunk deadline is what
		// ends the call. The handler returns as soon as the client
		// cancels (r.Context() fires) OR after a bounded 3s backstop, so
		// httptest.Server.Close never blocks on a wedged handler even if
		// the client-side cancellation doesn't propagate to the server's
		// request context promptly. If the product lacked a per-chunk
		// deadline, client.Do would instead block for the full 180s HTTP
		// client timeout — which the sub-second assertion below rejects.
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)

	// A 3-tier inventory trips the chunking / fan-out path (tiers >= 3).
	in := &DiscoveryScanContext{
		ScanID:    "scan-slow",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		ComputeInstances: []ComputeResourceCandidate{
			{ResourceID: "i-aaa", InstanceType: "t3.micro", Region: "us-east-1"},
		},
		Functions: []FunctionResourceCandidate{
			{ResourceID: "arn:aws:lambda:us-east-1:123:function:hello", Name: "hello", Runtime: "python3.11", Region: "us-east-1"},
		},
		Databases: []DatabaseResourceCandidate{
			{ResourceID: "db-1", Engine: "postgres", Region: "us-east-1"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := svc.runDiscoveryChunks(ctx, in)
	elapsed := time.Since(start)

	require.Error(t, err, "a stalled model call must fail the job, not hang")
	assert.Less(t, elapsed, 30*time.Second,
		"the deadline must fail the job promptly, well under the 180s HTTP client timeout")
	assert.GreaterOrEqual(t, atomic.LoadInt32(&hits), int32(1),
		"the proposer should have attempted at least one upstream call")
}

// TestRunDiscoveryChunks_FansOutTiersConcurrently guards the concurrent
// fan-out: every tier chunk must be dispatched and its plan step merged
// back (run under -race, this also guards the per-slot result writes
// against a data race). A fast fake server returns one plan step per
// call; a 3-tier inventory must yield 3 upstream calls and 3 merged steps.
func TestRunDiscoveryChunks_FansOutTiersConcurrently(t *testing.T) {
	var hits int32
	reply := anthropicReply(`{
  "kind": "plan",
  "declined": false,
  "plan": {
    "steps": [
      {
        "name": "instrument tier",
        "group_id": "123456789012",
        "inline_config_snippet": "resource \"aws_x\" \"y\" {}\n",
        "require_approval": true,
        "stages": [{"mode":"percent","percentage":100,"dwell_seconds":0}],
        "abort_criteria": {"max_drifted_agents":5,"max_error_logs_per_minute":50,"min_dwell_seconds_before_abort":120}
      }
    ]
  }
}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	in := &DiscoveryScanContext{
		ScanID:    "scan-fan",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		ComputeInstances: []ComputeResourceCandidate{
			{ResourceID: "i-aaa", InstanceType: "t3.micro", Region: "us-east-1"},
		},
		Functions: []FunctionResourceCandidate{
			{ResourceID: "arn:aws:lambda:us-east-1:123:function:hello", Name: "hello", Runtime: "python3.11", Region: "us-east-1"},
		},
		Databases: []DatabaseResourceCandidate{
			{ResourceID: "db-1", Engine: "postgres", Region: "us-east-1"},
		},
	}

	res, err := svc.runDiscoveryChunks(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, int32(3), atomic.LoadInt32(&hits), "every tier chunk should hit the proposer")
	assert.Len(t, res.Plan.Steps, 3, "one merged plan step per tier")
	assert.False(t, res.Declined)
}

func TestDiscoveryInventorySize(t *testing.T) {
	in := &DiscoveryScanContext{
		ComputeInstances: make([]ComputeResourceCandidate, 2),
		Functions:        make([]FunctionResourceCandidate, 1),
		Databases:        make([]DatabaseResourceCandidate, 3),
	}
	tiers, res := discoveryInventorySize(in)
	if tiers != 3 {
		t.Fatalf("tiers: got %d want 3", tiers)
	}
	if res != 6 {
		t.Fatalf("resources: got %d want 6", res)
	}
}

func TestSplitDiscoveryByTier(t *testing.T) {
	in := &DiscoveryScanContext{
		ScanID:           "scan-1",
		AccountID:        "111122223333",
		Provider:         "aws",
		Regions:          []string{"us-east-1"},
		ComputeInstances: make([]ComputeResourceCandidate, 2),
		Functions:        make([]FunctionResourceCandidate, 1),
		ObjectStores:     make([]ObjectStoreCandidate, 4),
	}
	chunks := splitDiscoveryByTier(in)
	if len(chunks) != 3 {
		t.Fatalf("chunks: got %d want 3 (one per non-empty tier)", len(chunks))
	}
	for _, c := range chunks {
		// scope fields preserved on every chunk
		if c.ScanID != "scan-1" || c.AccountID != "111122223333" || c.Provider != "aws" {
			t.Fatalf("chunk lost scope fields: %+v", c)
		}
		// exactly one tier populated per chunk
		nonEmpty := 0
		if len(c.ComputeInstances) > 0 {
			nonEmpty++
		}
		if len(c.Functions) > 0 {
			nonEmpty++
		}
		if len(c.ObjectStores) > 0 {
			nonEmpty++
		}
		if nonEmpty != 1 {
			t.Fatalf("chunk should have exactly one non-empty tier, got %d", nonEmpty)
		}
	}
}

func TestMergeDiscoveryResults(t *testing.T) {
	// two chunks with steps + one declined -> merged has all steps, not declined
	a := &ProposalResult{Kind: ProposalKindPlan, Plan: PlanCandidate{Steps: make([]PlanStepCandidate, 2)}, TokensIn: 100, TokensOut: 50, Model: "m", Reasoning: "ra"}
	b := &ProposalResult{Kind: ProposalKindPlan, Plan: PlanCandidate{Steps: make([]PlanStepCandidate, 1)}, TokensIn: 80, TokensOut: 40, Reasoning: "rb"}
	c := &ProposalResult{Declined: true, Reason: "nothing for queues"}
	m := mergeDiscoveryResults([]*ProposalResult{a, b, c})
	if m.Declined {
		t.Fatal("merged should not be declined when any chunk has steps")
	}
	if len(m.Plan.Steps) != 3 {
		t.Fatalf("merged steps: got %d want 3", len(m.Plan.Steps))
	}
	if m.TokensIn != 180 || m.TokensOut != 90 {
		t.Fatalf("token sums wrong: in=%d out=%d", m.TokensIn, m.TokensOut)
	}
	if m.Model != "m" {
		t.Fatalf("model: got %q want m", m.Model)
	}
}

func TestMergeDiscoveryResults_AllDeclined(t *testing.T) {
	m := mergeDiscoveryResults([]*ProposalResult{
		{Declined: true, Reason: "a"},
		{Declined: true, Reason: "b"},
	})
	if !m.Declined {
		t.Fatal("merged should be declined when all chunks declined")
	}
	if len(m.Plan.Steps) != 0 {
		t.Fatalf("declined merge should have 0 steps, got %d", len(m.Plan.Steps))
	}
}
