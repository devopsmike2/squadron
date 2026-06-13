// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package inventory is the v0.32 inventory reconciliation surface.
//
// Squadron knows about agents in two ways:
//
//  1. **Expected** — some CI/CD pipeline declared "these hostnames
//     should be running collectors", typically by POSTing a target
//     list at the end of a deploy job.
//  2. **Actual** — agents that dialed in via OpAMP and ended up in
//     the agents table.
//
// The reconciliation service answers the diff: which expected
// hostnames never showed up (or have gone silent) and which actual
// agents weren't declared.
//
// Matching is by hostname (case-insensitive). The OTel collector's
// resource attribute `host.name` provides this naturally; the
// adapter in main.go takes the agent's Name field as a proxy when
// host.name isn't reported. CI pipelines that deploy with a
// non-default agent name should configure the collector to emit
// `host.name` and Squadron's enricher will use it.
package inventory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// SilentThreshold is the wall-clock gap after which an expected
// agent is considered "missing" — that is, even though it was
// declared expected, we either never saw it or we haven't heard
// from it in too long. 10 minutes covers a few OpAMP heartbeats
// without false-positive flagging a brief network blip.
const SilentThreshold = 10 * time.Minute

// Store is the slice of the application store the inventory
// service uses. Declared as an interface so tests can stub.
type Store interface {
	ListExpectedAgents(ctx context.Context, source string) ([]*apptypes.ExpectedAgent, error)
	UpsertExpectedAgent(ctx context.Context, e *apptypes.ExpectedAgent) error
	DeleteExpectedAgent(ctx context.Context, hostname string) error
	ReplaceExpectedAgentsForSource(ctx context.Context, source string, entries []*apptypes.ExpectedAgent) error
	ListAgents(ctx context.Context) ([]*apptypes.Agent, error)
}

// Service answers reconciliation queries. Stateless — every method
// reads from the store.
type Service struct {
	store  Store
	logger *zap.Logger
}

// NewService constructs an inventory service.
func NewService(store Store, logger *zap.Logger) *Service {
	return &Service{store: store, logger: logger}
}

// Status enumerates the per-host states the dashboard renders.
type Status string

const (
	// StatusHealthy: expected AND recently seen.
	StatusHealthy Status = "healthy"
	// StatusMissing: expected but never seen, or quiet for longer
	// than SilentThreshold.
	StatusMissing Status = "missing"
	// StatusUnexpected: actually connected, but no row in the
	// expected_agents table. Usually means manual install or stray
	// host the CI pipeline doesn't know about.
	StatusUnexpected Status = "unexpected"
)

// Row is one entry in the reconciliation report. Each row is a
// hostname; hostnames in the expected list show up exactly once,
// and unexpected hosts show up once each.
type Row struct {
	Hostname      string            `json:"hostname"`
	Status        Status            `json:"status"`
	Source        string            `json:"source,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Notes         string            `json:"notes,omitempty"`
	LastSeen      *time.Time        `json:"last_seen,omitempty"`
	ExpectedSince *time.Time        `json:"expected_since,omitempty"`
	AgentID       string            `json:"agent_id,omitempty"`
}

// Report is the full reconciliation payload. Counts make the
// dashboard's stacked-bar cheap to render; Rows is the detail.
type Report struct {
	Healthy    int       `json:"healthy"`
	Missing    int       `json:"missing"`
	Unexpected int       `json:"unexpected"`
	Total      int       `json:"total"`
	Rows       []Row     `json:"rows"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Reconcile diffs the expected list against the actual agent table.
// Source is optional — pass empty to consider every expected entry
// regardless of which pipeline submitted it.
func (s *Service) Reconcile(ctx context.Context, source string) (*Report, error) {
	expected, err := s.store.ListExpectedAgents(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("list expected: %w", err)
	}
	actual, err := s.store.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list actual: %w", err)
	}

	now := time.Now().UTC()

	// Build hostname index for actual agents. The match key is
	// lowercase hostname for case-insensitive comparison; CI
	// pipelines often deploy to "Host01" while OTel reports
	// "host01". host.name resource attribute is preferred but the
	// Agent.Name field is the fallback.
	type actualEntry struct {
		agent *apptypes.Agent
	}
	actualByHost := map[string]actualEntry{}
	for _, a := range actual {
		if a == nil {
			continue
		}
		key := hostKey(a.Name)
		if key == "" {
			continue
		}
		actualByHost[key] = actualEntry{agent: a}
	}

	rows := make([]Row, 0, len(expected)+len(actual))

	// Walk expected; classify each as healthy or missing.
	seenInExpected := map[string]struct{}{}
	for _, e := range expected {
		if e == nil || e.Hostname == "" {
			continue
		}
		key := hostKey(e.Hostname)
		seenInExpected[key] = struct{}{}
		row := Row{
			Hostname: e.Hostname,
			Source:   e.Source,
			Labels:   e.Labels,
			Notes:    e.Notes,
		}
		if !e.ExpectedSince.IsZero() {
			es := e.ExpectedSince
			row.ExpectedSince = &es
		}
		if hit, ok := actualByHost[key]; ok {
			row.AgentID = hit.agent.ID.String()
			ls := hit.agent.LastSeen
			row.LastSeen = &ls
			// Quiet for too long? Still considered missing — the
			// distinction matters for alerting in v0.33 where a
			// transition from healthy → missing fires a webhook.
			if now.Sub(hit.agent.LastSeen) > SilentThreshold {
				row.Status = StatusMissing
			} else {
				row.Status = StatusHealthy
			}
		} else {
			row.Status = StatusMissing
		}
		rows = append(rows, row)
	}

	// Then walk actual; anything not in the expected map is
	// unexpected. Skip when we're filtering by source — if you
	// asked for one pipeline's view, hosts owned by other pipelines
	// are not "unexpected" from your perspective.
	if source == "" {
		for key, hit := range actualByHost {
			if _, ok := seenInExpected[key]; ok {
				continue
			}
			ls := hit.agent.LastSeen
			rows = append(rows, Row{
				Hostname: hit.agent.Name,
				Status:   StatusUnexpected,
				AgentID:  hit.agent.ID.String(),
				LastSeen: &ls,
			})
		}
	}

	// Stable order: status (worst first), then hostname.
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := statusRank(rows[i].Status), statusRank(rows[j].Status)
		if ri != rj {
			return ri < rj
		}
		return rows[i].Hostname < rows[j].Hostname
	})

	r := &Report{Rows: rows, UpdatedAt: now}
	for _, row := range rows {
		switch row.Status {
		case StatusHealthy:
			r.Healthy++
		case StatusMissing:
			r.Missing++
		case StatusUnexpected:
			r.Unexpected++
		}
	}
	r.Total = r.Healthy + r.Missing + r.Unexpected
	return r, nil
}

// ReplaceExpected is the bulk-rotate path used by CI/CD pipelines.
// Source is required — every entry in the new list is tagged with
// it. CI typically passes a job-specific identifier like
// "gha-otel-deploy" so multiple pipelines can coexist.
func (s *Service) ReplaceExpected(
	ctx context.Context,
	source string,
	entries []*apptypes.ExpectedAgent,
) error {
	if source == "" {
		return fmt.Errorf("source required")
	}
	return s.store.ReplaceExpectedAgentsForSource(ctx, source, entries)
}

// Upsert handles single-row additions (UI button, squadronctl) —
// useful for short-term overrides like adding a one-off canary host.
func (s *Service) Upsert(ctx context.Context, e *apptypes.ExpectedAgent) error {
	return s.store.UpsertExpectedAgent(ctx, e)
}

// Delete removes an expected entry by hostname.
func (s *Service) Delete(ctx context.Context, hostname string) error {
	return s.store.DeleteExpectedAgent(ctx, hostname)
}

// List returns every expected entry, filterable by source.
func (s *Service) List(ctx context.Context, source string) ([]*apptypes.ExpectedAgent, error) {
	return s.store.ListExpectedAgents(ctx, source)
}

// hostKey normalizes a hostname for comparison. We strip the FQDN
// suffix from one side if the other side is short — this is the
// most common mismatch ("host01" expected, "host01.example.com"
// actual). Lowercased throughout.
func hostKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if idx := strings.IndexByte(s, '.'); idx > 0 {
		return s[:idx]
	}
	return s
}

func statusRank(s Status) int {
	switch s {
	case StatusMissing:
		return 0
	case StatusUnexpected:
		return 1
	case StatusHealthy:
		return 2
	default:
		return 3
	}
}
