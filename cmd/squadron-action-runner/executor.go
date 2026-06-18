// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/actions"
)

// SystemdExecutor is the real Executor. It shells out to systemctl
// for the restart-systemd-service action type. Unknown action types
// produce a failure result so the protocol layer can record the
// problem; this keeps the runner future-safe as new types land.
//
// The dry_run phase is intentionally read only. For
// restart-systemd-service it runs `systemctl status <unit>` and
// reports whether the unit exists, whether it is active, and what
// the planned command would be on the execute phase. No state is
// changed.
//
// The execute phase runs the actual restart / try-restart / reload.
// stdout, stderr, and exit code are captured and returned in the
// Result.
type SystemdExecutor struct {
	logger *zap.Logger

	// commandRunner is the actual command exec. Swappable for tests
	// so we never call systemctl in CI.
	commandRunner CommandRunner
}

// CommandRunner is the thin abstraction we mock in tests. A real
// implementation calls exec.CommandContext; a fake records the
// command and returns canned output.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, exitCode int, err error)
}

// NewSystemdExecutor returns the production executor with the real
// systemctl runner installed.
func NewSystemdExecutor(logger *zap.Logger) *SystemdExecutor {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &SystemdExecutor{
		logger:        logger,
		commandRunner: defaultCommandRunner{},
	}
}

// SetCommandRunner swaps the runner. Tests inject a fake so the
// runner unit tests never call systemctl.
func (e *SystemdExecutor) SetCommandRunner(r CommandRunner) {
	e.commandRunner = r
}

// ExecuteRequest is the Executor entry point. It branches on action
// type so additional actions slot in here without touching the
// runner loop.
func (e *SystemdExecutor) ExecuteRequest(ctx context.Context, req *actions.Request) *actions.Result {
	started := time.Now().UTC()
	switch req.Action.Type {
	case actions.RestartSystemdServiceType:
		return e.executeRestartSystemd(ctx, req, started)
	case actions.RestartDockerContainerType:
		return e.executeRestartDocker(ctx, req, started)
	case actions.RunShellAllowlistType:
		return e.executeRunShellAllowlist(ctx, req, started)
	default:
		return &actions.Result{
			Status:      actions.StatusFailure,
			Stderr:      fmt.Sprintf("runner does not implement action type %q", req.Action.Type),
			StartedAt:   started,
			CompletedAt: time.Now().UTC(),
		}
	}
}

func (e *SystemdExecutor) executeRestartDocker(ctx context.Context, req *actions.Request, started time.Time) *actions.Result {
	var params actions.RestartDockerContainerParameters
	if err := json.Unmarshal(req.Action.Parameters, &params); err != nil {
		return &actions.Result{
			Status:      actions.StatusFailure,
			Stderr:      "decode parameters: " + err.Error(),
			StartedAt:   started,
			CompletedAt: time.Now().UTC(),
		}
	}
	timeoutArg := []string{"--time", "10"}
	if params.TimeoutSeconds > 0 {
		timeoutArg = []string{"--time", fmt.Sprintf("%d", params.TimeoutSeconds)}
	}

	if req.Phase == actions.PhaseDryRun {
		// Dry run = inspect the container. Reports whether it
		// exists and what we would run on execute.
		stdout, stderr, code, err := e.commandRunner.Run(ctx, "docker", "inspect", "--format", "{{.State.Status}}", params.Container)
		out := map[string]any{
			"planned_command": fmt.Sprintf("docker restart %s %s", strings.Join(timeoutArg, " "), params.Container),
			"container":       params.Container,
			"current_state":   strings.TrimSpace(stdout),
		}
		if err != nil {
			out["inspect_error"] = err.Error()
		}
		return &actions.Result{
			Status:      actions.StatusSuccess,
			Stdout:      truncate(stdout, 4000),
			Stderr:      truncate(stderr, 2000),
			ExitCode:    code,
			ResultData:  out,
			StartedAt:   started,
			CompletedAt: time.Now().UTC(),
		}
	}

	args := append([]string{"restart"}, timeoutArg...)
	args = append(args, params.Container)
	stdout, stderr, code, err := e.commandRunner.Run(ctx, "docker", args...)
	out := map[string]any{
		"ran_command": fmt.Sprintf("docker %s", strings.Join(args, " ")),
		"container":   params.Container,
		"exit_code":   code,
	}
	status := actions.StatusSuccess
	if err != nil || code != 0 {
		status = actions.StatusFailure
	}
	res := &actions.Result{
		Status:      status,
		Stdout:      truncate(stdout, 4000),
		Stderr:      truncate(stderr, 2000),
		ExitCode:    code,
		ResultData:  out,
		StartedAt:   started,
		CompletedAt: time.Now().UTC(),
	}
	if err != nil && status == actions.StatusFailure {
		res.Stderr = strings.TrimSpace(res.Stderr + "\n" + err.Error())
	}
	return res
}

// executeRunShellAllowlist runs an exact allowlisted shell command.
// Note: the capability matcher in actions/register_run_shell_allowlist.go
// already enforces the allowlist; the runner re-checks defensively
// would require knowing the runner's own capabilities here, which
// it does via the Runner's actionAllowed gate. By the time the
// executor sees the request the allowlist match has succeeded
// twice (Squadron + runner).
func (e *SystemdExecutor) executeRunShellAllowlist(ctx context.Context, req *actions.Request, started time.Time) *actions.Result {
	var params actions.RunShellAllowlistParameters
	if err := json.Unmarshal(req.Action.Parameters, &params); err != nil {
		return &actions.Result{
			Status:      actions.StatusFailure,
			Stderr:      "decode parameters: " + err.Error(),
			StartedAt:   started,
			CompletedAt: time.Now().UTC(),
		}
	}
	// Split the verbatim command. We do not invoke a shell; we
	// argv-split the allowlisted string and exec directly so shell
	// metacharacters (already blocked at validate time) cannot
	// expand even if they slipped through.
	fields := strings.Fields(params.Command)
	if len(fields) == 0 {
		return &actions.Result{
			Status:      actions.StatusFailure,
			Stderr:      "empty command after split",
			StartedAt:   started,
			CompletedAt: time.Now().UTC(),
		}
	}

	if req.Phase == actions.PhaseDryRun {
		return &actions.Result{
			Status: actions.StatusSuccess,
			ResultData: map[string]any{
				"planned_command": params.Command,
				"argv":            fields,
			},
			StartedAt:   started,
			CompletedAt: time.Now().UTC(),
		}
	}

	stdout, stderr, code, err := e.commandRunner.Run(ctx, fields[0], fields[1:]...)
	out := map[string]any{
		"ran_command": params.Command,
		"exit_code":   code,
	}
	status := actions.StatusSuccess
	if err != nil || code != 0 {
		status = actions.StatusFailure
	}
	res := &actions.Result{
		Status:      status,
		Stdout:      truncate(stdout, 4000),
		Stderr:      truncate(stderr, 2000),
		ExitCode:    code,
		ResultData:  out,
		StartedAt:   started,
		CompletedAt: time.Now().UTC(),
	}
	if err != nil && status == actions.StatusFailure {
		res.Stderr = strings.TrimSpace(res.Stderr + "\n" + err.Error())
	}
	return res
}

func (e *SystemdExecutor) executeRestartSystemd(ctx context.Context, req *actions.Request, started time.Time) *actions.Result {
	var params actions.RestartSystemdServiceParameters
	if err := json.Unmarshal(req.Action.Parameters, &params); err != nil {
		return &actions.Result{
			Status:      actions.StatusFailure,
			Stderr:      "decode parameters: " + err.Error(),
			StartedAt:   started,
			CompletedAt: time.Now().UTC(),
		}
	}
	unit := params.UnitName
	strategy := params.RestartStrategy
	if strategy == "" {
		strategy = "restart"
	}

	if req.Phase == actions.PhaseDryRun {
		// Status only. Reports whether the unit exists and what we
		// would do on execute. This is the bit that flows into the
		// operator's Slack message so they know what they are
		// approving.
		stdout, stderr, code, err := e.commandRunner.Run(ctx, "systemctl", "status", unit, "--no-pager")
		out := map[string]any{
			"planned_command": fmt.Sprintf("systemctl %s %s", strategy, unit),
			"unit_name":       unit,
			"strategy":        strategy,
			"unit_exit_code":  code,
			"unit_status":     truncate(stdout, 4000),
		}
		if err != nil {
			out["status_error"] = err.Error()
		}
		// We treat a status command failure as success for the
		// dry-run (the unit may not exist; that is information the
		// operator wants in the panel).
		return &actions.Result{
			Status:      actions.StatusSuccess,
			Stdout:      truncate(stdout, 4000),
			Stderr:      truncate(stderr, 2000),
			ExitCode:    code,
			ResultData:  out,
			StartedAt:   started,
			CompletedAt: time.Now().UTC(),
		}
	}

	// Execute phase. Pick the systemctl subcommand from the strategy.
	subcommand := strategy
	switch strategy {
	case "restart":
		subcommand = "restart"
	case "try-restart":
		subcommand = "try-restart"
	case "reload":
		subcommand = "reload"
	}
	stdout, stderr, code, err := e.commandRunner.Run(ctx, "systemctl", subcommand, unit)
	out := map[string]any{
		"ran_command": fmt.Sprintf("systemctl %s %s", subcommand, unit),
		"unit_name":   unit,
		"strategy":    strategy,
		"exit_code":   code,
	}
	status := actions.StatusSuccess
	if err != nil || code != 0 {
		status = actions.StatusFailure
	}
	res := &actions.Result{
		Status:      status,
		Stdout:      truncate(stdout, 4000),
		Stderr:      truncate(stderr, 2000),
		ExitCode:    code,
		ResultData:  out,
		StartedAt:   started,
		CompletedAt: time.Now().UTC(),
	}
	if err != nil && status == actions.StatusFailure {
		res.Stderr = strings.TrimSpace(res.Stderr + "\n" + err.Error())
	}
	return res
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

// defaultCommandRunner is the production runner.
type defaultCommandRunner struct{}

func (defaultCommandRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return stdout.String(), stderr.String(), exitCode, err
}
