// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"go.uber.org/zap"
)

// driftFakeScanStore implements handlers.DiscoveryScanStore for the notifier.
type driftFakeScanStore struct {
	list []*types.ScanRecord          // returned newest-first by ListDiscoveryScans
	byID map[string]*types.ScanRecord // full records (with ResultJSON)
}

func (f *driftFakeScanStore) SaveDiscoveryScan(context.Context, *types.ScanRecord) error { return nil }
func (f *driftFakeScanStore) ListDiscoveryScans(_ context.Context, _, _ string, limit int) ([]*types.ScanRecord, error) {
	if limit > 0 && len(f.list) > limit {
		return f.list[:limit], nil
	}
	return f.list, nil
}
func (f *driftFakeScanStore) GetDiscoveryScan(_ context.Context, id string) (*types.ScanRecord, error) {
	return f.byID[id], nil
}

// driftFakeAudit captures Record calls.
type driftFakeAudit struct{ recorded []services.AuditEntry }

func (a *driftFakeAudit) Record(_ context.Context, e services.AuditEntry) error {
	a.recorded = append(a.recorded, e)
	return nil
}
func (a *driftFakeAudit) List(context.Context, services.AuditEventFilter) ([]*services.AuditEvent, error) {
	return nil, nil
}
func (a *driftFakeAudit) Get(context.Context, string) (*services.AuditEvent, error) { return nil, nil }
func (a *driftFakeAudit) SetExplanation(context.Context, string, string, string, time.Time) error {
	return nil
}

func twoScans(olderJSON, newerJSON string) *driftFakeScanStore {
	older := &types.ScanRecord{ScanID: "old", Provider: "aws", ScopeID: "111", StartedAt: time.Now().Add(-2 * time.Hour), ResultJSON: olderJSON}
	newer := &types.ScanRecord{ScanID: "new", Provider: "aws", ScopeID: "111", StartedAt: time.Now().Add(-1 * time.Hour), ResultJSON: newerJSON}
	return &driftFakeScanStore{
		list: []*types.ScanRecord{newer, older}, // newest-first
		byID: map[string]*types.ScanRecord{"old": older, "new": newer},
	}
}

func TestEmitDriftIfChanged_RecordsOnChange(t *testing.T) {
	store := twoScans(
		`{"compute":[{"resource_id":"i-1","has_otel":true}]}`,
		`{"compute":[{"resource_id":"i-1","has_otel":false},{"resource_id":"i-2","has_otel":false}]}`,
	)
	audit := &driftFakeAudit{}
	emitDriftIfChanged(context.Background(), store, audit, zap.NewNop(), newDriftEmitter(0), "aws", "111")
	if len(audit.recorded) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(audit.recorded))
	}
	e := audit.recorded[0]
	if e.EventType != "discovery.scan_drift_detected" {
		t.Errorf("event type = %q", e.EventType)
	}
	if e.Payload["total_added"].(int) != 1 {
		t.Errorf("total_added = %v, want 1", e.Payload["total_added"])
	}
	// i-1 flipped true->false = a regression.
	regs, _ := e.Payload["instrumentation_regressions"].([]string)
	if len(regs) != 1 || regs[0] != "i-1" {
		t.Errorf("regressions = %v, want [i-1]", regs)
	}
	added, _ := e.Payload["added"].([]string)
	if len(added) != 1 || added[0] != "i-2" {
		t.Errorf("added payload = %v, want [i-2]", added)
	}
}

func TestEmitDriftIfChanged_CooldownSuppressesSecond(t *testing.T) {
	mk := func() *driftFakeScanStore {
		return twoScans(
			`{"compute":[{"resource_id":"i-1","has_otel":true}]}`,
			`{"compute":[{"resource_id":"i-1","has_otel":false}]}`,
		)
	}
	audit := &driftFakeAudit{}
	emitter := newDriftEmitter(time.Hour)
	emitDriftIfChanged(context.Background(), mk(), audit, zap.NewNop(), emitter, "aws", "111")
	emitDriftIfChanged(context.Background(), mk(), audit, zap.NewNop(), emitter, "aws", "111")
	if len(audit.recorded) != 1 {
		t.Fatalf("cooldown should suppress the 2nd event; got %d", len(audit.recorded))
	}
	// A different scope is not suppressed.
	emitDriftIfChanged(context.Background(), mk(), audit, zap.NewNop(), emitter, "aws", "222")
	if len(audit.recorded) != 2 {
		t.Errorf("different scope should emit; got %d", len(audit.recorded))
	}
}

func TestEmitDriftIfChanged_NoEventOnNoChange(t *testing.T) {
	inv := `{"compute":[{"resource_id":"i-1","has_otel":true}]}`
	audit := &driftFakeAudit{}
	emitDriftIfChanged(context.Background(), twoScans(inv, inv), audit, zap.NewNop(), newDriftEmitter(0), "aws", "111")
	if len(audit.recorded) != 0 {
		t.Errorf("expected no audit event for identical scans, got %d", len(audit.recorded))
	}
}

func TestEmitDriftIfChanged_NoEventWithSingleScan(t *testing.T) {
	store := &driftFakeScanStore{
		list: []*types.ScanRecord{{ScanID: "only", Provider: "aws", ScopeID: "111", ResultJSON: `{}`}},
		byID: map[string]*types.ScanRecord{},
	}
	audit := &driftFakeAudit{}
	emitDriftIfChanged(context.Background(), store, audit, zap.NewNop(), newDriftEmitter(0), "aws", "111")
	if len(audit.recorded) != 0 {
		t.Errorf("first scan should not emit drift, got %d events", len(audit.recorded))
	}
}

// TestEmitDriftIfChanged_PartialNewerSuppresses pins the false-removal guard: a
// scheduled scan that hit an IAM/tier failure persists an empty inventory with
// Partial=true. Diffing it against the previous full scan would fabricate a
// "removed everything" drift event. The partial newer scan must suppress emit.
func TestEmitDriftIfChanged_PartialNewerSuppresses(t *testing.T) {
	store := twoScans(
		`{"compute":[{"resource_id":"i-1","has_otel":true},{"resource_id":"i-2","has_otel":true}]}`,
		`{"compute":[]}`, // partial scan saw nothing (AccessDenied), not a real teardown
	)
	store.byID["new"].Partial = true
	store.byID["new"].PartialReason = "AccessDenied: ec2:DescribeInstances"
	audit := &driftFakeAudit{}
	emitDriftIfChanged(context.Background(), store, audit, zap.NewNop(), newDriftEmitter(0), "aws", "111")
	if len(audit.recorded) != 0 {
		t.Fatalf("partial newer scan must not emit false-removal drift, got %d", len(audit.recorded))
	}
}

// TestEmitDriftIfChanged_PartialOlderSuppresses covers the reciprocal: once a
// full scan follows a persisted partial one, the partial is the older/baseline
// side and its missing resources would read as "added everything". Also skipped.
func TestEmitDriftIfChanged_PartialOlderSuppresses(t *testing.T) {
	store := twoScans(
		`{"compute":[]}`, // previous scan was partial (empty)
		`{"compute":[{"resource_id":"i-1","has_otel":true},{"resource_id":"i-2","has_otel":true}]}`,
	)
	store.byID["old"].Partial = true
	audit := &driftFakeAudit{}
	emitDriftIfChanged(context.Background(), store, audit, zap.NewNop(), newDriftEmitter(0), "aws", "111")
	if len(audit.recorded) != 0 {
		t.Fatalf("partial older scan must not emit false-added drift, got %d", len(audit.recorded))
	}
}

// TestEmitDriftIfChanged_BothFullStillEmits guards against over-suppression: two
// full scans with a real change must still emit (the fix only gates on Partial).
func TestEmitDriftIfChanged_BothFullStillEmits(t *testing.T) {
	store := twoScans(
		`{"compute":[{"resource_id":"i-1","has_otel":true}]}`,
		`{"compute":[{"resource_id":"i-1","has_otel":true},{"resource_id":"i-2","has_otel":true}]}`,
	)
	// Partial defaults to false on both records.
	audit := &driftFakeAudit{}
	emitDriftIfChanged(context.Background(), store, audit, zap.NewNop(), newDriftEmitter(0), "aws", "111")
	if len(audit.recorded) != 1 {
		t.Fatalf("two full scans with a real change must emit, got %d", len(audit.recorded))
	}
}

func TestEmitDriftIfChanged_NilStoreOrAuditNoPanic(t *testing.T) {
	emitDriftIfChanged(context.Background(), nil, &driftFakeAudit{}, zap.NewNop(), newDriftEmitter(0), "aws", "111")
	emitDriftIfChanged(context.Background(), twoScans("{}", "{}"), nil, zap.NewNop(), newDriftEmitter(0), "aws", "111")
}
