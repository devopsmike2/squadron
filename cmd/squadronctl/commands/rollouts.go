// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

// Exit codes used by `rollouts wait` and `rollouts create --wait`. CI
// pipelines branch on these without parsing stdout.
const (
	exitSucceeded  = 0
	exitFailedWait = 2 // rolled_back or aborted-but-not-rolled-back
	exitTimeout    = 3 // --timeout elapsed before the rollout reached a terminal state
)

func newRolloutsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollouts",
		Short: "Manage safe staged config rollouts",
	}
	cmd.AddCommand(
		newRolloutsListCommand(),
		newRolloutsGetCommand(),
		newRolloutsCreateCommand(),
		newRolloutsAbortCommand(),
		newRolloutsPauseCommand(),
		newRolloutsResumeCommand(),
		newRolloutsWaitCommand(),
		newRolloutsPreviewCommand(),
		newRolloutsTemplatesCommand(),
	)
	return cmd
}

func newRolloutsListCommand() *cobra.Command {
	var (
		groupID string
		state   string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List rollouts",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			q := url.Values{}
			if groupID != "" {
				q.Set("group_id", groupID)
			}
			if state != "" {
				q.Set("state", state)
			}
			var resp cliapi.RolloutsResponse
			if err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/rollouts", q, nil, &resp); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(resp.Rollouts)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			sort.Slice(resp.Rollouts, func(i, j int) bool {
				return resp.Rollouts[i].CreatedAt.After(resp.Rollouts[j].CreatedAt)
			})
			rows := make([][]string, 0, len(resp.Rollouts))
			for _, r := range resp.Rollouts {
				rows = append(rows, []string{
					truncate(r.ID, 8),
					r.Name,
					r.State,
					fmt.Sprintf("%d/%d", r.CurrentStage+1, len(r.Stages)),
					truncate(r.GroupID, 8),
					r.CreatedAt.Format("2006-01-02 15:04:05"),
				})
			}
			table(cmd.OutOrStdout(), []string{"ID", "NAME", "STATE", "STAGE", "GROUP", "CREATED"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&groupID, "group", "", "Filter by group ID")
	cmd.Flags().StringVar(&state, "state", "", "Filter by state: pending | in_progress | paused | succeeded | aborted | rolled_back")
	return cmd
}

func newRolloutsGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show full details of one rollout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := fetchRollout(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(r)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			fmt.Printf("ID:     %s\n", r.ID)
			fmt.Printf("Name:   %s\n", r.Name)
			fmt.Printf("State:  %s\n", r.State)
			fmt.Printf("Group:  %s\n", r.GroupID)
			fmt.Printf("Target: %s\n", r.TargetConfigID)
			fmt.Printf("Stage:  %d of %d\n", r.CurrentStage+1, len(r.Stages))
			if r.AbortReason != "" {
				fmt.Printf("Reason: %s\n", r.AbortReason)
			}
			for i, s := range r.Stages {
				marker := "  "
				if i == r.CurrentStage {
					marker = "→ "
				}
				fmt.Printf("%sStage %d: %s\n", marker, i+1, stageSummary(s))
			}
			return nil
		},
	}
}

// newRolloutsCreateCommand is the most important command in the CLI
// for CI use. The two ways to call it:
//
//   --template=standard-percent-ramp --group=... --target-config=...
//   --stages=10:120,50:180,100:120 --group=... --target-config=...
//
// `--wait` blocks until the rollout reaches a terminal state and exits
// non-zero on rolled_back / aborted-without-rollback so the pipeline
// fails properly.
func newRolloutsCreateCommand() *cobra.Command {
	var (
		name         string
		groupID      string
		targetCfgID  string
		templateID   string
		stagesSpec   string
		maxDrift     int
		maxErr       int
		warmup       int
		notify       string
		wait         bool
		waitTimeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new rollout",
		Long: `Create a new rollout against a group with a target config.

There are two ways to specify stages:

  1. --template=<id>   — fill in stages + criteria + default name from
                         a server-side template (see 'squadronctl
                         rollouts templates'). Override individual fields
                         after the template loads.

  2. --stages=10:120,50:180,100:120
                       — comma-separated percent:dwell pairs. Each pair
                         becomes one stage. All percent-mode.

Pass --wait to block until the rollout reaches a terminal state. Exit
codes: 0=succeeded, 2=rolled_back or aborted, 3=timeout.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if groupID == "" {
				return fmt.Errorf("--group is required")
			}
			if targetCfgID == "" {
				return fmt.Errorf("--target-config is required")
			}

			input := cliapi.RolloutInput{
				Name:            name,
				GroupID:         groupID,
				TargetConfigID:  targetCfgID,
				NotificationURL: notify,
				AbortCriteria: cliapi.RolloutAbortCriteria{
					MaxDriftedAgents:           maxDrift,
					MaxErrorLogsPerMinute:      maxErr,
					MinDwellSecondsBeforeAbort: warmup,
				},
			}

			// Template path: start from a server-defined shape, then
			// allow operator overrides via flags. Flags set means
			// "override what the template gave us"; defaults mean
			// "keep the template's value".
			if templateID != "" {
				c := newClient()
				var tplResp cliapi.RolloutTemplatesResponse
				if err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/rollout-recipes/templates", nil, nil, &tplResp); err != nil {
					return err
				}
				var tpl *cliapi.RolloutTemplate
				for i := range tplResp.Templates {
					if tplResp.Templates[i].ID == templateID {
						tpl = &tplResp.Templates[i]
						break
					}
				}
				if tpl == nil {
					return fmt.Errorf("template %q not found — run 'squadronctl rollouts templates' to list available templates", templateID)
				}
				input.Stages = tpl.Stages
				// Take the template's criteria unless the operator
				// explicitly overrode a field via flag. Cobra exposes
				// flag.Changed() for "did the operator set this?".
				if !cmd.Flags().Changed("max-drifted-agents") {
					input.AbortCriteria.MaxDriftedAgents = tpl.AbortCriteria.MaxDriftedAgents
				}
				if !cmd.Flags().Changed("max-error-logs-per-minute") {
					input.AbortCriteria.MaxErrorLogsPerMinute = tpl.AbortCriteria.MaxErrorLogsPerMinute
				}
				if !cmd.Flags().Changed("warmup-seconds") {
					input.AbortCriteria.MinDwellSecondsBeforeAbort = tpl.AbortCriteria.MinDwellSecondsBeforeAbort
				}
				if name == "" {
					input.Name = tpl.DefaultName
				}
			}

			if stagesSpec != "" {
				stages, err := parseStagesSpec(stagesSpec)
				if err != nil {
					return err
				}
				input.Stages = stages
			}
			if len(input.Stages) == 0 {
				return fmt.Errorf("must specify --template or --stages")
			}
			if input.Name == "" {
				input.Name = fmt.Sprintf("rollout %s", time.Now().Format("2006-01-02 15:04"))
			}

			c := newClient()
			var created cliapi.Rollout
			if err := c.Do(cmd.Context(), http.MethodPost, "/api/v1/rollouts", nil, input, &created); err != nil {
				return err
			}

			if flags.Output == "json" && !wait {
				out, err := asJSON(created)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}

			fmt.Printf("Created rollout: %s\n", created.ID)
			fmt.Printf("Name:            %s\n", created.Name)
			fmt.Printf("State:           %s\n", created.State)
			fmt.Printf("Stages:          %d\n", len(created.Stages))

			if !wait {
				return nil
			}
			return waitForTerminal(cmd.Context(), created.ID, waitTimeout, flags.Output == "json")
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name for the rollout (defaults to template name or a timestamp)")
	cmd.Flags().StringVar(&groupID, "group", "", "Target group ID (required)")
	cmd.Flags().StringVar(&targetCfgID, "target-config", "", "Target config ID to roll out (required)")
	cmd.Flags().StringVar(&templateID, "template", "", "Template ID to use as the stage + criteria starting point")
	cmd.Flags().StringVar(&stagesSpec, "stages", "", "Stage spec: comma-separated percent:dwell pairs (e.g. 10:120,50:180,100:120). Overrides --template stages.")
	cmd.Flags().IntVar(&maxDrift, "max-drifted-agents", 0, "Auto-abort threshold for drifted canary agents")
	cmd.Flags().IntVar(&maxErr, "max-error-logs-per-minute", 0, "Auto-abort threshold for canary error logs per minute (0 disables)")
	cmd.Flags().IntVar(&warmup, "warmup-seconds", 30, "Seconds after stage start before the error-rate check fires")
	cmd.Flags().StringVar(&notify, "notify", "", "Webhook URL to POST on state transitions")
	cmd.Flags().BoolVar(&wait, "wait", false, "Block until the rollout reaches a terminal state")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "Maximum time to wait when --wait is set")
	return cmd
}

func newRolloutsAbortCommand() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "abort <id>",
		Short: "Abort a rollout. The engine then performs the rollback push.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			body := cliapi.AbortRequest{Reason: reason}
			var r cliapi.Rollout
			if err := c.Do(cmd.Context(), http.MethodPost, "/api/v1/rollouts/"+url.PathEscape(args[0])+"/abort", nil, body, &r); err != nil {
				return err
			}
			fmt.Printf("Aborted: %s (state=%s)\n", r.ID, r.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Reason to record in the audit log")
	return cmd
}

func newRolloutsPauseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "pause <id>",
		Short: "Pause an in-progress rollout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var r cliapi.Rollout
			if err := c.Do(cmd.Context(), http.MethodPost, "/api/v1/rollouts/"+url.PathEscape(args[0])+"/pause", nil, nil, &r); err != nil {
				return err
			}
			fmt.Printf("Paused: %s\n", r.ID)
			return nil
		},
	}
}

func newRolloutsResumeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <id>",
		Short: "Resume a paused rollout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var r cliapi.Rollout
			if err := c.Do(cmd.Context(), http.MethodPost, "/api/v1/rollouts/"+url.PathEscape(args[0])+"/resume", nil, nil, &r); err != nil {
				return err
			}
			fmt.Printf("Resumed: %s\n", r.ID)
			return nil
		},
	}
}

// newRolloutsWaitCommand polls a rollout until it reaches a terminal
// state. Designed for CI: kick off a rollout, then `wait` on it,
// exit on the result.
func newRolloutsWaitCommand() *cobra.Command {
	var (
		timeout  time.Duration
		interval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "Block until a rollout reaches a terminal state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return waitForTerminalWithInterval(cmd.Context(), args[0], timeout, interval, flags.Output == "json")
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "Maximum time to wait")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "Polling interval")
	return cmd
}

func newRolloutsPreviewCommand() *cobra.Command {
	var (
		groupID    string
		targetCfg  string
	)
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Show the diff + lint findings before creating a rollout",
		RunE: func(cmd *cobra.Command, args []string) error {
			if groupID == "" || targetCfg == "" {
				return fmt.Errorf("--group and --target-config are required")
			}
			c := newClient()
			q := url.Values{}
			q.Set("group_id", groupID)
			q.Set("target_config_id", targetCfg)
			var p cliapi.RolloutPreview
			if err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/rollout-preview", q, nil, &p); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(p)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			if p.Diff.Identical {
				fmt.Println("Target config is identical to the group's current effective config. A rollout would not change anything.")
				return nil
			}
			fmt.Printf("Diff: +%d / -%d lines\n", p.Diff.Added, p.Diff.Removed)
			if p.Current == nil {
				fmt.Println("(group has no current config — rollback target would be empty)")
			}
			if len(p.LintFindings) > 0 {
				fmt.Printf("Lint findings (%d):\n", len(p.LintFindings))
				for _, f := range p.LintFindings {
					line := ""
					if f.Line > 0 {
						line = fmt.Sprintf(" (line %d)", f.Line)
					}
					fmt.Printf("  [%s] %s%s — %s\n", f.Severity, f.Rule, line, f.Message)
				}
			}
			fmt.Println()
			fmt.Println(p.Diff.Unified)
			return nil
		},
	}
	cmd.Flags().StringVar(&groupID, "group", "", "Target group ID (required)")
	cmd.Flags().StringVar(&targetCfg, "target-config", "", "Candidate target config ID (required)")
	return cmd
}

func newRolloutsTemplatesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "templates",
		Short: "List server-curated rollout templates",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var resp cliapi.RolloutTemplatesResponse
			if err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/rollout-recipes/templates", nil, nil, &resp); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(resp.Templates)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			rows := make([][]string, 0, len(resp.Templates))
			for _, t := range resp.Templates {
				rows = append(rows, []string{t.ID, t.Name, fmt.Sprintf("%d stages", len(t.Stages)), t.Description})
			}
			table(cmd.OutOrStdout(), []string{"ID", "NAME", "SHAPE", "DESCRIPTION"}, rows)
			return nil
		},
	}
}

// stageSummary renders one stage as a single line for the get command.
func stageSummary(s cliapi.RolloutStage) string {
	dwell := fmt.Sprintf("%ds dwell", s.DwellSeconds)
	if s.Mode == "label" {
		parts := make([]string, 0, len(s.LabelSelector))
		for k, v := range s.LabelSelector {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(parts)
		return fmt.Sprintf("label[%s] · %s", strings.Join(parts, ","), dwell)
	}
	return fmt.Sprintf("%d%% · %s", s.Percentage, dwell)
}

// parseStagesSpec parses "10:120,50:180,100:120" into a stage slice.
// Each pair is percent:dwell_seconds. Designed for command-line use,
// not a full DSL — operators wanting label-mode rollouts should drive
// them via the UI or write the JSON body to a file and pipe it in.
func parseStagesSpec(spec string) ([]cliapi.RolloutStage, error) {
	parts := strings.Split(spec, ",")
	stages := make([]cliapi.RolloutStage, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		colon := strings.Index(p, ":")
		if colon < 0 {
			return nil, fmt.Errorf("stage %d: expected percent:dwell, got %q", i, p)
		}
		var pct, dwell int
		if _, err := fmt.Sscanf(p[:colon], "%d", &pct); err != nil {
			return nil, fmt.Errorf("stage %d: bad percentage %q: %w", i, p[:colon], err)
		}
		if _, err := fmt.Sscanf(p[colon+1:], "%d", &dwell); err != nil {
			return nil, fmt.Errorf("stage %d: bad dwell %q: %w", i, p[colon+1:], err)
		}
		stages = append(stages, cliapi.RolloutStage{
			Mode:         "percent",
			Percentage:   pct,
			DwellSeconds: dwell,
		})
	}
	return stages, nil
}

func fetchRollout(ctx context.Context, id string) (*cliapi.Rollout, error) {
	c := newClient()
	var r cliapi.Rollout
	if err := c.Do(ctx, http.MethodGet, "/api/v1/rollouts/"+url.PathEscape(id), nil, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func waitForTerminal(ctx context.Context, id string, timeout time.Duration, jsonOut bool) error {
	return waitForTerminalWithInterval(ctx, id, timeout, 5*time.Second, jsonOut)
}

// waitForTerminalWithInterval polls the rollout until it succeeds, is
// rolled back, or the timeout elapses. Exit codes match the constants
// at the top of this file so CI can branch on them.
func waitForTerminalWithInterval(ctx context.Context, id string, timeout, interval time.Duration, jsonOut bool) error {
	deadline := time.Now().Add(timeout)
	lastState := ""
	for {
		r, err := fetchRollout(ctx, id)
		if err != nil {
			return err
		}
		if r.State != lastState && !jsonOut {
			fmt.Printf("[%s] %s (stage %d/%d)\n",
				time.Now().Format("15:04:05"), r.State, r.CurrentStage+1, len(r.Stages))
			lastState = r.State
		}
		switch r.State {
		case "succeeded":
			if jsonOut {
				out, _ := asJSON(r)
				fmt.Println(out)
			}
			return nil
		case "rolled_back":
			if jsonOut {
				out, _ := asJSON(r)
				fmt.Println(out)
			}
			return cobraExit(exitFailedWait, "rollout rolled back: %s", r.AbortReason)
		case "aborted":
			// Aborted is a transient state — the engine moves it to
			// rolled_back on its next tick. Keep polling unless the
			// aborted state has stuck (which would indicate a bug).
		}
		if time.Now().After(deadline) {
			return cobraExit(exitTimeout, "wait timeout after %s (last state: %s)", timeout, r.State)
		}
		time.Sleep(interval)
	}
}

// cobraExit terminates the process with a specific exit code. Wait /
// create --wait need distinguishable codes for CI to branch on, and
// cobra itself only exposes success vs. error. Wired through package-
// level function variables so tests can swap them out and assert
// behavior without actually exiting.
func cobraExit(code int, format string, args ...any) error {
	fmt.Fprintln(stderrW, fmt.Errorf(format, args...))
	exitFn(code)
	return nil // unreachable in production; tests override exitFn to a no-op
}
