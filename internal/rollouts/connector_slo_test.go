// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/devopsmike2/squadron/internal/connectors"
	"github.com/devopsmike2/squadron/internal/services"
)

// fakeConnector is a test double for connectors.Connector. It returns a
// canned result (or error) from Query and records the query it received so
// the translation from RolloutSLOBurnCriterion can be asserted. It never
// touches an external backend — the SLO-burn criterion is read-only.
type fakeConnector struct {
	result    connectors.NormalizedResult
	queryErr  error
	lastQuery connectors.NormalizedQuery
	queried   bool
}

func (f *fakeConnector) Describe() connectors.Descriptor {
	return connectors.Descriptor{Type: "fake", DisplayName: "Fake (test only)"}
}

func (f *fakeConnector) Capabilities() connectors.Capabilities {
	return connectors.Capabilities{
		Signals:        []connectors.Signal{connectors.SignalMetrics},
		Modes:          []connectors.Mode{connectors.ModeFederated},
		SupportsScalar: true,
	}
}

func (f *fakeConnector) Query(_ context.Context, q connectors.NormalizedQuery) (connectors.NormalizedResult, error) {
	f.lastQuery = q
	f.queried = true
	if f.queryErr != nil {
		return connectors.NormalizedResult{}, f.queryErr
	}
	return f.result, nil
}

func (f *fakeConnector) HealthCheck(_ context.Context) error { return nil }

// fakeResolver is a test double for ConnectorResolver.
type fakeResolver struct {
	conn        connectors.Connector
	resolveErr  error
	resolvedIDs []string
}

func (r *fakeResolver) ResolveConnector(_ context.Context, id string) (connectors.Connector, error) {
	r.resolvedIDs = append(r.resolvedIDs, id)
	if r.resolveErr != nil {
		return nil, r.resolveErr
	}
	return r.conn, nil
}

// burnScalar builds a ScalarResult NormalizedResult for the tests.
func burnScalar(value, threshold float64, breached bool) connectors.NormalizedResult {
	now := time.Now().UTC()
	return connectors.NewScalarResult(connectors.ScalarResult{
		Value:       value,
		Unit:        "multiple",
		Kind:        connectors.ScalarBurnRate,
		Objective:   "checkout-availability",
		SLOTarget:   0.999,
		Threshold:   threshold,
		Breached:    breached,
		Window:      connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		EvaluatedAt: now,
	})
}

func sloGuard() *services.RolloutSLOBurnCriterion {
	return &services.RolloutSLOBurnCriterion{
		ConnectorID:   "conn-1",
		Signal:        "metrics",
		Selector:      "slo:checkout:burn_rate",
		WindowSeconds: 300,
	}
}

func TestEngine_EvaluateSLOBurn_BreachedAborts(t *testing.T) {
	// Connector reports Breached=true — the engine trusts that verdict.
	fc := &fakeConnector{result: burnScalar(14.4, 14.4, true)}
	e := &Engine{
		logger:            zap.NewNop(),
		connectorResolver: &fakeResolver{conn: fc},
	}
	r := &services.Rollout{ID: "ro-1"}

	reason := e.evaluateSLOBurn(context.Background(), r, sloGuard())
	assert.Contains(t, reason, "SLO burn")
	assert.Contains(t, reason, "breached")
	assert.True(t, fc.queried, "the connector must have been queried")
}

func TestEngine_EvaluateSLOBurn_HealthyContinues(t *testing.T) {
	// Healthy scalar: not breached, no configured threshold cross → continue.
	fc := &fakeConnector{result: burnScalar(1.0, 14.4, false)}
	e := &Engine{
		logger:            zap.NewNop(),
		connectorResolver: &fakeResolver{conn: fc},
	}
	r := &services.Rollout{ID: "ro-1"}

	reason := e.evaluateSLOBurn(context.Background(), r, sloGuard())
	assert.Empty(t, reason, "a healthy SLO scalar must not abort")
}

func TestEngine_EvaluateSLOBurn_ValueCrossesConfiguredThreshold(t *testing.T) {
	// Connector did NOT flag Breached, but the operator set a guard
	// Threshold the Value crosses — the engine aborts on the crossing.
	fc := &fakeConnector{result: burnScalar(9.0, 0, false)}
	e := &Engine{
		logger:            zap.NewNop(),
		connectorResolver: &fakeResolver{conn: fc},
	}
	guard := sloGuard()
	guard.Threshold = 6.0
	r := &services.Rollout{ID: "ro-1"}

	reason := e.evaluateSLOBurn(context.Background(), r, guard)
	assert.Contains(t, reason, "crossed configured threshold")
}

func TestEngine_EvaluateSLOBurn_ConnectorErrorFailsOpen(t *testing.T) {
	// A query error must NOT abort (fail-open default) and must log a warning.
	core, logs := observer.New(zap.WarnLevel)
	fc := &fakeConnector{queryErr: errors.New("backend timeout")}
	e := &Engine{
		logger:            zap.New(core),
		connectorResolver: &fakeResolver{conn: fc},
	}
	r := &services.Rollout{ID: "ro-1"}

	reason := e.evaluateSLOBurn(context.Background(), r, sloGuard())
	assert.Empty(t, reason, "a connector query error must fail open (no abort)")
	require.Equal(t, 1, logs.FilterMessage("rollout SLO-burn guard could not be evaluated").Len(),
		"fail-open must surface a warning")
	entry := logs.All()[0]
	assert.Equal(t, true, entry.ContextMap()["fail_open"])
}

func TestEngine_EvaluateSLOBurn_ResolveErrorFailsOpen(t *testing.T) {
	// The connector cannot even be resolved — still fail-open by default.
	core, logs := observer.New(zap.WarnLevel)
	e := &Engine{
		logger:            zap.New(core),
		connectorResolver: &fakeResolver{resolveErr: errors.New("unknown connector")},
	}
	r := &services.Rollout{ID: "ro-1"}

	reason := e.evaluateSLOBurn(context.Background(), r, sloGuard())
	assert.Empty(t, reason, "an unresolvable connector must fail open (no abort)")
	assert.Equal(t, 1, logs.FilterMessage("rollout SLO-burn guard could not be evaluated").Len())
}

func TestEngine_EvaluateSLOBurn_NonScalarResultFailsOpen(t *testing.T) {
	// A connector that returns a non-scalar result cannot be used to decide
	// a burn — fail open by default.
	fc := &fakeConnector{result: connectors.NewMetricsResult(nil)}
	e := &Engine{
		logger:            zap.NewNop(),
		connectorResolver: &fakeResolver{conn: fc},
	}
	r := &services.Rollout{ID: "ro-1"}

	reason := e.evaluateSLOBurn(context.Background(), r, sloGuard())
	assert.Empty(t, reason, "a non-scalar result must fail open (no abort)")
}

func TestEngine_EvaluateSLOBurn_FailClosedOnError(t *testing.T) {
	// With FailOpen=false the guard flips to fail-closed: a query error
	// aborts the rollout rather than rolling forward blind.
	failClosed := false
	fc := &fakeConnector{queryErr: errors.New("backend timeout")}
	e := &Engine{
		logger:            zap.NewNop(),
		connectorResolver: &fakeResolver{conn: fc},
	}
	guard := sloGuard()
	guard.FailOpen = &failClosed
	r := &services.Rollout{ID: "ro-1"}

	reason := e.evaluateSLOBurn(context.Background(), r, guard)
	assert.Contains(t, reason, "fail-closed")
}

func TestSLOQueryFromCriterion_Translation(t *testing.T) {
	guard := &services.RolloutSLOBurnCriterion{
		ConnectorID:   "conn-1",
		Signal:        "metrics",
		Selector:      "slo:checkout:burn_rate",
		Aggregation:   "rate",
		Raw:           "sum(rate(errors[5m]))",
		WindowSeconds: 300,
		Matchers: []services.RolloutSLOMatcher{
			{Label: "service", Op: "=", Value: "checkout"},
		},
	}
	q := sloQueryFromCriterion(guard)
	assert.Equal(t, connectors.SignalMetrics, q.Signal)
	assert.Equal(t, "slo:checkout:burn_rate", q.Selector)
	assert.Equal(t, connectors.AggRate, q.Aggregation)
	assert.Equal(t, "sum(rate(errors[5m]))", q.Raw)
	require.Len(t, q.Matchers, 1)
	assert.Equal(t, connectors.LabelMatcher{Label: "service", Op: connectors.MatchEqual, Value: "checkout"}, q.Matchers[0])
	// WindowSeconds > 0 → a trailing range ending ~now.
	assert.WithinDuration(t, time.Now(), q.TimeRange.End, 2*time.Second)
	assert.WithinDuration(t, time.Now().Add(-300*time.Second), q.TimeRange.Start, 2*time.Second)
	// The translated query must be valid for a connector to accept.
	require.NoError(t, q.Validate())
}

func TestEngine_EvaluateAbortCriteria_SLOBurnFires(t *testing.T) {
	// Full path: no drift, no error-rate criterion, but a configured SLO
	// guard whose connector reports a breach — the rollout aborts through
	// the same evaluateAbortCriteria path every other criterion uses.
	stub := &stubAgentService{agents: makeAgents(2, "group-a")}
	fc := &fakeConnector{result: burnScalar(20, 14.4, true)}
	e := &Engine{
		agentService:      stub,
		logger:            zap.NewNop(),
		connectorResolver: &fakeResolver{conn: fc},
	}
	r := &services.Rollout{
		GroupID: "group-a",
		Stages:  []services.RolloutStage{{Percentage: 100}},
		AbortCriteria: services.RolloutAbortCriteria{
			MaxDriftedAgents: 0,
			SLOBurn:          sloGuard(),
		},
	}

	reason := e.evaluateAbortCriteria(context.Background(), r, r.Stages[0])
	assert.Contains(t, reason, "SLO burn", "SLO guard breach must abort via evaluateAbortCriteria")
}

func TestEngine_EvaluateAbortCriteria_SLOBurnInertWithoutResolver(t *testing.T) {
	// A guard is configured but no connector resolver is wired (the OSS
	// default). The criterion must be inert — no abort, no panic.
	stub := &stubAgentService{agents: makeAgents(2, "group-a")}
	e := &Engine{
		agentService: stub,
		logger:       zap.NewNop(),
		// connectorResolver deliberately nil.
	}
	r := &services.Rollout{
		GroupID: "group-a",
		Stages:  []services.RolloutStage{{Percentage: 100}},
		AbortCriteria: services.RolloutAbortCriteria{
			MaxDriftedAgents: 0,
			SLOBurn:          sloGuard(),
		},
	}

	reason := e.evaluateAbortCriteria(context.Background(), r, r.Stages[0])
	assert.Empty(t, reason, "SLO guard must be inert when no resolver is wired")
}

// --- RegistryConnectorResolver ---

type fakeConfigSource struct {
	cfg connectors.Config
	ok  bool
	err error
}

func (f fakeConfigSource) ConnectorConfig(_ context.Context, _ string) (connectors.Config, bool, error) {
	return f.cfg, f.ok, f.err
}

// notFoundCredStore is a CredentialStore whose Get always reports "no
// credentials", exercising the resolver's unauthenticated-backend path.
type notFoundCredStore struct{}

func (notFoundCredStore) Put(context.Context, string, connectors.ConnectorCredentials) error {
	return nil
}

func (notFoundCredStore) Get(context.Context, string) (*connectors.ConnectorCredentials, error) {
	return nil, connectors.ErrCredentialsNotFound
}
func (notFoundCredStore) Has(context.Context, string) (bool, error) { return false, nil }
func (notFoundCredStore) Delete(context.Context, string) error      { return nil }

func TestRegistryConnectorResolver_BuildsFromStoredConfig(t *testing.T) {
	reg := connectors.NewRegistry()
	var gotCfg connectors.Config
	require.NoError(t, reg.Register("fake", func(cfg connectors.Config, _ connectors.ConnectorCredentials) (connectors.Connector, error) {
		gotCfg = cfg
		return &fakeConnector{}, nil
	}))

	src := fakeConfigSource{cfg: connectors.Config{ID: "conn-1", Type: "fake", Endpoint: "https://example"}, ok: true}
	resolver := NewRegistryConnectorResolver(reg, src, notFoundCredStore{})

	conn, err := resolver.ResolveConnector(context.Background(), "conn-1")
	require.NoError(t, err)
	require.NotNil(t, conn)
	assert.Equal(t, "conn-1", gotCfg.ID, "the stored config must be passed to the factory")
}

func TestRegistryConnectorResolver_UnknownConnectorErrors(t *testing.T) {
	reg := connectors.NewRegistry()
	src := fakeConfigSource{ok: false}
	resolver := NewRegistryConnectorResolver(reg, src, nil)

	_, err := resolver.ResolveConnector(context.Background(), "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no connector configured")
}
