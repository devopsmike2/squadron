// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package silentagents watches the agent table for hosts that have
// stopped checking in via OpAMP and dispatches webhook
// notifications when they cross the silence threshold.
//
// This is the v0.33 "tell me when something breaks" surface. It
// shares the existing alerting.NotificationPayload shape so an
// operator's webhook receiver can handle silent-agent events the
// same way it handles SquadronQL alerts.
//
// Distinct from the alerts package because:
//   - silent-agent events aren't queries — they're state transitions
//     on agents we know about
//   - the firing condition is wall-clock-since-last-seen, not a
//     numeric threshold over a metric value
//   - the catalog of "what to watch" is the entire agents table
//     plus the expected_agents table, not a user-authored rule list
package silentagents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/notify"
	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// DefaultSilenceThreshold is the wall-clock gap after which an
// agent that was previously checking in gets flagged as silent.
// 10 minutes is generous enough to absorb a brief network blip or
// a single missed OpAMP heartbeat while still catching real
// outages quickly.
const DefaultSilenceThreshold = 10 * time.Minute

// Config tunes the watcher loop. Zero values fall back to sensible
// defaults: 60s poll interval, 10min silence threshold.
type Config struct {
	SilenceThreshold time.Duration
	PollInterval     time.Duration
	// WebhookURL is the global webhook destination — every transition
	// fires here. Empty means "log only" (still useful in dev). v0.34+
	// will add per-source webhook routing keyed on the
	// expected_agents.source field.
	WebhookURL string
	// DestinationType picks the v0.43 vendor formatter — "slack",
	// "teams", "pagerduty", "opsgenie", or "" / "generic" for the
	// legacy plain-JSON shape.
	DestinationType string
	// DestinationExtra is vendor-specific config (routing_key for
	// PagerDuty, api_key for Opsgenie). Empty for Slack/Teams/Generic.
	DestinationExtra map[string]string
	// PublicBaseURL is the externally-reachable Squadron URL used to
	// build deep links inside the formatted notification (e.g.
	// "Open in Squadron" → /agents/<id>). Falls back to "" — vendor
	// formatters drop the link block in that case.
	PublicBaseURL string
}

// Store is the slice of the application store the watcher needs.
type Store interface {
	ListAgents(ctx context.Context) ([]*apptypes.Agent, error)
	ListExpectedAgents(ctx context.Context, source string) ([]*apptypes.ExpectedAgent, error)
}

// Watcher polls the store at PollInterval and fires webhooks on
// healthy↔silent transitions. Construct one with New and call
// Run in a goroutine; Stop closes the loop cleanly.
type Watcher struct {
	cfg        Config
	store      Store
	logger     *zap.Logger
	http       *http.Client
	dispatcher *notify.Dispatcher // v0.43 — vendor-aware webhook router

	mu        sync.Mutex
	lastState map[string]state // hostKey → last observed state, used to detect transitions
	stop      chan struct{}
}

type state string

const (
	stateHealthy state = "healthy"
	stateSilent  state = "silent"
)

// Event is what the watcher dispatches. The shape is deliberately
// close to alerting.NotificationPayload so operators can use the
// same webhook receiver for both. Kind disambiguates the source —
// alerting payloads carry rule_id / rule_name; ours carry agent
// metadata.
type Event struct {
	Kind         string            `json:"kind"`           // "silent_agent"
	State        string            `json:"state"`          // "firing" or "resolved"
	AgentID      string            `json:"agent_id,omitempty"`
	Hostname     string            `json:"hostname"`
	Source       string            `json:"source,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	LastSeen     time.Time         `json:"last_seen"`
	SilenceFor   string            `json:"silence_for"` // human-readable duration
	At           time.Time         `json:"at"`
}

// New constructs a watcher with the given config. Use DefaultConfig
// to fill in zero values.
func New(cfg Config, store Store, logger *zap.Logger) *Watcher {
	if cfg.SilenceThreshold == 0 {
		cfg.SilenceThreshold = DefaultSilenceThreshold
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 60 * time.Second
	}
	return &Watcher{
		cfg:        cfg,
		store:      store,
		logger:     logger,
		http:       &http.Client{Timeout: 10 * time.Second},
		dispatcher: notify.NewDispatcher(),
		lastState:  map[string]state{},
		stop:       make(chan struct{}),
	}
}

// Run blocks until Stop is called. Polls every PollInterval; each
// tick walks the agent table, classifies each agent as healthy or
// silent, and fires webhooks on transitions.
func (w *Watcher) Run(ctx context.Context) {
	w.logger.Info("silent-agent watcher started",
		zap.Duration("poll_interval", w.cfg.PollInterval),
		zap.Duration("silence_threshold", w.cfg.SilenceThreshold),
		zap.Bool("webhook_configured", w.cfg.WebhookURL != ""))
	t := time.NewTicker(w.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				w.logger.Warn("silent-agent watcher tick failed", zap.Error(err))
			}
		}
	}
}

// Stop signals the Run loop to exit at its next tick.
func (w *Watcher) Stop() {
	select {
	case <-w.stop:
		// already closed
	default:
		close(w.stop)
	}
}

// Tick runs one pass: classify, detect transitions, dispatch.
// Exposed for tests + an on-demand endpoint v0.34 will add.
func (w *Watcher) Tick(ctx context.Context) error {
	agents, err := w.store.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	// Build an expected-hostnames map so a silent agent gets the
	// CI source attribution attached to its event.
	expectedByHost := map[string]*apptypes.ExpectedAgent{}
	if exp, err := w.store.ListExpectedAgents(ctx, ""); err == nil {
		for _, e := range exp {
			if e == nil {
				continue
			}
			expectedByHost[hostKey(e.Hostname)] = e
		}
	}

	now := time.Now().UTC()
	w.mu.Lock()
	defer w.mu.Unlock()

	seen := map[string]struct{}{}

	for _, a := range agents {
		if a == nil {
			continue
		}
		key := hostKey(a.Name)
		seen[key] = struct{}{}
		curr := stateHealthy
		if now.Sub(a.LastSeen) > w.cfg.SilenceThreshold {
			curr = stateSilent
		}
		prev, hadPrev := w.lastState[key]
		w.lastState[key] = curr

		// Only fire on transition. We DON'T fire on initial discovery
		// of a silent agent (an agent that was already silent when the
		// watcher started) — that would create a noisy startup burst.
		if !hadPrev {
			continue
		}
		if prev == curr {
			continue
		}

		evt := Event{
			Kind:       "silent_agent",
			AgentID:    a.ID.String(),
			Hostname:   a.Name,
			LastSeen:   a.LastSeen,
			SilenceFor: now.Sub(a.LastSeen).Round(time.Second).String(),
			At:         now,
		}
		if exp, ok := expectedByHost[key]; ok {
			evt.Source = exp.Source
			evt.Labels = exp.Labels
		}
		if curr == stateSilent {
			evt.State = "firing"
		} else {
			evt.State = "resolved"
		}
		w.dispatch(ctx, evt)
	}

	// Garbage-collect lastState entries for agents that have been
	// removed from the store — keeps the map from growing unbounded
	// in long-running installs with high agent churn.
	for key := range w.lastState {
		if _, ok := seen[key]; !ok {
			delete(w.lastState, key)
		}
	}
	return nil
}

func (w *Watcher) dispatch(ctx context.Context, evt Event) {
	w.logger.Info("silent-agent transition",
		zap.String("state", evt.State),
		zap.String("hostname", evt.Hostname),
		zap.String("agent_id", evt.AgentID),
		zap.String("silence_for", evt.SilenceFor))
	if w.cfg.WebhookURL == "" {
		return
	}

	// v0.43 — if a destination_type is configured, route through the
	// notify.Dispatcher so Slack/Teams/PagerDuty/Opsgenie get the
	// vendor-shaped body. Empty / "generic" preserves the legacy
	// plain-JSON shape so existing operator-built receivers don't
	// break on upgrade.
	dest := notify.Destination{
		URL:   w.cfg.WebhookURL,
		Type:  notify.DestinationType(strings.ToLower(w.cfg.DestinationType)),
		Extra: w.cfg.DestinationExtra,
	}
	if dest.Type == "" || dest.Type == notify.TypeGeneric {
		// Legacy path: ship the raw Event JSON like we always did.
		// Falls through to the dispatcher's generic branch which
		// json-marshals whatever you hand it; we pre-marshal so the
		// shape on the wire is identical to pre-v0.43.
		raw, err := json.Marshal(evt)
		if err != nil {
			w.logger.Warn("silent-agent webhook marshal failed", zap.Error(err))
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			w.cfg.WebhookURL, bytes.NewReader(raw))
		if err != nil {
			w.logger.Warn("silent-agent webhook build failed", zap.Error(err))
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Squadron/silent-agents")
		resp, err := w.http.Do(req)
		if err != nil {
			w.logger.Warn("silent-agent webhook POST failed",
				zap.Error(err), zap.String("url", w.cfg.WebhookURL))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			w.logger.Warn("silent-agent webhook returned non-2xx",
				zap.Int("status", resp.StatusCode),
				zap.String("url", w.cfg.WebhookURL))
		}
		return
	}

	// v0.43 dispatcher path. Translate the Event into the canonical
	// notify.Event shape so the formatter doesn't need to know about
	// our internal struct.
	severity := notify.SeverityWarning
	if evt.State == "resolved" {
		severity = notify.SeverityInfo
	}
	action := "trigger"
	if evt.State == "resolved" {
		action = "resolve"
	}
	link := ""
	if w.cfg.PublicBaseURL != "" && evt.AgentID != "" {
		link = strings.TrimRight(w.cfg.PublicBaseURL, "/") + "/agents/" + evt.AgentID
	}
	title := fmt.Sprintf("Silent agent: %s", evt.Hostname)
	if evt.State == "resolved" {
		title = fmt.Sprintf("Silent agent resolved: %s", evt.Hostname)
	}
	summary := ""
	if evt.SilenceFor != "" {
		summary = fmt.Sprintf("Hasn't reported for %s.", evt.SilenceFor)
	}
	fields := []notify.Field{
		{Key: "Hostname", Value: evt.Hostname},
	}
	if evt.AgentID != "" {
		fields = append(fields, notify.Field{Key: "Agent ID", Value: evt.AgentID})
	}
	if evt.Source != "" {
		fields = append(fields, notify.Field{Key: "Source", Value: evt.Source})
	}
	if !evt.LastSeen.IsZero() {
		fields = append(fields, notify.Field{
			Key:   "Last seen",
			Value: evt.LastSeen.UTC().Format("Jan 2 15:04 MST"),
		})
	}
	canonical := notify.Event{
		Title:    title,
		Summary:  summary,
		Severity: severity,
		Kind:     "silent_agent." + evt.State,
		DedupKey: evt.AgentID,
		Action:   action,
		Link:     link,
		Fields:   fields,
		At:       time.Now().UTC(),
	}
	if err := w.dispatcher.Dispatch(ctx, dest, canonical); err != nil {
		w.logger.Warn("silent-agent webhook dispatch failed",
			zap.Error(err),
			zap.String("type", string(dest.Type)),
			zap.String("url", dest.URL))
	}
}

// hostKey normalizes a hostname for the lastState map. Same logic
// as internal/inventory.hostKey — duplicated here so this package
// doesn't depend on inventory.
func hostKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if idx := strings.IndexByte(s, '.'); idx > 0 {
		return s[:idx]
	}
	return s
}
