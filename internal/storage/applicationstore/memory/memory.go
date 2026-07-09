// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/devopsmike2/squadron/extension/identity"
	chain "github.com/devopsmike2/squadron/internal/audit/chain"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/devopsmike2/squadron/internal/traceindex"
	"github.com/google/uuid"
)

// memoryTraceIndexMaxRows mirrors the SQLite layer's
// defaultTraceIndexMaxRows. Slice 1 design doc §12.
const memoryTraceIndexMaxRows = 100_000

// Store is an in-memory implementation of ApplicationStore
type Store struct {
	mu           sync.RWMutex
	agents       map[uuid.UUID]*types.Agent
	groups       map[string]*types.Group
	configs      map[string]*types.Config
	savedQueries map[string]*types.SavedQuery
	alertRules   map[string]*types.AlertRule
	auditEvents  []*types.AuditEvent // append-only; sorted newest-first on read
	// v0.89.250 continuous-discovery slice 1 — persisted scans, append-only,
	// sorted newest-first on read.
	discoveryScans []*types.ScanRecord
	rollouts       map[string]*types.Rollout
	// ADR 0029 — N-of-M rollout approvals append-log. Outer key is
	// rollout_id, inner key is approver, so len(inner) is the
	// distinct-approver count. Guarded by the same mutex as rollouts.
	rolloutApprovals map[string]map[string]types.RolloutApproval
	apiTokens        map[string]*types.APIToken // keyed by ID; secondary index built on the fly for hash lookup
	// v0.25: recommendation dismissals — keyed by the engine's
	// deterministic recommendation_id hash.
	recDismissals map[string]*types.RecommendationDismissal
	// v0.28: outcomes from Apply clicks, keyed by outcome.ID.
	recOutcomes map[string]*types.RecommendationOutcome
	// v0.29: cost-spike events, keyed by event.ID. Ordered list
	// is rebuilt on read; spike volume is tiny so this is fine.
	costSpikes map[string]*types.CostSpikeEvent
	// v0.32: expected agents (inventory reconciliation), keyed by
	// hostname. Source filtering at read time.
	expectedAgents map[string]*types.ExpectedAgent
	// v0.34: deploy targets + runs (GitHub Actions integration).
	// Targets keyed by ID, runs keyed by ID.
	deployTargets map[string]*types.DeployTarget
	deployRuns    map[string]*types.DeployRun
	// v0.50: SIEM destinations, keyed by ID.
	siemDestinations map[string]*types.SiemDestination
	// v0.53: action runner registrations, keyed by runner_id, and
	// action requests keyed by id. Move 2 (action runner).
	actionRunners  map[string]*types.ActionRunnerRegistration
	actionRequests map[string]*types.ActionRequest
	// SQ-3 incident drafts, keyed by id. Move 3 (engineer copilot
	// auto-drafted ticket).
	incidentDrafts map[string]*types.IncidentDraft
	// v0.89.30 (#649) — webhook delivery dedupe. One entry per
	// X-GitHub-Delivery UUID the receiver has observed. The
	// store-wide mu mutex guards check + insert atomicity so two
	// concurrent replays of the same delivery_id can't both observe
	// firstTime=true.
	webhookDeliveries map[string]webhookDeliveryRecord
	// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) — operator-set
	// exclusion table for discovery recommendations. Keyed on
	// recommendation_id (PK), mirroring the SQLite
	// iac_recommendation_verdicts table.
	excludedRecommendations map[string]types.ExcludedRecommendation
	// excludedRecommendationFlags stores the exclude_from_learning
	// bit per row. The bridge's ListExcludedRecommendations only
	// surfaces rows with the bit set, but the row stays around with
	// the bit clear so a future re-toggle can return prevExcluded
	// honestly.
	excludedRecommendationFlags map[string]bool
	// v0.89.42 (#662 Stream 60, slice 1 chunk 1 of the GitHub Checks
	// API back-signal arc) — durable check-run state keyed on
	// recommendation_id. Mirrors the 5 new columns on the SQLite
	// iac_recommendation_verdicts table (check_run_id, head_sha,
	// status, conclusion, updated_at). One map keeps the in-memory
	// projection compact and lets the chunk-2/3/4 callers round-trip
	// the same CheckRunRef + status / conclusion the SQLite path
	// returns.
	checkRunState map[string]memoryCheckRunRecord

	// v0.89.74 (#705 Stream 103, slice 1 chunk 1 of the Trace
	// integration arc) — trace_resource_seen mirror. Keyed by
	// resource_key (the §3 fallback-chain projection). traceMaxRows
	// is the LRU cap enforced on UpsertTraceResources; defaults to
	// 100K per design doc §12 and overridable by the wiring layer
	// (chunk 2) via SetTraceIndexMaxRowsForTest. The SQLite store
	// reads the operator-supplied env var on construction; the
	// memory store keeps the same default so tests don't need to
	// touch the environment.
	traceResourceSeen map[string]traceindex.ResourceRow
	traceMaxRows      int
	// ADR 0027 slice 1 — per-tenant audit hash-chain mirror so the memory
	// store returns the same VerifyAuditChain OK the sqlite store does
	// (test-only parity). Keyed by tenant; each slice is the ordered chain.
	auditChains map[string][]memAuditChainRow
	// ADR 0027 slice 2 — retention checkpoints, keyed by (tenant, seq).
	// Test-only mirror of the sqlite audit_chain_checkpoints table.
	auditCheckpoints map[string]map[int64]types.AuditCheckpoint
}

// memoryCheckRunRecord — v0.89.42 (#662 Stream 60). Mirror of the 5
// check_run_* columns the v9 migration adds to the SQLite
// iac_recommendation_verdicts table. Kept private to the memory
// package so the storage interface stays cleanly described by the
// public CheckRunRef + status / conclusion strings.
type memoryCheckRunRecord struct {
	ref        types.CheckRunRef
	status     string
	conclusion string
	updatedAt  time.Time
}

// webhookDeliveryRecord is the memory-store payload for one
// X-GitHub-Delivery UUID. Mirrors the SQLite webhook_delivery_dedupe
// row shape — receivedAt + eventType, keyed externally by delivery_id.
// v0.89.30 (#649).
type webhookDeliveryRecord struct {
	receivedAt time.Time
	eventType  string
}

// NewStore creates a new in-memory store
func NewStore() *Store {
	return &Store{
		agents:           make(map[uuid.UUID]*types.Agent),
		groups:           make(map[string]*types.Group),
		configs:          make(map[string]*types.Config),
		savedQueries:     make(map[string]*types.SavedQuery),
		alertRules:       make(map[string]*types.AlertRule),
		auditEvents:      make([]*types.AuditEvent, 0, 64),
		rollouts:         make(map[string]*types.Rollout),
		rolloutApprovals: make(map[string]map[string]types.RolloutApproval),
		apiTokens:        make(map[string]*types.APIToken),
		recDismissals:    make(map[string]*types.RecommendationDismissal),
		recOutcomes:      make(map[string]*types.RecommendationOutcome),
		costSpikes:       make(map[string]*types.CostSpikeEvent),
		expectedAgents:   make(map[string]*types.ExpectedAgent),
		deployTargets:    make(map[string]*types.DeployTarget),
		deployRuns:       make(map[string]*types.DeployRun),
		siemDestinations: make(map[string]*types.SiemDestination),
		actionRunners:    make(map[string]*types.ActionRunnerRegistration),
		actionRequests:   make(map[string]*types.ActionRequest),
		incidentDrafts:   make(map[string]*types.IncidentDraft),
		// v0.89.30 (#649).
		webhookDeliveries: make(map[string]webhookDeliveryRecord),
		// v0.89.37 (#656 Stream 54).
		excludedRecommendations:     make(map[string]types.ExcludedRecommendation),
		excludedRecommendationFlags: make(map[string]bool),
		// v0.89.42 (#662 Stream 60).
		checkRunState: make(map[string]memoryCheckRunRecord),
		// v0.89.74 (#705 Stream 103).
		traceResourceSeen: make(map[string]traceindex.ResourceRow),
		traceMaxRows:      memoryTraceIndexMaxRows,
		// ADR 0027 slice 1.
		auditChains: make(map[string][]memAuditChainRow),
		// ADR 0027 slice 2.
		auditCheckpoints: make(map[string]map[int64]types.AuditCheckpoint),
	}
}

// RecordWebhookDelivery — v0.89.30 (#649). Memory-store mirror of the
// SQLite same-named method. Check + insert under the store-wide mutex
// so concurrent replays of the same delivery_id can't both observe
// firstTime=true.
func (s *Store) RecordWebhookDelivery(_ context.Context, deliveryID, eventType string) (bool, time.Time, error) {
	if deliveryID == "" {
		return false, time.Time{}, fmt.Errorf("delivery_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.webhookDeliveries[deliveryID]; ok {
		return false, existing.receivedAt, nil
	}
	now := time.Now().UTC()
	s.webhookDeliveries[deliveryID] = webhookDeliveryRecord{
		receivedAt: now,
		eventType:  eventType,
	}
	return true, now, nil
}

// GCWebhookDeliveries — v0.89.30 (#649). Memory-store mirror of the
// SQLite same-named method. Linear sweep over the dedupe map deleting
// entries whose receivedAt < before; returns the count deleted.
func (s *Store) GCWebhookDeliveries(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := 0
	for id, rec := range s.webhookDeliveries {
		if rec.receivedAt.Before(before) {
			delete(s.webhookDeliveries, id)
			deleted++
		}
	}
	return deleted, nil
}

// SetWebhookDeliveryReceivedAtForTest is a v0.89.30 (#649) test
// affordance: back-dates the receivedAt on an existing dedupe row so
// the GC sweep can be exercised without sleeping past the retention
// window. Production code never calls this; the SQLite store has no
// equivalent (tests there can INSERT with an explicit timestamp).
// No-op when the id isn't present.
func (s *Store) SetWebhookDeliveryReceivedAtForTest(deliveryID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.webhookDeliveries[deliveryID]
	if !ok {
		return
	}
	rec.receivedAt = at
	s.webhookDeliveries[deliveryID] = rec
}

// Agent management

func (s *Store) CreateAgent(ctx context.Context, agent *types.Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.agents[agent.ID]; exists {
		return fmt.Errorf("agent already exists: %s", agent.ID)
	}

	// Deep copy to prevent external modifications
	agentCopy := *agent
	if agent.Labels != nil {
		agentCopy.Labels = make(map[string]string, len(agent.Labels))
		for k, v := range agent.Labels {
			agentCopy.Labels[k] = v
		}
	}
	if agent.Capabilities != nil {
		agentCopy.Capabilities = make([]string, len(agent.Capabilities))
		copy(agentCopy.Capabilities, agent.Capabilities)
	}

	s.agents[agent.ID] = &agentCopy
	return nil
}

func (s *Store) GetAgent(ctx context.Context, id uuid.UUID) (*types.Agent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	agent, exists := s.agents[id]
	if !exists {
		return nil, nil
	}

	// Deep copy to prevent external modifications
	agentCopy := *agent
	if agent.Labels != nil {
		agentCopy.Labels = make(map[string]string, len(agent.Labels))
		for k, v := range agent.Labels {
			agentCopy.Labels[k] = v
		}
	}
	if agent.Capabilities != nil {
		agentCopy.Capabilities = make([]string, len(agent.Capabilities))
		copy(agentCopy.Capabilities, agent.Capabilities)
	}

	// v0.51 — tombstoned agents are hidden from GetAgent for the
	// operational view. The audit trail keyed by ID still resolves
	// via the audit_events table.
	if agentCopy.DeletedAt != nil {
		return nil, nil
	}
	return &agentCopy, nil
}

func (s *Store) ListAgents(ctx context.Context) ([]*types.Agent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	agents := make([]*types.Agent, 0, len(s.agents))
	for _, agent := range s.agents {
		// v0.51 — tombstoned agents stay in the map for audit
		// resolution but are hidden from the operational list.
		if agent.DeletedAt != nil {
			continue
		}
		// Deep copy
		agentCopy := *agent
		if agent.Labels != nil {
			agentCopy.Labels = make(map[string]string, len(agent.Labels))
			for k, v := range agent.Labels {
				agentCopy.Labels[k] = v
			}
		}
		if agent.Capabilities != nil {
			agentCopy.Capabilities = make([]string, len(agent.Capabilities))
			copy(agentCopy.Capabilities, agent.Capabilities)
		}
		agents = append(agents, &agentCopy)
	}

	return agents, nil
}

func (s *Store) UpdateAgentStatus(ctx context.Context, id uuid.UUID, status types.AgentStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, exists := s.agents[id]
	if !exists {
		return fmt.Errorf("agent not found: %s", id)
	}

	agent.Status = status
	agent.UpdatedAt = time.Now()
	return nil
}

func (s *Store) UpdateAgentLastSeen(ctx context.Context, id uuid.UUID, lastSeen time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, exists := s.agents[id]
	if !exists {
		return fmt.Errorf("agent not found: %s", id)
	}

	agent.LastSeen = lastSeen
	agent.UpdatedAt = time.Now()
	return nil
}

func (s *Store) UpdateAgentEffectiveConfig(ctx context.Context, id uuid.UUID, effectiveConfig string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, exists := s.agents[id]
	if !exists {
		return fmt.Errorf("agent not found: %s", id)
	}

	agent.EffectiveConfig = effectiveConfig
	agent.UpdatedAt = time.Now()
	return nil
}

// UpdateAgentRegistration writes the mutable registration/grouping fields
// (Name, Labels, Version, GroupID, GroupName) of an existing agent.
// Mirrors the sqlite impl; deep-copies Labels so the caller's map can't
// alias the stored one.
func (s *Store) UpdateAgentRegistration(ctx context.Context, agent *types.Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, exists := s.agents[agent.ID]
	if !exists || existing.DeletedAt != nil {
		return fmt.Errorf("agent not found: %s", agent.ID)
	}
	existing.Name = agent.Name
	if agent.Labels != nil {
		existing.Labels = make(map[string]string, len(agent.Labels))
		for k, v := range agent.Labels {
			existing.Labels[k] = v
		}
	} else {
		existing.Labels = nil
	}
	existing.Version = agent.Version
	existing.GroupID = agent.GroupID
	existing.GroupName = agent.GroupName
	existing.UpdatedAt = time.Now()
	return nil
}

func (s *Store) DeleteAgent(ctx context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, exists := s.agents[id]
	if !exists {
		return fmt.Errorf("agent not found: %s", id)
	}
	// v0.51 — soft delete. Keep the row so audit events still
	// resolve by ID; ListAgents filters tombstones out.
	now := time.Now().UTC()
	agent.DeletedAt = &now
	agent.UpdatedAt = now
	return nil
}

// Group management

func (s *Store) CreateGroup(ctx context.Context, group *types.Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.groups[group.ID]; exists {
		return fmt.Errorf("group already exists: %s", group.ID)
	}

	// Deep copy
	groupCopy := *group
	if group.Labels != nil {
		groupCopy.Labels = make(map[string]string, len(group.Labels))
		for k, v := range group.Labels {
			groupCopy.Labels[k] = v
		}
	}

	s.groups[group.ID] = &groupCopy
	return nil
}

func (s *Store) GetGroup(ctx context.Context, id string) (*types.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	group, exists := s.groups[id]
	if !exists {
		return nil, nil
	}

	// Deep copy
	groupCopy := *group
	if group.Labels != nil {
		groupCopy.Labels = make(map[string]string, len(group.Labels))
		for k, v := range group.Labels {
			groupCopy.Labels[k] = v
		}
	}

	return &groupCopy, nil
}

func (s *Store) ListGroups(ctx context.Context) ([]*types.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	groups := make([]*types.Group, 0, len(s.groups))
	for _, group := range s.groups {
		// Deep copy
		groupCopy := *group
		if group.Labels != nil {
			groupCopy.Labels = make(map[string]string, len(group.Labels))
			for k, v := range group.Labels {
				groupCopy.Labels[k] = v
			}
		}
		groups = append(groups, &groupCopy)
	}

	return groups, nil
}

// UpdateGroup writes mutable fields onto the stored group. Added in
// v0.48 for the approval-policy toggle; extended in v0.49 to
// round-trip ChangeWindowsJSON; v0.89.17 round-trips
// LearnFromVerdicts.
func (s *Store) UpdateGroup(ctx context.Context, group *types.Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.groups[group.ID]
	if !ok {
		return fmt.Errorf("group not found: %s", group.ID)
	}
	// Preserve immutable fields (ID, CreatedAt) so a careless caller
	// can't rewrite them. Anything else on group overwrites.
	existing.Name = group.Name
	existing.Labels = group.Labels
	existing.RequireApproval = group.RequireApproval
	existing.RequireApprovalForRollback = group.RequireApprovalForRollback
	existing.ChangeWindowsJSON = group.ChangeWindowsJSON
	existing.LearnFromVerdicts = group.LearnFromVerdicts
	existing.UpdatedAt = group.UpdatedAt
	return nil
}

func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.groups[id]; !exists {
		return fmt.Errorf("group not found: %s", id)
	}

	delete(s.groups, id)
	return nil
}

// Config management

func (s *Store) CreateConfig(ctx context.Context, config *types.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.configs[config.ID]; exists {
		return fmt.Errorf("config already exists: %s", config.ID)
	}

	// Deep copy
	configCopy := *config
	s.configs[config.ID] = &configCopy
	return nil
}

func (s *Store) GetConfig(ctx context.Context, id string) (*types.Config, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	config, exists := s.configs[id]
	if !exists {
		return nil, nil
	}

	// Deep copy
	configCopy := *config
	return &configCopy, nil
}

func (s *Store) GetLatestConfigForAgent(ctx context.Context, agentID uuid.UUID) (*types.Config, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var latestConfig *types.Config
	for _, config := range s.configs {
		if config.AgentID != nil && *config.AgentID == agentID {
			if latestConfig == nil || config.Version > latestConfig.Version ||
				(config.Version == latestConfig.Version && config.CreatedAt.After(latestConfig.CreatedAt)) {
				latestConfig = config
			}
		}
	}

	if latestConfig == nil {
		return nil, nil
	}

	// Deep copy
	configCopy := *latestConfig
	return &configCopy, nil
}

func (s *Store) GetLatestConfigForGroup(ctx context.Context, groupID string) (*types.Config, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var latestConfig *types.Config
	for _, config := range s.configs {
		if config.GroupID != nil && *config.GroupID == groupID {
			if latestConfig == nil || config.Version > latestConfig.Version ||
				(config.Version == latestConfig.Version && config.CreatedAt.After(latestConfig.CreatedAt)) {
				latestConfig = config
			}
		}
	}

	if latestConfig == nil {
		return nil, nil
	}

	// Deep copy
	configCopy := *latestConfig
	return &configCopy, nil
}

func (s *Store) ListConfigs(ctx context.Context, filter types.ConfigFilter) ([]*types.Config, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	configs := make([]*types.Config, 0)
	for _, config := range s.configs {
		// Apply filters
		if filter.AgentID != nil && (config.AgentID == nil || *config.AgentID != *filter.AgentID) {
			continue
		}
		if filter.GroupID != nil && (config.GroupID == nil || *config.GroupID != *filter.GroupID) {
			continue
		}

		// Deep copy
		configCopy := *config
		configs = append(configs, &configCopy)
	}

	// Apply limit
	if filter.Limit > 0 && len(configs) > filter.Limit {
		configs = configs[:filter.Limit]
	}

	return configs, nil
}

// Saved query management
func (s *Store) CreateSavedQuery(ctx context.Context, query *types.SavedQuery) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.savedQueries[query.ID]; exists {
		return fmt.Errorf("saved query already exists: %s", query.ID)
	}
	queryCopy := *query
	if query.Tags != nil {
		queryCopy.Tags = make([]string, len(query.Tags))
		copy(queryCopy.Tags, query.Tags)
	}
	s.savedQueries[query.ID] = &queryCopy
	return nil
}

func (s *Store) GetSavedQuery(ctx context.Context, id string) (*types.SavedQuery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query, ok := s.savedQueries[id]
	if !ok {
		return nil, nil
	}
	queryCopy := *query
	if query.Tags != nil {
		queryCopy.Tags = make([]string, len(query.Tags))
		copy(queryCopy.Tags, query.Tags)
	}
	return &queryCopy, nil
}

func (s *Store) ListSavedQueries(ctx context.Context) ([]*types.SavedQuery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	queries := make([]*types.SavedQuery, 0, len(s.savedQueries))
	for _, sq := range s.savedQueries {
		queryCopy := *sq
		if sq.Tags != nil {
			queryCopy.Tags = make([]string, len(sq.Tags))
			copy(queryCopy.Tags, sq.Tags)
		}
		queries = append(queries, &queryCopy)
	}

	return queries, nil
}

func (s *Store) UpdateSavedQuery(ctx context.Context, query *types.SavedQuery) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.savedQueries[query.ID]
	if !ok {
		return fmt.Errorf("saved query not found: %s", query.ID)
	}
	existing.Name = query.Name
	existing.Description = query.Description
	existing.Query = query.Query
	existing.UpdatedAt = query.UpdatedAt
	if query.Tags != nil {
		existing.Tags = make([]string, len(query.Tags))
		copy(existing.Tags, query.Tags)
	} else {
		existing.Tags = nil
	}
	return nil
}

func (s *Store) DeleteSavedQuery(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.savedQueries[id]; !ok {
		return fmt.Errorf("saved query not found: %s", id)
	}
	delete(s.savedQueries, id)
	return nil
}

// Alert rule management

func (s *Store) CreateAlertRule(ctx context.Context, rule *types.AlertRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.alertRules[rule.ID]; exists {
		return fmt.Errorf("alert rule already exists: %s", rule.ID)
	}
	ruleCopy := *rule
	s.alertRules[rule.ID] = &ruleCopy
	return nil
}

func (s *Store) GetAlertRule(ctx context.Context, id string) (*types.AlertRule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rule, ok := s.alertRules[id]
	if !ok {
		return nil, nil
	}
	ruleCopy := *rule
	return &ruleCopy, nil
}

func (s *Store) ListAlertRules(ctx context.Context) ([]*types.AlertRule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rules := make([]*types.AlertRule, 0, len(s.alertRules))
	for _, r := range s.alertRules {
		ruleCopy := *r
		rules = append(rules, &ruleCopy)
	}
	return rules, nil
}

func (s *Store) UpdateAlertRule(ctx context.Context, rule *types.AlertRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.alertRules[rule.ID]; !ok {
		return fmt.Errorf("alert rule not found: %s", rule.ID)
	}
	ruleCopy := *rule
	s.alertRules[rule.ID] = &ruleCopy
	return nil
}

func (s *Store) DeleteAlertRule(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.alertRules[id]; !ok {
		return fmt.Errorf("alert rule not found: %s", id)
	}
	delete(s.alertRules, id)
	return nil
}

// Audit log management

func (s *Store) CreateAuditEvent(ctx context.Context, e *types.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Defensive copy of the payload so callers can't mutate stored state.
	eventCopy := *e
	if e.Payload != nil {
		eventCopy.Payload = make(map[string]any, len(e.Payload))
		for k, v := range e.Payload {
			eventCopy.Payload[k] = v
		}
	}
	s.auditEvents = append(s.auditEvents, &eventCopy)

	// ADR 0027 slice 1 — extend the per-tenant hash-chain so VerifyAuditChain
	// returns OK on the memory store too. Tenant resolves the same way the
	// sqlite append path resolves it (DefaultTenant when unstamped/system).
	tenant := identity.TenantFromContext(ctx)
	var payloadStr string
	if e.Payload != nil {
		b, _ := json.Marshal(e.Payload)
		payloadStr = string(b)
	}
	tenantChain := s.auditChains[tenant]
	var prevSeq int64
	var prevHash string
	if n := len(tenantChain); n > 0 {
		prevSeq = tenantChain[n-1].seq
		prevHash = tenantChain[n-1].rowHash
	}
	seq := prevSeq + 1
	rowHash := chain.RowHash(e.ID, e.Actor, e.EventType, e.TargetType, e.TargetID, e.Action, payloadStr, tenant, seq, prevHash)
	s.auditChains[tenant] = append(tenantChain, memAuditChainRow{
		id: e.ID, actor: e.Actor, eventType: e.EventType, targetType: e.TargetType,
		targetID: e.TargetID, action: e.Action, payloadStr: payloadStr,
		seq: seq, prevHash: prevHash, rowHash: rowHash,
	})
	return nil
}

func (s *Store) ListAuditEvents(ctx context.Context, filter types.AuditEventFilter) ([]*types.AuditEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	// Walk in reverse (newest-first), apply filters, accumulate up to limit.
	out := make([]*types.AuditEvent, 0)
	for i := len(s.auditEvents) - 1; i >= 0; i-- {
		e := s.auditEvents[i]
		if filter.EventType != "" && e.EventType != filter.EventType {
			continue
		}
		if filter.TargetType != "" && e.TargetType != filter.TargetType {
			continue
		}
		if filter.TargetID != "" && e.TargetID != filter.TargetID {
			continue
		}
		if filter.Actor != "" && e.Actor != filter.Actor {
			continue
		}
		if !filter.Since.IsZero() && e.Timestamp.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && !e.Timestamp.Before(filter.Until) {
			continue // Timestamp >= Until → excluded (Until is exclusive upper bound)
		}
		// Defensive copy on read too.
		eventCopy := *e
		if e.Payload != nil {
			eventCopy.Payload = make(map[string]any, len(e.Payload))
			for k, v := range e.Payload {
				eventCopy.Payload[k] = v
			}
		}
		out = append(out, &eventCopy)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// GetAuditEvent fetches a single audit row by ID. Returns (nil, nil) when
// the row is absent so the caller can render a 404 distinct from a 500.
func (s *Store) GetAuditEvent(ctx context.Context, id string) (*types.AuditEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.auditEvents {
		if e.ID != id {
			continue
		}
		eventCopy := *e
		if e.Payload != nil {
			eventCopy.Payload = make(map[string]any, len(e.Payload))
			for k, v := range e.Payload {
				eventCopy.Payload[k] = v
			}
		}
		return &eventCopy, nil
	}
	return nil, nil
}

// UpdateAuditEventExplanation writes the cached AI explanation in place
// on the stored row. Audit rows are otherwise immutable; this is the one
// mutation the store allows, and it only touches the three explanation
// fields.
func (s *Store) UpdateAuditEventExplanation(ctx context.Context, id, explanation, model string, generatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.auditEvents {
		if e.ID != id {
			continue
		}
		e.AIExplanation = explanation
		e.AIExplanationModel = model
		t := generatedAt
		e.AIExplanationGeneratedAt = &t
		return nil
	}
	return fmt.Errorf("audit event %q not found", id)
}

// Rollout management

// RecordRolloutApproval records one distinct approver's approval of a rollout
// (ADR 0029). Idempotent: a second call with the same (rolloutID, approver)
// leaves the inner map size unchanged, so the distinct-approver count does not
// double.
func (s *Store) RecordRolloutApproval(ctx context.Context, rolloutID, approver, notes string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inner, ok := s.rolloutApprovals[rolloutID]
	if !ok {
		inner = make(map[string]types.RolloutApproval)
		s.rolloutApprovals[rolloutID] = inner
	}
	if _, exists := inner[approver]; exists {
		// Idempotent — keep the first recorded approval.
		return nil
	}
	inner[approver] = types.RolloutApproval{Approver: approver, Notes: notes, ApprovedAt: at}
	return nil
}

// CountRolloutApprovers returns the number of DISTINCT approvers recorded for a
// rollout (ADR 0029) — the size of the inner map.
func (s *Store) CountRolloutApprovers(ctx context.Context, rolloutID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.rolloutApprovals[rolloutID]), nil
}

// ListRolloutApprovers returns the recorded approvers for a rollout, oldest
// approval first, for audit / UI (ADR 0029).
func (s *Store) ListRolloutApprovers(ctx context.Context, rolloutID string) ([]types.RolloutApproval, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	inner := s.rolloutApprovals[rolloutID]
	if len(inner) == 0 {
		return nil, nil
	}
	out := make([]types.RolloutApproval, 0, len(inner))
	for _, a := range inner {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ApprovedAt.Before(out[j].ApprovedAt)
	})
	return out, nil
}

func (s *Store) CreateRollout(ctx context.Context, r *types.Rollout) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.rollouts[r.ID]; exists {
		return fmt.Errorf("rollout already exists: %s", r.ID)
	}
	rolloutCopy := copyRollout(r)
	// v0.53 — default proposed_by at the storage boundary so callers
	// that don't set it (legacy or test code) carry operator
	// semantics.
	if rolloutCopy.ProposedBy == "" {
		rolloutCopy.ProposedBy = types.RolloutProposedByOperator
	}
	// v0.89.14 — default step_kind to "rollout" so the in-memory
	// store matches what the SQLite scan does for pre-v0.89.14 rows.
	// Callers (engine, service) read StepKind authoritatively.
	if rolloutCopy.StepKind == "" {
		rolloutCopy.StepKind = types.StepKindRollout
	}
	// Optimistic-concurrency baseline, mirroring the SQLite column default.
	if rolloutCopy.Version == 0 {
		rolloutCopy.Version = 1
	}
	s.rollouts[r.ID] = &rolloutCopy
	return nil
}

func (s *Store) GetRollout(ctx context.Context, id string) (*types.Rollout, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rollouts[id]
	if !ok {
		return nil, nil
	}
	out := copyRollout(r)
	return &out, nil
}

// copyRollout makes a deep enough copy of a rollout that callers
// can mutate the returned value without affecting stored state.
// Centralized so every read/write path agrees on which fields need
// deep copies (Stages, EvidenceRefs) and which are safe to alias
// (time pointers — the engine reads these as values).
func copyRollout(r *types.Rollout) types.Rollout {
	out := *r
	if r.Stages != nil {
		out.Stages = make([]types.RolloutStage, len(r.Stages))
		copy(out.Stages, r.Stages)
	}
	if r.EvidenceRefs != nil {
		out.EvidenceRefs = make([]types.RolloutEvidenceRef, len(r.EvidenceRefs))
		copy(out.EvidenceRefs, r.EvidenceRefs)
	}
	if r.PushedAgentIDs != nil {
		out.PushedAgentIDs = make([]string, len(r.PushedAgentIDs))
		copy(out.PushedAgentIDs, r.PushedAgentIDs)
	}
	return out
}

func (s *Store) ListRollouts(ctx context.Context, filter types.RolloutFilter) ([]*types.Rollout, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	// Collect matching rollouts, sort newest-first by CreatedAt.
	var matches []*types.Rollout
	for _, r := range s.rollouts {
		if filter.GroupID != "" && r.GroupID != filter.GroupID {
			continue
		}
		if filter.State != "" && r.State != filter.State {
			continue
		}
		// v0.74 — narrow to one plan id.
		if filter.PlanID != "" && r.PlanID != filter.PlanID {
			continue
		}
		out := copyRollout(r)
		matches = append(matches, &out)
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].CreatedAt.After(matches[j].CreatedAt)
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

func (s *Store) UpdateRollout(ctx context.Context, r *types.Rollout) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.rollouts[r.ID]
	if !ok {
		return fmt.Errorf("rollout not found: %s", r.ID)
	}
	rolloutCopy := copyRollout(r)
	// v0.53 — preserve proposed_by semantics on update too.
	if rolloutCopy.ProposedBy == "" {
		rolloutCopy.ProposedBy = types.RolloutProposedByOperator
	}
	// v0.89.14 — preserve the step_kind sentinel on update too so a
	// Get after an Update that left StepKind empty still returns
	// "rollout".
	if rolloutCopy.StepKind == "" {
		rolloutCopy.StepKind = types.StepKindRollout
	}
	// Optimistic-concurrency guard, mirroring the SQLite store. A caller
	// carrying a loaded Version (>0) only commits if it still matches the
	// stored row, else ErrRolloutVersionConflict — catching the engine-vs-
	// operator lost-update race. Version==0 callers take the blind
	// last-write-wins path (still advancing the counter), so behavior is
	// unchanged until callers thread a loaded Version through.
	if r.Version > 0 {
		if stored.Version != r.Version {
			return types.ErrRolloutVersionConflict
		}
		rolloutCopy.Version = r.Version + 1
		s.rollouts[r.ID] = &rolloutCopy
		r.Version = rolloutCopy.Version
		return nil
	}
	rolloutCopy.Version = stored.Version + 1
	s.rollouts[r.ID] = &rolloutCopy
	return nil
}

// ListAIVerdictsForGroup mirrors the SQLite store's same-named method:
// AI-originated rollouts on the supplied group that have a terminal
// verdict (approved_at or rejected_at) recorded after the `since`
// cutoff, newest verdict first. Used by the proposer bridge to
// assemble the prior-verdicts few-shot block on cost-spike proposals.
// v0.89.17 (#633). See docs/proposals/531-proposer-learns-from-
// accepted-rejected.md §4.
func (s *Store) ListAIVerdictsForGroup(ctx context.Context, groupID string, since time.Time, limit int) ([]*types.Rollout, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	// verdictAt returns the COALESCE(approved_at, rejected_at) for a
	// rollout, or the zero time if neither is set. Mirrors the SQL
	// COALESCE used by the SQLite store so memory and SQLite return
	// the same shape.
	verdictAt := func(r *types.Rollout) time.Time {
		if r.ApprovedAt != nil {
			return *r.ApprovedAt
		}
		if r.RejectedAt != nil {
			return *r.RejectedAt
		}
		return time.Time{}
	}
	var matches []*types.Rollout
	for _, r := range s.rollouts {
		if r.GroupID != groupID {
			continue
		}
		if r.ProposedBy != types.RolloutProposedByAI {
			continue
		}
		if r.ApprovedAt == nil && r.RejectedAt == nil {
			continue
		}
		// v0.89.26 (#642) — per-rollout opt-out filter, mirroring
		// the SQLite store's `AND exclude_from_learning = 0`
		// predicate. Skips rows the operator suppressed via the
		// exclude-from-learning endpoint so they drop out of the
		// few-shot block without disabling the whole group.
		if r.ExcludeFromLearning {
			continue
		}
		va := verdictAt(r)
		if va.Before(since) {
			continue
		}
		out := copyRollout(r)
		matches = append(matches, &out)
	}
	sort.Slice(matches, func(i, j int) bool {
		return verdictAt(matches[i]).After(verdictAt(matches[j]))
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

// ListDiscoveryVerdicts — v0.89.36 (#655 Stream 53, #531 slice 2
// chunk 3). Memory-store mirror of the SQLite same-named method.
// Renamed from ListAcceptedDiscoveryRecommendations (v0.89.28) and
// widened to UNION both recommendation.pr_merged AND
// recommendation.pr_closed_not_merged rows under a State
// discriminator. Linear scan over s.auditEvents with the same
// predicate logic the SQL query applies: event_type in the two-event
// set AND timestamp>=since AND the payload's (connection_id,
// scope_id, region) tuple matches.
//
// v0.89.48 (#671 Stream 69, GCP discovery slice 1 chunk 5) — the
// scopeID parameter is provider-agnostic and matches the payload's
// account_id OR project_id field. AWS-shaped audit rows carry the
// scope under account_id (with project_id empty or absent); GCP-shaped
// audit rows carry it under project_id (with account_id empty or
// absent). The OR predicate keeps both round-trips clean without
// requiring a Provider parameter on the lookup.
//
// v0.89.53 (#678 Stream 76, Azure discovery slice 1 chunk 5) — the OR
// predicate extends to subscription_id (Azure shape). Azure-shaped
// audit rows carry the scope under subscription_id (with account_id
// and project_id empty or absent). All three provider shapes
// round-trip through one call.
func (s *Store) ListDiscoveryVerdicts(
	ctx context.Context,
	connectionID, scopeID, region string,
	since time.Time, limit int,
) ([]*types.DiscoveryVerdict, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if connectionID == "" || scopeID == "" || region == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Walk in reverse so the natural order is newest-first (the
	// append-only store grows oldest-first).
	var out []*types.DiscoveryVerdict
	for i := len(s.auditEvents) - 1; i >= 0; i-- {
		e := s.auditEvents[i]
		var state, actorKey, tsKey string
		switch e.EventType {
		case "recommendation.pr_merged":
			state = "merged"
			actorKey, tsKey = "merged_by", "merged_at"
		case "recommendation.pr_closed_not_merged":
			state = "closed_not_merged"
			actorKey, tsKey = "closed_by", "closed_at"
		default:
			continue
		}
		if e.Timestamp.Before(since) {
			continue
		}
		if e.Payload == nil {
			continue
		}
		// Predicate: connection_id + scope_id + region all match. The
		// scope_id is matched against account_id OR project_id OR
		// subscription_id OR tenancy_ocid so AWS, GCP, Azure, and OCI
		// audit shapes all round-trip through one call. See v0.89.48
		// (#671 Stream 69) — GCP discovery slice 1 chunk 5 — and
		// v0.89.53 (#678 Stream 76) — Azure discovery slice 1 chunk 5
		// — and v0.89.58 (#685 Stream 83) — OCI discovery slice 1
		// chunk 5 — for the broader substrate.
		if v, _ := e.Payload["connection_id"].(string); v != connectionID {
			continue
		}
		acct, _ := e.Payload["account_id"].(string)
		proj, _ := e.Payload["project_id"].(string)
		sub, _ := e.Payload["subscription_id"].(string)
		tenancy, _ := e.Payload["tenancy_ocid"].(string)
		if acct != scopeID && proj != scopeID && sub != scopeID && tenancy != scopeID {
			continue
		}
		if v, _ := e.Payload["region"].(string); v != region {
			continue
		}
		kind, _ := e.Payload["recommendation_kind"].(string)
		if kind == "" {
			// §10 Q2 — skip rows whose branch didn't carry a kind.
			continue
		}
		rec := &types.DiscoveryVerdict{
			State:              state,
			PRMergedAt:         e.Timestamp,
			RecommendationKind: kind,
		}
		if v, ok := e.Payload["pr_url"].(string); ok {
			rec.PRURL = v
		}
		if v, ok := e.Payload["branch"].(string); ok {
			rec.Branch = v
		}
		if v, ok := e.Payload[actorKey].(string); ok {
			rec.MergedBy = v
		}
		// Prefer the payload's explicit timestamp string when
		// present and parsable — keeps the projection aligned with
		// the GitHub-side truth rather than the audit row's
		// timestamp column. The seed helpers in tests round-trip
		// matching values; this branch is a no-op for cold-start
		// fixtures.
		if v, ok := e.Payload[tsKey].(string); ok && v != "" {
			if parsed, perr := time.Parse(time.RFC3339, v); perr == nil {
				rec.PRMergedAt = parsed
			}
		}
		out = append(out, rec)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// SetRecommendationExclusion — v0.89.37 (#656 Stream 54, #531 slice 2
// chunk 4). Memory-store mirror of the SQLite same-named method.
// Performs the same prevExcluded-read + upsert under the store-wide
// mutex so concurrent toggles on the same recommendation_id can't both
// observe prevExcluded=false.
//
// Transition rules match the SQLite impl exactly: ExcludedAt +
// ExcludedBy are stamped on a transition to excluded=true (defaulting
// ExcludedAt to now if the caller's value is zero), cleared on the
// transition to excluded=false, and preserved on a no-op toggle.
func (s *Store) SetRecommendationExclusion(
	_ context.Context,
	rec types.ExcludedRecommendation,
	excluded bool,
) (bool, error) {
	if rec.RecommendationID == "" {
		return false, fmt.Errorf("recommendation_id required")
	}
	if rec.ConnectionID == "" || rec.AccountID == "" || rec.Region == "" {
		return false, fmt.Errorf("scope tuple (connection_id, account_id, region) required")
	}
	if rec.RecommendationKind == "" {
		return false, fmt.Errorf("recommendation_kind required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, hadRow := s.excludedRecommendations[rec.RecommendationID]
	prevExcluded := s.excludedRecommendationFlags[rec.RecommendationID]

	now := time.Now().UTC()
	out := types.ExcludedRecommendation{
		RecommendationID:   rec.RecommendationID,
		ConnectionID:       rec.ConnectionID,
		AccountID:          rec.AccountID,
		Region:             rec.Region,
		RecommendationKind: rec.RecommendationKind,
		ResourceID:         rec.ResourceID,
	}
	switch {
	case excluded && !prevExcluded:
		// Transition to excluded=true. Stamp.
		if !rec.ExcludedAt.IsZero() {
			out.ExcludedAt = rec.ExcludedAt.UTC()
		} else {
			out.ExcludedAt = now
		}
		out.ExcludedBy = rec.ExcludedBy
	case !excluded && prevExcluded:
		// Transition to excluded=false. Clear stamps.
		// Leave ExcludedAt + ExcludedBy at zero / "".
	case hadRow:
		// No-op toggle on an existing row: preserve stamps.
		out.ExcludedAt = existing.ExcludedAt
		out.ExcludedBy = existing.ExcludedBy
	case !excluded && !hadRow:
		// Fresh row with excluded=false. No stamp.
	}
	s.excludedRecommendations[rec.RecommendationID] = out
	s.excludedRecommendationFlags[rec.RecommendationID] = excluded
	return prevExcluded, nil
}

// ListExcludedRecommendations — v0.89.37 (#656 Stream 54, #531 slice
// 2 chunk 4). Memory-store mirror of the SQLite same-named method.
// Linear scan over s.excludedRecommendations filtered by the scope
// tuple AND exclude_from_learning=1, sorted ExcludedAt DESC, capped
// at limit.
func (s *Store) ListExcludedRecommendations(
	_ context.Context,
	connectionID, accountID, region string,
	limit int,
) ([]types.ExcludedRecommendation, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if connectionID == "" || accountID == "" || region == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []types.ExcludedRecommendation
	for id, rec := range s.excludedRecommendations {
		if !s.excludedRecommendationFlags[id] {
			continue
		}
		if rec.ConnectionID != connectionID || rec.AccountID != accountID || rec.Region != region {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ExcludedAt.After(out[j].ExcludedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SetCheckRunForRecommendation — v0.89.42 (#662 Stream 60, slice 1
// chunk 1). Memory-store mirror of the SQLite same-named method.
// Upserts the durable check-run state on the row keyed by
// recommendation_id; creates the underlying excludedRecommendations
// row if it doesn't exist yet (with exclude_from_learning=0 and
// excluded_at / excluded_by zero-valued, mirroring the SQLite
// NULL-on-insert semantics).
//
// Per §11 Q3 of the design doc the row's existence here means
// "Squadron has a check run on this PR," not "operator has acted" —
// the chunk-4 listing path filters on excludedRecommendationFlags
// so a row created by this method does NOT surface in
// ListExcludedRecommendations until the operator clicks exclude.
func (s *Store) SetCheckRunForRecommendation(
	_ context.Context,
	rec types.ExcludedRecommendation,
	ref types.CheckRunRef,
	status, conclusion string,
) error {
	if rec.RecommendationID == "" {
		return fmt.Errorf("recommendation_id required")
	}
	if rec.ConnectionID == "" || rec.AccountID == "" || rec.Region == "" {
		return fmt.Errorf("scope tuple (connection_id, account_id, region) required")
	}
	if rec.RecommendationKind == "" {
		return fmt.Errorf("recommendation_kind required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// If no excludedRecommendations row exists, create one with the
	// projection's scope tuple and exclude_from_learning=false. The
	// chunk-4 SetRecommendationExclusion path's read-then-upsert is
	// unaffected — both paths converge on the same map entry keyed
	// by recommendation_id.
	if _, hadRow := s.excludedRecommendations[rec.RecommendationID]; !hadRow {
		s.excludedRecommendations[rec.RecommendationID] = types.ExcludedRecommendation{
			RecommendationID:   rec.RecommendationID,
			ConnectionID:       rec.ConnectionID,
			AccountID:          rec.AccountID,
			Region:             rec.Region,
			RecommendationKind: rec.RecommendationKind,
			ResourceID:         rec.ResourceID,
		}
		s.excludedRecommendationFlags[rec.RecommendationID] = false
	}

	s.checkRunState[rec.RecommendationID] = memoryCheckRunRecord{
		ref:        ref,
		status:     status,
		conclusion: conclusion,
		updatedAt:  time.Now().UTC(),
	}
	return nil
}

// GetCheckRunForRecommendation — v0.89.42 (#662 Stream 60, slice 1
// chunk 1). Memory-store mirror of the SQLite same-named method.
// Returns exists=false (no error) when no check-run state has been
// written for recommendationID.
//
// Note: matching SQLite semantics, exists=true means the underlying
// iac_recommendation_verdicts row carries check_run_* state. A row
// that exists only because the chunk-4 exclusion path wrote it
// (without ever calling SetCheckRunForRecommendation) returns
// exists=false here — chunks 2/3 use exists to decide whether to
// patch a check run on inbound webhook events.
func (s *Store) GetCheckRunForRecommendation(
	_ context.Context,
	recommendationID string,
) (types.CheckRunRef, string, string, bool, error) {
	if recommendationID == "" {
		return types.CheckRunRef{}, "", "", false, fmt.Errorf("recommendation_id required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.checkRunState[recommendationID]
	if !ok {
		return types.CheckRunRef{}, "", "", false, nil
	}
	return rec.ref, rec.status, rec.conclusion, true, nil
}

// API token management
//
// The map is keyed by token ID. For hash-based lookup the in-memory
// store scans linearly — fine for tests and small instances; the SQLite
// store has a proper index.

func (s *Store) CreateAPIToken(ctx context.Context, t *types.APIToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.apiTokens[t.ID]; exists {
		return fmt.Errorf("api token id already exists: %s", t.ID)
	}
	for _, existing := range s.apiTokens {
		if existing.Hash == t.Hash {
			return fmt.Errorf("api token hash collision")
		}
	}
	stored := copyToken(t)
	// ADR 0011: mirror the sqlite store's insert default — an unset tenant
	// lands in the OSS single tenant 'default'.
	if stored.TenantID == "" {
		stored.TenantID = "default"
	}
	s.apiTokens[t.ID] = stored
	return nil
}

func (s *Store) GetAPITokenByHash(ctx context.Context, hash string) (*types.APIToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.apiTokens {
		if t.Hash == hash {
			return copyToken(t), nil
		}
	}
	return nil, nil
}

func (s *Store) ListAPITokens(ctx context.Context) ([]*types.APIToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.APIToken, 0, len(s.apiTokens))
	for _, t := range s.apiTokens {
		out = append(out, copyToken(t))
	}
	// Newest-first to match the SQLite impl.
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// copyToken returns a defensive copy with a fresh Scopes slice so
// callers can't mutate stored state through the slice header. The
// *time.Time fields are aliased — time.Time itself is a value type
// so callers reading them can't corrupt the stored row, but they
// could swap the pointer out via the returned token. We rebind those
// pointers to fresh allocations for the same reason as Scopes.
func copyToken(t *types.APIToken) *types.APIToken {
	cp := *t
	if len(t.Scopes) > 0 {
		cp.Scopes = make([]string, len(t.Scopes))
		copy(cp.Scopes, t.Scopes)
	}
	if t.LastUsedAt != nil {
		v := *t.LastUsedAt
		cp.LastUsedAt = &v
	}
	if t.RevokedAt != nil {
		v := *t.RevokedAt
		cp.RevokedAt = &v
	}
	if t.ExpiresAt != nil {
		v := *t.ExpiresAt
		cp.ExpiresAt = &v
	}
	return &cp
}

func (s *Store) UpdateAPITokenLastUsed(ctx context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.apiTokens[id]
	if !ok {
		return fmt.Errorf("api token not found: %s", id)
	}
	atCopy := at
	t.LastUsedAt = &atCopy
	return nil
}

func (s *Store) RevokeAPIToken(ctx context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.apiTokens[id]
	if !ok {
		// Idempotent — matches the SQLite impl.
		return nil
	}
	if t.RevokedAt != nil {
		return nil
	}
	atCopy := at
	t.RevokedAt = &atCopy
	return nil
}

// RevokeAPITokensByConnection soft-revokes every non-revoked token whose
// ConnectionID matches, returning the revoked tokens (ID + Label populated) for
// per-token audit. Mirrors the sqlite impl; tenant-scope-unaware in the memory
// store (used in tests / OSS single-tenant). See the sqlite doc for the
// enterprise connection-delete revocation semantics.
func (s *Store) RevokeAPITokensByConnection(ctx context.Context, connectionID string, at time.Time) ([]types.APIToken, error) {
	if connectionID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var revoked []types.APIToken
	for _, t := range s.apiTokens {
		if t.ConnectionID != connectionID || t.RevokedAt != nil {
			continue
		}
		atCopy := at
		t.RevokedAt = &atCopy
		revoked = append(revoked, types.APIToken{ID: t.ID, Label: t.Label})
	}
	return revoked, nil
}

// purge removes all data from the store (for testing)
func (s *Store) purge(context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.agents = make(map[uuid.UUID]*types.Agent)
	s.groups = make(map[string]*types.Group)
	s.configs = make(map[string]*types.Config)
	s.savedQueries = make(map[string]*types.SavedQuery)
	s.alertRules = make(map[string]*types.AlertRule)
	s.auditEvents = make([]*types.AuditEvent, 0, 64)
	s.rollouts = make(map[string]*types.Rollout)
	s.apiTokens = make(map[string]*types.APIToken)
	s.recDismissals = make(map[string]*types.RecommendationDismissal)
	s.recOutcomes = make(map[string]*types.RecommendationOutcome)
}

// ----------------------------------------------------------------
// Recommendation dismissals (v0.25)
// ----------------------------------------------------------------

func (s *Store) DismissRecommendation(_ context.Context, d *types.RecommendationDismissal) error {
	if d == nil || d.RecommendationID == "" {
		return fmt.Errorf("recommendation_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *d
	if cp.DismissedAt.IsZero() {
		cp.DismissedAt = time.Now().UTC()
	}
	if cp.DismissedBy == "" {
		cp.DismissedBy = "system"
	}
	s.recDismissals[cp.RecommendationID] = &cp
	return nil
}

func (s *Store) RestoreRecommendation(_ context.Context, recommendationID string) error {
	if recommendationID == "" {
		return fmt.Errorf("recommendation_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recDismissals, recommendationID)
	return nil
}

func (s *Store) IsRecommendationDismissed(_ context.Context, recommendationID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.recDismissals[recommendationID]
	return ok, nil
}

// ----------------------------------------------------------------
// Recommendation outcomes (v0.28)
// ----------------------------------------------------------------

func (s *Store) CreateRecommendationOutcome(_ context.Context, o *types.RecommendationOutcome) error {
	if o == nil || o.ID == "" || o.RecommendationID == "" {
		return fmt.Errorf("id + recommendation_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *o
	if cp.AppliedAt.IsZero() {
		cp.AppliedAt = time.Now().UTC()
	}
	if cp.Status == "" {
		cp.Status = "pending"
	}
	if cp.AppliedBy == "" {
		cp.AppliedBy = "system"
	}
	s.recOutcomes[cp.ID] = &cp
	return nil
}

func (s *Store) UpdateRecommendationOutcome(_ context.Context, o *types.RecommendationOutcome) error {
	if o == nil || o.ID == "" {
		return fmt.Errorf("id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.recOutcomes[o.ID]
	if !ok {
		return fmt.Errorf("outcome not found: %s", o.ID)
	}
	// Mutate only the observation fields; the frozen snapshot fields stay.
	existing.LastObservedBytesPerHour = o.LastObservedBytesPerHour
	existing.LastObservedAt = o.LastObservedAt
	existing.RealizedSavingsPerMonthUSD = o.RealizedSavingsPerMonthUSD
	existing.Status = o.Status
	return nil
}

func (s *Store) ListRecommendationOutcomes(_ context.Context) ([]*types.RecommendationOutcome, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.RecommendationOutcome, 0, len(s.recOutcomes))
	for _, o := range s.recOutcomes {
		cp := *o
		out = append(out, &cp)
	}
	// Newest first to mirror SQLite.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].AppliedAt.After(out[i].AppliedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (s *Store) ListRecommendationDismissals(_ context.Context) ([]*types.RecommendationDismissal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.RecommendationDismissal, 0, len(s.recDismissals))
	for _, d := range s.recDismissals {
		cp := *d
		out = append(out, &cp)
	}
	// Stable order: newest first to match SQLite.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].DismissedAt.After(out[i].DismissedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// ===================================================================
// v0.29 cost-spike events
// ===================================================================

func (s *Store) CreateCostSpikeEvent(_ context.Context, e *types.CostSpikeEvent) error {
	if e == nil || e.ID == "" {
		return fmt.Errorf("id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *e
	if cp.StartedAt.IsZero() {
		cp.StartedAt = time.Now().UTC()
	}
	if cp.Severity == "" {
		cp.Severity = "warn"
	}
	s.costSpikes[cp.ID] = &cp
	return nil
}

func (s *Store) UpdateCostSpikeEvent(_ context.Context, e *types.CostSpikeEvent) error {
	if e == nil || e.ID == "" {
		return fmt.Errorf("id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.costSpikes[e.ID]
	if !ok {
		return fmt.Errorf("cost spike not found")
	}
	updated := *e
	updated.StartedAt = existing.StartedAt // preserve creation time
	s.costSpikes[e.ID] = &updated
	return nil
}

func (s *Store) GetCostSpikeEvent(_ context.Context, id string) (*types.CostSpikeEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.costSpikes[id]
	if !ok {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

func (s *Store) ListCostSpikeEvents(_ context.Context, filter types.CostSpikeFilter) ([]*types.CostSpikeEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.CostSpikeEvent, 0, len(s.costSpikes))
	for _, e := range s.costSpikes {
		switch filter.Status {
		case "open":
			if e.EndedAt != nil {
				continue
			}
		case "closed":
			if e.EndedAt == nil {
				continue
			}
		}
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s *Store) LatestOpenCostSpike(_ context.Context) (*types.CostSpikeEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest *types.CostSpikeEvent
	for _, e := range s.costSpikes {
		if e.EndedAt != nil {
			continue
		}
		if latest == nil || e.StartedAt.After(latest.StartedAt) {
			latest = e
		}
	}
	if latest == nil {
		return nil, nil
	}
	cp := *latest
	return &cp, nil
}

// ===================================================================
// v0.32 expected agents (inventory reconciliation)
// ===================================================================

func (s *Store) UpsertExpectedAgent(_ context.Context, e *types.ExpectedAgent) error {
	if e == nil || e.Hostname == "" {
		return fmt.Errorf("hostname required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if e.ExpectedSince.IsZero() {
		e.ExpectedSince = now
	}
	e.UpdatedAt = now
	cp := *e
	s.expectedAgents[e.Hostname] = &cp
	return nil
}

func (s *Store) DeleteExpectedAgent(_ context.Context, hostname string) error {
	if hostname == "" {
		return fmt.Errorf("hostname required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.expectedAgents, hostname)
	return nil
}

func (s *Store) ListExpectedAgents(_ context.Context, source string) ([]*types.ExpectedAgent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.ExpectedAgent, 0, len(s.expectedAgents))
	for _, e := range s.expectedAgents {
		if source != "" && e.Source != source {
			continue
		}
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}

// ===================================================================
// v0.34 deploy targets + runs (GitHub Actions integration)
// ===================================================================

func (s *Store) CreateDeployTarget(_ context.Context, t *types.DeployTarget) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.Provider == "" {
		t.Provider = "github"
	}
	if t.GitHubBranch == "" {
		t.GitHubBranch = "main"
	}
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	cp := *t
	s.deployTargets[t.ID] = &cp
	return nil
}

func (s *Store) UpdateDeployTarget(_ context.Context, t *types.DeployTarget) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.deployTargets[t.ID]
	if !ok {
		return fmt.Errorf("deploy target not found")
	}
	t.UpdatedAt = time.Now().UTC()
	cp := *t
	// Preserve the existing credential when the update doesn't carry
	// a new one — mirrors the sqlite "leave the secret alone" path.
	if len(cp.EncryptedCredential) == 0 {
		cp.EncryptedCredential = existing.EncryptedCredential
	}
	s.deployTargets[t.ID] = &cp
	return nil
}

func (s *Store) GetDeployTarget(_ context.Context, id string) (*types.DeployTarget, error) {
	if id == "" {
		return nil, fmt.Errorf("id required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.deployTargets[id]
	if !ok {
		return nil, nil
	}
	cp := *t
	cp.HasCredential = len(t.EncryptedCredential) > 0
	return &cp, nil
}

func (s *Store) ListDeployTargets(_ context.Context) ([]*types.DeployTarget, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.DeployTarget, 0, len(s.deployTargets))
	for _, t := range s.deployTargets {
		cp := *t
		cp.HasCredential = len(t.EncryptedCredential) > 0
		cp.EncryptedCredential = nil // mirror sqlite behavior: list never carries the secret
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) DeleteDeployTarget(_ context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deployTargets, id)
	return nil
}

func (s *Store) CreateDeployRun(_ context.Context, r *types.DeployRun) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Status == "" {
		r.Status = "queued"
	}
	if r.RequestedAt.IsZero() {
		r.RequestedAt = time.Now().UTC()
	}
	cp := *r
	s.deployRuns[r.ID] = &cp
	return nil
}

func (s *Store) UpdateDeployRun(_ context.Context, r *types.DeployRun) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.deployRuns[r.ID]; !ok {
		return fmt.Errorf("deploy run not found")
	}
	cp := *r
	s.deployRuns[r.ID] = &cp
	return nil
}

func (s *Store) GetDeployRun(_ context.Context, id string) (*types.DeployRun, error) {
	if id == "" {
		return nil, fmt.Errorf("id required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.deployRuns[id]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (s *Store) ListDeployRuns(_ context.Context, filter types.DeployRunFilter) ([]*types.DeployRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.DeployRun, 0, len(s.deployRuns))
	for _, r := range s.deployRuns {
		if filter.TargetID != "" && r.TargetID != filter.TargetID {
			continue
		}
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt.After(out[j].RequestedAt) })
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s *Store) ReplaceExpectedAgentsForSource(_ context.Context, source string, entries []*types.ExpectedAgent) error {
	if source == "" {
		return fmt.Errorf("source required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Remove every entry tagged with this source.
	for hostname, e := range s.expectedAgents {
		if e.Source == source {
			delete(s.expectedAgents, hostname)
		}
	}
	now := time.Now().UTC()
	for _, e := range entries {
		if e == nil || e.Hostname == "" {
			continue
		}
		cp := *e
		cp.Source = source
		if cp.ExpectedSince.IsZero() {
			cp.ExpectedSince = now
		}
		cp.UpdatedAt = now
		s.expectedAgents[cp.Hostname] = &cp
	}
	return nil
}

// --- SIEM destinations (v0.50) ----------------------------------------

func (s *Store) CreateSiemDestination(ctx context.Context, d *types.SiemDestination) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.siemDestinations[d.ID]; exists {
		return fmt.Errorf("siem destination already exists: %s", d.ID)
	}
	cp := *d
	s.siemDestinations[d.ID] = &cp
	return nil
}

func (s *Store) GetSiemDestination(ctx context.Context, id string) (*types.SiemDestination, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.siemDestinations[id]
	if !ok {
		return nil, nil
	}
	cp := *d
	return &cp, nil
}

func (s *Store) ListSiemDestinations(ctx context.Context) ([]*types.SiemDestination, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.SiemDestination, 0, len(s.siemDestinations))
	for _, d := range s.siemDestinations {
		cp := *d
		out = append(out, &cp)
	}
	return out, nil
}

func (s *Store) UpdateSiemDestination(ctx context.Context, d *types.SiemDestination) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.siemDestinations[d.ID]
	if !ok {
		return fmt.Errorf("siem destination not found: %s", d.ID)
	}
	// Preserve dispatcher-owned status fields — UpdateSiemDestination
	// is the operator path and shouldn't clobber telemetry.
	d.LastEventSentAt = existing.LastEventSentAt
	d.LastError = existing.LastError
	d.LastErrorAt = existing.LastErrorAt
	d.CreatedAt = existing.CreatedAt
	cp := *d
	s.siemDestinations[d.ID] = &cp
	return nil
}

func (s *Store) DeleteSiemDestination(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.siemDestinations[id]; !ok {
		return fmt.Errorf("siem destination not found: %s", id)
	}
	delete(s.siemDestinations, id)
	return nil
}

func (s *Store) UpdateSiemDestinationStatus(ctx context.Context, id string, sentAt *time.Time, errMsg string, errAt *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.siemDestinations[id]
	if !ok {
		return fmt.Errorf("siem destination not found: %s", id)
	}
	existing.LastEventSentAt = sentAt
	existing.LastError = errMsg
	existing.LastErrorAt = errAt
	return nil
}

// inferDiscoveryProviderScope derives the origin cloud + scope id from a
// recommendation verdict audit payload by which scope key is populated.
// Cross-cloud citations (v0.89.247) use this so a verdict surfaced on a
// different cloud can be labeled with where it actually came from.
func inferDiscoveryProviderScope(payload map[string]any) (provider, scopeID string) {
	if v, _ := payload["account_id"].(string); v != "" {
		return "aws", v
	}
	if v, _ := payload["project_id"].(string); v != "" {
		return "gcp", v
	}
	if v, _ := payload["subscription_id"].(string); v != "" {
		return "azure", v
	}
	if v, _ := payload["tenancy_ocid"].(string); v != "" {
		return "oci", v
	}
	return "", ""
}

// ListCrossScopeDiscoveryVerdicts returns recent recommendation verdicts
// (pr_merged / pr_closed_not_merged) recorded against connections OTHER than
// excludeConnectionID, each tagged with its origin Provider + ScopeID. This
// is the substrate behind cross-cloud citations (v0.89.247): a decline
// recorded on one cloud can surface, origin-labeled, in another cloud's
// verdict block, where the proposer correlates by recommendation_kind. The
// current connection's rows are excluded because the caller already includes
// them via the same-scope ListDiscoveryVerdicts path.
func (s *Store) ListCrossScopeDiscoveryVerdicts(
	ctx context.Context,
	excludeScopeID string,
	since time.Time, limit int,
) ([]*types.DiscoveryVerdict, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*types.DiscoveryVerdict
	for i := len(s.auditEvents) - 1; i >= 0; i-- {
		e := s.auditEvents[i]
		var state, actorKey, tsKey string
		switch e.EventType {
		case "recommendation.pr_merged":
			state = "merged"
			actorKey, tsKey = "merged_by", "merged_at"
		case "recommendation.pr_closed_not_merged":
			state = "closed_not_merged"
			actorKey, tsKey = "closed_by", "closed_at"
		default:
			continue
		}
		if e.Timestamp.Before(since) {
			continue
		}
		if e.Payload == nil {
			continue
		}
		provider, scopeID := inferDiscoveryProviderScope(e.Payload)
		if scopeID == "" || scopeID == excludeScopeID {
			continue
		}
		kind, _ := e.Payload["recommendation_kind"].(string)
		if kind == "" {
			continue
		}
		rec := &types.DiscoveryVerdict{
			State:              state,
			PRMergedAt:         e.Timestamp,
			RecommendationKind: kind,
			Provider:           provider,
			ScopeID:            scopeID,
		}
		if v, ok := e.Payload["pr_url"].(string); ok {
			rec.PRURL = v
		}
		if v, ok := e.Payload["branch"].(string); ok {
			rec.Branch = v
		}
		if v, ok := e.Payload[actorKey].(string); ok {
			rec.MergedBy = v
		}
		if v, ok := e.Payload[tsKey].(string); ok && v != "" {
			if parsed, perr := time.Parse(time.RFC3339, v); perr == nil {
				rec.PRMergedAt = parsed
			}
		}
		out = append(out, rec)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// --- Discovery scan persistence (v0.89.250, continuous-discovery slice 1) ---

// SaveDiscoveryScan records a completed scan. Upserts on ScanID.
func (s *Store) SaveDiscoveryScan(ctx context.Context, rec *types.ScanRecord) error {
	if rec == nil || rec.ScanID == "" {
		return fmt.Errorf("memory: SaveDiscoveryScan requires a non-empty ScanID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *rec
	if rec.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	for i, existing := range s.discoveryScans {
		if existing.ScanID == rec.ScanID {
			s.discoveryScans[i] = &cp
			return nil
		}
	}
	s.discoveryScans = append(s.discoveryScans, &cp)
	return nil
}

// ListDiscoveryScans returns the newest-first scan history for a scope.
// ResultJSON is omitted to keep list responses small. A blank scopeID lists
// every scan for the provider.
func (s *Store) ListDiscoveryScans(ctx context.Context, provider, scopeID string, limit int) ([]*types.ScanRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*types.ScanRecord
	for _, rec := range s.discoveryScans {
		if rec.Provider != provider {
			continue
		}
		if scopeID != "" && rec.ScopeID != scopeID {
			continue
		}
		cp := *rec
		cp.ResultJSON = ""
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetDiscoveryScan returns one scan including the full inventory (ResultJSON).
// Returns (nil, nil) when no scan matches.
func (s *Store) GetDiscoveryScan(ctx context.Context, scanID string) (*types.ScanRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rec := range s.discoveryScans {
		if rec.ScanID == scanID {
			cp := *rec
			return &cp, nil
		}
	}
	return nil, nil
}
