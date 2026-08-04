// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/testutils"
)

// stubTelemetry is a no-op TelemetryQueryService for the topology handler
// tests. Every method returns zero values; the empty-fleet topology test only
// exercises QueryRaw (services lookup), which returns no rows.
type stubTelemetry struct{}

func (stubTelemetry) QueryMetrics(_ context.Context, _ services.MetricQuery) ([]services.Metric, error) {
	return nil, nil
}
func (stubTelemetry) QueryLogs(_ context.Context, _ services.LogQuery) ([]services.Log, error) {
	return nil, nil
}
func (stubTelemetry) QueryTraces(_ context.Context, _ services.TraceQuery) ([]services.Trace, error) {
	return nil, nil
}
func (stubTelemetry) QueryRaw(_ context.Context, _ string, _ ...interface{}) ([]map[string]interface{}, error) {
	return nil, nil
}
func (stubTelemetry) CreateRollups(_ context.Context, _ time.Time, _ services.RollupInterval) error {
	return nil
}
func (stubTelemetry) QueryRollups(_ context.Context, _ services.RollupQuery) ([]services.Rollup, error) {
	return nil, nil
}
func (stubTelemetry) CleanupOldData(_ context.Context, _ time.Duration) error { return nil }
func (stubTelemetry) GetTelemetryOverview(_ context.Context) (*services.TelemetryOverview, error) {
	return nil, nil
}
func (stubTelemetry) GetServices(_ context.Context) ([]string, error) { return nil, nil }

// TestHandleGetTopology_EmptyFleet_ReturnsEmptyArraysNotNull locks the wire
// contract that blanked the Fleet Map in the OpenShift pilot (2026-08): with no
// agents or groups, GET /topology built nil slices, which Go marshals as JSON
// `null`. The ui types nodes/edges/groups/services as non-optional arrays and
// iterated them (`for...of null`), crashing the whole page on first load with no
// data. The handler must emit `[]`, never `null`.
func TestHandleGetTopology_EmptyFleet_ReturnsEmptyArraysNotNull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewTopologyHandlers(testutils.NewMockAgentService(), stubTelemetry{}, zap.NewNop())

	r := gin.New()
	r.GET("/topology", h.HandleGetTopology)
	req := httptest.NewRequest(http.MethodGet, "/topology", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("topology(empty): want 200, got %d (body %s)", w.Code, w.Body.String())
	}

	// Raw-body contract: the arrays must serialize as [] not null.
	body := w.Body.String()
	for _, field := range []string{`"nodes":[]`, `"edges":[]`, `"groups":[]`, `"services":[]`} {
		if !strings.Contains(body, field) {
			t.Fatalf("empty topology must contain %s (nil slice marshaled null?): %s", field, body)
		}
	}

	// And the decoded slices must be non-nil.
	var resp TopologyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal topology: %v", err)
	}
	if resp.Nodes == nil || resp.Edges == nil || resp.Groups == nil || resp.Services == nil {
		t.Fatalf("decoded slices must be non-nil: nodes=%v edges=%v groups=%v services=%v",
			resp.Nodes, resp.Edges, resp.Groups, resp.Services)
	}
}
