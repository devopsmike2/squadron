package processor

import (
	"context"

	"github.com/devopsmike2/squadron/internal/otlp"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Enricher enriches telemetry data with group information based on agent lookups
type Enricher struct {
	agentService services.AgentService
	logger       *zap.Logger
}

// NewEnricher creates a new telemetry enricher
func NewEnricher(agentService services.AgentService, logger *zap.Logger) *Enricher {
	return &Enricher{
		agentService: agentService,
		logger:       logger,
	}
}

// EnrichTraces enriches traces with group information
func (e *Enricher) EnrichTraces(ctx context.Context, traces []otlp.TraceData) {
	memo := make(map[string]*services.Agent) // per-batch agent memo (see enrichTelemetry)
	for i := range traces {
		e.enrichTelemetry(ctx, memo, &traces[i].AgentID, &traces[i].GroupID, &traces[i].GroupName)
	}
}

// EnrichMetrics enriches metrics with group information
func (e *Enricher) EnrichMetrics(ctx context.Context, sums []otlp.MetricSumData, gauges []otlp.MetricGaugeData, histograms []otlp.MetricHistogramData) {
	memo := make(map[string]*services.Agent) // one memo across the whole metrics batch (sums+gauges+histograms share agents)

	// Enrich sums
	for i := range sums {
		e.enrichTelemetry(ctx, memo, &sums[i].AgentID, &sums[i].GroupID, &sums[i].GroupName)
	}

	// Enrich gauges
	for i := range gauges {
		e.enrichTelemetry(ctx, memo, &gauges[i].AgentID, &gauges[i].GroupID, &gauges[i].GroupName)
	}

	// Enrich histograms
	for i := range histograms {
		e.enrichTelemetry(ctx, memo, &histograms[i].AgentID, &histograms[i].GroupID, &histograms[i].GroupName)
	}
}

// EnrichLogs enriches logs with group information
func (e *Enricher) EnrichLogs(ctx context.Context, logs []otlp.LogData) {
	memo := make(map[string]*services.Agent) // per-batch agent memo (see enrichTelemetry)
	for i := range logs {
		e.enrichTelemetry(ctx, memo, &logs[i].AgentID, &logs[i].GroupID, &logs[i].GroupName)
	}
}

// enrichTelemetry populates group_id and group_name from the agent's group.
//
// memo collapses the agent lookup to once per unique agent id per batch: a
// 50-item single-agent batch previously issued 50 identical GetAgent (SQLite)
// reads. It caches both hits AND misses (nil) so a repeated or unknown agent
// id within the batch is never looked up twice. memo is a caller-owned,
// per-batch LOCAL map — never a shared field — so the singleton Enricher stays
// race-free under the worker pool's concurrent workers. Output is identical to
// the pre-memo path.
func (e *Enricher) enrichTelemetry(ctx context.Context, memo map[string]*services.Agent, agentID *string, groupID *string, groupName *string) {
	// Skip if agentID is empty
	if agentID == nil || *agentID == "" || *agentID == "default" {
		return
	}

	agent, seen := memo[*agentID]
	if !seen {
		agent = e.lookupAgent(ctx, *agentID) // the one real lookup for this id this batch
		memo[*agentID] = agent               // cache the hit OR the miss (nil)
	}
	if agent == nil {
		return
	}

	// Populate group information if agent has a group
	if agent.GroupID != nil && *agent.GroupID != "" {
		*groupID = *agent.GroupID
	}

	if agent.GroupName != nil && *agent.GroupName != "" {
		*groupName = *agent.GroupName
	}
}

// lookupAgent parses the id and fetches the agent, returning nil for a
// malformed id, a not-found agent, or a lookup error — all treated as
// "no enrichment", matching the prior behavior.
func (e *Enricher) lookupAgent(ctx context.Context, agentID string) *services.Agent {
	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		e.logger.Debug("Failed to parse agent ID for enrichment",
			zap.String("agentID", agentID),
			zap.Error(err))
		return nil
	}

	agent, err := e.agentService.GetAgent(ctx, agentUUID)
	if err != nil || agent == nil {
		// Agent not found - this can happen for telemetry sent before agent registers.
		// Debug level because it's expected in some scenarios.
		e.logger.Debug("Agent not found for telemetry enrichment",
			zap.String("agentID", agentID),
			zap.Error(err))
		return nil
	}
	return agent
}
