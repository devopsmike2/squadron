// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

func newGroupsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "groups",
		Short: "List groups and manage group config",
	}
	cmd.AddCommand(newGroupsListCommand(), newGroupsAssignConfigCommand())
	return cmd
}

func newGroupsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List groups",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var resp cliapi.GroupsResponse
			if err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/groups", nil, nil, &resp); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(resp.Groups)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			rows := make([][]string, 0, len(resp.Groups))
			for _, g := range resp.Groups {
				rows = append(rows, []string{g.ID, g.Name, g.CreatedAt.Format("2006-01-02 15:04:05")})
			}
			table(cmd.OutOrStdout(), []string{"ID", "NAME", "CREATED"}, rows)
			return nil
		},
	}
}

// newGroupsAssignConfigCommand wraps POST /api/v1/groups/:id/config. The
// handler takes a config_id reference (not inline content): it clones the
// referenced config into a new group-scoped version and pushes it to
// every agent in the group. Requires the groups:write scope.
func newGroupsAssignConfigCommand() *cobra.Command {
	var configID string
	cmd := &cobra.Command{
		Use:   "assign-config <group-id>",
		Short: "Assign a config to a group (POST /groups/:id/config)",
		Long: `Assign an existing config to a group. The server clones the
referenced config into a new group-scoped version and pushes it to
every agent in the group.

The endpoint takes a config ID reference (not inline content) — create
or look up the config first with 'squadronctl configs', then reference
it here:

  squadronctl groups assign-config <group-id> --config <config-id>

Requires an API token with the groups:write scope.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(configID) == "" {
				return fmt.Errorf("--config <config-id> is required")
			}
			c := newClient()
			body := cliapi.AssignGroupConfigRequest{ConfigID: configID}
			var resp cliapi.AssignGroupConfigResponse
			if err := c.Do(cmd.Context(), http.MethodPost,
				"/api/v1/groups/"+url.PathEscape(args[0])+"/config", nil, body, &resp); err != nil {
				return renderWriteError(err, "groups:write")
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
			fmt.Fprintf(w, "config_id: %s\n", resp.Config.ID)
			if resp.Config.GroupID != nil {
				fmt.Fprintf(w, "group_id:  %s\n", *resp.Config.GroupID)
			}
			fmt.Fprintf(w, "version:   %d\n", resp.Config.Version)
			return nil
		},
	}
	cmd.Flags().StringVar(&configID, "config", "", "ID of the config to assign to the group (required)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
