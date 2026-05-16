// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

func newGroupsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "groups",
		Short: "List groups",
	}
	cmd.AddCommand(newGroupsListCommand())
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
