// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/devopsmike2/squadron/internal/services"
)

// statefulAgentService embeds the testify MockAgentService (so it satisfies the
// full services.AgentService surface) and overrides only the two methods the
// adopt-on-supervise path exercises with REAL stateful behavior: CreateConfig
// stores an agent-scoped config, GetLatestConfigForAgent reads it back. This
// gives true create-once / idempotency semantics without standing up a store.
// The agents under test seed no persisted row (agents map empty), so GetAgent
// returns nil, the persisted-group fallback resolves to "", and
// GetLatestConfigForGroup is never reached — adopt must not touch the group path
// for these genuinely-ungrouped agents.
type statefulAgentService struct {
	*MockAgentService
	mu           sync.Mutex
	configs      map[uuid.UUID]*services.Config
	agents       map[uuid.UUID]*services.Agent
	groupConfigs map[string]*services.Config
	created      int
}

func newStatefulAgentService() *statefulAgentService {
	return &statefulAgentService{
		MockAgentService: new(MockAgentService),
		configs:          make(map[uuid.UUID]*services.Config),
		agents:           make(map[uuid.UUID]*services.Agent),
		groupConfigs:     make(map[string]*services.Config),
	}
}

func (f *statefulAgentService) GetLatestConfigForAgent(_ context.Context, id uuid.UUID) (*services.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configs[id], nil
}

// GetAgent returns the persisted agent row, or nil when none was seeded. The
// resolver consults it to fall back to the persisted group membership when the
// in-memory GroupID is empty; the agents in most adopt tests carry no persisted
// row, so this returns nil and the group path stays inert (adopt still fires).
func (f *statefulAgentService) GetAgent(_ context.Context, id uuid.UUID) (*services.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents[id], nil
}

// GetLatestConfigForGroup reads back a seeded group config; nil when none.
func (f *statefulAgentService) GetLatestConfigForGroup(_ context.Context, groupID string) (*services.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groupConfigs[groupID], nil
}

func (f *statefulAgentService) CreateConfig(_ context.Context, cfg *services.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cfg.AgentID != nil {
		f.configs[*cfg.AgentID] = cfg
	}
	f.created++
	return nil
}

func (f *statefulAgentService) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

// fakeAudit captures adopt-on-supervise audit events.
type fakeAudit struct {
	mu      sync.Mutex
	entries []services.AuditEntry
}

func (f *fakeAudit) Record(_ context.Context, e services.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAudit) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

// brownfieldEffectiveConfig is a fully-wired collector config as a supervised
// brownfield agent would REPORT it: a direct opamp extension (both the block and
// the service.extensions reference), health_check + zpages + oauth2client
// extensions, filelog/hostmetrics/otlp receivers, an otlphttp exporter with an
// ${ENV} reference, and service.telemetry (self-telemetry). Adopt-on-supervise
// must strip ONLY the opamp extension and keep everything else.
const brownfieldEffectiveConfig = `extensions:
  opamp:
    server:
      ws:
        endpoint: ${SQUADRON_OPAMP_ENDPOINT}
  health_check:
    endpoint: 0.0.0.0:13133
  zpages:
    endpoint: 0.0.0.0:55679
  oauth2client:
    client_id: ${OAUTH_CLIENT_ID}
    client_secret: ${OAUTH_CLIENT_SECRET}
    token_url: https://auth.example.com/token
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
  filelog:
    include:
      - /var/log/app/*.log
  hostmetrics:
    scrapers:
      cpu:
      memory:
processors:
  batch:
exporters:
  otlphttp:
    endpoint: ${BACKEND_OTLP_ENDPOINT}
    auth:
      authenticator: oauth2client
service:
  extensions:
    - opamp
    - health_check
    - zpages
  pipelines:
    logs:
      receivers: [filelog]
      processors: [batch]
      exporters: [otlphttp]
    metrics:
      receivers: [hostmetrics]
      processors: [batch]
      exporters: [otlphttp]
  telemetry:
    metrics:
      level: detailed
    logs:
      level: info
`

func remoteConfigAgent(id uuid.UUID, effective string, caps uint64) *Agent {
	return &Agent{
		InstanceId:      id,
		InstanceIdStr:   id.String(),
		FleetId:         id,
		FleetIdStr:      id.String(),
		Status:          &protobufs.AgentToServer{Capabilities: caps},
		EffectiveConfig: effective,
	}
}

const acceptsRemoteConfig = uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)

// TestAdoptOnSupervise_AdoptsFromEffectiveConfig: an accepts_remote_config agent
// reporting a non-empty effective config, with no assigned config, gets its
// initial config SEEDED from that effective config — opamp extension stripped,
// everything else intact, ${ENV} preserved, validated, created + assigned, audit
// emitted — and it is NOT the skeleton.
func TestAdoptOnSupervise_AdoptsFromEffectiveConfig(t *testing.T) {
	svc := newStatefulAgentService()
	audit := &fakeAudit{}
	server := &Server{logger: zap.NewNop(), agents: NewAgents(zap.NewNop()), agentService: svc, audit: audit}

	id := uuid.New()
	agent := remoteConfigAgent(id, brownfieldEffectiveConfig, acceptsRemoteConfig)

	got := server.getConfigForAgent(context.Background(), agent)

	// Not the skeleton.
	require.NotEqual(t, DefaultOTelConfig, got)

	// Created + assigned exactly one config to this agent.
	assert.Equal(t, 1, svc.createdCount())
	stored := svc.configs[id]
	require.NotNil(t, stored)
	require.NotNil(t, stored.AgentID)
	assert.Equal(t, id, *stored.AgentID)
	assert.Equal(t, got, stored.Content)

	// Audit event emitted.
	require.Equal(t, 1, audit.count())
	assert.Equal(t, services.AuditEventAgentConfigAdopted, audit.entries[0].EventType)
	assert.Equal(t, stored.ID, audit.entries[0].TargetID)

	// opamp extension stripped: neither the extensions block nor the
	// service.extensions reference survives.
	var parsed map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(got), &parsed))

	exts, _ := parsed["extensions"].(map[string]any)
	require.NotNil(t, exts)
	_, hasOpamp := exts["opamp"]
	assert.False(t, hasOpamp, "extensions.opamp should be stripped")
	assert.Contains(t, exts, "health_check", "non-opamp extensions must be kept")
	assert.Contains(t, exts, "zpages")
	assert.Contains(t, exts, "oauth2client")

	svcSection, _ := parsed["service"].(map[string]any)
	require.NotNil(t, svcSection)
	svcExts, _ := svcSection["extensions"].([]any)
	for _, e := range svcExts {
		assert.NotEqual(t, "opamp", e, "opamp must be removed from service.extensions")
	}
	assert.Contains(t, svcExts, "health_check")
	assert.Contains(t, svcExts, "zpages")

	// Pipelines / receivers / exporters / self-telemetry intact.
	assert.NotContains(t, got, "opamp", "no residual opamp reference anywhere")
	assert.Contains(t, got, "filelog")
	assert.Contains(t, got, "hostmetrics")
	assert.Contains(t, got, "otlphttp")
	assert.Contains(t, got, "oauth2client")
	assert.Contains(t, got, "telemetry", "service.telemetry (self-telemetry) must be kept")

	// ${ENV} references preserved (not baked into literal values).
	assert.Contains(t, got, "${BACKEND_OTLP_ENDPOINT}")
	assert.Contains(t, got, "${OAUTH_CLIENT_ID}")
	assert.Contains(t, got, "${OAUTH_CLIENT_SECRET}")
}

// TestAdoptOnSupervise_NoEffectiveConfig_Skeleton: an accepts_remote_config agent
// that has reported NO effective config falls back to the skeleton (fresh agent,
// unchanged behavior) — nothing is created.
func TestAdoptOnSupervise_NoEffectiveConfig_Skeleton(t *testing.T) {
	svc := newStatefulAgentService()
	server := &Server{logger: zap.NewNop(), agents: NewAgents(zap.NewNop()), agentService: svc}

	id := uuid.New()
	agent := remoteConfigAgent(id, "", acceptsRemoteConfig)

	got := server.getConfigForAgent(context.Background(), agent)

	assert.Equal(t, DefaultOTelConfig, got)
	assert.Equal(t, 0, svc.createdCount())
}

// TestAdoptOnSupervise_InvalidEffectiveConfig_Skeleton: an accepts_remote_config
// agent reporting an UNPARSEABLE effective config falls back to the skeleton
// rather than pushing garbage — nothing is created.
func TestAdoptOnSupervise_InvalidEffectiveConfig_Skeleton(t *testing.T) {
	svc := newStatefulAgentService()
	audit := &fakeAudit{}
	server := &Server{logger: zap.NewNop(), agents: NewAgents(zap.NewNop()), agentService: svc, audit: audit}

	id := uuid.New()
	// Invalid YAML: a mapping value that opens a flow sequence and never closes,
	// plus bad indentation — yaml.Unmarshal rejects it.
	agent := remoteConfigAgent(id, "service:\n  pipelines: [logs\n    receivers: ]]{", acceptsRemoteConfig)

	got := server.getConfigForAgent(context.Background(), agent)

	assert.Equal(t, DefaultOTelConfig, got)
	assert.Equal(t, 0, svc.createdCount())
	assert.Equal(t, 0, audit.count())
}

// TestAdoptOnSupervise_AssignedConfigWins: when a managed config is already
// assigned to the agent, the assigned config wins — no adopt, no duplicate.
func TestAdoptOnSupervise_AssignedConfigWins(t *testing.T) {
	svc := newStatefulAgentService()
	server := &Server{logger: zap.NewNop(), agents: NewAgents(zap.NewNop()), agentService: svc}

	id := uuid.New()
	assigned := &services.Config{ID: "assigned-1", AgentID: &id, Content: "assigned-config-content", Version: 3}
	svc.configs[id] = assigned

	agent := remoteConfigAgent(id, brownfieldEffectiveConfig, acceptsRemoteConfig)

	got := server.getConfigForAgent(context.Background(), agent)

	assert.Equal(t, "assigned-config-content", got)
	assert.Equal(t, 0, svc.createdCount(), "assigned config must not trigger an adopt/create")
}

// TestAdoptOnSupervise_Idempotent: resolving twice creates the seeded config once
// — the second resolution reads back the now-assigned config and does NOT
// re-adopt.
func TestAdoptOnSupervise_Idempotent(t *testing.T) {
	svc := newStatefulAgentService()
	audit := &fakeAudit{}
	server := &Server{logger: zap.NewNop(), agents: NewAgents(zap.NewNop()), agentService: svc, audit: audit}

	id := uuid.New()
	agent := remoteConfigAgent(id, brownfieldEffectiveConfig, acceptsRemoteConfig)

	first := server.getConfigForAgent(context.Background(), agent)
	second := server.getConfigForAgent(context.Background(), agent)

	assert.Equal(t, first, second)
	assert.Equal(t, 1, svc.createdCount(), "second resolution must not create a second config")
	assert.Equal(t, 1, audit.count(), "second resolution must not re-audit")
}

// TestAdoptOnSupervise_ReportOnly_NoAdopt: a report-only agent (no
// accepts_remote_config) never adopts. In production such an agent never reaches
// getConfigForAgent at all (the caller gates the whole config push on the
// capability); this asserts the adopt guard itself is capability-gated.
func TestAdoptOnSupervise_ReportOnly_NoAdopt(t *testing.T) {
	svc := newStatefulAgentService()
	audit := &fakeAudit{}
	server := &Server{logger: zap.NewNop(), agents: NewAgents(zap.NewNop()), agentService: svc, audit: audit}

	id := uuid.New()
	agent := remoteConfigAgent(id, brownfieldEffectiveConfig, 0) // no capabilities

	got := server.getConfigForAgent(context.Background(), agent)

	// resolveStoredConfig finds nothing, adopt is capability-gated off, so the
	// skeleton is returned and nothing is created/audited.
	assert.Equal(t, DefaultOTelConfig, got)
	assert.Equal(t, 0, svc.createdCount())
	assert.Equal(t, 0, audit.count())
}

// TestAdoptOnSupervise_Disabled_Skeleton: with the feature toggled off, an
// otherwise-eligible agent falls back to the skeleton and nothing is created.
func TestAdoptOnSupervise_Disabled_Skeleton(t *testing.T) {
	SetAdoptOnSupervise(false)
	defer SetAdoptOnSupervise(true)

	svc := newStatefulAgentService()
	server := &Server{logger: zap.NewNop(), agents: NewAgents(zap.NewNop()), agentService: svc}

	id := uuid.New()
	agent := remoteConfigAgent(id, brownfieldEffectiveConfig, acceptsRemoteConfig)

	got := server.getConfigForAgent(context.Background(), agent)

	assert.Equal(t, DefaultOTelConfig, got)
	assert.Equal(t, 0, svc.createdCount())
}

// TestStripOpampExtension_NoOpampIsNoop: stripping a config that has no opamp
// extension leaves its sections intact and remains valid YAML.
func TestStripOpampExtension_NoOpampIsNoop(t *testing.T) {
	in := `extensions:
  health_check:
    endpoint: 0.0.0.0:13133
receivers:
  otlp:
    protocols:
      grpc:
service:
  extensions:
    - health_check
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp]
`
	out, err := stripOpampExtension(in)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(out), &parsed))
	exts := parsed["extensions"].(map[string]any)
	assert.Contains(t, exts, "health_check")
	assert.False(t, strings.Contains(out, "opamp"))
}
