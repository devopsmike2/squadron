// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"context"
	"fmt"
	"time"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// MultiProvider routes each Provider call to the right underlying
// implementation based on target.Provider. It's the simplest path
// to multi-backend support: every call site in service.go already
// gets the *DeployTarget; the router uses its Provider field to pick.
//
// Today we ship two backends — "github" and "azure_devops" — but the
// pattern accepts more without changing the Service surface. When we
// add Ansible Tower in v0.42, it slots in here with one extra case.
//
// Empty Provider falls back to GitHub for back-compat. Targets
// created before v0.41 don't have the field populated in the UI
// flow and we'd rather avoid forcing a migration just for that.
//
// Added in v0.41.0 (connectors part 1: Azure DevOps).
type MultiProvider struct {
	github      Provider
	azureDevOps Provider
}

// NewMultiProvider constructs a router holding both backends. Either
// argument may be nil — calls routed to a nil backend return a
// typed error so the UI surfaces "provider not configured" rather
// than a runtime panic.
func NewMultiProvider(github, azureDevOps Provider) *MultiProvider {
	return &MultiProvider{github: github, azureDevOps: azureDevOps}
}

// pick returns the right provider for a target, or an error if no
// implementation is wired for that target's provider string.
func (m *MultiProvider) pick(target *apptypes.DeployTarget) (Provider, error) {
	switch target.Provider {
	case "", "github":
		if m.github == nil {
			return nil, fmt.Errorf("github provider not configured")
		}
		return m.github, nil
	case "azure_devops":
		if m.azureDevOps == nil {
			return nil, fmt.Errorf("azure_devops provider not configured")
		}
		return m.azureDevOps, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", target.Provider)
	}
}

// Provider interface methods — each one picks then forwards. Keeping
// the dispatch trivial means the routing logic stays here in one
// place rather than sprinkled across the Service.

func (m *MultiProvider) Dispatch(ctx context.Context, target *apptypes.DeployTarget, pat string, inputs map[string]string) (string, error) {
	p, err := m.pick(target)
	if err != nil {
		return "", err
	}
	return p.Dispatch(ctx, target, pat, inputs)
}

func (m *MultiProvider) GetRun(ctx context.Context, target *apptypes.DeployTarget, pat string, runID int64) (*RunStatus, error) {
	p, err := m.pick(target)
	if err != nil {
		return nil, err
	}
	return p.GetRun(ctx, target, pat, runID)
}

func (m *MultiProvider) LatestRunSince(ctx context.Context, target *apptypes.DeployTarget, pat string, since time.Time) (*RunStatus, error) {
	p, err := m.pick(target)
	if err != nil {
		return nil, err
	}
	return p.LatestRunSince(ctx, target, pat, since)
}

func (m *MultiProvider) FetchFile(ctx context.Context, target *apptypes.DeployTarget, pat string, path string) ([]byte, error) {
	p, err := m.pick(target)
	if err != nil {
		return nil, err
	}
	return p.FetchFile(ctx, target, pat, path)
}

func (m *MultiProvider) ProbeAuth(ctx context.Context, target *apptypes.DeployTarget, pat string) error {
	p, err := m.pick(target)
	if err != nil {
		return err
	}
	return p.ProbeAuth(ctx, target, pat)
}

func (m *MultiProvider) ProbeWorkflow(ctx context.Context, target *apptypes.DeployTarget, pat string) error {
	p, err := m.pick(target)
	if err != nil {
		return err
	}
	return p.ProbeWorkflow(ctx, target, pat)
}

func (m *MultiProvider) ListSuccessfulRuns(ctx context.Context, target *apptypes.DeployTarget, pat string, since time.Time) ([]WorkflowRunSummary, error) {
	p, err := m.pick(target)
	if err != nil {
		return nil, err
	}
	return p.ListSuccessfulRuns(ctx, target, pat, since)
}

func (m *MultiProvider) FetchFileAtRef(ctx context.Context, target *apptypes.DeployTarget, pat string, path string, ref string) ([]byte, error) {
	p, err := m.pick(target)
	if err != nil {
		return nil, err
	}
	return p.FetchFileAtRef(ctx, target, pat, path, ref)
}
