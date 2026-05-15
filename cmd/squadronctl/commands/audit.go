// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

func newAuditCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Browse the audit log",
	}
	cmd.AddCommand(newAuditListCommand())
	return cmd
}

func newAuditListCommand() *cobra.Command {
	var (
		targetType string
		targetID   string
		limit      int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List audit events",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			q := url.Values{}
			if targetType != "" {
				q.Set("target_type", targetType)
			}
			if targetID != "" {
				q.Set("target_id", targetID)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			var resp cliapi.AuditResponse
			if err := c.Do(http.MethodGet, "/api/v1/audit/events", q, nil, &resp); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(resp.Events)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			rows := make([][]string, 0, len(resp.Events))
			for _, e := range resp.Events {
				target := e.TargetType
				if e.TargetID != "" {
					target = fmt.Sprintf("%s/%s", e.TargetType, truncate(e.TargetID, 8))
				}
				rows = append(rows, []string{
					e.Timestamp.Format("2006-01-02 15:04:05"),
					e.EventType,
					e.Actor,
					target,
					e.Action,
				})
			}
			table(cmd.OutOrStdout(), []string{"TIMESTAMP", "EVENT", "ACTOR", "TARGET", "ACTION"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&targetType, "target-type", "", "Filter by target type: agent | group | config | rule | rollout")
	cmd.Flags().StringVar(&targetID, "target-id", "", "Filter by target ID")
	cmd.Flags().IntVar(&limit, "limit", 50, "Max events to return (server caps at 1000)")
	return cmd
}
