// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

func newIncidentsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "incidents",
		Short: "Review AI drafted incident tickets",
		Long: `Squadron's action runner writes a postmortem ticket draft after each
action runs. This subcommand lets operators triage the inbox without
opening the UI: list drafts, view the rendered markdown, dismiss
noise, or publish through a registered provider (GitHub Issues,
clipboard, or a stamp only provider).`,
	}
	cmd.AddCommand(
		newIncidentsListCommand(),
		newIncidentsViewCommand(),
		newIncidentsDismissCommand(),
		newIncidentsPublishCommand(),
	)
	return cmd
}

func newIncidentsListCommand() *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List incident drafts",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			q := url.Values{}
			if status != "" {
				q.Set("status", status)
			}
			var resp cliapi.IncidentsResponse
			if err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/incidents/drafts", q, nil, &resp); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(resp.Drafts)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			rows := make([][]string, 0, len(resp.Drafts))
			for _, d := range resp.Drafts {
				rows = append(rows, []string{
					truncate(d.ID, 8),
					d.Status,
					d.CreatedAt.Format("2006-01-02 15:04"),
					truncate(d.Title, 70),
					truncate(d.ActionRequestID, 8),
				})
			}
			table(cmd.OutOrStdout(), []string{"ID", "STATUS", "CREATED", "TITLE", "ACTION"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "draft",
		"Filter by status: draft | published | dismissed")
	return cmd
}

func newIncidentsViewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "view <draft-id>",
		Short: "Print the rendered body of one draft",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var d cliapi.IncidentDraft
			if err := c.Do(cmd.Context(), http.MethodGet,
				"/api/v1/incidents/drafts/"+url.PathEscape(args[0]), nil, nil, &d); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(d)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "id:      %s\n", d.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "status:  %s\n", d.Status)
			fmt.Fprintf(cmd.OutOrStdout(), "created: %s\n", d.CreatedAt.Format("2006-01-02 15:04:05 MST"))
			if d.ActionRequestID != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "action:  %s\n", d.ActionRequestID)
			}
			if d.RolloutID != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "rollout: %s\n", d.RolloutID)
			}
			if d.Provider != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "provider: %s\n", d.Provider)
			}
			if d.ExternalURL != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "ticket:  %s\n", d.ExternalURL)
			}
			fmt.Fprintln(cmd.OutOrStdout(), strings.Repeat("-", 60))
			fmt.Fprintln(cmd.OutOrStdout(), d.BodyMarkdown)
			return nil
		},
	}
	return cmd
}

func newIncidentsDismissCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dismiss <draft-id>",
		Short: "Mark a draft as dismissed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var d cliapi.IncidentDraft
			if err := c.Do(cmd.Context(), http.MethodPost,
				"/api/v1/incidents/drafts/"+url.PathEscape(args[0])+"/dismiss",
				nil, []byte("{}"), &d); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "dismissed: %s\n", d.ID)
			return nil
		},
	}
	return cmd
}

func newIncidentsPublishCommand() *cobra.Command {
	var (
		provider    string
		externalID  string
		externalURL string
	)
	cmd := &cobra.Command{
		Use:   "publish <draft-id>",
		Short: "Publish a draft through a configured provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			payload, err := json.Marshal(map[string]any{
				"provider":     provider,
				"external_id":  externalID,
				"external_url": externalURL,
			})
			if err != nil {
				return err
			}
			var d cliapi.IncidentDraft
			if err := c.Do(cmd.Context(), http.MethodPost,
				"/api/v1/incidents/drafts/"+url.PathEscape(args[0])+"/publish",
				nil, payload, &d); err != nil {
				return err
			}
			switch {
			case d.ExternalURL != "":
				fmt.Fprintf(cmd.OutOrStdout(), "published via %s: %s\n", d.Provider, d.ExternalURL)
			case d.ExternalID != "":
				fmt.Fprintf(cmd.OutOrStdout(), "published via %s: %s\n", d.Provider, d.ExternalID)
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "published via %s\n", d.Provider)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "clipboard",
		"Provider: clipboard | github | linear | jira | servicenow | generic")
	cmd.Flags().StringVar(&externalID, "external-id", "",
		"External tracker ID (ignored when the provider returns its own)")
	cmd.Flags().StringVar(&externalURL, "external-url", "",
		"External tracker URL (ignored when the provider returns its own)")
	return cmd
}
