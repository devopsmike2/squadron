// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeStore is a minimal Store impl for service tests.
type fakeStore struct {
	targets  map[string]*apptypes.DeployTarget
	runs     map[string]*apptypes.DeployRun
	configs  map[string]*apptypes.Config
	expected map[string]*apptypes.ExpectedAgent
	agents   []*apptypes.Agent
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		targets:  map[string]*apptypes.DeployTarget{},
		runs:     map[string]*apptypes.DeployRun{},
		configs:  map[string]*apptypes.Config{},
		expected: map[string]*apptypes.ExpectedAgent{},
	}
}

func (f *fakeStore) CreateDeployTarget(_ context.Context, t *apptypes.DeployTarget) error {
	f.targets[t.ID] = t
	return nil
}
func (f *fakeStore) UpdateDeployTarget(_ context.Context, t *apptypes.DeployTarget) error {
	f.targets[t.ID] = t
	return nil
}
func (f *fakeStore) GetDeployTarget(_ context.Context, id string) (*apptypes.DeployTarget, error) {
	return f.targets[id], nil
}
func (f *fakeStore) ListDeployTargets(_ context.Context) ([]*apptypes.DeployTarget, error) {
	out := []*apptypes.DeployTarget{}
	for _, t := range f.targets {
		out = append(out, t)
	}
	return out, nil
}
func (f *fakeStore) DeleteDeployTarget(_ context.Context, id string) error {
	delete(f.targets, id)
	return nil
}
func (f *fakeStore) CreateDeployRun(_ context.Context, r *apptypes.DeployRun) error {
	f.runs[r.ID] = r
	return nil
}
func (f *fakeStore) UpdateDeployRun(_ context.Context, r *apptypes.DeployRun) error {
	f.runs[r.ID] = r
	return nil
}
func (f *fakeStore) GetDeployRun(_ context.Context, id string) (*apptypes.DeployRun, error) {
	return f.runs[id], nil
}
func (f *fakeStore) ListDeployRuns(_ context.Context, _ apptypes.DeployRunFilter) ([]*apptypes.DeployRun, error) {
	out := []*apptypes.DeployRun{}
	for _, r := range f.runs {
		out = append(out, r)
	}
	return out, nil
}
func (f *fakeStore) GetConfig(_ context.Context, id string) (*apptypes.Config, error) {
	return f.configs[id], nil
}
func (f *fakeStore) UpsertExpectedAgent(_ context.Context, e *apptypes.ExpectedAgent) error {
	f.expected[e.Hostname] = e
	return nil
}
func (f *fakeStore) ListAgents(_ context.Context) ([]*apptypes.Agent, error) {
	return f.agents, nil
}

// fakeProvider records every Dispatch call and returns a canned
// status from GetRun / LatestRunSince.
type fakeProvider struct {
	dispatched       []map[string]string
	latest           *RunStatus
	getRunResponse   *RunStatus
	fetched          map[string][]byte // path → content for FetchFile
	refFetched       map[string][]byte // "ref:path" → content for FetchFileAtRef
	runsList         []WorkflowRunSummary
	probeAuthErr     error
	probeWorkflowErr error
}

func (p *fakeProvider) Dispatch(_ context.Context, _ *apptypes.DeployTarget, _ string, inputs map[string]string) (string, error) {
	p.dispatched = append(p.dispatched, inputs)
	return "", nil
}
func (p *fakeProvider) GetRun(_ context.Context, _ *apptypes.DeployTarget, _ string, _ int64) (*RunStatus, error) {
	return p.getRunResponse, nil
}
func (p *fakeProvider) LatestRunSince(_ context.Context, _ *apptypes.DeployTarget, _ string, _ time.Time) (*RunStatus, error) {
	return p.latest, nil
}
func (p *fakeProvider) FetchFile(_ context.Context, _ *apptypes.DeployTarget, _ string, path string) ([]byte, error) {
	if p.fetched == nil {
		return nil, nil
	}
	if v, ok := p.fetched[path]; ok {
		return v, nil
	}
	return nil, nil
}
func (p *fakeProvider) ProbeAuth(_ context.Context, _ *apptypes.DeployTarget, _ string) error {
	if p.probeAuthErr != nil {
		return p.probeAuthErr
	}
	return nil
}
func (p *fakeProvider) ProbeWorkflow(_ context.Context, _ *apptypes.DeployTarget, _ string) error {
	if p.probeWorkflowErr != nil {
		return p.probeWorkflowErr
	}
	return nil
}
func (p *fakeProvider) ListSuccessfulRuns(_ context.Context, _ *apptypes.DeployTarget, _ string, _ time.Time) ([]WorkflowRunSummary, error) {
	return p.runsList, nil
}
func (p *fakeProvider) FetchFileAtRef(_ context.Context, _ *apptypes.DeployTarget, _ string, path string, ref string) ([]byte, error) {
	if p.refFetched != nil {
		if v, ok := p.refFetched[ref+":"+path]; ok {
			return v, nil
		}
	}
	return nil, nil
}

func TestService_Trigger_LintHardBlock(t *testing.T) {
	store := newFakeStore()
	provider := &fakeProvider{}
	crypter := testCrypter(t)

	store.configs["cfg-broken"] = &apptypes.Config{
		ID: "cfg-broken",
		// Force a real lint error: empty pipeline + unknown
		// receiver. The configlint package treats no receivers in a
		// pipeline as an error.
		Content: "receivers:\nservice:\n  pipelines:\n    metrics:\n      receivers: []\n      exporters: []\n",
	}
	sealed, _ := crypter.Encrypt([]byte("ghp_fake"))
	store.targets["t1"] = &apptypes.DeployTarget{
		ID:                  "t1",
		Name:                "lint-block test",
		GitHubOwner:         "o",
		GitHubRepo:          "r",
		GitHubWorkflow:      "w.yml",
		GitHubBranch:        "main",
		EncryptedCredential: sealed,
		ConfigID:            "cfg-broken",
	}

	svc := NewService(store, provider, crypter, zap.NewNop())
	_, err := svc.Trigger(context.Background(), TriggerRequest{TargetID: "t1"})
	if err == nil {
		t.Fatal("expected lint to block deploy, got nil error")
	}
	var lerr *LintGateError
	if !errors.As(err, &lerr) {
		t.Fatalf("expected LintGateError, got %T: %v", err, err)
	}
	if len(provider.dispatched) != 0 {
		t.Fatalf("dispatch should not have run when lint blocks: %d calls", len(provider.dispatched))
	}
}

func TestService_Trigger_MergesInputs(t *testing.T) {
	store := newFakeStore()
	provider := &fakeProvider{}
	crypter := testCrypter(t)
	sealed, _ := crypter.Encrypt([]byte("ghp_fake"))
	store.targets["t1"] = &apptypes.DeployTarget{
		ID:                  "t1",
		Name:                "merge test",
		GitHubOwner:         "o",
		GitHubRepo:          "r",
		GitHubWorkflow:      "w.yml",
		GitHubBranch:        "main",
		EncryptedCredential: sealed,
		DefaultInputs:       map[string]string{"env": "prod", "region": "us-east-1"},
	}
	svc := NewService(store, provider, crypter, zap.NewNop())
	_, err := svc.Trigger(context.Background(), TriggerRequest{
		TargetID: "t1",
		Inputs:   map[string]string{"region": "us-west-2"}, // overrides
	})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if len(provider.dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(provider.dispatched))
	}
	got := provider.dispatched[0]
	if got["env"] != "prod" {
		t.Errorf("default not preserved: env=%s", got["env"])
	}
	if got["region"] != "us-west-2" {
		t.Errorf("override not applied: region=%s", got["region"])
	}
}

func TestService_SyncRun_AutoRegistersExpectedHosts(t *testing.T) {
	store := newFakeStore()
	crypter := testCrypter(t)
	sealed, _ := crypter.Encrypt([]byte("ghp_fake"))
	store.targets["t1"] = &apptypes.DeployTarget{
		ID:                  "t1",
		Name:                "expected-hosts test",
		GitHubOwner:         "o",
		GitHubRepo:          "r",
		GitHubWorkflow:      "w.yml",
		EncryptedCredential: sealed,
	}
	run := &apptypes.DeployRun{
		ID:            "run-1",
		TargetID:      "t1",
		Status:        "in_progress",
		GitHubRunID:   42,
		RequestedAt:   time.Now().UTC(),
		ExpectedHosts: []string{"host01", "host02", "host03"},
	}
	store.runs["run-1"] = run

	completedAt := time.Now().UTC()
	provider := &fakeProvider{
		getRunResponse: &RunStatus{
			GitHubRunID: 42,
			Status:      "completed",
			Conclusion:  "success",
			CompletedAt: &completedAt,
		},
	}
	svc := NewService(store, provider, crypter, zap.NewNop())
	updated, err := svc.SyncRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if updated.Conclusion != "success" {
		t.Fatalf("expected conclusion=success, got %q", updated.Conclusion)
	}
	if len(store.expected) != 3 {
		t.Fatalf("expected 3 hosts auto-registered, got %d", len(store.expected))
	}
	for _, host := range []string{"host01", "host02", "host03"} {
		e, ok := store.expected[host]
		if !ok {
			t.Errorf("host %s not registered", host)
			continue
		}
		if !strings.HasPrefix(e.Source, "squadron-deploy:") {
			t.Errorf("expected source prefix, got %q", e.Source)
		}
	}
}

func TestService_Trigger_UsesInventoryPathWhenSet(t *testing.T) {
	store := newFakeStore()
	crypter := testCrypter(t)
	sealed, _ := crypter.Encrypt([]byte("ghp_fake"))
	store.targets["t1"] = &apptypes.DeployTarget{
		ID:                  "t1",
		Name:                "inventory-driven",
		GitHubOwner:         "o",
		GitHubRepo:          "r",
		GitHubWorkflow:      "w.yml",
		GitHubBranch:        "main",
		EncryptedCredential: sealed,
		InventoryPath:       "winOtel/ansible/inventory.ini",
	}
	provider := &fakeProvider{
		fetched: map[string][]byte{
			"winOtel/ansible/inventory.ini": []byte("[windows]\n#10.10.40.7\nGAXGPAP158UA\n"),
		},
	}
	svc := NewService(store, provider, crypter, zap.NewNop())

	// Even though the caller passes ExpectedHosts, the inventory file
	// is the source of truth and overrides them.
	run, err := svc.Trigger(context.Background(), TriggerRequest{
		TargetID:      "t1",
		ExpectedHosts: []string{"hacker-injected-host"},
	})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if len(run.ExpectedHosts) != 1 || run.ExpectedHosts[0] != "GAXGPAP158UA" {
		t.Fatalf("expected hosts from inventory.ini, got %v", run.ExpectedHosts)
	}
}

func TestService_FetchInventory(t *testing.T) {
	store := newFakeStore()
	crypter := testCrypter(t)
	sealed, _ := crypter.Encrypt([]byte("ghp_fake"))
	store.targets["t1"] = &apptypes.DeployTarget{
		ID:                  "t1",
		Name:                "inv preview",
		EncryptedCredential: sealed,
		InventoryPath:       "winOtel/ansible/inventory.ini",
	}
	provider := &fakeProvider{
		fetched: map[string][]byte{
			"winOtel/ansible/inventory.ini": []byte("[windows]\nGAXGPAP158UA\nhost02\n"),
		},
	}
	svc := NewService(store, provider, crypter, zap.NewNop())
	path, hosts, err := svc.FetchInventory(context.Background(), "t1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if path != "winOtel/ansible/inventory.ini" {
		t.Errorf("path = %q", path)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %v", hosts)
	}

	// Target with no inventory_path returns empty without error.
	store.targets["t2"] = &apptypes.DeployTarget{
		ID: "t2", EncryptedCredential: sealed,
	}
	if path, hosts, err := svc.FetchInventory(context.Background(), "t2"); err != nil || path != "" || hosts != nil {
		t.Fatalf("no-inventory target: path=%q hosts=%v err=%v", path, hosts, err)
	}
}

func TestService_Disabled(t *testing.T) {
	svc := NewService(newFakeStore(), &fakeProvider{}, nil, zap.NewNop())
	if svc.Enabled() {
		t.Fatal("nil crypter should disable the service")
	}
	if _, err := svc.Trigger(context.Background(), TriggerRequest{}); err == nil {
		t.Fatal("expected Trigger to fail when service is disabled")
	}
}
