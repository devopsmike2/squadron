package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pmezard/go-difflib/difflib"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/confignorm"
	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// AgentServiceImpl implements the AgentService interface
type AgentServiceImpl struct {
	appStore     applicationstore.ApplicationStore
	logger       *zap.Logger
	driftMetrics *metrics.DriftMetrics
	broker       *events.Broker // optional; nil means no events published
	audit        AuditService   // optional; nil means no audit log entries
}

// NewAgentService creates a new agent service.
//
// driftMetrics, broker, and audit are all optional — pass nil in tests to
// suppress side effects. Production callers wire all three.
func NewAgentService(appStore applicationstore.ApplicationStore, driftMetrics *metrics.DriftMetrics, broker *events.Broker, audit AuditService, logger *zap.Logger) AgentService {
	if driftMetrics == nil {
		driftMetrics = metrics.NewDriftMetrics(metrics.NullFactory)
	}
	return &AgentServiceImpl{
		appStore:     appStore,
		logger:       logger,
		driftMetrics: driftMetrics,
		broker:       broker,
		audit:        audit,
	}
}

// CreateAgent creates an agent
func (s *AgentServiceImpl) CreateAgent(ctx context.Context, agent *Agent) error {
	storageAgent := &applicationstore.Agent{
		ID:           agent.ID,
		Name:         agent.Name,
		Labels:       agent.Labels,
		Status:       applicationstore.AgentStatus(agent.Status),
		LastSeen:     agent.LastSeen,
		GroupID:      agent.GroupID,
		GroupName:    agent.GroupName,
		Version:      agent.Version,
		Capabilities: agent.Capabilities,
		CreatedAt:    agent.CreatedAt,
		UpdatedAt:    agent.UpdatedAt,
	}
	if err := s.appStore.CreateAgent(ctx, storageAgent); err != nil {
		return err
	}
	if s.broker != nil {
		s.broker.Publish(events.Event{
			Type: events.AgentRegistered,
			Data: map[string]any{
				"id":     agent.ID.String(),
				"name":   agent.Name,
				"status": string(agent.Status),
			},
		})
	}
	if s.audit != nil {
		_ = s.audit.Record(ctx, AuditEntry{
			Actor:      AuditActorOpAMP,
			EventType:  AuditEventAgentRegistered,
			TargetType: AuditTargetAgent,
			TargetID:   agent.ID.String(),
			Action:     "created",
			Payload: map[string]any{
				"name":    agent.Name,
				"version": agent.Version,
			},
		})
	}
	return nil
}

// GetAgent gets an agent by ID
func (s *AgentServiceImpl) GetAgent(ctx context.Context, id uuid.UUID) (*Agent, error) {
	agent, err := s.appStore.GetAgent(ctx, id)
	if err != nil {
		return nil, err
	}

	if agent == nil {
		return nil, nil
	}

	result := &Agent{
		ID:              agent.ID,
		Name:            agent.Name,
		Labels:          agent.Labels,
		Status:          AgentStatus(agent.Status),
		LastSeen:        agent.LastSeen,
		GroupID:         agent.GroupID,
		GroupName:       agent.GroupName,
		Version:         agent.Version,
		Capabilities:    agent.Capabilities,
		EffectiveConfig: agent.EffectiveConfig,
		DriftStatus:     ConfigDriftStatusUnknown,
		CreatedAt:       agent.CreatedAt,
		UpdatedAt:       agent.UpdatedAt,
	}

	if err := s.populateAgentConfigState(ctx, result, true); err != nil {
		s.logger.Warn("Failed to populate agent config state",
			zap.String("agent_id", result.ID.String()),
			zap.Error(err),
		)
	}

	return result, nil
}

// ListAgents lists all agents
func (s *AgentServiceImpl) ListAgents(ctx context.Context) ([]*Agent, error) {
	agents, err := s.appStore.ListAgents(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*Agent, len(agents))
	for i, agent := range agents {
		current := &Agent{
			ID:              agent.ID,
			Name:            agent.Name,
			Labels:          agent.Labels,
			Status:          AgentStatus(agent.Status),
			LastSeen:        agent.LastSeen,
			GroupID:         agent.GroupID,
			GroupName:       agent.GroupName,
			Version:         agent.Version,
			Capabilities:    agent.Capabilities,
			EffectiveConfig: agent.EffectiveConfig,
			DriftStatus:     ConfigDriftStatusUnknown,
			CreatedAt:       agent.CreatedAt,
			UpdatedAt:       agent.UpdatedAt,
		}

		if err := s.populateAgentConfigState(ctx, current, false); err != nil {
			s.logger.Warn("Failed to populate agent config state",
				zap.String("agent_id", current.ID.String()),
				zap.Error(err),
			)
		}

		result[i] = current
	}

	// Refresh fleet drift gauges as a side effect. This is the dominant code
	// path that walks the whole fleet, so gauges stay reasonably fresh as long
	// as someone (the UI poll, a Prometheus scrape via the API, etc.) is
	// calling ListAgents.
	s.refreshFleetDriftGauges(result)

	return result, nil
}

// refreshFleetDriftGauges tallies agents by drift status and updates the gauge
// values. Concurrent ListAgents callers can all execute this safely — each
// gauge Update is atomic and concurrent calls converge on the same value.
func (s *AgentServiceImpl) refreshFleetDriftGauges(agents []*Agent) {
	var synced, drifted, noIntent, noEffective, unknown int64
	for _, a := range agents {
		switch a.DriftStatus {
		case ConfigDriftStatusSynced:
			synced++
		case ConfigDriftStatusDrifted:
			drifted++
		case ConfigDriftStatusNoIntent:
			noIntent++
		case ConfigDriftStatusNoEffective:
			noEffective++
		default:
			unknown++
		}
	}
	s.driftMetrics.FleetAgentsTotal.Update(int64(len(agents)))
	s.driftMetrics.FleetSynced.Update(synced)
	s.driftMetrics.FleetDrifted.Update(drifted)
	s.driftMetrics.FleetNoIntent.Update(noIntent)
	s.driftMetrics.FleetNoEffective.Update(noEffective)
	s.driftMetrics.FleetUnknown.Update(unknown)
}

// UpdateAgentStatus updates agent status
func (s *AgentServiceImpl) UpdateAgentStatus(ctx context.Context, id uuid.UUID, status AgentStatus) error {
	return s.appStore.UpdateAgentStatus(ctx, id, applicationstore.AgentStatus(status))
}

// UpdateAgentLastSeen updates agent last seen timestamp
func (s *AgentServiceImpl) UpdateAgentLastSeen(ctx context.Context, id uuid.UUID, lastSeen time.Time) error {
	return s.appStore.UpdateAgentLastSeen(ctx, id, lastSeen)
}

// UpdateAgentEffectiveConfig updates agent effective config.
//
// Side effect: drift status is re-evaluated before and after the update; if
// the status transitions (e.g. synced -> drifted because the agent reverted
// its config), the corresponding transition counter is incremented and the
// transition is logged. This is the primary signal alerts will fire on.
func (s *AgentServiceImpl) UpdateAgentEffectiveConfig(ctx context.Context, id uuid.UUID, effectiveConfig string) error {
	prevStatus := s.snapshotDriftStatus(ctx, id)

	if err := s.appStore.UpdateAgentEffectiveConfig(ctx, id, effectiveConfig); err != nil {
		return err
	}

	currStatus := s.snapshotDriftStatus(ctx, id)
	if currStatus != prevStatus {
		s.recordDriftTransition(id, prevStatus, currStatus)
	}
	return nil
}

// UpdateAgentRegistration persists the mutable registration/grouping
// fields (Name, Labels, Version, GroupID, GroupName) of an existing
// agent. Status, LastSeen, EffectiveConfig, Capabilities and the
// immutable CreatedAt are intentionally NOT written by this path —
// status/last-seen have their own dedicated heartbeat updates and the
// registration UPDATE must not clobber them. Used by the OpAMP server
// when a registered agent re-reports a changed AgentDescription, and by
// the operator group-assignment endpoint.
func (s *AgentServiceImpl) UpdateAgentRegistration(ctx context.Context, agent *Agent) error {
	storageAgent := &applicationstore.Agent{
		ID:        agent.ID,
		Name:      agent.Name,
		Labels:    agent.Labels,
		Version:   agent.Version,
		GroupID:   agent.GroupID,
		GroupName: agent.GroupName,
	}
	return s.appStore.UpdateAgentRegistration(ctx, storageAgent)
}

// snapshotDriftStatus returns the drift status that a fresh GetAgent would
// compute. Returns ConfigDriftStatusUnknown if the agent can't be fetched —
// callers should treat that as "no change worth alerting on" and skip
// transition recording.
func (s *AgentServiceImpl) snapshotDriftStatus(ctx context.Context, id uuid.UUID) ConfigDriftStatus {
	agent, err := s.GetAgent(ctx, id)
	if err != nil || agent == nil {
		return ConfigDriftStatusUnknown
	}
	return agent.DriftStatus
}

// recordDriftTransition increments the appropriate transition counter and
// logs the transition. The from->drifted transition is the one operators
// will most often alert on, so it gets a WARN; recoveries log at INFO.
func (s *AgentServiceImpl) recordDriftTransition(agentID uuid.UUID, from, to ConfigDriftStatus) {
	fields := []zap.Field{
		zap.String("agent_id", agentID.String()),
		zap.String("from", string(from)),
		zap.String("to", string(to)),
	}
	switch to {
	case ConfigDriftStatusDrifted:
		s.driftMetrics.TransitionsToDrifted.Inc(1)
		s.logger.Warn("agent drifted", fields...)
	case ConfigDriftStatusSynced:
		s.driftMetrics.TransitionsToSynced.Inc(1)
		s.logger.Info("agent drift resolved", fields...)
	default:
		s.logger.Debug("agent drift status changed", fields...)
	}
	if s.broker != nil {
		s.broker.Publish(events.Event{
			Type: events.AgentDriftChanged,
			Data: map[string]any{
				"agent_id": agentID.String(),
				"from":     string(from),
				"to":       string(to),
			},
		})
	}
	if s.audit != nil {
		eventType := AuditEventAgentDriftSynced
		if to == ConfigDriftStatusDrifted {
			eventType = AuditEventAgentDriftDrifted
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.audit.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  eventType,
			TargetType: AuditTargetAgent,
			TargetID:   agentID.String(),
			Action:     "drift",
			Payload: map[string]any{
				"from": string(from),
				"to":   string(to),
			},
		})
	}
}

// DeleteAgent soft-deletes (tombstones) an agent. v0.51 — the row is
// retained so historical audit events still resolve by ID; an
// agent.decommissioned event is emitted so the operator who took the
// action is captured in the evidence path that CIP-007-6 R4.3 / R4.4
// auditors look at.
func (s *AgentServiceImpl) DeleteAgent(ctx context.Context, id uuid.UUID) error {
	// Snapshot the row before tombstoning so the audit event can
	// include name + group context. Best-effort: if the lookup
	// fails we still proceed with the delete and emit a minimal
	// event, because the delete itself is the operator's intent.
	pre, _ := s.appStore.GetAgent(ctx, id)
	if err := s.appStore.DeleteAgent(ctx, id); err != nil {
		return err
	}
	if s.audit != nil {
		payload := map[string]any{
			"reason": "operator decommission",
		}
		if pre != nil {
			payload["name"] = pre.Name
			if pre.GroupName != nil {
				payload["group"] = *pre.GroupName
			}
			payload["last_seen"] = pre.LastSeen.Format(time.RFC3339)
		}
		_ = s.audit.Record(ctx, AuditEntry{
			EventType:  "agent.decommissioned",
			TargetType: AuditTargetAgent,
			TargetID:   id.String(),
			Action:     "decommissioned",
			Payload:    payload,
		})
	}
	return nil
}

// CreateGroup creates a group
func (s *AgentServiceImpl) CreateGroup(ctx context.Context, group *Group) error {
	// v0.49 — serialize change windows to JSON for storage. The
	// changewindow package owns the type; storage just keeps the
	// blob. Validation happens at the handler boundary so a bad
	// window never reaches CreateGroup.
	cwJSON := "[]"
	if len(group.ChangeWindows) > 0 {
		raw, err := json.Marshal(group.ChangeWindows)
		if err != nil {
			return fmt.Errorf("failed to marshal change_windows: %w", err)
		}
		cwJSON = string(raw)
	}
	storageGroup := &applicationstore.Group{
		ID:                         group.ID,
		Name:                       group.Name,
		Labels:                     group.Labels,
		RequireApproval:            group.RequireApproval,
		RequireApprovalForRollback: group.RequireApprovalForRollback,
		// v0.89.18 (#634) — round-trip the verdict-learning policy
		// through the service layer so handlers can flip it.
		LearnFromVerdicts: group.LearnFromVerdicts,
		ChangeWindowsJSON: cwJSON,
		CreatedAt:         group.CreatedAt,
		UpdatedAt:         group.UpdatedAt,
	}
	return s.appStore.CreateGroup(ctx, storageGroup)
}

// GetGroup gets a group by ID
func (s *AgentServiceImpl) GetGroup(ctx context.Context, id string) (*Group, error) {
	group, err := s.appStore.GetGroup(ctx, id)
	if err != nil {
		return nil, err
	}

	if group == nil {
		return nil, nil
	}

	return groupToService(group), nil
}

// GetGroupByName gets a group by name
func (s *AgentServiceImpl) GetGroupByName(ctx context.Context, name string) (*Group, error) {
	groups, err := s.appStore.ListGroups(ctx)
	if err != nil {
		return nil, err
	}

	for _, group := range groups {
		if group.Name == name {
			return groupToService(group), nil
		}
	}

	return nil, nil
}

// ListGroups lists all groups
func (s *AgentServiceImpl) ListGroups(ctx context.Context) ([]*Group, error) {
	groups, err := s.appStore.ListGroups(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*Group, len(groups))
	for i, group := range groups {
		result[i] = groupToService(group)
	}

	return result, nil
}

// UpdateGroup writes mutable fields to the existing group. v0.48
// added this for the approval-policy toggle; v0.49 extended to
// round-trip change windows. UpdatedAt is bumped here so the
// storage layer doesn't have to.
func (s *AgentServiceImpl) UpdateGroup(ctx context.Context, group *Group) error {
	group.UpdatedAt = time.Now().UTC()
	cwJSON := "[]"
	if len(group.ChangeWindows) > 0 {
		raw, err := json.Marshal(group.ChangeWindows)
		if err != nil {
			return fmt.Errorf("failed to marshal change_windows: %w", err)
		}
		cwJSON = string(raw)
	}
	storageGroup := &applicationstore.Group{
		ID:                         group.ID,
		Name:                       group.Name,
		Labels:                     group.Labels,
		RequireApproval:            group.RequireApproval,
		RequireApprovalForRollback: group.RequireApprovalForRollback,
		// v0.89.18 (#634) — round-trip the verdict-learning policy
		// on update too. Without this, the v0.89.17 (#633) flag was
		// settable through SQL only.
		LearnFromVerdicts: group.LearnFromVerdicts,
		ChangeWindowsJSON: cwJSON,
		CreatedAt:         group.CreatedAt,
		UpdatedAt:         group.UpdatedAt,
	}
	return s.appStore.UpdateGroup(ctx, storageGroup)
}

// DeleteGroup deletes a group
func (s *AgentServiceImpl) DeleteGroup(ctx context.Context, id string) error {
	return s.appStore.DeleteGroup(ctx, id)
}

// groupToService is the storage→service projection. Kept local so
// the various Get* methods can share it without leaking the storage
// type beyond this file. v0.49 — parses ChangeWindowsJSON into the
// typed slice so callers (engine, handlers) don't have to deal with
// the storage blob.
func groupToService(g *applicationstore.Group) *Group {
	out := &Group{
		ID:                         g.ID,
		Name:                       g.Name,
		Labels:                     g.Labels,
		RequireApproval:            g.RequireApproval,
		RequireApprovalForRollback: g.RequireApprovalForRollback,
		// v0.89.18 (#634) — surface the v0.89.17 (#633) flag on the
		// service-layer Group so handlers and the JSON wire shape
		// see it.
		LearnFromVerdicts: g.LearnFromVerdicts,
		CreatedAt:         g.CreatedAt,
		UpdatedAt:         g.UpdatedAt,
	}
	if g.ChangeWindowsJSON != "" && g.ChangeWindowsJSON != "[]" {
		// Failing closed (empty slice) on malformed JSON is the safe
		// choice — a bad blob in storage shouldn't take the whole
		// group list down. Operators see "no windows" in the UI
		// and can re-set policy.
		_ = json.Unmarshal([]byte(g.ChangeWindowsJSON), &out.ChangeWindows)
	}
	return out
}

// CreateConfig creates a configuration
func (s *AgentServiceImpl) CreateConfig(ctx context.Context, config *Config) error {
	storageConfig := &applicationstore.Config{
		ID:         config.ID,
		Name:       config.Name,
		AgentID:    config.AgentID,
		GroupID:    config.GroupID,
		ConfigHash: config.ConfigHash,
		Content:    config.Content,
		Version:    config.Version,
		CreatedAt:  config.CreatedAt,
	}
	return s.appStore.CreateConfig(ctx, storageConfig)
}

// GetConfig gets a configuration by ID
func (s *AgentServiceImpl) GetConfig(ctx context.Context, id string) (*Config, error) {
	config, err := s.appStore.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	if config == nil {
		return nil, nil
	}

	return &Config{
		ID:         config.ID,
		Name:       config.Name,
		AgentID:    config.AgentID,
		GroupID:    config.GroupID,
		ConfigHash: config.ConfigHash,
		Content:    config.Content,
		Version:    config.Version,
		CreatedAt:  config.CreatedAt,
	}, nil
}

// GetLatestConfigForAgent gets the latest configuration for an agent
func (s *AgentServiceImpl) GetLatestConfigForAgent(ctx context.Context, agentID uuid.UUID) (*Config, error) {
	config, err := s.appStore.GetLatestConfigForAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}

	if config == nil {
		return nil, nil
	}

	return &Config{
		ID:         config.ID,
		Name:       config.Name,
		AgentID:    config.AgentID,
		GroupID:    config.GroupID,
		ConfigHash: config.ConfigHash,
		Content:    config.Content,
		Version:    config.Version,
		CreatedAt:  config.CreatedAt,
	}, nil
}

// GetLatestConfigForGroup gets the latest configuration for a group
func (s *AgentServiceImpl) GetLatestConfigForGroup(ctx context.Context, groupID string) (*Config, error) {
	config, err := s.appStore.GetLatestConfigForGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}

	if config == nil {
		return nil, nil
	}

	return &Config{
		ID:         config.ID,
		Name:       config.Name,
		AgentID:    config.AgentID,
		GroupID:    config.GroupID,
		ConfigHash: config.ConfigHash,
		Content:    config.Content,
		Version:    config.Version,
		CreatedAt:  config.CreatedAt,
	}, nil
}

// ListConfigs lists configurations with filters
func (s *AgentServiceImpl) ListConfigs(ctx context.Context, filter ConfigFilter) ([]*Config, error) {
	storageFilter := applicationstore.ConfigFilter{
		AgentID: filter.AgentID,
		GroupID: filter.GroupID,
		Limit:   filter.Limit,
	}

	configs, err := s.appStore.ListConfigs(ctx, storageFilter)
	if err != nil {
		return nil, err
	}

	result := make([]*Config, len(configs))
	for i, config := range configs {
		result[i] = &Config{
			ID:         config.ID,
			Name:       config.Name,
			AgentID:    config.AgentID,
			GroupID:    config.GroupID,
			ConfigHash: config.ConfigHash,
			Content:    config.Content,
			Version:    config.Version,
			CreatedAt:  config.CreatedAt,
		}
	}

	return result, nil
}

// StoreConfigForAgent validates and stores configuration for an agent (storage only, no delivery)
func (s *AgentServiceImpl) StoreConfigForAgent(ctx context.Context, agentID uuid.UUID, content string) (*Config, error) {
	// 1. Validate agent exists and has remote config capability
	agent, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("failed to get agent: %w", err)
	}
	if agent == nil {
		return nil, fmt.Errorf("agent not found")
	}

	// 2. Check if agent has remote config capability
	hasCapability := false
	for _, cap := range agent.Capabilities {
		if cap == "accepts_remote_config" {
			hasCapability = true
			break
		}
	}
	if !hasCapability {
		return nil, fmt.Errorf("agent does not support remote config")
	}

	// 3. Store config in database with versioning
	configHash := hashConfigContent(content)

	// Get latest version for this agent
	latestConfig, _ := s.GetLatestConfigForAgent(ctx, agentID)
	version := 1
	if latestConfig != nil {
		version = latestConfig.Version + 1
	}

	newConfig := &Config{
		ID:         uuid.New().String(),
		AgentID:    &agentID,
		ConfigHash: configHash,
		Content:    content,
		Version:    version,
		CreatedAt:  time.Now(),
	}

	if err := s.CreateConfig(ctx, newConfig); err != nil {
		return nil, fmt.Errorf("failed to store config: %w", err)
	}

	s.logger.Info("Configuration stored for agent",
		zap.String("agent_id", agentID.String()),
		zap.String("config_id", newConfig.ID),
		zap.Int("version", version))

	if s.audit != nil {
		_ = s.audit.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  AuditEventConfigStored,
			TargetType: AuditTargetConfig,
			TargetID:   newConfig.ID,
			Action:     "stored",
			Payload: map[string]any{
				"agent_id":    agentID.String(),
				"version":     version,
				"config_hash": configHash,
			},
		})
	}

	return newConfig, nil
}

func (s *AgentServiceImpl) populateAgentConfigState(ctx context.Context, agent *Agent, includeContent bool) error {
	if agent == nil {
		return nil
	}

	intent, err := s.determineConfigIntent(ctx, agent, includeContent)
	if err != nil {
		return err
	}

	agent.ConfigIntent = intent
	status, details := computeConfigDrift(intent, agent.EffectiveConfig, includeContent)
	agent.DriftStatus = status
	agent.DriftDetails = details
	return nil
}

func (s *AgentServiceImpl) determineConfigIntent(ctx context.Context, agent *Agent, includeContent bool) (*ConfigIntent, error) {
	if agent == nil {
		return nil, nil
	}

	// Prefer agent-specific config
	agentConfig, err := s.appStore.GetLatestConfigForAgent(ctx, agent.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get agent config: %w", err)
	}
	if agentConfig != nil {
		return buildConfigIntent(agentConfig, ConfigIntentSourceAgent, agent.Name, includeContent), nil
	}

	// Fallback to group-level config
	if agent.GroupID != nil && *agent.GroupID != "" {
		groupConfig, err := s.appStore.GetLatestConfigForGroup(ctx, *agent.GroupID)
		if err != nil {
			return nil, fmt.Errorf("failed to get group config: %w", err)
		}
		if groupConfig != nil {
			var sourceName string
			if agent.GroupName != nil {
				sourceName = *agent.GroupName
			}
			return buildConfigIntent(groupConfig, ConfigIntentSourceGroup, sourceName, includeContent), nil
		}
	}

	return nil, nil
}

func buildConfigIntent(cfg *applicationstore.Config, source ConfigIntentSource, sourceName string, includeContent bool) *ConfigIntent {
	if cfg == nil {
		return nil
	}

	intent := &ConfigIntent{
		Source:     source,
		SourceName: sourceName,
		ConfigID:   cfg.ID,
		Version:    cfg.Version,
		Hash:       cfg.ConfigHash,
		UpdatedAt:  cfg.CreatedAt,
	}

	if includeContent {
		intent.Content = cfg.Content
	}

	return intent
}

func computeConfigDrift(intent *ConfigIntent, effectiveConfig string, includeDiff bool) (ConfigDriftStatus, *ConfigDriftDetails) {
	checkedAt := time.Now()
	normalizedEffective := normalizeConfigContent(effectiveConfig)

	if intent == nil {
		return ConfigDriftStatusNoIntent, &ConfigDriftDetails{
			EffectiveHash: hashConfigContent(normalizedEffective),
			CheckedAt:     checkedAt,
		}
	}

	if normalizedEffective == "" {
		return ConfigDriftStatusNoEffective, &ConfigDriftDetails{
			IntentHash: intent.Hash,
			CheckedAt:  checkedAt,
		}
	}

	effectiveHash := hashConfigContent(normalizedEffective)
	if effectiveHash == intent.Hash {
		return ConfigDriftStatusSynced, &ConfigDriftDetails{
			IntentHash:    intent.Hash,
			EffectiveHash: effectiveHash,
			CheckedAt:     checkedAt,
		}
	}

	details := &ConfigDriftDetails{
		IntentHash:    intent.Hash,
		EffectiveHash: effectiveHash,
		CheckedAt:     checkedAt,
	}

	if includeDiff && intent.Content != "" {
		diff := buildUnifiedDiff(intent.Content, effectiveConfig)
		if diff != "" {
			details.Diff = diff
		}
	}

	return ConfigDriftStatusDrifted, details
}

// normalizeConfigContent and hashConfigContent delegate to the canonical
// confignorm package so drift detection, the rollout preview diff
// (internal/configdiff), and config-creation hashing (api/handlers) all
// normalize and fingerprint config content identically. See confignorm's
// package doc for why divergence here was a real bug.
func normalizeConfigContent(content string) string {
	return confignorm.Normalize(content)
}

func hashConfigContent(content string) string {
	return confignorm.Hash(content)
}

func buildUnifiedDiff(expected, actual string) string {
	expectedLines := strings.Split(normalizeConfigContent(expected), "\n")
	actualLines := strings.Split(normalizeConfigContent(actual), "\n")
	diff := difflib.UnifiedDiff{
		A:        expectedLines,
		B:        actualLines,
		FromFile: "intended",
		ToFile:   "effective",
		Context:  3,
	}

	var buf bytes.Buffer
	if err := difflib.WriteUnifiedDiff(&buf, diff); err != nil {
		return ""
	}

	return buf.String()
}
