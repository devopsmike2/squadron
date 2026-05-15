// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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
				switch {
				case t.RevokedAt != nil:
					status = "revoked"
				case t.ExpiresAt != nil && !time.Now().Before(*t.ExpiresAt):
					status = "expired"
				}
				lastUsed := "—"
				if t.LastUsedAt != nil {
					lastUsed = t.LastUsedAt.Format("2006-01-02 15:04:05")
				}
				expires := "never"
				if t.ExpiresAt != nil {
					expires = t.ExpiresAt.Format("2006-01-02 15:04:05")
				}
				rows = append(rows, []string{
					truncate(t.ID, 8),
					t.Label,
					summarizeScopes(t.Scopes),
					t.CreatedAt.Format("2006-01-02 15:04:05"),
					lastUsed,
					expires,
					status,
				})
			}
			table(cmd.OutOrStdout(), []string{"ID", "LABEL", "SCOPES", "CREATED", "LAST USED", "EXPIRES", "STATUS"}, rows)
			return nil
		},
	}
}

// summarizeScopes is the same display logic the UI uses: empty scopes
// = legacy pre-v0.10 full-access, "*" = explicit full-access, otherwise
// the count (or the single scope if there's only one).
func summarizeScopes(scopes []string) string {
	if len(scopes) == 0 {
		return "legacy:*"
	}
	for _, s := range scopes {
		if s == "*" {
			return "*"
		}
	}
	if len(scopes) == 1 {
		return scopes[0]
	}
	return fmt.Sprintf("%d scopes", len(scopes))
}

func newAuthTokensCreateCommand() *cobra.Command {
	var (
		label      string
		scopes     []string
		fullAccess bool
		expiresIn  string
		expiresAt  string
	)
	cmd := &cobra.Command{
		Use:   "create-token",
		Short: "Issue a new API token. Plaintext printed ONCE.",
		Long: `Issue a new API token.

Scope flags (at least one is required — operators must opt in to the
permissions they're granting):
  --scope agents:read         repeatable; one per scope
  --full-access               shortcut for --scope='*' (the wildcard)

Expiry flags (optional — defaults to no expiry):
  --expires-in 90d            duration shorthand: d, h, m, s
  --expires-at 2026-12-31T23:59:59Z  RFC3339 timestamp

Common bundles to copy:
  read-only viewer:   --scope agents:read --scope rollouts:read --scope audit:read
  CI deploy pipeline: --scope configs:write --scope rollouts:write --scope rollouts:read
  alerts manager:     --scope alerts:read --scope alerts:write --scope audit:read`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if label == "" {
				return fmt.Errorf("--label is required")
			}
			if fullAccess {
				scopes = []string{"*"}
			}
			if len(scopes) == 0 {
				return fmt.Errorf("at least one --scope is required (or --full-access)")
			}
			if expiresIn != "" && expiresAt != "" {
				return fmt.Errorf("--expires-in and --expires-at are mutually exclusive")
			}
			var expiry *time.Time
			if expiresIn != "" {
				d, err := parseDurationShorthand(expiresIn)
				if err != nil {
					return fmt.Errorf("--expires-in: %w", err)
				}
				t := time.Now().Add(d).UTC()
				expiry = &t
			} else if expiresAt != "" {
				t, err := time.Parse(time.RFC3339, expiresAt)
				if err != nil {
					return fmt.Errorf("--expires-at: must be RFC3339 (e.g. 2026-12-31T23:59:59Z): %w", err)
				}
				expiry = &t
			}

			c := newClient()
			body := map[string]any{"label": label, "scopes": scopes}
			if expiry != nil {
				body["expires_at"] = expiry.Format(time.RFC3339)
			}
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
			fmt.Printf("Scopes:       %v\n", resp.Token.Scopes)
			if resp.Token.ExpiresAt != nil {
				fmt.Printf("Expires:      %s\n", resp.Token.ExpiresAt.Format(time.RFC3339))
			} else {
				fmt.Println("Expires:      never")
			}
			fmt.Printf("Plaintext:    %s\n", resp.Plaintext)
			fmt.Println("\nCopy the plaintext above NOW. Squadron stores only a hash — there's no way to retrieve it later.")
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Human-readable label (required)")
	cmd.Flags().StringSliceVar(&scopes, "scope", nil, "Scope to grant (repeatable). E.g. --scope agents:read")
	cmd.Flags().BoolVar(&fullAccess, "full-access", false, "Shortcut for --scope='*' (the wildcard)")
	cmd.Flags().StringVar(&expiresIn, "expires-in", "", "Token lifetime (e.g. 90d, 24h, 30m)")
	cmd.Flags().StringVar(&expiresAt, "expires-at", "", "Token expiry as RFC3339 (e.g. 2026-12-31T23:59:59Z)")
	return cmd
}

// parseDurationShorthand extends time.ParseDuration with a 'd' suffix
// for days. Operators write '90d' more often than '2160h' and the
// stdlib doesn't accept days by design.
func parseDurationShorthand(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid days: %q", s)
		}
		if days <= 0 {
			return 0, fmt.Errorf("duration must be positive")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return d, nil
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
