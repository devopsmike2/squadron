// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

func newAgentsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "List, inspect, and operate OpenTelemetry agents",
	}
	cmd.AddCommand(
		newAgentsListCommand(),
		newAgentsGetCommand(),
		newAgentsRestartCommand(),
		newAgentsSetGroupCommand(),
		newAgentsClearGroupCommand(),
		newAgentsClearConfigCommand(),
	)
	return cmd
}

func newAgentsListCommand() *cobra.Command {
	var (
		groupID string
		status  string
		drift   bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			q := url.Values{}
			if groupID != "" {
				q.Set("group_id", groupID)
			}
			if status != "" {
				q.Set("status", status)
			}
			if drift {
				q.Set("drift", "drifted")
			}
			var resp cliapi.AgentsResponse
			if err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/agents", q, nil, &resp); err != nil {
				return err
			}

			// Flatten the id-keyed map for stable, sorted output. Useful
			// for piping to less / grep without reproducing the same
			// agent across consecutive runs.
			agents := make([]cliapi.Agent, 0, len(resp.Agents))
			for _, a := range resp.Agents {
				agents = append(agents, a)
			}
			sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })

			if flags.Output == "json" {
				out, err := asJSON(agents)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			rows := make([][]string, 0, len(agents))
			for _, a := range agents {
				group := "—"
				if a.GroupName != nil && *a.GroupName != "" {
					group = *a.GroupName
				} else if a.GroupID != nil {
					group = truncate(*a.GroupID, 8)
				}
				rows = append(rows, []string{
					truncate(a.ID, 8),
					a.Name,
					a.Status,
					group,
					a.DriftStatus,
					a.Version,
				})
			}
			table(cmd.OutOrStdout(), []string{"ID", "NAME", "STATUS", "GROUP", "DRIFT", "VERSION"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&groupID, "group", "", "Filter to agents in this group ID")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status: online | offline | error")
	cmd.Flags().BoolVar(&drift, "drifted", false, "Only show drifted agents")
	return cmd
}

func newAgentsGetCommand() *cobra.Command {
	var (
		effective bool
		drift     bool
	)
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show full details of one agent",
		Long: `Show one agent's status, group, and config-drift state.

The default view surfaces the drift verdict (drift_status), the
resolved config intent (source + config id/version) and the group —
the fields the pilot checks constantly.

  --effective   print only the agent's reported effective config (the
                raw YAML the collector says it is running) — pipe it to
                a file or a diff tool.
  --drift       add the drift detail block: intent/effective/delivered
                hashes and the unified diff (when the agent is drifted).

-o json prints the full agent object including effective_config,
config_intent and drift_details.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var a cliapi.Agent
			if err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/agents/"+url.PathEscape(args[0]), nil, nil, &a); err != nil {
				return renderAPIError(err)
			}

			w := cmd.OutOrStdout()

			// --effective: print only the reported effective config, raw.
			// The most common pilot need — capture what the collector is
			// actually running to a file or a diff. -o json wraps it so a
			// script can still pick it off with jq.
			if effective {
				if flags.Output == "json" {
					out, err := asJSON(map[string]string{"effective_config": a.EffectiveConfig})
					if err != nil {
						return err
					}
					fmt.Fprintln(w, out)
					return nil
				}
				if strings.TrimSpace(a.EffectiveConfig) == "" {
					return fmt.Errorf("agent %s has not reported an effective config yet", a.ID)
				}
				fmt.Fprintln(w, a.EffectiveConfig)
				return nil
			}

			if flags.Output == "json" {
				out, err := asJSON(a)
				if err != nil {
					return err
				}
				fmt.Fprintln(w, out)
				return nil
			}

			fmt.Fprintf(w, "ID:      %s\n", a.ID)
			fmt.Fprintf(w, "Name:    %s\n", a.Name)
			fmt.Fprintf(w, "Status:  %s\n", a.Status)
			fmt.Fprintf(w, "Drift:   %s\n", a.DriftStatus)
			fmt.Fprintf(w, "Version: %s\n", a.Version)
			if a.GroupName != nil && *a.GroupName != "" {
				fmt.Fprintf(w, "Group:   %s\n", *a.GroupName)
			} else if a.GroupID != nil && *a.GroupID != "" {
				fmt.Fprintf(w, "Group:   %s\n", *a.GroupID)
			} else {
				fmt.Fprintln(w, "Group:   —")
			}

			// Config intent — where the desired config comes from and which
			// version. This is the "what should it be running" half of the
			// drift picture the pilot reads next to drift_status.
			if a.ConfigIntent != nil {
				src := a.ConfigIntent.Source
				if a.ConfigIntent.SourceName != "" {
					src = fmt.Sprintf("%s (%s)", a.ConfigIntent.Source, a.ConfigIntent.SourceName)
				}
				fmt.Fprintf(w, "Intent:  %s config %s v%d\n", src, truncate(a.ConfigIntent.ConfigID, 8), a.ConfigIntent.Version)
			} else {
				fmt.Fprintln(w, "Intent:  (none — no agent- or group-scoped config resolved)")
			}

			if len(a.Labels) > 0 {
				fmt.Fprintln(w, "Labels:")
				for k, v := range a.Labels {
					fmt.Fprintf(w, "  %s=%s\n", k, v)
				}
			}

			// --drift: the hash triple + unified diff behind the verdict.
			if drift {
				fmt.Fprintln(w, "Drift detail:")
				if a.DriftDetails == nil {
					fmt.Fprintln(w, "  (no drift detail reported)")
				} else {
					d := a.DriftDetails
					fmt.Fprintf(w, "  intent_hash:    %s\n", emptyDash(d.IntentHash))
					fmt.Fprintf(w, "  effective_hash: %s\n", emptyDash(d.EffectiveHash))
					if d.DeliveredHash != "" {
						fmt.Fprintf(w, "  delivered_hash: %s\n", d.DeliveredHash)
					}
					if strings.TrimSpace(d.Diff) != "" {
						fmt.Fprintln(w, "  diff:")
						for _, line := range strings.Split(strings.TrimRight(d.Diff, "\n"), "\n") {
							fmt.Fprintf(w, "    %s\n", line)
						}
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&effective, "effective", false, "Print only the agent's reported effective config (raw YAML)")
	cmd.Flags().BoolVar(&drift, "drift", false, "Include the drift detail block (hashes + unified diff)")
	return cmd
}

// newAgentsRestartCommand wraps POST /api/v1/agents/:id/restart. Sends a
// restart command to the agent over OpAMP. Requires the agents:write
// scope. Replaces the pilot's hand-rolled curl POST.
func newAgentsRestartCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart <agent-id>",
		Short: "Restart an agent (POST /agents/:id/restart)",
		Long: `Send a restart command to one agent over OpAMP.

Requires an API token with the agents:write scope. The agent must
advertise the restart capability; the server returns a clear error
otherwise.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var resp cliapi.RestartAgentResponse
			if err := c.Do(cmd.Context(), http.MethodPost,
				"/api/v1/agents/"+url.PathEscape(args[0])+"/restart", nil, nil, &resp); err != nil {
				return renderWriteError(err, "agents:write")
			}
			if flags.Output == "json" {
				out, err := asJSON(resp)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), out)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), resp.Message)
			return nil
		},
	}
	return cmd
}

// newAgentsSetGroupCommand wraps PATCH /api/v1/agents/:id/group. --group
// takes a group ID or name (names resolve via GET /api/v1/groups);
// --none clears the assignment. Requires agents:write.
func newAgentsSetGroupCommand() *cobra.Command {
	var (
		group string
		none  bool
	)
	cmd := &cobra.Command{
		Use:   "set-group <agent-id>",
		Short: "Assign an agent to a group (PATCH /agents/:id/group)",
		Long: `Re-point an agent at a different group, or clear its assignment.

  --group <id-or-name>   assign to this group. A value that isn't a
                         UUID is resolved by name against the group
                         list (case-insensitive; an ambiguous name is
                         rejected — pass the ID).
  --none                 clear the agent's group assignment.

Clearing the group here does NOT clear an agent-scoped config; use
'squadronctl agents clear-config' for that. Requires agents:write.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if none && group != "" {
				return fmt.Errorf("pass either --group or --none, not both")
			}
			if !none && group == "" {
				return fmt.Errorf("set --group <id-or-name> (or --none to clear the assignment)")
			}
			c := newClient()
			var body cliapi.UpdateAgentGroupRequest
			if !none {
				gid, err := resolveGroupID(cmd.Context(), c, group)
				if err != nil {
					return err
				}
				body.GroupID = &gid
			}
			// body.GroupID stays nil for --none → the handler clears.
			return patchAgentGroup(cmd, c, args[0], body)
		},
	}
	cmd.Flags().StringVar(&group, "group", "", "Group ID or name to assign the agent to")
	cmd.Flags().BoolVar(&none, "none", false, "Clear the agent's group assignment")
	return cmd
}

// newAgentsClearGroupCommand is the explicit clear form of set-group. It
// PATCHes a null group_id. Kept as its own verb because "clear-group" is
// how the pilot names the operation; set-group --none is the equivalent.
func newAgentsClearGroupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clear-group <agent-id>",
		Short: "Clear an agent's group assignment (PATCH /agents/:id/group)",
		Long: `Clear an agent's group assignment (equivalent to
'agents set-group <id> --none'). Does not touch an agent-scoped config.
Requires agents:write.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			// A nil GroupID marshals to {"group_id":null} → the handler
			// treats null/empty as "clear assignment".
			return patchAgentGroup(cmd, c, args[0], cliapi.UpdateAgentGroupRequest{})
		},
	}
	return cmd
}

// patchAgentGroup issues the PATCH and renders the returned agent. Shared
// by set-group and clear-group so the output shape stays identical.
func patchAgentGroup(cmd *cobra.Command, c *cliapi.Client, agentID string, body cliapi.UpdateAgentGroupRequest) error {
	var a cliapi.Agent
	if err := c.Do(cmd.Context(), http.MethodPatch,
		"/api/v1/agents/"+url.PathEscape(agentID)+"/group", nil, body, &a); err != nil {
		return renderWriteError(err, "agents:write")
	}
	if flags.Output == "json" {
		out, err := asJSON(a)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), out)
		return nil
	}
	w := cmd.OutOrStdout()
	switch {
	case a.GroupName != nil && *a.GroupName != "":
		fmt.Fprintf(w, "Agent %s assigned to group %s\n", truncate(a.ID, 8), *a.GroupName)
	case a.GroupID != nil && *a.GroupID != "":
		fmt.Fprintf(w, "Agent %s assigned to group %s\n", truncate(a.ID, 8), *a.GroupID)
	default:
		fmt.Fprintf(w, "Agent %s group assignment cleared\n", truncate(a.ID, 8))
	}
	return nil
}

// newAgentsClearConfigCommand wraps DELETE /api/v1/agents/:id/config.
// Drops the agent's own agent-scoped config so resolution falls back to
// the group config. Prints fell_back_to / group_id / pushed. Requires
// agents:write.
func newAgentsClearConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clear-config <agent-id>",
		Short: "Clear an agent's agent-scoped config (DELETE /agents/:id/config)",
		Long: `Delete an agent's OWN agent-scoped config so config resolution falls
back to the agent's GROUP config. Only the agent-scoped config is
removed — the group config is never touched, and no other agent is
affected.

If the agent is in a group that has a config, that group config is
delivered promptly (pushed=true) or on the next reconcile. If the
agent is in no group with a config, it's left with no assigned config.
Requires agents:write.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var resp cliapi.ClearAgentConfigResponse
			if err := c.Do(cmd.Context(), http.MethodDelete,
				"/api/v1/agents/"+url.PathEscape(args[0])+"/config", nil, nil, &resp); err != nil {
				return renderWriteError(err, "agents:write")
			}
			if flags.Output == "json" {
				out, err := asJSON(resp)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), out)
				return nil
			}
			w := cmd.OutOrStdout()
			fmt.Fprintln(w, resp.Message)
			fmt.Fprintf(w, "cleared:      %t\n", resp.Cleared)
			fmt.Fprintf(w, "fell_back_to: %s\n", resp.FellBackTo)
			if resp.GroupID != "" {
				fmt.Fprintf(w, "group_id:     %s\n", resp.GroupID)
			}
			fmt.Fprintf(w, "pushed:       %t\n", resp.Pushed)
			return nil
		},
	}
	return cmd
}

// resolveGroupID turns a --group value into a group ID. A value that
// already parses as a UUID is returned as-is; otherwise it's resolved by
// matching the group name (case-insensitive) against GET /api/v1/groups.
// An unknown or ambiguous name is a clear client-side error.
func resolveGroupID(ctx context.Context, c *cliapi.Client, groupRef string) (string, error) {
	if _, err := uuid.Parse(groupRef); err == nil {
		return groupRef, nil
	}
	var resp cliapi.GroupsResponse
	if err := c.Do(ctx, http.MethodGet, "/api/v1/groups", nil, nil, &resp); err != nil {
		return "", renderWriteError(err, "groups:read")
	}
	var matches []cliapi.Group
	for _, g := range resp.Groups {
		if strings.EqualFold(g.Name, groupRef) {
			matches = append(matches, g)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no group named %q found; pass a group ID or a valid name (see 'squadronctl groups list')", groupRef)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("group name %q is ambiguous (%d groups share it); pass the group ID instead", groupRef, len(matches))
	}
}

// renderWriteError humanises the 401/403 an operator hits on a mutation
// when their token lacks the scope. 403 names the missing scope so the
// fix is obvious ("issue a token with agents:write"); everything else
// funnels through renderAPIError (which already handles the 401 hint).
func renderWriteError(err error, scope string) error {
	var apiErr *cliapi.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
		return fmt.Errorf("forbidden — your API token is missing the %q scope. Issue a token with %q from the Squadron UI's API tokens page.", scope, scope)
	}
	return renderAPIError(err)
}

// emptyDash renders "—" for an empty string, used in the drift detail
// block so a missing hash reads clearly rather than as blank.
func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
