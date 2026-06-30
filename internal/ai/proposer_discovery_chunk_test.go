package ai

import "testing"

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
