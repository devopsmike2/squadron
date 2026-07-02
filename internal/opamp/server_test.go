// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// MockAgentService is a mock implementation of AgentService for testing
type MockAgentService struct {
	mock.Mock
}

func (m *MockAgentService) CreateAgent(ctx context.Context, agent *services.Agent) error {
	args := m.Called(ctx, agent)
	return args.Error(0)
}

func (m *MockAgentService) GetAgent(ctx context.Context, id uuid.UUID) (*services.Agent, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*services.Agent), args.Error(1)
}

func (m *MockAgentService) ListAgents(ctx context.Context) ([]*services.Agent, error) {
	args := m.Called(ctx)
	return args.Get(0).([]*services.Agent), args.Error(1)
}

func (m *MockAgentService) UpdateAgentStatus(ctx context.Context, id uuid.UUID, status services.AgentStatus) error {
	args := m.Called(ctx, id, status)
	return args.Error(0)
}

func (m *MockAgentService) UpdateAgentLastSeen(ctx context.Context, id uuid.UUID, lastSeen time.Time) error {
	args := m.Called(ctx, id, lastSeen)
	return args.Error(0)
}

func (m *MockAgentService) UpdateAgentEffectiveConfig(ctx context.Context, id uuid.UUID, effectiveConfig string) error {
	args := m.Called(ctx, id, effectiveConfig)
	return args.Error(0)
}

func (m *MockAgentService) UpdateAgentRegistration(ctx context.Context, agent *services.Agent) error {
	args := m.Called(ctx, agent)
	return args.Error(0)
}

func (m *MockAgentService) DeleteAgent(ctx context.Context, id uuid.UUID) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockAgentService) CreateGroup(ctx context.Context, group *services.Group) error {
	args := m.Called(ctx, group)
	return args.Error(0)
}

func (m *MockAgentService) GetGroup(ctx context.Context, id string) (*services.Group, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*services.Group), args.Error(1)
}

func (m *MockAgentService) GetGroupByName(ctx context.Context, name string) (*services.Group, error) {
	args := m.Called(ctx, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*services.Group), args.Error(1)
}

func (m *MockAgentService) ListGroups(ctx context.Context) ([]*services.Group, error) {
	args := m.Called(ctx)
	return args.Get(0).([]*services.Group), args.Error(1)
}

func (m *MockAgentService) DeleteGroup(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

// UpdateGroup was added to the services.AgentService interface in
// v0.48 for the approval-policy toggle on Groups settings. This
// mock stub keeps the opamp tests compiling; none of them exercise
// UpdateGroup directly, so the testify Called pattern is enough.
func (m *MockAgentService) UpdateGroup(ctx context.Context, group *services.Group) error {
	args := m.Called(ctx, group)
	return args.Error(0)
}

func (m *MockAgentService) CreateConfig(ctx context.Context, config *services.Config) error {
	args := m.Called(ctx, config)
	return args.Error(0)
}

func (m *MockAgentService) GetConfig(ctx context.Context, id string) (*services.Config, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*services.Config), args.Error(1)
}

func (m *MockAgentService) GetLatestConfigForAgent(ctx context.Context, agentID uuid.UUID) (*services.Config, error) {
	args := m.Called(ctx, agentID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*services.Config), args.Error(1)
}

func (m *MockAgentService) GetLatestConfigForGroup(ctx context.Context, groupID string) (*services.Config, error) {
	args := m.Called(ctx, groupID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*services.Config), args.Error(1)
}

func (m *MockAgentService) ListConfigs(ctx context.Context, filter services.ConfigFilter) ([]*services.Config, error) {
	args := m.Called(ctx, filter)
	return args.Get(0).([]*services.Config), args.Error(1)
}

func (m *MockAgentService) StoreConfigForAgent(ctx context.Context, agentID uuid.UUID, content string) (*services.Config, error) {
	args := m.Called(ctx, agentID, content)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*services.Config), args.Error(1)
}

// Tests

func TestSendConfigToAgent_AgentNotFound(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)

	configSender := NewConfigSender(agents, logger)

	agentID := uuid.New()
	err := configSender.SendConfigToAgent(agentID, "test-config")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent not found")
}

func TestSendConfigToAgent_NoCapability(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)

	configSender := NewConfigSender(agents, logger)

	// Create an agent without remote config capability
	agentID := uuid.New()
	agent := &Agent{
		InstanceId:    agentID,
		InstanceIdStr: agentID.String(),
		Status: &protobufs.AgentToServer{
			Capabilities: 0, // No capabilities
		},
	}

	agents.agentsById[agentID] = agent

	err := configSender.SendConfigToAgent(agentID, "test-config")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support remote config")
}

func TestGetConfigForAgent_AgentConfig(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)
	mockService := new(MockAgentService)

	agentID := uuid.New()
	agentConfig := &services.Config{
		ID:         "config-1",
		AgentID:    &agentID,
		ConfigHash: "hash1",
		Content:    "agent-specific-config",
		Version:    1,
		CreatedAt:  time.Now(),
	}

	mockService.On("GetLatestConfigForAgent", mock.Anything, agentID).Return(agentConfig, nil)

	server := &Server{
		logger:       logger,
		agents:       agents,
		agentService: mockService,
	}

	agent := &Agent{
		InstanceId:    agentID,
		InstanceIdStr: agentID.String(),
	}

	config := server.getConfigForAgent(context.Background(), agent)

	assert.Equal(t, "agent-specific-config", config)
	mockService.AssertExpectations(t)
}

func TestGetConfigForAgent_GroupConfig(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)
	mockService := new(MockAgentService)

	agentID := uuid.New()
	groupID := "group-1"
	groupConfig := &services.Config{
		ID:         "config-2",
		GroupID:    &groupID,
		ConfigHash: "hash2",
		Content:    "group-config",
		Version:    1,
		CreatedAt:  time.Now(),
	}

	// No agent config
	mockService.On("GetLatestConfigForAgent", mock.Anything, agentID).Return(nil, nil)
	// Has group config
	mockService.On("GetLatestConfigForGroup", mock.Anything, groupID).Return(groupConfig, nil)

	server := &Server{
		logger:       logger,
		agents:       agents,
		agentService: mockService,
	}

	agent := &Agent{
		InstanceId:    agentID,
		InstanceIdStr: agentID.String(),
		GroupID:       &groupID,
	}

	config := server.getConfigForAgent(context.Background(), agent)

	assert.Equal(t, "group-config", config)
	mockService.AssertExpectations(t)
}

func TestGetConfigForAgent_DefaultConfig(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)
	mockService := new(MockAgentService)

	agentID := uuid.New()

	// No agent config
	mockService.On("GetLatestConfigForAgent", mock.Anything, agentID).Return(nil, nil)

	server := &Server{
		logger:       logger,
		agents:       agents,
		agentService: mockService,
	}

	agent := &Agent{
		InstanceId:    agentID,
		InstanceIdStr: agentID.String(),
		GroupID:       nil, // No group
	}

	config := server.getConfigForAgent(context.Background(), agent)

	assert.Equal(t, DefaultOTelConfig, config)
	mockService.AssertExpectations(t)
}

func TestGetConfigForAgent_Priority(t *testing.T) {
	// Test that agent config takes priority over group config
	logger := zap.NewNop()
	agents := NewAgents(logger)
	mockService := new(MockAgentService)

	agentID := uuid.New()
	groupID := "group-1"

	agentConfig := &services.Config{
		ID:         "config-1",
		AgentID:    &agentID,
		ConfigHash: "hash1",
		Content:    "agent-specific-config",
		Version:    1,
		CreatedAt:  time.Now(),
	}

	// Agent config exists, so group config should not be fetched
	mockService.On("GetLatestConfigForAgent", mock.Anything, agentID).Return(agentConfig, nil)

	server := &Server{
		logger:       logger,
		agents:       agents,
		agentService: mockService,
	}

	agent := &Agent{
		InstanceId:    agentID,
		InstanceIdStr: agentID.String(),
		GroupID:       &groupID, // Has group but agent config should take priority
	}

	config := server.getConfigForAgent(context.Background(), agent)

	// Should get agent config, not group config
	assert.Equal(t, "agent-specific-config", config)
	mockService.AssertExpectations(t)
	// GetLatestConfigForGroup should NOT be called
	mockService.AssertNotCalled(t, "GetLatestConfigForGroup", mock.Anything, groupID)
}

func TestGetAgent(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)
	mockService := new(MockAgentService)

	server := &Server{
		logger:       logger,
		agents:       agents,
		agentService: mockService,
	}

	agentID := uuid.New()
	agent := &Agent{
		InstanceId:    agentID,
		InstanceIdStr: agentID.String(),
	}

	agents.agentsById[agentID] = agent

	// Test success case
	retrievedAgent, err := server.GetAgent(agentID)
	require.NoError(t, err)
	assert.Equal(t, agentID, retrievedAgent.InstanceId)

	// Test not found case
	nonExistentID := uuid.New()
	_, err = server.GetAgent(nonExistentID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent not found")
}

func TestExtractGroupInfo(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)
	mockService := new(MockAgentService)

	server := &Server{
		logger:       logger,
		agents:       agents,
		agentService: mockService,
	}

	tests := []struct {
		name         string
		description  *protobufs.AgentDescription
		expectedID   string
		expectedName string
	}{
		{
			name:         "nil description",
			description:  nil,
			expectedID:   "",
			expectedName: "",
		},
		{
			name: "group.id attribute",
			description: &protobufs.AgentDescription{
				IdentifyingAttributes: []*protobufs.KeyValue{
					{
						Key: "group.id",
						Value: &protobufs.AnyValue{
							Value: &protobufs.AnyValue_StringValue{StringValue: "group-1"},
						},
					},
				},
			},
			expectedID:   "group-1",
			expectedName: "",
		},
		{
			name: "group.name attribute",
			description: &protobufs.AgentDescription{
				NonIdentifyingAttributes: []*protobufs.KeyValue{
					{
						Key: "group.name",
						Value: &protobufs.AnyValue{
							Value: &protobufs.AnyValue_StringValue{StringValue: "test-group"},
						},
					},
				},
			},
			expectedID:   "",
			expectedName: "test-group",
		},
		{
			name: "both id and name",
			description: &protobufs.AgentDescription{
				IdentifyingAttributes: []*protobufs.KeyValue{
					{
						Key: "group.id",
						Value: &protobufs.AnyValue{
							Value: &protobufs.AnyValue_StringValue{StringValue: "group-1"},
						},
					},
					{
						Key: "group.name",
						Value: &protobufs.AnyValue{
							Value: &protobufs.AnyValue_StringValue{StringValue: "test-group"},
						},
					},
				},
			},
			expectedID:   "group-1",
			expectedName: "test-group",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupID, groupName := server.extractGroupInfo(tt.description)
			assert.Equal(t, tt.expectedID, groupID)
			assert.Equal(t, tt.expectedName, groupName)
		})
	}
}

// strAttr builds a single string-valued KeyValue for an AgentDescription.
func strAttr(k, v string) *protobufs.KeyValue {
	return &protobufs.KeyValue{
		Key:   k,
		Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: v}},
	}
}

// TestPersistAgent_ExistingAgent_WithDescription_UpdatesRegistration:
// when a registered agent re-reports a changed AgentDescription, the
// else branch now persists the new name/labels/version via
// UpdateAgentRegistration (previously dropped). This is the core of the
// slice-3 fix that keeps the stored GroupID rollout scoping reads in sync.
func TestPersistAgent_ExistingAgent_WithDescription_UpdatesRegistration(t *testing.T) {
	logger := zap.NewNop()
	mockService := new(MockAgentService)

	agentID := uuid.New()
	existing := &services.Agent{ID: agentID, Name: "original", Version: "1.0.0"}

	mockService.On("GetAgent", mock.Anything, agentID).Return(existing, nil)
	mockService.On("UpdateAgentStatus", mock.Anything, agentID, mock.Anything).Return(nil)
	mockService.On("UpdateAgentLastSeen", mock.Anything, agentID, mock.Anything).Return(nil)
	mockService.On("UpdateAgentRegistration", mock.Anything, mock.MatchedBy(func(a *services.Agent) bool {
		return a.ID == agentID && a.Name == "renamed" && a.Version == "2.0.0"
	})).Return(nil)

	server := &Server{logger: logger, agentService: mockService}

	agent := &Agent{InstanceId: agentID, InstanceIdStr: agentID.String()}
	msg := &protobufs.AgentToServer{
		AgentDescription: &protobufs.AgentDescription{
			IdentifyingAttributes: []*protobufs.KeyValue{
				strAttr("service.name", "renamed"),
				strAttr("service.version", "2.0.0"),
			},
			NonIdentifyingAttributes: []*protobufs.KeyValue{
				strAttr("tier", "gold"),
			},
		},
	}

	server.persistAgent(context.Background(), agent, msg)

	mockService.AssertCalled(t, "UpdateAgentRegistration", mock.Anything, mock.Anything)
	mockService.AssertExpectations(t)
}

// TestPersistAgent_ExistingAgent_NoDescription_SkipsRegistration: a
// description-less heartbeat must NOT call UpdateAgentRegistration —
// otherwise it would clobber the stored identity (name="unknown",
// labels={}, version="") and the GroupID with empty values.
func TestPersistAgent_ExistingAgent_NoDescription_SkipsRegistration(t *testing.T) {
	logger := zap.NewNop()
	mockService := new(MockAgentService)

	agentID := uuid.New()
	existing := &services.Agent{ID: agentID, Name: "original", Version: "1.0.0"}

	mockService.On("GetAgent", mock.Anything, agentID).Return(existing, nil)
	mockService.On("UpdateAgentStatus", mock.Anything, agentID, mock.Anything).Return(nil)
	mockService.On("UpdateAgentLastSeen", mock.Anything, agentID, mock.Anything).Return(nil)

	server := &Server{logger: logger, agentService: mockService}

	agent := &Agent{InstanceId: agentID, InstanceIdStr: agentID.String()}
	msg := &protobufs.AgentToServer{} // no AgentDescription

	server.persistAgent(context.Background(), agent, msg)

	mockService.AssertNotCalled(t, "UpdateAgentRegistration", mock.Anything, mock.Anything)
	mockService.AssertExpectations(t)
}

func TestExtractAgentName(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name string
		desc *protobufs.AgentDescription
		want string
	}{
		{"nil description", nil, "unknown"},
		{"host.name preferred over generic service.name",
			&protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{
				strAttr("service.name", "otelcol-contrib"), strAttr("host.name", "web-01")}}, "web-01"},
		{"service.name when no host.name",
			&protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{
				strAttr("service.name", "payments-api")}}, "payments-api"},
		{"agent.name explicit override wins over host.name",
			&protobufs.AgentDescription{
				IdentifyingAttributes:    []*protobufs.KeyValue{strAttr("host.name", "web-01")},
				NonIdentifyingAttributes: []*protobufs.KeyValue{strAttr("agent.name", "custom")}}, "custom"},
		{"no usable attrs", &protobufs.AgentDescription{}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.extractAgentName(tc.desc); got != tc.want {
				t.Fatalf("extractAgentName = %q, want %q", got, tc.want)
			}
		})
	}
}
