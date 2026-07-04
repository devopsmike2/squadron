package opamp

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
)

// Agent represents a connected Agent.
type Agent struct {
	// Some fields in this struct are exported so that we can render them in the UI.

	// Agent's instance id. This is an immutable field. It is the raw OpAMP
	// instance_uid off the wire (msg.InstanceUid) and is what the OpAMP
	// subsystem uses to route messages, spans, and connection settings.
	InstanceId    uuid.UUID
	InstanceIdStr string

	// Agent's Squadron fleet id — the identity under which this agent is
	// persisted in the store and correlated with OTLP telemetry. Derived from
	// the AgentDescription via agentid.Derive so a host that is both
	// OpAMP-managed and shipping OTLP resolves to ONE fleet row. Defaults to
	// InstanceId until a description is seen (and as a no-regression fallback
	// when the description carries no usable identity). Guarded by mux.
	FleetId    uuid.UUID
	FleetIdStr string

	// Group information
	GroupID   *string
	GroupName *string

	// Connection to the Agent.
	conn types.Connection

	// mutex for the fields that follow it.
	mux sync.RWMutex

	// Agent's current status.
	Status *protobufs.AgentToServer

	// The time when the agent has started. Valid only if Status.Health.Up==true
	StartedAt time.Time

	// Effective config reported by the Agent.
	EffectiveConfig string

	// Optional special remote config for this particular instance defined by
	// the user in the UI.
	CustomInstanceConfig string

	// Client certificate
	ClientCert                  *x509.Certificate
	ClientCertSha256Fingerprint string
	ClientCertOfferError        string

	// Remote config that we will give to this Agent.
	remoteConfig *protobufs.AgentRemoteConfig

	// Channels to notify when this Agent's status is updated next time.
	statusUpdateWatchers []chan<- struct{}
}

// NewAgent creates a new Agent instance
func NewAgent(
	instanceId uuid.UUID,
	conn types.Connection,
) *Agent {
	agent := &Agent{
		InstanceId:    instanceId,
		InstanceIdStr: instanceId.String(),
		// Default the fleet id to the wire instance id. Recomputed from the
		// AgentDescription on the first message that carries one (SetFleetId),
		// and kept as-is when no usable identity is reported (no regression).
		FleetId:    instanceId,
		FleetIdStr: instanceId.String(),
		conn:       conn,
	}
	tslConn, ok := conn.Connection().(*tls.Conn)
	if ok {
		// Client is using TLS connection.
		connState := tslConn.ConnectionState()
		if len(connState.PeerCertificates) > 0 {
			// Client uses client-side certificate. Get certificate details to display in the UI.
			leafClientCert := connState.PeerCertificates[0]
			fingerprint := sha256.Sum256(leafClientCert.Raw)
			agent.ClientCert = leafClientCert
			agent.ClientCertSha256Fingerprint = fmt.Sprintf("%X", fingerprint)
		}
	}

	return agent
}

// CloneReadonly returns a copy of the Agent that is safe to read.
// Functions that modify the Agent should not be called on the cloned copy.
func (agent *Agent) CloneReadonly() *Agent {
	agent.mux.RLock()
	defer agent.mux.RUnlock()

	return &Agent{
		InstanceId:                  agent.InstanceId,
		InstanceIdStr:               agent.InstanceIdStr,
		FleetId:                     agent.FleetId,
		FleetIdStr:                  agent.FleetIdStr,
		GroupID:                     agent.GroupID,
		GroupName:                   agent.GroupName,
		conn:                        agent.conn,
		Status:                      agent.Status,
		StartedAt:                   agent.StartedAt,
		EffectiveConfig:             agent.EffectiveConfig,
		CustomInstanceConfig:        agent.CustomInstanceConfig,
		ClientCert:                  agent.ClientCert,
		ClientCertSha256Fingerprint: agent.ClientCertSha256Fingerprint,
		ClientCertOfferError:        agent.ClientCertOfferError,
		remoteConfig:                agent.remoteConfig,
	}
}

// setFleetId updates the agent's fleet id under its own mux. Called only by
// Agents.SetFleetId, which additionally maintains the fleet-id index and
// serializes on the Agents mux, so this stays a small leaf write.
func (agent *Agent) setFleetId(fleetId uuid.UUID) {
	agent.mux.Lock()
	defer agent.mux.Unlock()
	agent.FleetId = fleetId
	agent.FleetIdStr = fleetId.String()
}

// storeID returns the id under which this agent is persisted and correlated —
// the Squadron fleet id — falling back to the wire instance id if the fleet id
// was never set (an agent constructed directly, e.g. in a test, or before any
// AgentDescription arrived). Guards the invariant that a zero UUID is never
// used as a store primary key.
func (agent *Agent) storeID() uuid.UUID {
	if agent.FleetId != (uuid.UUID{}) {
		return agent.FleetId
	}
	return agent.InstanceId
}

// hasCapability checks if the agent has a specific capability.
//
// This is the UNLOCKED form: it reads agent.Status without taking agent.mux and
// therefore assumes the caller already holds the lock (it is called from
// processStatusUpdate while UpdateStatus holds agent.mux.Lock, and from the
// connection-goroutine paths that run inside that same OnMessage call). It must
// NOT take the lock itself — agent.mux is a non-reentrant sync.RWMutex, so an
// RLock here would self-deadlock the status-update path. External goroutines
// (the rollout config-sender, API handlers) must use HasCapability instead.
func (agent *Agent) hasCapability(capability protobufs.AgentCapabilities) bool {
	return agent.Status != nil && agent.Status.Capabilities&uint64(capability) != 0
}

// HasCapability reports whether the agent advertises the given capability. Safe
// to call from any goroutine: it takes the read lock before reading Status, so
// it will not race the OpAMP connection goroutine's UpdateStatus writer (which
// swaps agent.Status under the write lock). Use this from external callers such
// as the rollout config-sender and API handlers; the connection goroutine's own
// under-lock paths use the unexported hasCapability.
func (agent *Agent) HasCapability(capability protobufs.AgentCapabilities) bool {
	agent.mux.RLock()
	defer agent.mux.RUnlock()
	return agent.hasCapability(capability)
}

// EffectiveConfigSnapshot returns the agent's last-reported effective config
// under the read lock, so an API-goroutine read cannot race the connection
// goroutine updating agent.EffectiveConfig in updateEffectiveConfig.
func (agent *Agent) EffectiveConfigSnapshot() string {
	agent.mux.RLock()
	defer agent.mux.RUnlock()
	return agent.EffectiveConfig
}

// GetConnection returns the agent's connection
func (agent *Agent) GetConnection() types.Connection {
	return agent.conn
}

// GetRemoteConfig returns the agent's remote config (for internal use)
func (agent *Agent) GetRemoteConfig() *protobufs.AgentRemoteConfig {
	agent.mux.RLock()
	defer agent.mux.RUnlock()
	return agent.remoteConfig
}

// SetRemoteConfig sets the agent's remote config (for internal use)
func (agent *Agent) SetRemoteConfig(config *protobufs.AgentRemoteConfig) {
	agent.mux.Lock()
	defer agent.mux.Unlock()
	agent.remoteConfig = config
}

// AddStatusUpdateWatcher adds a channel to be notified when the agent's status updates
func (agent *Agent) AddStatusUpdateWatcher(ch chan<- struct{}) {
	agent.mux.Lock()
	defer agent.mux.Unlock()
	agent.statusUpdateWatchers = append(agent.statusUpdateWatchers, ch)
}
