// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// fleetsim is a synthetic OpAMP load generator for Squadron.
//
// It spawns N simulated agents that connect to a running Squadron
// instance over the standard OpAMP WebSocket protocol — each agent
// is a real opamp-go client, not a mock, so they exercise the same
// server code path a production OTel collector would. Cheaper than
// spinning up N real collectors in Docker; gives us deterministic
// scenarios for finding scale bottlenecks.
//
// Usage:
//
//	fleetsim --count=100 --target=ws://localhost:4320/v1/opamp
//	fleetsim --count=500 --ramp=60s --drift-pct=20 --offline-pct=5
//
// What gets simulated:
//   - Instance UUID per agent
//   - AgentDescription with service.name, service.version, host.name,
//     and a label sprinkle (host.arch, deployment.environment, etc.)
//   - Periodic health pings (ComponentHealth)
//   - RemoteConfig handling: ACK by default; with --drift-pct, a
//     percentage of agents report a tweaked effective config so the
//     drift-detection path on the server side activates.
//
// What is NOT simulated (yet — defer to v0.22.x):
//   - OTLP traffic at scale (this stresses the receiver path, not
//     the OpAMP server path, which is what we care about now).
//   - Reconnect storms / network jitter.
//   - Custom-capability negotiation (Squadron's traceparent
//     capability is server-initiated; receiving it doesn't require
//     simulated agents to do anything).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// quietLogger swallows the opamp-go client's own debug/error chatter
// — we have N clients all logging in parallel and the noise drowns
// out our own progress reporting. The fleetsim status line is the
// authoritative source of truth for runs.
type quietLogger struct{}

func (quietLogger) Debugf(context.Context, string, ...interface{}) {}
func (quietLogger) Errorf(context.Context, string, ...interface{}) {}

type config struct {
	target      string
	count       int
	ramp        time.Duration
	driftPct    int
	offlinePct  int
	version     string
	groupLabel  string
	labelPrefix string
	healthEvery time.Duration
	verbose     bool
}

// stats is the shared progress counter all agents update. Reads are
// done from the status-line goroutine; writes use atomics so we
// avoid a mutex on the hot path.
type stats struct {
	connected   atomic.Int64
	failed      atomic.Int64
	disconnects atomic.Int64
	configsRcvd atomic.Int64
	driftedAck  atomic.Int64
	cleanAck    atomic.Int64
}

func main() {
	cfg := parseFlags()

	log.SetFlags(log.Ltime)

	st := &stats{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SIGINT / SIGTERM stops the simulation cleanly so the connection
	// counters drain to zero rather than dropping abruptly.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Status-line printer ticks every second; cheap heartbeat for an
	// otherwise mostly-silent process.
	go statusLine(ctx, st, cfg.count)

	log.Printf("fleetsim: starting %d simulated agents → %s (ramp %s)",
		cfg.count, cfg.target, cfg.ramp)

	var wg sync.WaitGroup
	// Stagger initial connects across the ramp window so we don't
	// blast N WebSocket connects in a 10ms window — that's not a
	// realistic production scenario and just tests the server's
	// accept-burst handling.
	gap := time.Duration(0)
	if cfg.count > 0 && cfg.ramp > 0 {
		gap = cfg.ramp / time.Duration(cfg.count)
	}
	for i := 0; i < cfg.count; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			runAgent(ctx, cfg, st, i)
		}()
		if gap > 0 {
			select {
			case <-time.After(gap):
			case <-ctx.Done():
				return
			}
		}
	}

	// Block until SIGINT, then cancel and drain.
	<-sigCh
	log.Println("fleetsim: shutting down, draining connections…")
	cancel()
	wg.Wait()
	log.Printf("fleetsim: done. final stats — connected:%d failed:%d configs:%d drift_ack:%d clean_ack:%d",
		st.connected.Load(), st.failed.Load(), st.configsRcvd.Load(),
		st.driftedAck.Load(), st.cleanAck.Load())
}

// runAgent runs one simulated agent end-to-end: dial, identify,
// honor inbound config, stay alive sending health pings until ctx
// cancels.
func runAgent(ctx context.Context, cfg config, st *stats, idx int) {
	// Deterministic UUIDs make runs reproducible — same --count
	// twice in a row produces the same fleet, so the UI test
	// scenarios are stable across reloads.
	instanceUUID := deterministicUUID(idx)
	var instanceUid types.InstanceUid
	copy(instanceUid[:], instanceUUID[:])

	c := client.NewWebSocket(quietLogger{})

	// drift decision per-agent at startup so the same agent
	// consistently reports drifted on repeat config pushes.
	drifted := rand.Intn(100) < cfg.driftPct
	// offlinePct agents connect once, then disconnect and stay
	// silent — exercises the server's offline detection path.
	willGoOffline := rand.Intn(100) < cfg.offlinePct

	desc := agentDescription(idx, cfg)

	connectedOnce := false
	settings := types.StartSettings{
		OpAMPServerURL: cfg.target,
		InstanceUid:    instanceUid,
		Capabilities: protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig |
			protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth,
		Callbacks: types.CallbacksStruct{
			OnConnectFunc: func(_ context.Context) {
				connectedOnce = true
				st.connected.Add(1)
				if cfg.verbose {
					log.Printf("agent#%d connected", idx)
				}
			},
			OnConnectFailedFunc: func(_ context.Context, err error) {
				st.failed.Add(1)
				if cfg.verbose {
					log.Printf("agent#%d connect failed: %v", idx, err)
				}
			},
			OnMessageFunc: func(_ context.Context, msg *types.MessageData) {
				if msg.RemoteConfig == nil {
					return
				}
				st.configsRcvd.Add(1)
				if drifted {
					st.driftedAck.Add(1)
				} else {
					st.cleanAck.Add(1)
				}
				// Reflect the (optionally tweaked) config back as
				// effective config so the server's drift detector
				// has data to work with.
				_ = c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{
					LastRemoteConfigHash: msg.RemoteConfig.ConfigHash,
					Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				})
			},
		},
	}
	if err := c.SetAgentDescription(desc); err != nil {
		st.failed.Add(1)
		return
	}
	// SetHealth with status:true so the server-side gauge reflects
	// the simulated agent as healthy. The realtime health-ping loop
	// below keeps the flag fresh.
	_ = c.SetHealth(&protobufs.ComponentHealth{Healthy: true, StartTimeUnixNano: uint64(time.Now().UnixNano())})

	if err := c.Start(ctx, settings); err != nil {
		st.failed.Add(1)
		return
	}

	// If this agent is in the offline cohort, hold the connection
	// briefly then disconnect — simulates an agent that briefly
	// flapped and the heartbeat watcher should mark offline.
	if willGoOffline {
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
		_ = c.Stop(context.Background())
		if connectedOnce {
			st.disconnects.Add(1)
			st.connected.Add(-1)
		}
		return
	}

	// Health-ping loop. opamp-go batches outbound messages so a
	// SetHealth call doesn't trigger an immediate send — it gets
	// folded into the next status update. This is fine: we just
	// want a periodic keepalive proving the agent is alive.
	ticker := time.NewTicker(cfg.healthEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = c.Stop(context.Background())
			return
		case <-ticker.C:
			_ = c.SetHealth(&protobufs.ComponentHealth{
				Healthy:            true,
				StartTimeUnixNano:  uint64(time.Now().UnixNano()),
				StatusTimeUnixNano: uint64(time.Now().UnixNano()),
			})
		}
	}
}

// agentDescription builds a believable AgentDescription with both
// identifying attributes (service.name, version, instance) and
// non-identifying ones (deployment.environment, host.arch, the
// group label so Squadron's group-by-label tooling has something
// to match).
func agentDescription(idx int, cfg config) *protobufs.AgentDescription {
	pid := os.Getpid()
	hostname, _ := os.Hostname()
	hostName := fmt.Sprintf("%s-sim-%d-%d", hostname, pid, idx)

	identifying := []*protobufs.KeyValue{
		stringKV("service.name", "synthetic-collector"),
		stringKV("service.version", cfg.version),
		stringKV("service.instance.id", deterministicUUID(idx).String()),
	}
	nonIdentifying := []*protobufs.KeyValue{
		stringKV("host.name", hostName),
		stringKV("host.arch", randomChoice([]string{"arm64", "amd64"}, idx)),
		stringKV("os.type", randomChoice([]string{"linux", "darwin"}, idx)),
		stringKV("deployment.environment",
			randomChoice([]string{"prod", "staging", "dev"}, idx)),
	}
	if cfg.groupLabel != "" {
		// Squadron's group-by-label resolver picks up agent.group_name.
		// fleetsim sets it explicitly so the simulated fleet shows up
		// inside whatever group --group points at.
		nonIdentifying = append(nonIdentifying,
			stringKV("agent.group_name", cfg.groupLabel),
		)
	}
	if cfg.labelPrefix != "" {
		nonIdentifying = append(nonIdentifying,
			stringKV("simulated.fleet", cfg.labelPrefix),
		)
	}
	return &protobufs.AgentDescription{
		IdentifyingAttributes:    identifying,
		NonIdentifyingAttributes: nonIdentifying,
	}
}

func stringKV(k, v string) *protobufs.KeyValue {
	return &protobufs.KeyValue{
		Key: k,
		Value: &protobufs.AnyValue{
			Value: &protobufs.AnyValue_StringValue{StringValue: v},
		},
	}
}

// deterministicUUID derives a stable UUID from an index. Same index
// → same UUID across runs, so /agents stays referentially stable
// while we iterate on the UI.
func deterministicUUID(idx int) uuid.UUID {
	// Use a fixed namespace so the same idx always maps to the same
	// uuid.UUIDv5 result. The "fleetsim" namespace UUID is itself
	// a v5 of "fleetsim" under uuid.NameSpaceDNS — anything stable
	// works here.
	ns := uuid.NewSHA1(uuid.NameSpaceDNS, []byte("fleetsim"))
	return uuid.NewSHA1(ns, fmt.Appendf(nil, "agent-%d", idx))
}

// randomChoice picks deterministically based on the agent index so
// repeat runs produce the same label distribution.
func randomChoice(opts []string, idx int) string {
	return opts[idx%len(opts)]
}

func statusLine(ctx context.Context, st *stats, total int) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fmt.Fprintf(os.Stderr,
				"\rconnected:%d/%d  failed:%d  disc:%d  configs_rcvd:%d  drift_ack:%d  clean_ack:%d   ",
				st.connected.Load(), total,
				st.failed.Load(),
				st.disconnects.Load(),
				st.configsRcvd.Load(),
				st.driftedAck.Load(),
				st.cleanAck.Load(),
			)
		}
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.target, "target", "ws://localhost:4320/v1/opamp",
		"OpAMP server URL to connect to")
	flag.IntVar(&cfg.count, "count", 100,
		"Number of simulated agents to spawn")
	flag.DurationVar(&cfg.ramp, "ramp", 30*time.Second,
		"Spread initial connects over this duration (0 = all at once)")
	flag.IntVar(&cfg.driftPct, "drift-pct", 10,
		"Percent of agents that report a drifted effective config (0-100)")
	flag.IntVar(&cfg.offlinePct, "offline-pct", 0,
		"Percent of agents that briefly connect then disconnect (0-100)")
	flag.StringVar(&cfg.version, "version", "0.119.0",
		"service.version reported in AgentDescription")
	flag.StringVar(&cfg.groupLabel, "group", "",
		"agent.group_name label applied to every simulated agent (empty = no group)")
	flag.StringVar(&cfg.labelPrefix, "label-prefix", "fleetsim",
		"value of the 'simulated.fleet' label so simulated agents are filterable in the UI")
	flag.DurationVar(&cfg.healthEvery, "health-interval", 15*time.Second,
		"How often each agent sends a health ping")
	flag.BoolVar(&cfg.verbose, "v", false,
		"Verbose per-agent logging (off by default to keep the status line readable)")
	flag.Parse()

	if cfg.count <= 0 {
		fmt.Fprintln(os.Stderr, "fleetsim: --count must be > 0")
		os.Exit(2)
	}
	if cfg.driftPct < 0 || cfg.driftPct > 100 {
		fmt.Fprintln(os.Stderr, "fleetsim: --drift-pct must be 0-100")
		os.Exit(2)
	}
	if cfg.offlinePct < 0 || cfg.offlinePct > 100 {
		fmt.Fprintln(os.Stderr, "fleetsim: --offline-pct must be 0-100")
		os.Exit(2)
	}
	return cfg
}
