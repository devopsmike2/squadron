package opamp

import (
	"bytes"
	"context"
	"crypto/sha256"

	"github.com/devopsmike2/squadron/internal/confignorm"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// SetCustomConfig sets a custom config for this Agent.
// notifyWhenConfigIsApplied channel is notified after the remote config is applied
// to the Agent and after the Agent reports back the effective config.
// If the provided config is equal to the current remoteConfig of the Agent
// then we will not send any config to the Agent and notifyWhenConfigIsApplied channel
// will be notified immediately. This requires that notifyWhenConfigIsApplied channel
// has a buffer size of at least 1.
func (agent *Agent) SetCustomConfig(
	config *protobufs.AgentConfigMap,
	notifyWhenConfigIsApplied chan<- struct{},
) {
	agent.setCustomConfigWithTraceparent(config, notifyWhenConfigIsApplied, nil)
}

// SetCustomConfigWithContext is the trace-aware variant. When ctx
// carries an active OTel span, the outbound ServerToAgent message
// gets a CustomMessage attached carrying the W3C TraceContext
// headers — see internal/opamp/traceparent.go. Used by the
// ConfigSender path so rollout / direct / drift-remediation pushes
// propagate the originating trace across the agent boundary.
func (agent *Agent) SetCustomConfigWithContext(
	ctx context.Context,
	config *protobufs.AgentConfigMap,
	notifyWhenConfigIsApplied chan<- struct{},
) {
	agent.setCustomConfigWithTraceparent(config, notifyWhenConfigIsApplied, buildTraceparentMessage(ctx))
}

// setCustomConfigWithTraceparent is the shared implementation. The
// optional traceparent argument rides along on the same ServerToAgent
// wire frame as the RemoteConfig so agents that subscribe to the
// custom capability see the trace context with the config they're
// being asked to apply.
func (agent *Agent) setCustomConfigWithTraceparent(
	config *protobufs.AgentConfigMap,
	notifyWhenConfigIsApplied chan<- struct{},
	traceparent *protobufs.CustomMessage,
) {
	agent.mux.Lock()

	agent.CustomInstanceConfig = string(config.ConfigMap[""].Body)

	configChanged := agent.calcRemoteConfig()
	if configChanged {
		if notifyWhenConfigIsApplied != nil {
			// The caller wants to be notified when the Agent reports a status
			// update next time. This is typically used in the UI to wait until
			// the configuration changes are propagated successfully to the Agent.
			agent.statusUpdateWatchers = append(
				agent.statusUpdateWatchers,
				notifyWhenConfigIsApplied,
			)
		}
		msg := &protobufs.ServerToAgent{
			RemoteConfig: agent.remoteConfig,
		}
		if traceparent != nil {
			msg.CustomMessage = traceparent
		}
		agent.mux.Unlock()

		agent.SendToAgent(msg)
	} else {
		agent.mux.Unlock()

		if notifyWhenConfigIsApplied != nil {
			// No config change. We are not going to send config to the Agent and
			// as a result we do not expect status update from the Agent, so we will
			// just notify the waiter that the config change is done.
			notifyWhenConfigIsApplied <- struct{}{}
		}
	}
}

// calcRemoteConfig calculates the remote config for this Agent. It returns true if
// the calculated new config is different from the existing config stored in
// Agent.remoteConfig.
func (agent *Agent) calcRemoteConfig() bool {
	cfg := protobufs.AgentRemoteConfig{
		Config: &protobufs.AgentConfigMap{
			// Add the custom config for this particular Agent instance. Use empty
			// string as the config file name.
			ConfigMap: map[string]*protobufs.AgentConfigFile{
				"": {Body: []byte(agent.CustomInstanceConfig)},
			},
		},
	}

	// Calculate the hash via the shared routine so the DELIVERED hash the
	// reconcile loop computes for the same content is byte-identical to what we
	// stamp on the wire here (they must never drift — see wireConfigHash).
	cfg.ConfigHash = wireConfigHash(agent.CustomInstanceConfig)

	configChanged := !isEqualRemoteConfig(agent.remoteConfig, &cfg)

	agent.remoteConfig = &cfg

	return configChanged
}

// wireConfigHash computes the OpAMP remote-config hash for a single-file
// (empty-string key, no content type) config body. It is the exact hash
// calcRemoteConfig stamps on AgentRemoteConfig.ConfigHash and that the agent
// echoes back as RemoteConfigStatus.LastRemoteConfigHash once delivered.
//
// Factored out so the per-instance reconcile loop can compute the desired
// DELIVERED hash from stored config content and compare it against the agent's
// LastRemoteConfigHash without duplicating — and drifting from — the hashing in
// calcRemoteConfig.
func wireConfigHash(body string) []byte {
	hash := sha256.New()
	hash.Write([]byte("")) // config file key (empty string, per the map above)
	hash.Write([]byte(body))
	hash.Write([]byte("")) // content type (unset)
	return hash.Sum(nil)
}

// appliedConfigHash returns the confignorm hash of the config content the agent
// has confirmed applied, and true, when the agent has ACKED the remote config
// Squadron currently has staged for it — its last-reported RemoteConfigStatus
// hash equals the wire hash stamped on agent.remoteConfig (which calcRemoteConfig
// derived from CustomInstanceConfig). Returns ("", false) otherwise: the agent
// has not acked the current desired config (never delivered, mid-rollout, or a
// report-only agent that has no remoteConfig staged).
//
// The returned hash is the confignorm hash of CustomInstanceConfig — the same
// canonical hash the store stamps on the stored config (ConfigHash) and that
// drift detection uses for the intent (see internal/confignorm). Because the
// acked wire hash pins CustomInstanceConfig to exactly what the agent applied,
// this value is directly comparable to the drift intent hash (ADR 0040): the
// OpAMP server persists it via UpdateAgentDeliveredConfigHash so the services
// layer can tell "the agent applied exactly what Squadron assigned" apart from
// the env-/default-expanded effective config a supervised collector reports.
func (agent *Agent) appliedConfigHash() (string, bool) {
	if agent.Status == nil || agent.Status.RemoteConfigStatus == nil {
		return "", false
	}
	if agent.remoteConfig == nil {
		return "", false
	}
	acked := agent.Status.RemoteConfigStatus.LastRemoteConfigHash
	if len(acked) == 0 {
		return "", false
	}
	if !bytes.Equal(acked, agent.remoteConfig.ConfigHash) {
		return "", false
	}
	return confignorm.Hash(agent.CustomInstanceConfig), true
}

// Configuration comparison helper functions

// isEqualRemoteConfig compares two remote configurations
func isEqualRemoteConfig(c1, c2 *protobufs.AgentRemoteConfig) bool {
	if c1 == c2 {
		return true
	}
	if c1 == nil || c2 == nil {
		return false
	}
	return isEqualConfigSet(c1.Config, c2.Config)
}

// isEqualConfigSet compares two configuration sets
func isEqualConfigSet(c1, c2 *protobufs.AgentConfigMap) bool {
	if c1 == c2 {
		return true
	}
	if c1 == nil || c2 == nil {
		return false
	}
	if len(c1.ConfigMap) != len(c2.ConfigMap) {
		return false
	}
	for k, f1 := range c1.ConfigMap {
		f2 := c2.ConfigMap[k]
		if !isEqualConfigFile(f1, f2) {
			return false
		}
	}
	return true
}

// isEqualConfigFile compares two configuration files
func isEqualConfigFile(f1, f2 *protobufs.AgentConfigFile) bool {
	if f1 == f2 {
		return true
	}
	if f1 == nil || f2 == nil {
		return false
	}
	return bytes.Equal(f1.Body, f2.Body) && f1.ContentType == f2.ContentType
}
