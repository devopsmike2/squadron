// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// countExported returns how many audit.exported self-audit rows the service
// holds — used to assert exports are (and list-polls are not) self-audited.
func countExported(t *testing.T, svc services.AuditService) int {
	t.Helper()
	evs, err := svc.List(t.Context(), services.AuditEventFilter{EventType: "audit.exported"})
	require.NoError(t, err)
	return len(evs)
}

// TestHandleListAuditEvents_CSVExport pins ADR 0020's OSS breadth slice: a
// ?format=csv request downloads a tenant-scoped CSV attachment (header + one row
// per event, freeform payload as a JSON column) and self-audits the export.
func TestHandleListAuditEvents_CSVExport(t *testing.T) {
	h, svc := setupAuditHandlers(t)
	ctx := t.Context()
	for i := 0; i < 2; i++ {
		require.NoError(t, svc.Record(ctx, services.AuditEntry{
			Actor: services.AuditActorSystem, EventType: "x.y", TargetType: services.AuditTargetAgent,
			TargetID: "a", Action: "z", Payload: map[string]any{"k": "v"},
		}))
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/audit/events?format=csv", nil)
	h.HandleListAuditEvents(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/csv")
	assert.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	assert.Contains(t, w.Header().Get("Content-Disposition"), ".csv")

	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	require.NoError(t, err)
	require.Len(t, rows, 3, "header + 2 data rows")
	assert.Equal(t, auditCSVHeader, rows[0])
	// payload column is the last one, JSON-encoded.
	assert.Contains(t, rows[1][len(auditCSVHeader)-1], `"k":"v"`)

	assert.Equal(t, 1, countExported(t, svc), "the CSV export must be self-audited exactly once")
}

// TestHandleListAuditEvents_JSONExportSelfAudits pins that ?format=json is an
// export (attachment + self-audit), distinct from the default list poll.
func TestHandleListAuditEvents_JSONExportSelfAudits(t *testing.T) {
	h, svc := setupAuditHandlers(t)
	require.NoError(t, svc.Record(t.Context(), services.AuditEntry{
		Actor: services.AuditActorSystem, EventType: "x.y", TargetType: services.AuditTargetAgent, TargetID: "a", Action: "z",
	}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/audit/events?format=json", nil)
	h.HandleListAuditEvents(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	assert.Contains(t, w.Body.String(), `"events"`)
	assert.Equal(t, 1, countExported(t, svc))
}

// TestHandleListAuditEvents_DefaultNotSelfAudited pins that the plain list
// (no format) is NOT self-audited — the /audit page polls it and would flood
// the log otherwise.
func TestHandleListAuditEvents_DefaultNotSelfAudited(t *testing.T) {
	h, svc := setupAuditHandlers(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/audit/events", nil)
	h.HandleListAuditEvents(c)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Content-Disposition"))
	assert.Equal(t, 0, countExported(t, svc), "a plain list poll must not be self-audited")
}

// TestHandleListAuditEvents_ActorFilter pins the ?actor= filter (ADR 0020,
// backs per-actor access-review timelines): only rows by the named actor return.
func TestHandleListAuditEvents_ActorFilter(t *testing.T) {
	h, svc := setupAuditHandlers(t)
	ctx := t.Context()
	require.NoError(t, svc.Record(ctx, services.AuditEntry{
		Actor: "operator:alice@x.io", EventType: "x.y", TargetType: services.AuditTargetAgent, TargetID: "a", Action: "z",
	}))
	require.NoError(t, svc.Record(ctx, services.AuditEntry{
		Actor: "operator:bob@x.io", EventType: "x.y", TargetType: services.AuditTargetAgent, TargetID: "b", Action: "z",
	}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/audit/events?actor=operator:alice@x.io", nil)
	h.HandleListAuditEvents(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Events []services.AuditEvent `json:"events"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Events, 1)
	assert.Equal(t, "operator:alice@x.io", resp.Events[0].Actor)
}

// TestHandleListAuditEvents_InvalidFormat pins the 400 on an unknown format.
func TestHandleListAuditEvents_InvalidFormat(t *testing.T) {
	h, _ := setupAuditHandlers(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/audit/events?format=xml", nil)
	h.HandleListAuditEvents(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func setupAuditHandlers(t *testing.T) (*AuditHandlers, services.AuditService) {
	t.Helper()
	svc := services.NewAuditService(memory.NewStore(), nil, zap.NewNop())
	return NewAuditHandlers(svc, nil, nil, zap.NewNop()), svc
}

func TestHandleListAuditEvents_ReturnsRecorded(t *testing.T) {
	h, svc := setupAuditHandlers(t)
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		require.NoError(t, svc.Record(ctx, services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  "test.event",
			TargetType: services.AuditTargetAgent,
			TargetID:   "agent-x",
			Action:     "tick",
		}))
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/audit/events", nil)
	h.HandleListAuditEvents(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Events []services.AuditEvent `json:"events"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Events, 3)
}

func TestHandleListAuditEvents_EmptyReturnsNonNilArray(t *testing.T) {
	h, _ := setupAuditHandlers(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/audit/events", nil)
	h.HandleListAuditEvents(c)
	require.Equal(t, http.StatusOK, w.Code)

	// The UI depends on .length without a null guard. Verify the field
	// serializes to [] when the result is empty, not null.
	assert.Contains(t, w.Body.String(), `"events":[]`)
}

func TestHandleListAuditEvents_TargetFilter(t *testing.T) {
	h, svc := setupAuditHandlers(t)
	ctx := t.Context()

	require.NoError(t, svc.Record(ctx, services.AuditEntry{
		Actor: services.AuditActorSystem, EventType: "x.y", TargetType: services.AuditTargetAgent, TargetID: "a", Action: "z",
	}))
	require.NoError(t, svc.Record(ctx, services.AuditEntry{
		Actor: services.AuditActorSystem, EventType: "x.y", TargetType: services.AuditTargetConfig, TargetID: "c", Action: "z",
	}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/audit/events?target_type=agent", nil)
	h.HandleListAuditEvents(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Events []services.AuditEvent `json:"events"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Events, 1)
	assert.Equal(t, services.AuditTargetAgent, resp.Events[0].TargetType)
}

func TestHandleListAuditEvents_EventTypeFilter(t *testing.T) {
	// Regression guard for #580 (v0.87.2): the handler used to read
	// target_type / target_id / since / limit but silently ignored
	// event_type, returning the full unfiltered set. The client side
	// dedupes on event_type as a workaround, but that masks
	// "filter ignored" from "no matching rows". This test seeds two
	// distinct event types and pins that ?event_type=X returns only
	// rows whose event_type is X.
	h, svc := setupAuditHandlers(t)
	ctx := t.Context()

	const wanted = "discovery.aws.connection_created"
	const other = "discovery.aws.connection_read"

	for i := 0; i < 3; i++ {
		require.NoError(t, svc.Record(ctx, services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  wanted,
			TargetType: "aws_connection",
			TargetID:   "acct-w",
			Action:     "created",
		}))
	}
	for i := 0; i < 5; i++ {
		require.NoError(t, svc.Record(ctx, services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  other,
			TargetType: "aws_connection",
			TargetID:   "acct-r",
			Action:     "read",
		}))
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet,
		"/api/v1/audit/events?event_type="+wanted, nil)
	h.HandleListAuditEvents(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Events []services.AuditEvent `json:"events"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// Count must match the seeded "wanted" count exactly — if the
	// handler regresses to ignoring event_type, this returns 8.
	require.Len(t, resp.Events, 3,
		"expected only the 3 %q rows; if this is 8 the handler ignored event_type", wanted)

	// Every returned row must carry the requested event_type — pins
	// that the filter is doing the right thing, not just returning
	// the right count by coincidence.
	for i, ev := range resp.Events {
		assert.Equal(t, wanted, ev.EventType,
			"row[%d].event_type = %q, want %q", i, ev.EventType, wanted)
	}
}

func TestHandleListAuditEvents_RejectsBadSince(t *testing.T) {
	h, _ := setupAuditHandlers(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/audit/events?since=yesterday", nil)
	h.HandleListAuditEvents(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleListAuditEvents_SinceFilter(t *testing.T) {
	h, svc := setupAuditHandlers(t)
	ctx := t.Context()

	require.NoError(t, svc.Record(ctx, services.AuditEntry{
		Actor: services.AuditActorSystem, EventType: "test", TargetType: "x", Action: "first",
	}))
	cutoff := time.Now().UTC().Add(50 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, svc.Record(ctx, services.AuditEntry{
		Actor: services.AuditActorSystem, EventType: "test", TargetType: "x", Action: "second",
	}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/audit/events?since="+cutoff.Format(time.RFC3339Nano), nil)
	h.HandleListAuditEvents(c)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Events []services.AuditEvent `json:"events"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Events, 1, "since filter should exclude the earlier event")
	assert.Equal(t, "second", resp.Events[0].Action)
}
