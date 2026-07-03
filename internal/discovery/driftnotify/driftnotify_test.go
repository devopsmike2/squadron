// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package driftnotify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"go.uber.org/zap"
)

// fakeScanStore implements ScanStore for the notifier.
type fakeScanStore struct {
	list []*types.ScanRecord          // returned newest-first by ListDiscoveryScans
	byID map[string]*types.ScanRecord // full records (with ResultJSON)
	err  error                        // when non-nil, ListDiscoveryScans returns it
}

func (f *fakeScanStore) ListDiscoveryScans(_ context.Context, _, _ string, limit int) ([]*types.ScanRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	if limit > 0 && len(f.list) > limit {
		return f.list[:limit], nil
	}
	return f.list, nil
}
func (f *fakeScanStore) GetDiscoveryScan(_ context.Context, id string) (*types.ScanRecord, error) {
	return f.byID[id], nil
}

// fakeAudit captures Record calls.
type fakeAudit struct{ recorded []AuditEntry }

func (a *fakeAudit) Record(_ context.Context, e AuditEntry) error {
	a.recorded = append(a.recorded, e)
	return nil
}

func twoScans(olderJSON, newerJSON string) *fakeScanStore {
	older := &types.ScanRecord{ScanID: "old", Provider: "aws", ScopeID: "111", StartedAt: time.Now().Add(-2 * time.Hour), ResultJSON: olderJSON}
	newer := &types.ScanRecord{ScanID: "new", Provider: "aws", ScopeID: "111", StartedAt: time.Now().Add(-1 * time.Hour), ResultJSON: newerJSON}
	return &fakeScanStore{
		list: []*types.ScanRecord{newer, older}, // newest-first
		byID: map[string]*types.ScanRecord{"old": older, "new": newer},
	}
}

func TestEmitIfChanged_RecordsOnChange(t *testing.T) {
	store := twoScans(
		`{"compute":[{"resource_id":"i-1","has_otel":true}]}`,
		`{"compute":[{"resource_id":"i-1","has_otel":false},{"resource_id":"i-2","has_otel":false}]}`,
	)
	audit := &fakeAudit{}
	EmitIfChanged(context.Background(), store, audit, zap.NewNop(), NewEmitter(0), "aws", "111")
	if len(audit.recorded) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(audit.recorded))
	}
	e := audit.recorded[0]
	if e.EventType != "discovery.scan_drift_detected" {
		t.Errorf("event type = %q", e.EventType)
	}
	if e.Actor != "system" || e.TargetType != "cloud_connection" {
		t.Errorf("actor/target = %q/%q, want system/cloud_connection", e.Actor, e.TargetType)
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

func TestEmitIfChanged_CooldownSuppressesSecond(t *testing.T) {
	mk := func() *fakeScanStore {
		return twoScans(
			`{"compute":[{"resource_id":"i-1","has_otel":true}]}`,
			`{"compute":[{"resource_id":"i-1","has_otel":false}]}`,
		)
	}
	audit := &fakeAudit{}
	emitter := NewEmitter(time.Hour)
	EmitIfChanged(context.Background(), mk(), audit, zap.NewNop(), emitter, "aws", "111")
	EmitIfChanged(context.Background(), mk(), audit, zap.NewNop(), emitter, "aws", "111")
	if len(audit.recorded) != 1 {
		t.Fatalf("cooldown should suppress the 2nd event; got %d", len(audit.recorded))
	}
	// A different scope is not suppressed.
	EmitIfChanged(context.Background(), mk(), audit, zap.NewNop(), emitter, "aws", "222")
	if len(audit.recorded) != 2 {
		t.Errorf("different scope should emit; got %d", len(audit.recorded))
	}
}

func TestEmitIfChanged_NoEventOnNoChange(t *testing.T) {
	inv := `{"compute":[{"resource_id":"i-1","has_otel":true}]}`
	audit := &fakeAudit{}
	EmitIfChanged(context.Background(), twoScans(inv, inv), audit, zap.NewNop(), NewEmitter(0), "aws", "111")
	if len(audit.recorded) != 0 {
		t.Errorf("expected no audit event for identical scans, got %d", len(audit.recorded))
	}
}

func TestEmitIfChanged_NoEventWithSingleScan(t *testing.T) {
	store := &fakeScanStore{
		list: []*types.ScanRecord{{ScanID: "only", Provider: "aws", ScopeID: "111", ResultJSON: `{}`}},
		byID: map[string]*types.ScanRecord{},
	}
	audit := &fakeAudit{}
	EmitIfChanged(context.Background(), store, audit, zap.NewNop(), NewEmitter(0), "aws", "111")
	if len(audit.recorded) != 0 {
		t.Errorf("first scan should not emit drift, got %d events", len(audit.recorded))
	}
}

// TestEmitIfChanged_PartialNewerSuppresses pins the false-removal guard: a
// scheduled scan that hit an IAM/tier failure persists an empty inventory with
// Partial=true. Diffing it against the previous full scan would fabricate a
// "removed everything" drift event. The partial newer scan must suppress emit.
func TestEmitIfChanged_PartialNewerSuppresses(t *testing.T) {
	store := twoScans(
		`{"compute":[{"resource_id":"i-1","has_otel":true},{"resource_id":"i-2","has_otel":true}]}`,
		`{"compute":[]}`, // partial scan saw nothing (AccessDenied), not a real teardown
	)
	store.byID["new"].Partial = true
	store.byID["new"].PartialReason = "AccessDenied: ec2:DescribeInstances"
	audit := &fakeAudit{}
	EmitIfChanged(context.Background(), store, audit, zap.NewNop(), NewEmitter(0), "aws", "111")
	if len(audit.recorded) != 0 {
		t.Fatalf("partial newer scan must not emit false-removal drift, got %d", len(audit.recorded))
	}
}

// TestEmitIfChanged_PartialOlderSuppresses covers the reciprocal: once a full
// scan follows a persisted partial one, the partial is the older/baseline side
// and its missing resources would read as "added everything". Also skipped.
func TestEmitIfChanged_PartialOlderSuppresses(t *testing.T) {
	store := twoScans(
		`{"compute":[]}`, // previous scan was partial (empty)
		`{"compute":[{"resource_id":"i-1","has_otel":true},{"resource_id":"i-2","has_otel":true}]}`,
	)
	store.byID["old"].Partial = true
	audit := &fakeAudit{}
	EmitIfChanged(context.Background(), store, audit, zap.NewNop(), NewEmitter(0), "aws", "111")
	if len(audit.recorded) != 0 {
		t.Fatalf("partial older scan must not emit false-added drift, got %d", len(audit.recorded))
	}
}

// TestEmitIfChanged_BothFullStillEmits guards against over-suppression: two full
// scans with a real change must still emit (the fix only gates on Partial).
func TestEmitIfChanged_BothFullStillEmits(t *testing.T) {
	store := twoScans(
		`{"compute":[{"resource_id":"i-1","has_otel":true}]}`,
		`{"compute":[{"resource_id":"i-1","has_otel":true},{"resource_id":"i-2","has_otel":true}]}`,
	)
	audit := &fakeAudit{}
	EmitIfChanged(context.Background(), store, audit, zap.NewNop(), NewEmitter(0), "aws", "111")
	if len(audit.recorded) != 1 {
		t.Fatalf("two full scans with a real change must emit, got %d", len(audit.recorded))
	}
}

// TestEmitIfChanged_StoreErrorFailOpen: a store error must not emit and must not
// panic (fail-open); the warn log makes the otherwise-silent loss diagnosable.
func TestEmitIfChanged_StoreErrorFailOpen(t *testing.T) {
	store := &fakeScanStore{err: errors.New("db unavailable")}
	audit := &fakeAudit{}
	EmitIfChanged(context.Background(), store, audit, zap.NewNop(), NewEmitter(0), "aws", "111")
	if len(audit.recorded) != 0 {
		t.Fatalf("store error must not emit a drift event, got %d", len(audit.recorded))
	}
	// Nil-logger path must also not panic.
	EmitIfChanged(context.Background(), store, audit, nil, NewEmitter(0), "aws", "111")
}

func TestEmitIfChanged_NilStoreOrAuditNoPanic(t *testing.T) {
	EmitIfChanged(context.Background(), nil, &fakeAudit{}, zap.NewNop(), NewEmitter(0), "aws", "111")
	EmitIfChanged(context.Background(), twoScans("{}", "{}"), nil, zap.NewNop(), NewEmitter(0), "aws", "111")
}
