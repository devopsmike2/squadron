package opamp

import (
	"sync"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
	"go.uber.org/zap"
)

type Agents struct {
	mux sync.RWMutex
	// agentsById is keyed by the wire OpAMP instance_uid. It backs the
	// message/connection plumbing (FindOrCreateAgent, disconnect, tracer).
	agentsById map[uuid.UUID]*Agent
	// agentsByFleetId is keyed by the Squadron fleet id (agentid.Derive of the
	// AgentDescription). It backs every store-facing lookup — config push,
	// restart, cert rotation, API GetAgent — because the ids those callers hold
	// are store ids (= fleet ids), which diverge from instance_uid for any
	// agent whose OpAMP instance_uid != its OTLP service.instance.id.
	agentsByFleetId map[uuid.UUID]*Agent
	connections     map[types.Connection]map[uuid.UUID]bool
	logger          *zap.Logger
}

// NewAgents creates a new Agents instance with dependency injection
func NewAgents(logger *zap.Logger) *Agents {
	return &Agents{
		agentsById:      make(map[uuid.UUID]*Agent),
		agentsByFleetId: make(map[uuid.UUID]*Agent),
		connections:     make(map[types.Connection]map[uuid.UUID]bool),
		logger:          logger,
	}
}

// RemoveConnection removes the connection and all Agent instances associated with the
// connection.
func (agents *Agents) RemoveConnection(conn types.Connection) {
	agents.mux.Lock()
	defer agents.mux.Unlock()

	// Get the list of agents to remove
	agentsToRemove := agents.connections[conn]

	// Remove from connections map
	delete(agents.connections, conn)

	// Remove the agents from BOTH indexes. agentsToRemove is keyed by wire
	// instance_uid; resolve each agent to drop its fleet-id entry too so the
	// fleet-id map doesn't leak stale pointers after a disconnect.
	for agentId := range agentsToRemove {
		if a := agents.agentsById[agentId]; a != nil {
			delete(agents.agentsByFleetId, a.FleetId)
		}
		delete(agents.agentsById, agentId)
	}
}

// SetFleetId records the agent's Squadron fleet id and indexes it under that
// id. Idempotent; if the fleet id changed from a previous value (e.g. the first
// description refined it away from the instance_uid default) the stale index
// entry is removed. Serializes on the Agents mux; the per-agent field write is
// delegated to agent.setFleetId (agent mux). No reverse lock ordering exists.
func (agents *Agents) SetFleetId(agent *Agent, fleetId uuid.UUID) {
	agents.mux.Lock()
	defer agents.mux.Unlock()

	prev := agent.FleetId
	if prev == fleetId {
		// Ensure the index is populated even on a no-op (first call where the
		// default already equals the derived id).
		agents.agentsByFleetId[fleetId] = agent
		return
	}
	if prev != (uuid.UUID{}) {
		delete(agents.agentsByFleetId, prev)
	}
	agent.setFleetId(fleetId)
	agents.agentsByFleetId[fleetId] = agent
}

func (agents *Agents) SetCustomConfigForAgent(
	agentId uuid.UUID,
	config *protobufs.AgentConfigMap,
	notifyNextStatusUpdate chan<- struct{},
) {
	agent := agents.FindAgent(agentId)
	if agent != nil {
		agent.SetCustomConfig(config, notifyNextStatusUpdate)
	}
}

// FindAgent resolves an agent by id. Store-facing callers (config push, restart,
// cert rotation, the API layer, the rollout engine) hold a fleet id, so the
// fleet-id index is consulted first. It falls back to the wire instance_uid
// index so intra-subsystem callers that iterate the instance_uid-keyed map
// (e.g. group broadcasts) and agents not yet described (fleet id == instance_uid)
// still resolve — a clean no-regression fallback.
func (agents *Agents) FindAgent(agentId uuid.UUID) *Agent {
	agents.mux.RLock()
	defer agents.mux.RUnlock()
	if a := agents.agentsByFleetId[agentId]; a != nil {
		return a
	}
	return agents.agentsById[agentId]
}

func (agents *Agents) FindOrCreateAgent(agentId uuid.UUID, conn types.Connection) *Agent {
	agents.mux.Lock()
	defer agents.mux.Unlock()

	// Ensure the Agent is in the agentsById map.
	agent := agents.agentsById[agentId]
	if agent == nil {
		agent = NewAgent(agentId, conn)
		agents.agentsById[agentId] = agent

		// Ensure the Agent's instance id is associated with the connection.
		if agents.connections[conn] == nil {
			agents.connections[conn] = map[uuid.UUID]bool{}
		}
		agents.connections[conn][agentId] = true
	}

	return agent
}

func (agents *Agents) GetAgentReadonlyClone(agentId uuid.UUID) *Agent {
	agent := agents.FindAgent(agentId)
	if agent == nil {
		return nil
	}

	// Return a clone to allow safe access after returning.
	return agent.CloneReadonly()
}

func (agents *Agents) GetAllAgentsReadonlyClone() map[uuid.UUID]*Agent {
	agents.mux.RLock()

	// Clone the map first
	m := map[uuid.UUID]*Agent{}
	for id, agent := range agents.agentsById {
		m[id] = agent
	}
	agents.mux.RUnlock()

	// Clone agents in the map
	for id, agent := range m {
		// Return a clone to allow safe access after returning.
		m[id] = agent.CloneReadonly()
	}
	return m
}

func (a *Agents) OfferAgentConnectionSettings(
	id uuid.UUID,
	offers *protobufs.ConnectionSettingsOffers,
) {
	a.logger.Info("Begin rotate client certificate", zap.String("agentId", id.String()))

	a.mux.Lock()
	defer a.mux.Unlock()

	// Cert rotation is triggered from the store-facing side, so the id is a
	// fleet id; fall back to the wire instance_uid index for parity with FindAgent.
	agent := a.agentsByFleetId[id]
	if agent == nil {
		agent = a.agentsById[id]
	}
	if agent != nil {
		agent.OfferConnectionSettings(offers)
		a.logger.Info("Client certificate offers sent", zap.String("agentId", id.String()))
	} else {
		a.logger.Warn("Agent not found", zap.String("agentId", id.String()))
	}
}
