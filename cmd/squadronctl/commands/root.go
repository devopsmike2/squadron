// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

// globalFlags holds the bound values of every persistent flag at root.
// We capture them on a single struct so subcommands can read what's
// effective without re-querying cobra's flag tree.
type globalFlags struct {
	Server     string
	Token      string
	Output     string // "human" | "json"
	ConfigPath string
}

var flags globalFlags

// endSpanFn is set by PersistentPreRunE and called by
// PersistentPostRunE / on error so each invocation's root span gets
// closed exactly once. Pointer-to-fn so the closure can capture a
// pre-RunE-determined "did RunE return an error" value.
var endSpanFn func(error) = func(error) {}

// NewRootCommand returns the top-level squadronctl command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "squadronctl",
		Short: "Command-line client for Squadron",
		Long: `squadronctl wraps the Squadron REST API for scripting,
CI pipelines, and ad-hoc terminal use. Set SQUADRON_URL to your
server's address and SQUADRON_TOKEN to an API token issued from the
Squadron UI's API tokens page.

Tracing: when OTEL_EXPORTER_OTLP_ENDPOINT is set, squadronctl wraps
each invocation in an OTel span and exports it via OTLP. The
outbound API call carries a W3C traceparent header so the
server-side span becomes a child of squadronctl's. If TRACEPARENT
is set in the environment (the otel-cli convention),
squadronctl's span nests under that. See docs/squadronctl.md
"Tracing" for the full picture.`,
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := initTracing(cmd.Context()); err != nil {
				return err
			}
			ctx, end := beginCommandSpan(cmd.Context(), commandSpanName(cmd))
			cmd.SetContext(ctx)
			endSpanFn = end
			return nil
		},
		PersistentPostRunE: func(_ *cobra.Command, _ []string) error {
			endSpanFn(nil)
			shutdownTracing()
			return nil
		},
	}

	root.PersistentFlags().StringVar(&flags.Server, "server", "",
		"Squadron server URL (env: SQUADRON_URL, default: http://localhost:8080)")
	root.PersistentFlags().StringVar(&flags.Token, "token", "",
		"API token (env: SQUADRON_TOKEN). Empty is allowed when auth is disabled server-side.")
	root.PersistentFlags().StringVarP(&flags.Output, "output", "o", "human",
		"Output format: human | json")
	root.PersistentFlags().StringVar(&flags.ConfigPath, "config", "",
		"Path to config file (default: $HOME/.squadronctl/config.yaml)")

	root.AddCommand(
		newVersionCommand(),
		newAuthCommand(),
		newAgentsCommand(),
		newGroupsCommand(),
		newConfigsCommand(),
		newRolloutsCommand(),
		newAuditCommand(),
	)
	return root
}

// commandSpanName returns the OTel span name for a given subcommand.
// Builds it as "squadronctl <full path>" — e.g. "squadronctl rollouts
// create" — so trace UIs sort related invocations together. The
// root command itself ("squadronctl --help" etc.) returns just
// "squadronctl".
func commandSpanName(cmd *cobra.Command) string {
	parts := []string{}
	for c := cmd; c != nil && c.Name() != ""; c = c.Parent() {
		parts = append([]string{c.Name()}, parts...)
	}
	return strings.Join(parts, " ")
}

// EndRootSpan closes the per-invocation root span. Exposed for main.go
// to call from the error path — cobra's PersistentPostRunE only fires
// when RunE returns nil, so failures need an explicit hook.
func EndRootSpan(err error) {
	endSpanFn(err)
	shutdownTracing()
	// Reset to no-op so a future call (or a test re-running the root
	// command in-process) doesn't double-close.
	endSpanFn = func(error) {}
}

// rootContext returns context.Background() for tests / non-cobra
// entry points. Kept as a function rather than a constant so any
// future cross-cutting setup (signal handling, deadline) lives here.
func rootContext() context.Context { return context.Background() }

// fileConfig is the optional ~/.squadronctl/config.yaml structure.
// Field names mirror the env-var conventions so it's obvious what
// goes where.
type fileConfig struct {
	Server string `yaml:"server"`
	Token  string `yaml:"token"`
}

// loadFileConfig reads ~/.squadronctl/config.yaml (or the --config
// path if set). Missing file is not an error — it just yields the
// zero value, and env vars + flags fill in the gaps.
func loadFileConfig() fileConfig {
	path := flags.ConfigPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fileConfig{}
		}
		path = filepath.Join(home, ".squadronctl", "config.yaml")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileConfig{}
	}
	var fc fileConfig
	_ = yaml.Unmarshal(data, &fc)
	return fc
}

// resolvedConfig returns the effective server + token after merging
// flags > env vars > file. Used by every subcommand to build its
// client.
func resolvedConfig() (server, token string) {
	fc := loadFileConfig()
	server = firstNonEmpty(flags.Server, os.Getenv("SQUADRON_URL"), fc.Server, "http://localhost:8080")
	token = firstNonEmpty(flags.Token, os.Getenv("SQUADRON_TOKEN"), fc.Token)
	return server, token
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// newClient builds the cliapi.Client every subcommand uses. Token may
// be empty (servers with auth.enabled=false accept unauthenticated
// requests; servers with auth on return 401 and the command layer
// translates that to a friendly error).
func newClient() *cliapi.Client {
	server, token := resolvedConfig()
	return cliapi.New(server, token)
}

// printErr writes a friendly error message to stderr. 401s get a
// special hint pointing at SQUADRON_TOKEN since "did you set the env
// var?" is the most common CLI failure mode.
func printErr(err error) {
	if cliapi.Is401(err) {
		fmt.Fprintln(os.Stderr, "Error: unauthorized — set SQUADRON_TOKEN to an API token issued from the Squadron UI.")
		return
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
}

// asJSON is the default JSON pretty-printer used by -o json across
// every subcommand. Single helper so output formatting stays consistent.
func asJSON(v any) (string, error) {
	return prettyJSON(v)
}
