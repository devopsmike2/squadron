// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeStore is a minimal Store implementation for unit tests. Just
// enough to drive Reconcile through its branches.
type fakeStore struct {
	expected []*apptypes.ExpectedAgent
	actual   []*apptypes.Agent
}

func (f *fakeStore) ListExpectedAgents(_ context.Context, source string) ([]*apptypes.ExpectedAgent, error) {
	if source == "" {
		return f.expected, nil
	}
	out := []*apptypes.ExpectedAgent{}
	for _, e := range f.expected {
		if e.Source == source {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeStore) UpsertExpectedAgent(_ context.Context, e *apptypes.ExpectedAgent) error {
	f.expected = append(f.expected, e)
	return nil
}
func (f *fakeStore) DeleteExpectedAgent(_ context.Context, hostname string) error { return nil }
func (f *fakeStore) ReplaceExpectedAgentsForSource(_ context.Context, source string, entries []*apptypes.ExpectedAgent) error {
	f.expected = entries
	return nil
}
func (f *fakeStore) ListAgents(_ context.Context) ([]*apptypes.Agent, error) { return f.actual, nil }

func TestReconcile_HealthyAndMissing(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		expected: []*apptypes.ExpectedAgent{
			{Hostname: "host01", Source: "gha"},
			{Hostname: "host02", Source: "gha"},
			{Hostname: "host03", Source: "gha"},
		},
		actual: []*apptypes.Agent{
			// host01 connected and recent → healthy
			{ID: uuid.New(), Name: "host01", LastSeen: now.Add(-1 * time.Minute)},
			// host02 connected but quiet for > silent threshold → missing
			{ID: uuid.New(), Name: "host02", LastSeen: now.Add(-1 * time.Hour)},
			// host03 never connected
		},
	}
	svc := NewService(store, zap.NewNop())
	report, err := svc.Reconcile(context.Background(), "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Healthy != 1 {
		t.Errorf("Healthy = %d, want 1", report.Healthy)
	}
	if report.Missing != 2 {
		t.Errorf("Missing = %d, want 2 (one silent + one never-seen)", report.Missing)
	}
	if report.Unexpected != 0 {
		t.Errorf("Unexpected = %d, want 0", report.Unexpected)
	}
}

func TestReconcile_Unexpected(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		expected: []*apptypes.ExpectedAgent{
			{Hostname: "host01", Source: "gha"},
		},
		actual: []*apptypes.Agent{
			{ID: uuid.New(), Name: "host01", LastSeen: now},
			// host99 connected but not in the expected list
			{ID: uuid.New(), Name: "host99", LastSeen: now},
		},
	}
	svc := NewService(store, zap.NewNop())
	report, err := svc.Reconcile(context.Background(), "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Unexpected != 1 {
		t.Errorf("Unexpected = %d, want 1", report.Unexpected)
	}
	// Source-filtered view should hide unexpected — they belong to
	// nobody so they're not the filtered pipeline's problem.
	filtered, err := svc.Reconcile(context.Background(), "gha")
	if err != nil {
		t.Fatalf("filtered Reconcile: %v", err)
	}
	if filtered.Unexpected != 0 {
		t.Errorf("filtered Unexpected = %d, want 0 (source filter should hide unexpected)", filtered.Unexpected)
	}
}

func TestReconcile_FQDNNormalization(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		expected: []*apptypes.ExpectedAgent{
			// short name expected
			{Hostname: "host01", Source: "gha"},
		},
		actual: []*apptypes.Agent{
			// FQDN reported by the collector — should still match
			{ID: uuid.New(), Name: "host01.example.com", LastSeen: now},
		},
	}
	svc := NewService(store, zap.NewNop())
	report, err := svc.Reconcile(context.Background(), "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Healthy != 1 {
		t.Fatalf("FQDN normalization failed: Healthy=%d Missing=%d", report.Healthy, report.Missing)
	}
}

func TestReconcile_RowOrdering(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		expected: []*apptypes.ExpectedAgent{
			{Hostname: "zhealthy", Source: "gha"},
			{Hostname: "amissing", Source: "gha"},
		},
		actual: []*apptypes.Agent{
			{ID: uuid.New(), Name: "zhealthy", LastSeen: now},
			{ID: uuid.New(), Name: "unexpected1", LastSeen: now},
		},
	}
	svc := NewService(store, zap.NewNop())
	report, _ := svc.Reconcile(context.Background(), "")
	if len(report.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(report.Rows))
	}
	// missing should sort first
	if report.Rows[0].Status != StatusMissing {
		t.Errorf("first row status = %s, want missing (worst-first)", report.Rows[0].Status)
	}
}
