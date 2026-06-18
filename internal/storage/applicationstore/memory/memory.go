// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/google/uuid"
)

// Store is an in-memory implementation of ApplicationStore
type Store struct {
	mu           sync.RWMutex
	agents       map[uuid.UUID]*types.Agent
	groups       map[string]*types.Group
	configs      map[string]*types.Config
	savedQueries map[string]*types.SavedQuery
	alertRules   map[string]*types.AlertRule
	auditEvents  []*types.AuditEvent // append-only; sorted newest-first on read
	rollouts     map[string]*types.Rollout
	apiTokens    map[string]*types.APIToken // keyed by ID; secondary index built on the fly for hash lookup
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
}

// NewStore creates a new in-memory store
func NewStore() *Store {
	return &Store{
		agents:        make(map[uuid.UUID]*types.Agent),
		groups:        make(map[string]*types.Group),
		configs:       make(map[string]*types.Config),
		savedQueries:  make(map[string]*types.SavedQuery),
		alertRules:    make(map[string]*types.AlertRule),
		auditEvents:   make([]*types.AuditEvent, 0, 64),
		rollouts:      make(map[string]*types.Rollout),
		apiTokens:     make(map[string]*types.APIToken),
		recDismissals: make(map[string]*types.RecommendationDismissal),
		recOutcomes:   make(map[string]*types.RecommendationOutcome),
		costSpikes:    make(map[string]*types.CostSpikeEvent),
		expectedAgents: make(map[string]*types.ExpectedAgent),
		deployTargets:    make(map[string]*types.DeployTarget),
		deployRuns:       make(map[string]*types.DeployRun),
		siemDestinations: make(map[string]*types.SiemDestination),
		actionRunners:    make(map[string]*types.ActionRunnerRegistration),
		actionRequests:   make(map[string]*types.ActionRequest),
	}
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
// round-trip ChangeWindowsJSON.
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
	existing.ChangeWindowsJSON = group.ChangeWindowsJSON
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
		if filter.TargetType != "" && e.TargetType != filter.TargetType {
			continue
		}
		if filter.TargetID != "" && e.TargetID != filter.TargetID {
			continue
		}
		if !filter.Since.IsZero() && e.Timestamp.Before(filter.Since) {
			continue
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

// Rollout management

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
	if _, ok := s.rollouts[r.ID]; !ok {
		return fmt.Errorf("rollout not found: %s", r.ID)
	}
	rolloutCopy := copyRollout(r)
	// v0.53 — preserve proposed_by semantics on update too.
	if rolloutCopy.ProposedBy == "" {
		rolloutCopy.ProposedBy = types.RolloutProposedByOperator
	}
	s.rollouts[r.ID] = &rolloutCopy
	return nil
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
	s.apiTokens[t.ID] = copyToken(t)
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
