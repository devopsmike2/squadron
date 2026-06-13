// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"context"
	"time"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Provider abstracts over the deployment system Squadron is talking
// to. v0.34 ships GitHub Actions; the interface exists so v0.35
// can layer in Jenkins / GitLab without changing the service layer.
//
// Method shapes:
//
//   - Dispatch fires the workflow. Inputs is the merged
//     default+request input map. The provider returns a best-effort
//     "run ref" — for GitHub workflow_dispatch this is empty
//     because the API doesn't return the run_id; the service then
//     polls /actions/runs to attach the actual ID. The non-empty
//     case is reserved for providers that return a run identifier
//     synchronously.
//
//   - GetRun fetches the current status of a run. Used by the
//     polling fallback when webhooks aren't reachable.
//
//   - LatestRunSince finds the newest run on the configured
//     workflow that started after the given timestamp. Used to
//     attach a GitHub run_id to the Squadron deploy run after
//     workflow_dispatch (which returns 204 with no body).
type Provider interface {
	Dispatch(ctx context.Context, target *apptypes.DeployTarget, pat string, inputs map[string]string) (runRef string, err error)
	GetRun(ctx context.Context, target *apptypes.DeployTarget, pat string, runID int64) (*RunStatus, error)
	LatestRunSince(ctx context.Context, target *apptypes.DeployTarget, pat string, since time.Time) (*RunStatus, error)
}

// RunStatus is the normalized status snapshot the provider returns.
// Squadron stores these values verbatim in the deploy_runs table.
type RunStatus struct {
	GitHubRunID  int64      `json:"github_run_id"`
	GitHubRunURL string     `json:"github_run_url"`
	Status       string     `json:"status"`     // "queued" | "in_progress" | "completed"
	Conclusion   string     `json:"conclusion"` // "success" | "failure" | "cancelled" | "timed_out" | "skipped" | ""
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}
