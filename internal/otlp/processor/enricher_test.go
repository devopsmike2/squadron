package processor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/devopsmike2/squadron/internal/otlp"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// countingAgentService implements services.AgentService by embedding the
// interface (nil) and overriding only GetAgent — the sole method the enricher
// calls. It counts GetAgent invocations (atomically, so the concurrency test
// is race-clean) so tests can assert the per-batch memo collapses duplicate
// lookups. agents is read-only after construction (safe for concurrent reads).
type countingAgentService struct {
	services.AgentService
	agents map[uuid.UUID]*services.Agent
	calls  atomic.Int64
}

func (c *countingAgentService) GetAgent(_ context.Context, id uuid.UUID) (*services.Agent, error) {
	c.calls.Add(1)
	return c.agents[id], nil // nil => not found; enricher treats as no-op
}

func agentWithGroup(id uuid.UUID, gid, gname string) *services.Agent {
	return &services.Agent{ID: id, GroupID: &gid, GroupName: &gname}
}

// A single-agent 50-item batch must hit GetAgent exactly ONCE, and every item
// must still be enriched identically.
func TestEnrichTraces_SingleAgent_OneLookup(t *testing.T) {
	id := uuid.New()
	svc := &countingAgentService{agents: map[uuid.UUID]*services.Agent{
		id: agentWithGroup(id, "grp-1", "prod"),
	}}
	e := NewEnricher(svc, zap.NewNop())

	traces := make([]otlp.TraceData, 50)
	for i := range traces {
		traces[i].AgentID = id.String()
	}
	e.EnrichTraces(context.Background(), traces)

	if got := svc.calls.Load(); got != 1 {
		t.Fatalf("GetAgent calls = %d, want 1 (memo should collapse the 50-item single-agent batch)", got)
	}
	for i := range traces {
		if traces[i].GroupID != "grp-1" || traces[i].GroupName != "prod" {
			t.Fatalf("trace %d not enriched: group_id=%q group_name=%q", i, traces[i].GroupID, traces[i].GroupName)
		}
	}
}

// N unique agents interleaved across a batch => exactly N lookups (one per
// unique id), each enriched with its own group.
func TestEnrichTraces_MultipleAgents_OneLookupEach(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	svc := &countingAgentService{agents: map[uuid.UUID]*services.Agent{
		a: agentWithGroup(a, "ga", "na"),
		b: agentWithGroup(b, "gb", "nb"),
		c: agentWithGroup(c, "gc", "nc"),
	}}
	e := NewEnricher(svc, zap.NewNop())

	ids := []uuid.UUID{a, b, c}
	traces := make([]otlp.TraceData, 30)
	for i := range traces {
		traces[i].AgentID = ids[i%3].String() // interleaved a,b,c,a,b,c,...
	}
	e.EnrichTraces(context.Background(), traces)

	if got := svc.calls.Load(); got != 3 {
		t.Fatalf("GetAgent calls = %d, want 3 (one per unique agent)", got)
	}
	want := map[string]string{a.String(): "ga", b.String(): "gb", c.String(): "gc"}
	for i := range traces {
		if traces[i].GroupID != want[traces[i].AgentID] {
			t.Fatalf("trace %d agent %s enriched group_id=%q, want %q", i, traces[i].AgentID, traces[i].GroupID, want[traces[i].AgentID])
		}
	}
}

// An unknown agent id repeated across a batch is looked up ONCE (negative
// memo) and leaves group fields empty.
func TestEnrichTraces_UnknownAgent_NegativeMemoized(t *testing.T) {
	unknown := uuid.New().String()
	svc := &countingAgentService{agents: map[uuid.UUID]*services.Agent{}} // empty => always not found
	e := NewEnricher(svc, zap.NewNop())

	traces := make([]otlp.TraceData, 25)
	for i := range traces {
		traces[i].AgentID = unknown
	}
	e.EnrichTraces(context.Background(), traces)

	if got := svc.calls.Load(); got != 1 {
		t.Fatalf("GetAgent calls = %d, want 1 (misses must be negative-memoized)", got)
	}
	for i := range traces {
		if traces[i].GroupID != "" || traces[i].GroupName != "" {
			t.Fatalf("trace %d unexpectedly enriched: group_id=%q group_name=%q", i, traces[i].GroupID, traces[i].GroupName)
		}
	}
}

// EnrichMetrics shares one memo across sums+gauges+histograms of the same
// batch: the same agent across all three slices is looked up once.
func TestEnrichMetrics_SharedMemoAcrossSlices(t *testing.T) {
	id := uuid.New()
	svc := &countingAgentService{agents: map[uuid.UUID]*services.Agent{
		id: agentWithGroup(id, "grp-m", "metrics"),
	}}
	e := NewEnricher(svc, zap.NewNop())

	sums := make([]otlp.MetricSumData, 10)
	gauges := make([]otlp.MetricGaugeData, 10)
	histos := make([]otlp.MetricHistogramData, 10)
	for i := 0; i < 10; i++ {
		sums[i].AgentID = id.String()
		gauges[i].AgentID = id.String()
		histos[i].AgentID = id.String()
	}
	e.EnrichMetrics(context.Background(), sums, gauges, histos)

	if got := svc.calls.Load(); got != 1 {
		t.Fatalf("GetAgent calls = %d, want 1 (one memo across sums+gauges+histograms)", got)
	}
	if sums[9].GroupID != "grp-m" || gauges[9].GroupID != "grp-m" || histos[9].GroupID != "grp-m" {
		t.Fatalf("metrics not enriched across all slices: sum=%q gauge=%q histo=%q", sums[9].GroupID, gauges[9].GroupID, histos[9].GroupID)
	}
}

// The memo is per-batch LOCAL state, so concurrent Enrich* calls on the SHARED
// singleton enricher must be race-free. Run under `go test -race`.
func TestEnricher_ConcurrentBatches_NoSharedState(t *testing.T) {
	id := uuid.New()
	svc := &countingAgentService{agents: map[uuid.UUID]*services.Agent{
		id: agentWithGroup(id, "grp-x", "x"),
	}}
	e := NewEnricher(svc, zap.NewNop())

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			traces := make([]otlp.TraceData, 20)
			for i := range traces {
				traces[i].AgentID = id.String()
			}
			e.EnrichTraces(context.Background(), traces)
			for i := range traces {
				if traces[i].GroupID != "grp-x" {
					t.Errorf("concurrent enrich missed: %q", traces[i].GroupID)
				}
			}
		}()
	}
	wg.Wait()
	// 8 independent batches, each memoizes independently => exactly 8 lookups.
	if got := svc.calls.Load(); got != 8 {
		t.Fatalf("GetAgent calls = %d, want 8 (one per concurrent batch)", got)
	}
}
