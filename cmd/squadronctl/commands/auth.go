// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

func newAuthCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Authenticate to Squadron and manage API tokens",
	}
	cmd.AddCommand(
		newAuthWhoamiCommand(),
		newAuthTokensListCommand(),
		newAuthTokensCreateCommand(),
		newAuthTokensRevokeCommand(),
	)
	return cmd
}

// newAuthWhoamiCommand verifies the current token by hitting an
// authenticated endpoint. If auth is on and the token is bad you get
// the friendly 401 hint; if auth is off you see "auth disabled".
func newAuthWhoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Check the configured server and token",
		RunE: func(cmd *cobra.Command, args []string) error {
			server, token := resolvedConfig()
			fmt.Printf("Server: %s\n", server)
			if token == "" {
				fmt.Println("Token:  (none — auth may be disabled server-side)")
			} else {
				fmt.Printf("Token:  %s (sha-prefix only shown)\n", maskToken(token))
			}

			c := newClient()
			var resp cliapi.TokensResponse
			if err := c.Do(http.MethodGet, "/api/v1/auth/tokens", nil, nil, &resp); err != nil {
				if cliapi.Is401(err) {
					fmt.Println("Status: unauthorized (set SQUADRON_TOKEN to a valid token)")
					return err
				}
				return err
			}
			active := 0
			for _, t := range resp.Tokens {
				if t.RevokedAt == nil {
					active++
				}
			}
			fmt.Printf("Status: authenticated (%d active tokens visible)\n", active)
			return nil
		},
	}
}

func newAuthTokensListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "tokens",
		Short: "List API tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var resp cliapi.TokensResponse
			if err := c.Do(http.MethodGet, "/api/v1/auth/tokens", nil, nil, &resp); err != nil {
				return err
			}
			if flags.Output == "json" {
				out, err := asJSON(resp.Tokens)
				if err != nil {
					return err
				}
				fmt.Println(out)
				return nil
			}
			rows := make([][]string, 0, len(resp.Tokens))
			for _, t := range resp.Tokens {
				status := "active"
				if t.RevokedAt != nil {
					status = "revoked"
				}
				lastUsed := "—"
				if t.LastUsedAt != nil {
					lastUsed = t.LastUsedAt.Format("2006-01-02 15:04:05")
				}
				rows = append(rows, []string{
					truncate(t.ID, 8),
					t.Label,
					t.CreatedAt.Format("2006-01-02 15:04:05"),
					lastUsed,
					status,
				})
			}
			table(cmd.OutOrStdout(), []string{"ID", "LABEL", "CREATED", "LAST USED", "STATUS"}, rows)
			return nil
		},
	}
}

func newAuthTokensCreateCommand() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "create-token",
		Short: "Issue a new API token. Plaintext printed ONCE.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if label == "" {
				return fmt.Errorf("--label is required")
			}
			c := newClient()
			body := map[string]string{"label": label}
			var resp cliapi.CreateTokenResponse
			if err := c.Do(http.MethodPost, "/api/v1/auth/tokens", nil, body, &resp); err != nil {
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
			fmt.Printf("Token issued: %s\n", resp.Token.Label)
			fmt.Printf("ID:           %s\n", resp.Token.ID)
			fmt.Printf("Plaintext:    %s\n", resp.Plaintext)
			fmt.Println("\nCopy the plaintext above NOW. Squadron stores only a hash — there's no way to retrieve it later.")
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Human-readable label (required)")
	return cmd
}

func newAuthTokensRevokeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke-token <id>",
		Short: "Revoke an API token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			path := "/api/v1/auth/tokens/" + url.PathEscape(args[0]) + "/revoke"
			if err := c.Do(http.MethodPost, path, nil, nil, nil); err != nil {
				return err
			}
			fmt.Printf("Revoked: %s\n", args[0])
			return nil
		},
	}
}

// maskToken returns the first 8 chars of the token plus an ellipsis.
// Just enough for an operator to spot-check they're talking to the
// right Squadron without echoing the full credential to the terminal.
func maskToken(t string) string {
	if len(t) <= 8 {
		return "<short>"
	}
	return t[:8] + "…"
}
