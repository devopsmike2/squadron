// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

// v0.77 — squadronctl plans subcommand. Wraps the v0.73/v0.74 HTTP
// plans API so operators driving plans from CI scripts or
// terminals don't need the UI.
//
// Scope: two verbs — get and create. Approve / reject convenience
// wrappers were considered and rejected; operators can hit
// `rollouts approve <step0_id>` directly with the step 0 id that
// `plans get` prints, and the wrapper would couple plans to the
// rollouts approval logic for a small UX win.

func newPlansCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plans",
		Short: "Inspect and create multi step rollout plans",
		Long: `A plan is a sequence of N rollout intents that ship under one
approval and one audit arc. The AI proposer creates them for cost spikes
that need more than one fix; CI scripts and operators can create them
directly via this command.

See docs/multi-step-plans-design.md for the protocol.`,
	}
	cmd.AddCommand(
		newPlansGetCommand(),
		newPlansCreateCommand(),
	)
	return cmd
}

func newPlansGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <plan-id>",
		Short: "Show the plan envelope and step list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var p cliapi.Plan
			if err := c.Do(cmd.Context(), http.MethodGet,
				"/api/v1/rollouts/plans/"+url.PathEscape(args[0]),
				nil, nil, &p); err != nil {
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
			// Header block: shared metadata.
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "plan_id:     %s\n", p.PlanID)
			fmt.Fprintf(w, "group_id:    %s\n", p.GroupID)
			fmt.Fprintf(w, "state:       %s\n", p.State)
			fmt.Fprintf(w, "step_count:  %d\n", p.StepCount)
			fmt.Fprintf(w, "created:     %s\n", p.CreatedAt.Format("2006-01-02 15:04:05 MST"))
			fmt.Fprintf(w, "updated:     %s\n", p.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
			fmt.Fprintln(w, strings.Repeat("-", 60))

			// Forward steps.
			fmt.Fprintln(w, "Steps:")
			rows := make([][]string, 0, len(p.Steps))
			for _, s := range p.Steps {
				rows = append(rows, []string{
					fmt.Sprintf("%d", s.PlanStepIndex),
					truncate(s.ID, 8),
					s.State,
					truncate(s.Name, 60),
				})
			}
			table(w, []string{"#", "ID", "STATE", "NAME"}, rows)

			// Rollback steps (only if the v0.72 backwards walk fired).
			if len(p.RollbackSteps) > 0 {
				fmt.Fprintln(w, "")
				fmt.Fprintln(w, "Rollback steps:")
				rows = make([][]string, 0, len(p.RollbackSteps))
				for _, s := range p.RollbackSteps {
					rows = append(rows, []string{
						fmt.Sprintf("%d", s.PlanStepIndex),
						truncate(s.ID, 8),
						s.State,
						truncate(s.Name, 60),
					})
				}
				table(w, []string{"#", "ID", "STATE", "NAME"}, rows)
			}
			return nil
		},
	}
	return cmd
}

func newPlansCreateCommand() *cobra.Command {
	var stepsFile string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a plan from a steps file",
		Long: `Create a multi step rollout plan. The body is read from --steps (or stdin
if --steps is "-") and must be a JSON object with the shape:

  {"steps": [<RolloutInput>, ...]}

Each RolloutInput is the same shape POST /api/v1/rollouts accepts. The
server assigns a shared plan id and PlanStepIndex 0..N-1 in step order;
the request's PlanID and PlanStepIndex fields are ignored. Only step 0's
require_approval flag is honored — the plan approves as a unit at step 0.

Example:

  squadronctl plans create --steps plan.json
  cat plan.json | squadronctl plans create --steps -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := readStepsBody(stepsFile)
			if err != nil {
				return err
			}
			c := newClient()
			var resp cliapi.CreatePlanResponse
			if err := c.Do(cmd.Context(), http.MethodPost,
				"/api/v1/rollouts/plans", nil, body, &resp); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(resp)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "plan_id:    %s\n", resp.PlanID)
			fmt.Fprintf(w, "step_count: %d\n", resp.Count)
			fmt.Fprintln(w, "")
			rows := make([][]string, 0, len(resp.Steps))
			for _, s := range resp.Steps {
				rows = append(rows, []string{
					fmt.Sprintf("%d", s.PlanStepIndex),
					truncate(s.ID, 8),
					s.State,
					truncate(s.Name, 60),
				})
			}
			table(w, []string{"#", "ID", "STATE", "NAME"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVarP(&stepsFile, "steps", "f", "",
		`Path to a JSON file containing the plan body, or "-" for stdin.`)
	_ = cmd.MarkFlagRequired("steps")
	return cmd
}

// readStepsBody reads the plan body from a file or stdin and
// validates that it parses as CreatePlanRequest before returning
// the raw bytes. We validate up front so an operator who hands us
// malformed JSON gets a tight client side error rather than the
// server's 400.
func readStepsBody(path string) ([]byte, error) {
	var raw []byte
	var err error
	switch path {
	case "":
		return nil, fmt.Errorf("--steps is required")
	case "-":
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
	default:
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	var probe cliapi.CreatePlanRequest
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("steps file is not a valid CreatePlanRequest: %w", err)
	}
	if len(probe.Steps) == 0 {
		return nil, fmt.Errorf("steps file contains no steps")
	}
	return raw, nil
}
