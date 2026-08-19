package opamp

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	"github.com/open-telemetry/opamp-go/server/types"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/agentid"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/services"
)

// tenantHeader is the per-connection tenant header an OpAMP client (or a
// fronting proxy) stamps to declare which tenant its agent/group store writes
// land in (ADR 0012 §Decision 2). It mirrors the x-squadron-agent-id header
// this package already injects into the own-telemetry connection settings
// offer (see calcConnectionSettings). Empty resolves to identity.DefaultTenant,
// keeping OSS single-tenant behavior inert.
const tenantHeader = "x-squadron-tenant"

// rejectUntenantedConnections gates whether an OpAMP connection presenting no
// x-squadron-tenant header is rejected at connect time. It mirrors the
// sqlite.SetStrictTenantScoping seam: the enterprise wire calls
// SetRejectUntenantedConnections(true) at startup so untenanted ingress is
// refused (ADR 0012 §Decision 2); OSS never calls it, leaving it false, so an
// empty header resolves to identity.DefaultTenant and the connection is
// accepted — OSS single-tenant behavior stays inert.
var rejectUntenantedConnections bool

// SetRejectUntenantedConnections toggles rejection of OpAMP connections that
// declare no tenant via the x-squadron-tenant header. The enterprise wire sets
// it true at startup (mirroring sqlite.SetStrictTenantScoping) so untenanted
// telemetry ingress is rejected with 401; OSS never calls it, leaving it false
// → empty header falls back to identity.DefaultTenant and the connection is
// accepted (legacy single-tenant behavior).
func SetRejectUntenantedConnections(v bool) { rejectUntenantedConnections = v }

// DefaultOTelConfig provides the default OpenTelemetry Collector configuration
const DefaultOTelConfig = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:

exporters:
  otlp:
    endpoint: localhost:4317
    tls:
      insecure: true

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
`

type Server struct {
	logger           *zap.Logger
	opampServer      server.OpAMPServer
	agents           *Agents
	agentService     services.AgentService
	metrics          *metrics.OpAMPMetrics
	tracer           *Tracer // optional; nil disables OTel connection spans
	otlpGRPCEndpoint string  // OTLP gRPC endpoint to offer to agents
	otlpHTTPEndpoint string  // OTLP HTTP endpoint to offer to agents

	// HA S3b (ADR 0035) — connection registry seam. registry records which
	// instance owns each agent's WebSocket (upsert on connect, delete on clean
	// disconnect); instanceID is this process's stable boot-time id, written as
	// the owner. Both are optional: nil registry (the default, and every test
	// harness) makes the connect/disconnect registry writes no-ops, so
	// single-instance behavior is unchanged. Wired at startup via
	// SetConnectionRegistry.
	registry   connectionRegistry
	instanceID string

	// audit is the optional audit seam for adopt-on-first-supervise (ADR 0039):
	// when the connect path seeds an agent's initial config from its reported
	// effective config, it records an agent.config_adopted_on_supervise event
	// here. nil (the default, and every test harness that doesn't set it) makes
	// the audit write a no-op, so pre-0039 behavior is unchanged. Wired at
	// startup via SetAuditRecorder.
	audit auditRecorder
}

// SetConnectionRegistry wires the HA S3b connection-registry seam (ADR 0035):
// on connect this instance upserts ownership of the agent, on clean disconnect
// it deletes the row. instanceID is this process's stable boot-time id, written
// as the owner. Called once at startup (main.go) with the application store,
// which satisfies connectionRegistry. Until called, the registry writes are
// no-ops — preserving the pre-S3b single-instance behavior exactly. Not safe
// for concurrent use with a running server; call before Start.
func (s *Server) SetConnectionRegistry(reg connectionRegistry, instanceID string) {
	s.registry = reg
	s.instanceID = instanceID
}

// zapToOpAmpLogger adapts zap.Logger to opamp's logger interface
type zapToOpAmpLogger struct {
	*zap.Logger
}

func (z *zapToOpAmpLogger) Debugf(ctx context.Context, format string, args ...interface{}) {
	z.Sugar().Debugf(format, args...)
}

func (z *zapToOpAmpLogger) Errorf(ctx context.Context, format string, args ...interface{}) {
	z.Sugar().Errorf(format, args...)
}

func NewServer(agents *Agents, agentService services.AgentService, metricsInstance *metrics.OpAMPMetrics, otlpGRPCEndpoint, otlpHTTPEndpoint string, logger *zap.Logger) (*Server, error) {
	return NewServerWithTracer(agents, agentService, metricsInstance, nil, otlpGRPCEndpoint, otlpHTTPEndpoint, logger)
}

// NewServerWithTracer is the production constructor used when
// telemetry.enabled is true. Identical to NewServer except for the
// tracer wiring. Separate constructor keeps existing test callers
// untouched.
func NewServerWithTracer(agents *Agents, agentService services.AgentService, metricsInstance *metrics.OpAMPMetrics, tracer *Tracer, otlpGRPCEndpoint, otlpHTTPEndpoint string, logger *zap.Logger) (*Server, error) {
	s := &Server{
		logger:           logger,
		agents:           agents,
		agentService:     agentService,
		metrics:          metricsInstance,
		tracer:           tracer,
		otlpGRPCEndpoint: otlpGRPCEndpoint,
		otlpHTTPEndpoint: otlpHTTPEndpoint,
	}

	// Create the OpAMP server
	s.opampServer = server.New(&zapToOpAmpLogger{logger})

	return s, nil
}

func (s *Server) Start(port int) error {
	s.logger.Info("Starting OpAMP server...", zap.Int("port", port))

	// Record server start time
	if s.metrics != nil {
		s.metrics.ServerStartTime.Update(time.Now().Unix())
	}

	settings := server.StartSettings{
		Settings: server.Settings{
			Callbacks: server.CallbacksStruct{
				OnConnectingFunc: func(request *http.Request) types.ConnectionResponse {
					// Track connection attempts
					if s.metrics != nil {
						s.metrics.AgentConnectionsTotal.Inc(1)
					}
					// ADR 0012 §Decision 2: resolve the per-connection tenant
					// from the x-squadron-tenant header at connect time — the
					// only point the raw *http.Request is in scope. It's
					// captured in the per-connection callback closures below so
					// onDisconnect (which runs on context.Background() after the
					// wire is gone) can still stamp the connection's tenant
					// without re-deriving it. Empty header resolves to
					// identity.DefaultTenant, keeping OSS single-tenant behavior
					// inert.
					//
					// ADR 0012 §Decision 2: enterprise strict rejects empty
					// x-squadron-tenant — flipped on via
					// SetRejectUntenantedConnections at the enterprise wire. OSS
					// leaves the flag false and does NOT reject here.
					if rejectUntenantedConnections && rawConnTenant(request) == "" {
						if s.metrics != nil {
							s.metrics.MessageErrors.Inc(1)
						}
						s.logger.Warn("rejecting untenanted OpAMP connection: no x-squadron-tenant header (strict tenant scoping)")
						return types.ConnectionResponse{
							Accept:         false,
							HTTPStatusCode: http.StatusUnauthorized,
						}
					}
					connTenant := resolveConnTenant(request)
					return types.ConnectionResponse{
						Accept: true,
						ConnectionCallbacks: server.ConnectionCallbacksStruct{
							OnMessageFunc: func(ctx context.Context, conn types.Connection, msg *protobufs.AgentToServer) *protobufs.ServerToAgent {
								return s.onMessage(identity.WithTenant(ctx, connTenant), conn, msg)
							},
							OnConnectionCloseFunc: func(conn types.Connection) {
								s.onDisconnect(conn, connTenant)
							},
						},
					}
				},
			},
		},
		ListenEndpoint: fmt.Sprintf(":%d", port),
	}

	if err := s.opampServer.Start(settings); err != nil {
		return fmt.Errorf("failed to start OpAMP server: %w", err)
	}

	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("Stopping OpAMP server...")
	_ = s.opampServer.Stop(ctx)
	// Flush any in-flight agent connection spans. The OpAMP layer
	// just closes connections without firing onDisconnect for each
	// agent on shutdown, so we flush explicitly to avoid silently
	// dropping the spans.
	s.tracer.Shutdown()
	return nil
}

// resolveConnTenant reads the x-squadron-tenant header off the raw connect-time
// request and resolves it to a tenant id. Empty (or a nil request, as in tests)
// resolves to identity.DefaultTenant, so OSS single-tenant behavior is inert.
// ADR 0012 §Decision 2.
func resolveConnTenant(request *http.Request) string {
	if request == nil {
		return identity.DefaultTenant
	}
	if t := request.Header.Get(tenantHeader); t != "" {
		return t
	}
	return identity.DefaultTenant
}

// rawConnTenant returns the raw x-squadron-tenant header value (empty when
// absent or when the request is nil), WITHOUT the DefaultTenant fallback. It is
// the reject-decision input: SetRejectUntenantedConnections uses it to
// distinguish "no tenant declared" (empty → reject under strict) from a
// declared tenant. Kept separate from resolveConnTenant so the accept path
// still gets the DefaultTenant fallback in OSS. ADR 0012 §Decision 2.
func rawConnTenant(request *http.Request) string {
	if request == nil {
		return ""
	}
	return request.Header.Get(tenantHeader)
}

func (s *Server) onDisconnect(conn types.Connection, connTenant string) {
	// Track disconnections
	if s.metrics != nil {
		s.metrics.AgentDisconnectsTotal.Inc(1)
	}

	// Get agents before removing connection
	s.agents.mux.Lock()
	agentsToMarkOffline := s.agents.connections[conn]
	s.agents.mux.Unlock()

	// Mark all agents on this connection as offline in storage AND
	// close their tracer spans with the clean "client_disconnected"
	// reason. Both happen in the same loop so connection-close
	// observability stays in sync.
	if s.agentService != nil {
		// ADR 0012 §Decision 2: onDisconnect runs on context.Background()
		// after the wire is gone, so the tenant can't be re-derived from a
		// request. Stamp the per-connection tenant captured at connect time so
		// the offline UpdateAgentStatus write lands in the connection's tenant.
		ctx := identity.WithTenant(context.Background(), connTenant)
		for agentId := range agentsToMarkOffline {
			// agentsToMarkOffline is keyed by wire instance_uid; the store row is
			// keyed by fleet id. Resolve the agent to mark the correct row offline.
			// The tracer stays keyed by instance_uid (wire-level spans).
			fleetId := agentId
			if a := s.agents.FindAgent(agentId); a != nil {
				fleetId = a.storeID()
			}
			if err := s.agentService.UpdateAgentStatus(ctx, fleetId, services.AgentStatusOffline); err != nil {
				s.logger.Error("Failed to mark agent offline on disconnect",
					zap.String("agentId", fleetId.String()),
					zap.Error(err))
			}
			// HA S3b (ADR 0035): clean disconnect — this instance relinquishes
			// ownership of the agent, so drop its registry row. Whichever
			// instance the agent's socket reconnects to re-owns it via upsert.
			s.removeConnectionOwner(fleetId)
			s.tracer.EndAgentConnection(agentId, "client_disconnected")
		}
	} else {
		// AgentService not wired (test harness path); still close
		// the trace spans so they don't leak into Shutdown.
		for agentId := range agentsToMarkOffline {
			s.tracer.EndAgentConnection(agentId, "client_disconnected")
		}
	}

	s.agents.RemoveConnection(conn)

	// Update current connections gauge
	if s.metrics != nil {
		s.metrics.AgentConnections.Update(int64(len(s.agents.GetAllAgentsReadonlyClone())))
	}
}

// recordConnectionOwner records (upserts) this instance's ownership of the
// agent identified by fleetID in the HA S3b connection registry (ADR 0035). A
// nil registry (unwired, and every test harness) is a no-op, so single-instance
// behavior is unchanged. SYSTEM-SCOPED: the registry is instance-identity,
// orthogonal to tenant, so the write is stamped WithSystemContext regardless of
// the message's per-connection tenant. Best-effort: a registry write failure is
// logged, never fatal — the registry is ownership/coverage visibility, NOT on
// the S3a delivery correctness path.
func (s *Server) recordConnectionOwner(ctx context.Context, fleetID uuid.UUID) {
	if s.registry == nil {
		return
	}
	sysCtx := identity.WithSystemContext(ctx)
	if err := s.registry.UpsertConnectionOwner(sysCtx, fleetID, s.instanceID, time.Now()); err != nil {
		s.logger.Debug("connection registry upsert failed",
			zap.String("agentId", fleetID.String()),
			zap.Error(err))
	}
}

// removeConnectionOwner deletes the agent's ownership row on a clean disconnect
// (HA S3b, ADR 0035). Nil registry is a no-op; SYSTEM-SCOPED; best-effort.
func (s *Server) removeConnectionOwner(fleetID uuid.UUID) {
	if s.registry == nil {
		return
	}
	sysCtx := identity.WithSystemContext(context.Background())
	if err := s.registry.DeleteConnectionOwner(sysCtx, fleetID); err != nil {
		s.logger.Debug("connection registry delete failed",
			zap.String("agentId", fleetID.String()),
			zap.Error(err))
	}
}

func (s *Server) onMessage(ctx context.Context, conn types.Connection, msg *protobufs.AgentToServer) *protobufs.ServerToAgent {
	start := time.Now()
	response := &protobufs.ServerToAgent{}
	instanceId := uuid.UUID(msg.InstanceUid)

	// Track message received
	if s.metrics != nil {
		s.metrics.MessagesReceived.Inc(1)
	}

	// Process the message
	agent := s.agents.FindOrCreateAgent(instanceId, conn)
	if agent == nil {
		if s.metrics != nil {
			s.metrics.MessageErrors.Inc(1)
		}
		return response
	}

	// Resolve the Squadron fleet id from the AgentDescription the same way the
	// OTLP ingest path derives it (agentid.Derive), so a host that is both
	// OpAMP-managed and shipping OTLP telemetry converges to ONE fleet row
	// instead of two. Only recompute when a description is present; heartbeats
	// keep the current fleet id. Falls back to instance_uid when the agent
	// reports no usable identity (no regression vs. prior behavior).
	if msg.AgentDescription != nil {
		s.agents.SetFleetId(agent, s.deriveFleetId(instanceId, msg.AgentDescription))
		// HA S3b (ADR 0035): this instance owns the agent's WebSocket. Record
		// ownership keyed by the resolved fleet id. Gated on AgentDescription
		// (present on the OpAMP connect message and on description changes, not
		// on plain heartbeats) so the write stays off the per-message hot path;
		// the reconcile loop's heartbeat refreshes last_heartbeat_at thereafter.
		s.recordConnectionOwner(ctx, agent.storeID())
	}
	// Open the per-agent connection span on the first message we
	// see from this instance. Idempotent on subsequent messages so
	// the span lives for the full connection lifetime.
	s.tracer.BeginAgentConnection(ctx, instanceId)

	// Update connections gauge
	if s.metrics != nil {
		s.metrics.AgentConnections.Update(int64(len(s.agents.GetAllAgentsReadonlyClone())))
	}

	// Track status update if present
	if msg.AgentDescription != nil || msg.RemoteConfigStatus != nil {
		if s.metrics != nil {
			s.metrics.StatusUpdateReceived.Inc(1)
		}
	}
	// Attach the agent's reported version to the connection span as
	// soon as it shows up in an AgentDescription. The tracer ignores
	// empty strings + unknown agent IDs, so this is safe to call
	// unconditionally on every message that carries a description.
	if msg.AgentDescription != nil {
		if v := s.extractAgentVersion(msg.AgentDescription); v != "" {
			s.tracer.RecordAgentVersion(instanceId, v)
		}
	}

	// Track health report if present
	if msg.Health != nil {
		if s.metrics != nil {
			s.metrics.HealthReportReceived.Inc(1)
		}
	}

	// Process agent grouping if agent description changed
	s.processAgentGrouping(ctx, agent, msg)

	agent.UpdateStatus(msg, response)

	// Offer connection settings for own telemetry if agent supports it
	s.calcConnectionSettings(agent, response)

	// Persist agent to storage
	if s.agentService != nil {
		s.persistAgent(ctx, agent, msg)
	}

	// Track message sent
	if s.metrics != nil {
		s.metrics.MessagesSent.Inc(1)
		s.metrics.MessageProcessDuration.Record(time.Since(start))
	}

	return response
}

func (s *Server) GetEffectiveConfig(agentId uuid.UUID) (string, error) {
	agent := s.agents.FindAgent(agentId)
	if agent != nil {
		// Locked read: the connection goroutine may be updating EffectiveConfig
		// while this API-goroutine call reads it.
		return agent.EffectiveConfigSnapshot(), nil
	}
	return "", fmt.Errorf("agent %s not found", agentId)
}

func (s *Server) UpdateConfig(agentId uuid.UUID, config map[string]interface{}, notifyNextStatusUpdate chan<- struct{}) error {
	agent := s.agents.FindAgent(agentId)
	if agent == nil {
		return fmt.Errorf("agent %s not found", agentId)
	}

	// Convert config to YAML or JSON string
	// For now, we'll use a simple string representation
	// In a real implementation, you'd marshal this to YAML
	configStr := DefaultOTelConfig

	configMap := &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigFile{
			"": {Body: []byte(configStr)},
		},
	}

	s.agents.SetCustomConfigForAgent(agentId, configMap, notifyNextStatusUpdate)
	return nil
}

// GetAgent returns an agent by ID (for API handler access)
func (s *Server) GetAgent(agentId uuid.UUID) (*Agent, error) {
	agent := s.agents.FindAgent(agentId)
	if agent == nil {
		return nil, fmt.Errorf("agent not found")
	}
	return agent, nil
}

func (s *Server) ListAgents() map[uuid.UUID]*Agent {
	return s.agents.GetAllAgentsReadonlyClone()
}

// RestartAgent sends a restart command to the specified agent
func (s *Server) RestartAgent(agentId uuid.UUID) error {
	agent := s.agents.FindAgent(agentId)
	if agent == nil {
		return fmt.Errorf("agent not found")
	}

	// Check if agent has capability to accept restart command. Locked accessor:
	// this API-goroutine call can race the agent's connection goroutine.
	if !agent.HasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand) {
		return fmt.Errorf("agent does not support restart command")
	}

	agent.SendRestartCommand()
	s.logger.Info("Restart command sent to agent", zap.String("agentId", agentId.String()))
	return nil
}

// processAgentGrouping handles group resolution for agents
// In OSS version, this is simplified - no backend API calls
func (s *Server) processAgentGrouping(ctx context.Context, agent *Agent, msg *protobufs.AgentToServer) {
	// Only process if agent description is provided (indicates change or first connect)
	if msg.AgentDescription == nil {
		return
	}

	// Extract group information from agent description attributes
	groupID, groupName := s.extractGroupInfo(msg.AgentDescription)

	// Check if group information has changed
	groupChanged := false
	if agent.GroupID == nil && groupID != "" {
		groupChanged = true
	} else if agent.GroupID != nil && groupID != *agent.GroupID {
		groupChanged = true
	} else if agent.GroupName != nil && *agent.GroupName != groupName {
		groupChanged = true
	}

	// Update agent's group information
	agent.mux.Lock()
	previousGroupID := agent.GroupID
	agent.GroupID = &groupID
	agent.GroupName = &groupName
	agent.mux.Unlock()

	// Log group membership changes
	if previousGroupID == nil && groupID != "" {
		s.logger.Info("Agent joined group",
			zap.String("agentId", agent.InstanceIdStr),
			zap.String("groupId", groupID),
			zap.String("groupName", groupName))
	} else if previousGroupID != nil && groupID == "" {
		s.logger.Info("Agent left group",
			zap.String("agentId", agent.InstanceIdStr),
			zap.String("previousGroupId", *previousGroupID))
	} else if previousGroupID != nil && groupID != "" && *previousGroupID != groupID {
		s.logger.Info("Agent changed groups",
			zap.String("agentId", agent.InstanceIdStr),
			zap.String("previousGroupId", *previousGroupID),
			zap.String("newGroupId", groupID),
			zap.String("groupName", groupName))
	}

	// Set initial config based on group membership (or default)
	// Apply config on first connect OR when group changes
	isFirstConnect := agent.Status == nil || agent.CustomInstanceConfig == ""

	if groupChanged || isFirstConnect {
		// Check if agent accepts remote config
		if agent.hasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) {
			config := s.getConfigForAgent(ctx, agent)
			if config != "" {
				agent.mux.Lock()
				agent.CustomInstanceConfig = config
				configChanged := agent.calcRemoteConfig()
				agent.mux.Unlock()

				s.logger.Info("Set initial config for agent",
					zap.String("agentId", agent.InstanceIdStr),
					zap.String("groupId", groupID),
					zap.Bool("firstConnect", isFirstConnect),
					zap.Bool("groupChanged", groupChanged),
					zap.Bool("configChanged", configChanged))
			}
		}
	}
}

// extractGroupInfo extracts group ID and name from agent description
func (s *Server) extractGroupInfo(desc *protobufs.AgentDescription) (groupID string, groupName string) {
	if desc == nil {
		return "", ""
	}

	// Look for group information in identifying or non-identifying attributes
	attrs := append(desc.IdentifyingAttributes, desc.NonIdentifyingAttributes...)
	for _, attr := range attrs {
		if attr.Key == "group.id" || attr.Key == "service.group.id" || attr.Key == "agent.group_id" {
			if attr.Value != nil && attr.Value.GetStringValue() != "" {
				groupID = attr.Value.GetStringValue()
			}
		}
		if attr.Key == "group.name" || attr.Key == "service.group.name" || attr.Key == "agent.group_name" {
			if attr.Value != nil && attr.Value.GetStringValue() != "" {
				groupName = attr.Value.GetStringValue()
			}
		}
	}

	return groupID, groupName
}

// getConfigForAgent returns the configuration for an agent
// Priority: Agent-specific config > Group config > Default config
//
// The agent > group store precedence lives in the shared resolveStoredConfig
// resolver so the connect path here and the per-instance reconcile loop
// (internal/opamp/reconciler.go) cannot drift on what "desired config" means.
// This path layers the synthesized DefaultOTelConfig on when no stored config
// exists; the reconcile loop deliberately does not (it never synthesizes).
func (s *Server) getConfigForAgent(ctx context.Context, agent *Agent) string {
	if content, found := resolveStoredConfig(ctx, s.agentService, agent); found {
		return content
	}

	// No stored agent/group config. Before falling back to the synthesized
	// skeleton, try adopt-on-first-supervise (ADR 0039): if this is a supervised
	// brownfield agent already reporting an effective config, seed + assign its
	// initial managed config FROM that reported effective config (opamp extension
	// stripped, ${ENV} preserved) and push that instead of the skeleton — which
	// would otherwise clobber the agent's wired pipelines. Returns false for a
	// fresh agent with no usable reported config (or on any validation/store
	// error), leaving the skeleton fallback below intact.
	if content, ok := s.tryAdoptEffectiveConfig(ctx, agent); ok {
		return content
	}

	// Fall back to default config when the agent has no stored agent/group config.
	s.logger.Debug("Using default config for agent",
		zap.String("agentId", agent.InstanceIdStr))
	return DefaultOTelConfig
}

// isEmptyGroup reports whether a group pointer carries no usable group
// membership — either nil or a pointer to the empty string. extractGroupInfo
// resolves an agent that reports no group.* attribute to an empty (NOT nil)
// GroupID, so both cases must count as "no group reported" when deciding
// whether a (re)connect should preserve the persisted membership.
func isEmptyGroup(g *string) bool {
	return g == nil || *g == ""
}

// persistAgent persists agent information to storage
func (s *Server) persistAgent(ctx context.Context, agent *Agent, msg *protobufs.AgentToServer) {
	// Check if agent already exists in storage. Keyed by the FLEET id (not the
	// wire instance_uid) so this row converges with the OTLP discovery row and
	// the telemetry agent_id for the same host — one card, config + telemetry.
	existingAgent, err := s.agentService.GetAgent(ctx, agent.storeID())
	if err != nil {
		s.logger.Debug("Error checking existing agent",
			zap.String("agentId", agent.InstanceIdStr),
			zap.Error(err))
	}

	now := time.Now()

	// Extract agent details
	name := s.extractAgentName(msg.AgentDescription)
	labels := s.extractAgentLabels(msg.AgentDescription)
	version := s.extractAgentVersion(msg.AgentDescription)
	capabilities := s.extractAgentCapabilities(msg.Capabilities)
	status := s.determineAgentStatus(msg)

	if existingAgent == nil {
		// Resolve (and auto-create) the agent's group, setting
		// agent.GroupID in memory.
		s.ensureAgentGroup(ctx, agent, now)

		// Create new agent
		serviceAgent := &services.Agent{
			ID:           agent.storeID(),
			Name:         name,
			Labels:       labels,
			Status:       services.AgentStatus(status),
			LastSeen:     now,
			GroupID:      agent.GroupID,
			GroupName:    agent.GroupName,
			Version:      version,
			Capabilities: capabilities,
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		if err := s.agentService.CreateAgent(ctx, serviceAgent); err != nil {
			s.logger.Error("Failed to create agent in storage",
				zap.String("agentId", agent.InstanceIdStr),
				zap.Error(err))
		} else {
			s.logger.Info("Agent persisted to storage",
				zap.String("agentId", agent.InstanceIdStr),
				zap.String("name", name))
		}
	} else {
		// Update existing agent
		if err := s.agentService.UpdateAgentStatus(ctx, agent.storeID(), services.AgentStatus(status)); err != nil {
			s.logger.Error("Failed to update agent status",
				zap.String("agentId", agent.InstanceIdStr),
				zap.Error(err))
		}

		if err := s.agentService.UpdateAgentLastSeen(ctx, agent.storeID(), now); err != nil {
			s.logger.Error("Failed to update agent last seen",
				zap.String("agentId", agent.InstanceIdStr),
				zap.Error(err))
		}

		// Persist registration/grouping changes — but ONLY when this
		// message carried an AgentDescription. A description-less
		// heartbeat extracts to name="unknown", labels={}, version=""
		// (see extractAgent* helpers), which would clobber the stored
		// identity and, critically, the GroupID that rollout canary
		// scoping reads back. The same nil-guard gates processAgentGrouping.
		if msg.AgentDescription != nil {
			// Resolve (and auto-create) the group so a description that
			// names a new group lands the agent in it on the existing
			// path too, not just first connect.
			s.ensureAgentGroup(ctx, agent, now)

			// Preserve a previously-persisted group membership when THIS
			// (re)connect reports no group. extractGroupInfo yields an EMPTY
			// (not nil) GroupID for an agent that carries no group.* attribute,
			// which — before this guard — flowed straight into
			// UpdateAgentRegistration and wiped the stored group_id. That is the
			// data-durability bug: a manual assignment made via
			// PATCH /agents/:id/group (or any earlier agent-reported grouping)
			// vanished on the next reconnect, and a control-plane restart —
			// which reconnects every agent — wiped every manual membership at
			// once (rollout canary scoping reads that stored group_id back).
			//
			// When the agent DOES report a group, ensureAgentGroup has already
			// set agent.GroupID and the agent-declared group wins here
			// (unchanged): an agent that reports a DIFFERENT group than the
			// manual one still overrides it on reconnect. Only an EMPTY report
			// is treated as "leave the persisted membership alone". The operator
			// PATCH handler is a separate authoritative writer (explicit
			// set/clear) and is unaffected by this guard.
			groupID := agent.GroupID
			groupName := agent.GroupName
			if isEmptyGroup(groupID) && existingAgent != nil && !isEmptyGroup(existingAgent.GroupID) {
				groupID = existingAgent.GroupID
				groupName = existingAgent.GroupName
				// Keep the in-memory registry consistent with the store so the
				// per-instance reconciler and connect-path config resolver —
				// both of which read agent.GroupID — resolve this agent's group
				// config on subsequent ticks rather than treating it as ungrouped.
				agent.mux.Lock()
				agent.GroupID = groupID
				agent.GroupName = groupName
				agent.mux.Unlock()
			}

			registration := &services.Agent{
				ID:        agent.storeID(),
				Name:      name,
				Labels:    labels,
				Version:   version,
				GroupID:   groupID,
				GroupName: groupName,
				// Persist the capabilities reported on this (re)connect. Before
				// this, UpdateAgentRegistration never re-wrote capabilities, so an
				// agent whose row first registered via telemetry discovery kept an
				// empty capabilities column forever (the drift "2nd miss" root
				// cause, PR #35). The store PRESERVES the existing set when this is
				// empty, so a description-carrying-but-capability-less report can't
				// wipe it.
				Capabilities: capabilities,
			}
			if err := s.agentService.UpdateAgentRegistration(ctx, registration); err != nil {
				s.logger.Error("Failed to update agent registration",
					zap.String("agentId", agent.InstanceIdStr),
					zap.Error(err))
			}
		}

		// Update effective config if present
		if agent.EffectiveConfig != "" {
			if err := s.agentService.UpdateAgentEffectiveConfig(ctx, agent.storeID(), agent.EffectiveConfig); err != nil {
				s.logger.Error("Failed to update agent effective config",
					zap.String("agentId", agent.InstanceIdStr),
					zap.Error(err))
			}
		}

		// Persist the DELIVERED/APPLIED signal (ADR 0040 + follow-up): when the
		// agent has acked the remote config Squadron assigned it, stamp the
		// confignorm hash of that config so drift detection can tell a supervised
		// agent running exactly what Squadron assigned (Synced) apart from its
		// env-/default-expanded, opamp-extension-carrying effective config (which
		// never hash-matches the compact intent). Two equivalent sources, tried in
		// order:
		//   1. appliedConfigHash — the in-memory staged-remoteConfig ack. Fast
		//      path; fires on a fresh push+ack while remoteConfig is staged.
		//   2. appliedConfigHashFromDesired — the store-derived reconciler
		//      equivalence (LastRemoteConfigHash == wireConfigHash(desired stored
		//      config)). Fires for an already-applied, already-converged supervised
		//      agent that merely heartbeats after a control-plane upgrade, when no
		//      remoteConfig is staged in memory — the case appliedConfigHash misses
		//      and the reconciler does not (ADR 0040 follow-up).
		// Only fires when the agent has confirmed applying the CURRENT desired
		// config; report-only agents and agents mid-delivery leave the stored hash
		// untouched.
		hash, ok := agent.appliedConfigHash()
		if !ok {
			hash, ok = appliedConfigHashFromDesired(ctx, s.agentService, agent)
		}
		if ok {
			if err := s.agentService.UpdateAgentDeliveredConfigHash(ctx, agent.storeID(), hash); err != nil {
				s.logger.Error("Failed to update agent delivered config hash",
					zap.String("agentId", agent.InstanceIdStr),
					zap.Error(err))
			}
		}
	}
}

// ensureAgentGroup resolves the agent's group from its in-memory
// GroupName (set by processAgentGrouping from the AgentDescription),
// auto-creating the group if it doesn't yet exist, and writes the
// resolved GroupID back onto the in-memory agent. No-op when the agent
// carries no group name. Safe to call from both the create and update
// paths in persistAgent.
func (s *Server) ensureAgentGroup(ctx context.Context, agent *Agent, now time.Time) {
	if agent.GroupName == nil || *agent.GroupName == "" {
		return
	}

	existingGroup, err := s.agentService.GetGroupByName(ctx, *agent.GroupName)
	if err != nil {
		s.logger.Debug("Error checking existing group",
			zap.String("groupName", *agent.GroupName),
			zap.Error(err))
	}

	if existingGroup == nil {
		// Group doesn't exist, create it
		newGroup := &services.Group{
			ID:        uuid.New().String(),
			Name:      *agent.GroupName,
			Labels:    make(map[string]string),
			CreatedAt: now,
			UpdatedAt: now,
		}

		if err := s.agentService.CreateGroup(ctx, newGroup); err != nil {
			s.logger.Error("Failed to auto-create group",
				zap.String("groupName", *agent.GroupName),
				zap.Error(err))
		} else {
			s.logger.Info("Auto-created group for agent",
				zap.String("groupName", *agent.GroupName),
				zap.String("groupId", newGroup.ID))
			agent.GroupID = &newGroup.ID
		}
	} else {
		// Group exists, set GroupID
		agent.GroupID = &existingGroup.ID
	}
}

// deriveFleetId computes the Squadron fleet id for an OpAMP agent from its
// AgentDescription, mirroring the OTLP ingest path (agentid.Derive) so both
// converge on the same identity. If the description carries no usable identity
// (agentid returns the "default" sentinel) or the derived value isn't a valid
// UUID, it falls back to the wire instance_uid — preserving today's behavior
// for agents that report nothing correlatable.
func (s *Server) deriveFleetId(instanceId uuid.UUID, desc *protobufs.AgentDescription) uuid.UUID {
	if desc == nil {
		return instanceId
	}
	derived := agentid.Derive(fleetIdentityAttrs(desc))
	if derived == "default" {
		return instanceId
	}
	parsed, err := uuid.Parse(derived)
	if err != nil {
		return instanceId
	}
	return parsed
}

// fleetIdentityAttrs pulls the identity-bearing attributes agentid.Derive keys
// off (service.instance.id, host.name, service.name) out of the description's
// identifying + non-identifying attributes.
func fleetIdentityAttrs(desc *protobufs.AgentDescription) map[string]string {
	attrs := make(map[string]string, 3)
	all := append(desc.IdentifyingAttributes, desc.NonIdentifyingAttributes...)
	for _, attr := range all {
		if attr.Value == nil {
			continue
		}
		switch attr.Key {
		case "service.instance.id", "host.name", "service.name":
			if v := attr.Value.GetStringValue(); v != "" {
				// Identifying attributes win; don't let a non-identifying dup
				// clobber a value we already captured.
				if _, seen := attrs[attr.Key]; !seen {
					attrs[attr.Key] = v
				}
			}
		}
	}
	return attrs
}

// extractAgentName extracts the agent name from agent description
func (s *Server) extractAgentName(desc *protobufs.AgentDescription) string {
	if desc == nil {
		return "unknown"
	}

	attrs := append(desc.IdentifyingAttributes, desc.NonIdentifyingAttributes...)
	get := func(key string) string {
		for _, attr := range attrs {
			if attr.Key == key && attr.Value != nil {
				if v := attr.Value.GetStringValue(); v != "" {
					return v
				}
			}
		}
		return ""
	}

	// Precedence mirrors the OTLP discovery path (internal/discovery/service.go):
	// prefer a per-host identifier over service.name, which defaults to the
	// collector binary name ("otelcol-contrib") and makes every agent in the
	// fleet look identical. agent.name is an explicit Squadron override and wins.
	if v := get("agent.name"); v != "" {
		return v
	}
	if v := get("host.name"); v != "" {
		return v
	}
	if v := get("service.name"); v != "" {
		return v
	}
	return "unknown"
}

// extractAgentLabels extracts labels from agent description
func (s *Server) extractAgentLabels(desc *protobufs.AgentDescription) map[string]string {
	labels := make(map[string]string)

	if desc == nil {
		return labels
	}

	// Extract all non-identifying attributes as labels
	for _, attr := range desc.NonIdentifyingAttributes {
		if attr.Value != nil {
			labels[attr.Key] = attr.Value.GetStringValue()
		}
	}

	return labels
}

// extractAgentVersion extracts version from agent description
func (s *Server) extractAgentVersion(desc *protobufs.AgentDescription) string {
	if desc == nil {
		return "unknown"
	}

	// Look for service.version or agent.version
	attrs := append(desc.IdentifyingAttributes, desc.NonIdentifyingAttributes...)
	for _, attr := range attrs {
		if attr.Key == "service.version" || attr.Key == "agent.version" {
			if attr.Value != nil && attr.Value.GetStringValue() != "" {
				return attr.Value.GetStringValue()
			}
		}
	}

	return "unknown"
}

// extractAgentCapabilities extracts capabilities from OpAMP message
func (s *Server) extractAgentCapabilities(caps uint64) []string {
	capabilities := []string{}

	// Map OpAMP capabilities to strings
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus) != 0 {
		capabilities = append(capabilities, "reports_status")
	}
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) != 0 {
		capabilities = append(capabilities, "accepts_remote_config")
	}
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig) != 0 {
		capabilities = append(capabilities, "reports_effective_config")
	}
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnTraces) != 0 {
		capabilities = append(capabilities, "reports_own_traces")
	}
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnMetrics) != 0 {
		capabilities = append(capabilities, "reports_own_metrics")
	}
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsOwnLogs) != 0 {
		capabilities = append(capabilities, "reports_own_logs")
	}
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsPackages) != 0 {
		capabilities = append(capabilities, "accepts_packages")
	}
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsPackageStatuses) != 0 {
		capabilities = append(capabilities, "reports_package_statuses")
	}
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth) != 0 {
		capabilities = append(capabilities, "reports_health")
	}
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig) != 0 {
		capabilities = append(capabilities, "reports_remote_config")
	}
	if caps&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand) != 0 {
		capabilities = append(capabilities, "accepts_restart_command")
	}

	return capabilities
}

// determineAgentStatus determines agent status from OpAMP message
func (s *Server) determineAgentStatus(msg *protobufs.AgentToServer) services.AgentStatus {
	// If we're receiving a message, the agent is connected
	// Check health status if provided
	if msg.Health != nil {
		if msg.Health.Healthy {
			return services.AgentStatusOnline
		}
		return services.AgentStatusError
	}

	// No health info means agent is connected but not reporting health
	// This is normal for initial connections, so mark as online
	return services.AgentStatusOnline
}

// getOTLPEndpointForAgent determines the appropriate OTLP endpoint to offer to the agent
// If the endpoint is bound to 0.0.0.0, convert it to localhost for agents on the same host
// This automatic conversion only happens if no explicit agent endpoint was configured
func (s *Server) getOTLPEndpointForAgent(endpoint string) string {
	// Only convert 0.0.0.0 to localhost if endpoint starts with 0.0.0.0
	// Otherwise, use the endpoint as-is (for docker service names, IPs, etc.)
	if len(endpoint) >= 7 && endpoint[:7] == "0.0.0.0" {
		return "localhost" + endpoint[7:]
	}
	return endpoint
}

// calcConnectionSettings calculates connection settings for the agent
// Offers OTLP endpoints for agents to send their own telemetry if they support it
func (s *Server) calcConnectionSettings(agent *Agent, response *protobufs.ServerToAgent) {
	// Check if agent has capability to report own telemetry
	hasMetrics, hasTraces, hasLogs := agent.shouldOfferOwnTelemetry()

	// If agent doesn't support any own telemetry, no need to offer anything
	if !hasMetrics && !hasTraces && !hasLogs {
		return
	}

	// Prefer HTTP endpoint if configured, as supervisor defaults to HTTP/Protobuf for own telemetry
	// Fall back to gRPC endpoint if HTTP not configured
	var baseEndpoint string
	if s.otlpHTTPEndpoint != "" {
		baseEndpoint = s.getOTLPEndpointForAgent(s.otlpHTTPEndpoint)
	} else {
		baseEndpoint = s.getOTLPEndpointForAgent(s.otlpGRPCEndpoint)
	}

	// Build full URLs with protocol and paths for OTLP HTTP
	metricsURL := "http://" + baseEndpoint + "/v1/metrics"
	tracesURL := "http://" + baseEndpoint + "/v1/traces"
	logsURL := "http://" + baseEndpoint + "/v1/logs"

	s.logger.Debug("Offering own telemetry connection settings to agent",
		zap.String("agentId", agent.InstanceIdStr),
		zap.Bool("metrics", hasMetrics),
		zap.Bool("traces", hasTraces),
		zap.Bool("logs", hasLogs),
		zap.String("baseEndpoint", baseEndpoint),
		zap.String("metricsURL", metricsURL))

	// Initialize connection settings if not present
	if response.ConnectionSettings == nil {
		response.ConnectionSettings = &protobufs.ConnectionSettingsOffers{}
	}

	// Create headers with the agent's Squadron identity for filtering. Use the
	// fleet id so any self-telemetry tagged with this header correlates to the
	// same fleet row as the agent's config and host telemetry.
	headers := &protobufs.Headers{
		Headers: []*protobufs.Header{
			{
				Key:   "x-squadron-agent-id",
				Value: agent.FleetIdStr,
			},
		},
	}

	// Offer metrics endpoint if agent supports it
	if hasMetrics {
		response.ConnectionSettings.OwnMetrics = &protobufs.TelemetryConnectionSettings{
			DestinationEndpoint: metricsURL,
			Headers:             headers,
		}
	}

	// Offer traces endpoint if agent supports it
	if hasTraces {
		response.ConnectionSettings.OwnTraces = &protobufs.TelemetryConnectionSettings{
			DestinationEndpoint: tracesURL,
			Headers:             headers,
		}
	}

	// Offer logs endpoint if agent supports it
	if hasLogs {
		response.ConnectionSettings.OwnLogs = &protobufs.TelemetryConnectionSettings{
			DestinationEndpoint: logsURL,
			Headers:             headers,
		}
	}
}
