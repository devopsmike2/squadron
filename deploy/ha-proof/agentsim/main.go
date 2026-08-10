// Command agentsim is a minimal OpAMP agent for the HA proof harness.
//
// It exists because neither off-the-shelf option fits the enterprise HA test:
//   - the stock otelcol-contrib opampextension only REPORTS effective config;
//     it cannot ACCEPT + apply a remote config without the OpAMP supervisor, so
//     it never converges; and
//   - Squadron's own cmd/fleetsim accepts remote config but sends no connection
//     headers, so the enterprise build's strict tenant scoping (which requires
//     an x-squadron-tenant header on OpAMP connect, ADR 0012) rejects it.
//
// agentsim connects with the tenant header, advertises AcceptsRemoteConfig +
// ReportsEffectiveConfig, and — crucially — echoes each received remote config
// straight back as its EffectiveConfig. That lets Squadron's drift detector see
// the agent's effective config match the pushed target (delivered -> effective
// -> Synced), which is what the S3d convergence gate advances a rollout on.
//
// It is HARNESS TOOLING (a test double), not product code, and lives under
// deploy/ha-proof/ so it builds inside the squadron module (opamp-go is already
// a dependency). Run it against the NON-LEADER instance to prove cross-instance
// config delivery via the S3a per-instance reconcile loop.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
)

type quietLogger struct{}

func (quietLogger) Debugf(context.Context, string, ...interface{}) {}
func (quietLogger) Errorf(_ context.Context, f string, v ...interface{}) {
	log.Printf("opamp-error: "+f, v...)
}

func stringKV(k, v string) *protobufs.KeyValue {
	return &protobufs.KeyValue{Key: k, Value: &protobufs.AnyValue{
		Value: &protobufs.AnyValue_StringValue{StringValue: v},
	}}
}

func main() {
	target := flag.String("target", "ws://127.0.0.1:14320/v1/opamp", "OpAMP server WS URL (point at the NON-LEADER instance)")
	tenant := flag.String("tenant", "default", "x-squadron-tenant header value (enterprise strict tenant scoping)")
	group := flag.String("group", "ha-proof-group", "agent.group_name label")
	name := flag.String("name", "ha-proof-agentsim", "service.name")
	flag.Parse()

	instID := uuid.New()
	var iu types.InstanceUid
	copy(iu[:], instID[:])

	var mu sync.Mutex
	var effective *protobufs.AgentConfigMap // last remote config, echoed back as effective

	c := client.NewWebSocket(quietLogger{})

	settings := types.StartSettings{
		OpAMPServerURL: *target,
		InstanceUid:    iu,
		// Enterprise strict tenant scoping requires this header on OpAMP connect.
		Header: http.Header{"x-squadron-tenant": {*tenant}},
		Capabilities: protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig |
			protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth,
		Callbacks: types.CallbacksStruct{
			OnConnectFunc:       func(context.Context) { log.Printf("connected to %s (tenant=%s group=%s)", *target, *tenant, *group) },
			OnConnectFailedFunc: func(_ context.Context, err error) { log.Printf("connect failed: %v", err) },
			// Echo the received remote config back as our effective config so the
			// server sees delivered -> effective -> Synced.
			OnMessageFunc: func(ctx context.Context, msg *types.MessageData) {
				if msg.RemoteConfig == nil {
					return
				}
				mu.Lock()
				effective = msg.RemoteConfig.Config
				mu.Unlock()
				_ = c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{
					LastRemoteConfigHash: msg.RemoteConfig.ConfigHash,
					Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				})
				_ = c.UpdateEffectiveConfig(ctx)
				log.Printf("received remote config (%d bytes hash) -> applied + reported effective", len(msg.RemoteConfig.ConfigHash))
			},
			GetEffectiveConfigFunc: func(context.Context) (*protobufs.EffectiveConfig, error) {
				mu.Lock()
				defer mu.Unlock()
				if effective == nil {
					return &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{
						ConfigMap: map[string]*protobufs.AgentConfigFile{
							"": {Body: []byte("# ha-proof-agentsim: awaiting remote config\n")},
						},
					}}, nil
				}
				return &protobufs.EffectiveConfig{ConfigMap: effective}, nil
			},
		},
	}

	desc := &protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{
			stringKV("service.name", *name),
			stringKV("service.version", "ha-proof"),
			stringKV("service.instance.id", instID.String()),
		},
		NonIdentifyingAttributes: []*protobufs.KeyValue{
			stringKV("agent.group_name", *group),
			stringKV("deployment.environment", "ha-proof"),
			stringKV("os.type", "linux"),
		},
	}
	if err := c.SetAgentDescription(desc); err != nil {
		log.Fatalf("set agent description: %v", err)
	}
	_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: true, StartTimeUnixNano: uint64(time.Now().UnixNano())})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Start(ctx, settings); err != nil {
		log.Fatalf("start: %v", err)
	}
	log.Printf("agentsim started instance=%s", instID)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-sig:
			_ = c.Stop(context.Background())
			return
		case <-t.C:
			_ = c.SetHealth(&protobufs.ComponentHealth{
				Healthy:            true,
				StartTimeUnixNano:  uint64(time.Now().UnixNano()),
				StatusTimeUnixNano: uint64(time.Now().UnixNano()),
			})
		}
	}
}
