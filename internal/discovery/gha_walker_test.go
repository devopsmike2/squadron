// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/deploy"
	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeWalkerStore implements GHAWalkerStore.
type fakeWalkerStore struct {
	mu       sync.Mutex
	targets  []*apptypes.DeployTarget
	expected map[string]*apptypes.ExpectedAgent
	// ADR 0013 D6-a — capture the tenant resolved from the upsert ctx so
	// the isolation test can assert the expected_agents row landed in the
	// owning deploy target's tenant, not `default`.
	lastUpsertTenant string
}

func newFakeWalkerStore() *fakeWalkerStore {
	return &fakeWalkerStore{expected: map[string]*apptypes.ExpectedAgent{}}
}
func (f *fakeWalkerStore) ListDeployTargets(_ context.Context) ([]*apptypes.DeployTarget, error) {
	return f.targets, nil
}
func (f *fakeWalkerStore) GetDeployTarget(_ context.Context, id string) (*apptypes.DeployTarget, error) {
	for _, t := range f.targets {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, nil
}
func (f *fakeWalkerStore) UpsertExpectedAgent(ctx context.Context, e *apptypes.ExpectedAgent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUpsertTenant = effectiveWriteTenant(ctx)
	f.expected[e.Hostname] = e
	return nil
}

// fakeBridge implements the GHAWalkerDeployBridge interface.
type fakeBridge struct{ pat string }

func (b *fakeBridge) DecryptedPAT(_ context.Context, _ *apptypes.DeployTarget) (string, error) {
	return b.pat, nil
}

// fakeWalkerProvider implements deploy.Provider.
type fakeWalkerProvider struct {
	runs          []deploy.WorkflowRunSummary
	contentsByRef map[string]map[string][]byte // ref -> path -> content
}

func (p *fakeWalkerProvider) Dispatch(_ context.Context, _ *apptypes.DeployTarget, _ string, _ map[string]string) (string, error) {
	return "", nil
}
func (p *fakeWalkerProvider) GetRun(_ context.Context, _ *apptypes.DeployTarget, _ string, _ int64) (*deploy.RunStatus, error) {
	return nil, nil
}
func (p *fakeWalkerProvider) LatestRunSince(_ context.Context, _ *apptypes.DeployTarget, _ string, _ time.Time) (*deploy.RunStatus, error) {
	return nil, nil
}
func (p *fakeWalkerProvider) FetchFile(_ context.Context, _ *apptypes.DeployTarget, _ string, _ string) ([]byte, error) {
	return nil, nil
}
func (p *fakeWalkerProvider) ProbeAuth(_ context.Context, _ *apptypes.DeployTarget, _ string) error {
	return nil
}
func (p *fakeWalkerProvider) ProbeWorkflow(_ context.Context, _ *apptypes.DeployTarget, _ string) error {
	return nil
}
func (p *fakeWalkerProvider) ListSuccessfulRuns(_ context.Context, _ *apptypes.DeployTarget, _ string, _ time.Time) ([]deploy.WorkflowRunSummary, error) {
	return p.runs, nil
}
func (p *fakeWalkerProvider) FetchFileAtRef(_ context.Context, _ *apptypes.DeployTarget, _ string, path string, ref string) ([]byte, error) {
	if paths, ok := p.contentsByRef[ref]; ok {
		return paths[path], nil
	}
	return nil, nil
}

func TestGHAWalker_RegistersHostsFromHistoricalRuns(t *testing.T) {
	store := newFakeWalkerStore()
	store.targets = []*apptypes.DeployTarget{
		{
			ID:                  "t1",
			Name:                "Deploy otelcol to Windows",
			GitHubOwner:         "o",
			GitHubRepo:          "r",
			GitHubWorkflow:      "win_deploy.yml",
			GitHubBranch:        "main",
			InventoryPath:       "winOtel/ansible/inventory.ini",
			EncryptedCredential: []byte("not-empty"),
		},
	}
	provider := &fakeWalkerProvider{
		runs: []deploy.WorkflowRunSummary{
			{RunID: 95, HeadSHA: "sha-95", CreatedAt: time.Now().Add(-1 * time.Hour)},
			{RunID: 94, HeadSHA: "sha-94", CreatedAt: time.Now().Add(-24 * time.Hour)},
			// Older run with same SHA as 94 → should be deduped.
			{RunID: 92, HeadSHA: "sha-94", CreatedAt: time.Now().Add(-48 * time.Hour)},
		},
		contentsByRef: map[string]map[string][]byte{
			"sha-95": {
				"winOtel/ansible/inventory.ini": []byte("[windows]\nGAXGPAP158UA\nhost02\n"),
			},
			"sha-94": {
				"winOtel/ansible/inventory.ini": []byte("[windows]\nhost-retired\n"),
			},
		},
	}
	walker := NewGHAWalker(store, &fakeBridge{pat: "ghp_fake"}, provider,
		time.Hour, 30*24*time.Hour, zap.NewNop())

	if err := walker.WalkAll(context.Background()); err != nil {
		t.Fatalf("walk: %v", err)
	}

	// We expect GAXGPAP158UA + host02 (from sha-95) and host-retired
	// (from sha-94). sha-94 should only be fetched once because of
	// the dedup, even though two runs reference it.
	if len(store.expected) != 3 {
		t.Fatalf("expected 3 hosts upserted, got %d: %+v", len(store.expected), store.expected)
	}
	for _, host := range []string{"GAXGPAP158UA", "host02", "host-retired"} {
		e, ok := store.expected[host]
		if !ok {
			t.Errorf("host %q not registered", host)
			continue
		}
		if e.Source != "gha-history:t1" {
			t.Errorf("host %q source = %q, want gha-history:t1", host, e.Source)
		}
		if e.Notes == "" {
			t.Errorf("host %q missing notes", host)
		}
	}
}

func TestGHAWalker_SkipsTargetsWithoutInventoryPath(t *testing.T) {
	store := newFakeWalkerStore()
	store.targets = []*apptypes.DeployTarget{
		{
			ID:                  "t1",
			Name:                "manual",
			EncryptedCredential: []byte("not-empty"),
		},
	}
	provider := &fakeWalkerProvider{}
	walker := NewGHAWalker(store, &fakeBridge{pat: "x"}, provider, 0, 0, zap.NewNop())
	if err := walker.WalkAll(context.Background()); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(store.expected) != 0 {
		t.Errorf("expected no upserts; got %d", len(store.expected))
	}
}
