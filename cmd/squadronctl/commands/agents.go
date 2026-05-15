// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

func newAgentsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "List and inspect OpenTelemetry agents",
	}
	cmd.AddCommand(newAgentsListCommand(), newAgentsGetCommand())
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
			if err := c.Do(http.MethodGet, "/api/v1/agents", q, nil, &resp); err != nil {
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
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show full details of one agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var a cliapi.Agent
			if err := c.Do(http.MethodGet, "/api/v1/agents/"+url.PathEscape(args[0]), nil, nil, &a); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(a)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			fmt.Printf("ID:      %s\n", a.ID)
			fmt.Printf("Name:    %s\n", a.Name)
			fmt.Printf("Status:  %s\n", a.Status)
			fmt.Printf("Drift:   %s\n", a.DriftStatus)
			fmt.Printf("Version: %s\n", a.Version)
			if a.GroupName != nil {
				fmt.Printf("Group:   %s\n", *a.GroupName)
			}
			if len(a.Labels) > 0 {
				fmt.Println("Labels:")
				for k, v := range a.Labels {
					fmt.Printf("  %s=%s\n", k, v)
				}
			}
			return nil
		},
	}
}
