// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

func newConfigsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "configs",
		Short: "Manage agent configurations",
	}
	cmd.AddCommand(
		newConfigsListCommand(),
		newConfigsApplyCommand(),
		newConfigsLintCommand(),
	)
	return cmd
}

func newConfigsListCommand() *cobra.Command {
	var (
		groupID string
		agentID string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configs",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			q := url.Values{}
			if groupID != "" {
				q.Set("group_id", groupID)
			}
			if agentID != "" {
				q.Set("agent_id", agentID)
			}
			var resp cliapi.ConfigsResponse
			if err := c.Do(http.MethodGet, "/api/v1/configs", q, nil, &resp); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(resp.Configs)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			rows := make([][]string, 0, len(resp.Configs))
			for _, c := range resp.Configs {
				group := "—"
				if c.GroupID != nil {
					group = truncate(*c.GroupID, 8)
				}
				agent := "—"
				if c.AgentID != nil {
					agent = truncate(*c.AgentID, 8)
				}
				rows = append(rows, []string{
					truncate(c.ID, 8),
					c.Name,
					fmt.Sprintf("v%d", c.Version),
					group,
					agent,
					c.CreatedAt.Format("2006-01-02 15:04:05"),
				})
			}
			table(cmd.OutOrStdout(), []string{"ID", "NAME", "VERSION", "GROUP", "AGENT", "CREATED"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&groupID, "group", "", "Filter to configs bound to this group")
	cmd.Flags().StringVar(&agentID, "agent", "", "Filter to configs bound to this agent")
	return cmd
}

// newConfigsApplyCommand uploads a YAML file as a new config. This is
// THE CLI's most-used command in CI pipelines: a release artifact lands
// in the repo, squadronctl uploads it, and the returned config ID is
// what feeds the next `rollouts create`.
func newConfigsApplyCommand() *cobra.Command {
	var (
		path    string
		name    string
		groupID string
		agentID string
	)
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create a new config from a YAML file",
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				return fmt.Errorf("--file is required")
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			body := cliapi.CreateConfigRequest{
				Name:    name,
				Content: string(content),
			}
			if groupID != "" {
				g := groupID
				body.GroupID = &g
			}
			if agentID != "" {
				a := agentID
				body.AgentID = &a
			}

			c := newClient()
			var created cliapi.Config
			if err := c.Do(http.MethodPost, "/api/v1/configs", nil, body, &created); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(created)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			fmt.Printf("Created config: %s\n", created.ID)
			fmt.Printf("Name:           %s\n", created.Name)
			fmt.Printf("Version:        %d\n", created.Version)
			fmt.Printf("Hash:           %s\n", created.ConfigHash)
			return nil
		},
	}
	cmd.Flags().StringVarP(&path, "file", "f", "", "YAML config file to upload (required)")
	cmd.Flags().StringVar(&name, "name", "", "Display name for the config (required)")
	cmd.Flags().StringVar(&groupID, "group", "", "Bind the config to this group")
	cmd.Flags().StringVar(&agentID, "agent", "", "Bind the config to a single agent")
	return cmd
}

// newConfigsLintCommand runs the server's lint engine against a local
// YAML file without uploading it. Useful as a pre-commit / CI gate
// before `configs apply` — fail fast on anti-patterns.
func newConfigsLintCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Lint a local YAML config against Squadron's anti-pattern engine",
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				return fmt.Errorf("--file is required")
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			c := newClient()
			var resp cliapi.LintResponse
			body := map[string]string{"content": string(content)}
			if err := c.Do(http.MethodPost, "/api/v1/configs/lint", nil, body, &resp); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(resp.Findings)
				if err != nil {
					return err
				}
				fmt.Println(out)
				if anyErrors(resp.Findings) {
					return fmt.Errorf("lint failed: %d error finding(s)", countSeverity(resp.Findings, "error"))
				}
				return nil
			}
			if len(resp.Findings) == 0 {
				fmt.Println("✓ no findings")
				return nil
			}
			for _, f := range resp.Findings {
				prefix := "  "
				switch f.Severity {
				case "error":
					prefix = "✗ "
				case "warning":
					prefix = "! "
				case "info":
					prefix = "ℹ "
				}
				line := ""
				if f.Line > 0 {
					line = fmt.Sprintf(" (line %d)", f.Line)
				}
				fmt.Printf("%s[%s] %s%s — %s\n", prefix, f.Severity, f.Rule, line, f.Message)
			}
			if anyErrors(resp.Findings) {
				return fmt.Errorf("lint failed: %d error finding(s)", countSeverity(resp.Findings, "error"))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&path, "file", "f", "", "YAML config file to lint (required)")
	return cmd
}

func anyErrors(fs []cliapi.LintFinding) bool {
	for _, f := range fs {
		if f.Severity == "error" {
			return true
		}
	}
	return false
}

func countSeverity(fs []cliapi.LintFinding, sev string) int {
	n := 0
	for _, f := range fs {
		if f.Severity == sev {
			n++
		}
	}
	return n
}
