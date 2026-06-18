// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Command squadron-action-runner is the daemon that lives on an
// operator node and turns Squadron's signed action requests into
// real changes (a systemd restart, a config refresh, whatever the
// catalog grows to support). It is the execute end of Move 2.
//
// The runner is intentionally small. It reads a YAML config at
// start, registers with Squadron, then loops:
//
//  1. GET /api/v1/runners/:id/pending
//  2. for each request: verify signature, validate type, run the
//     action via the registered executor, POST the result.
//
// Everything that requires trust (which units to restart, which
// Squadron instance to obey) is pinned in the config and refused
// otherwise. The runner never accepts a command outside its declared
// capabilities and never accepts a request signed by a key other
// than the one in the config.
//
// Operator workflow:
//
//	squadron-action-runner start --config /etc/squadron/action-runner.yaml
//
// or, for dev, override fields with --squadron-url / --runner-id flags
// (planned in a follow up). The MVP requires a config file.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var (
	configPath string
	dryRunOnly bool
	logLevel   string
)

func main() {
	root := &cobra.Command{
		Use:   "squadron-action-runner",
		Short: "Runner daemon for Squadron's signed action requests.",
		Long: "Polls a Squadron control plane for signed action requests, " +
			"verifies them against a pinned issuer key, and executes the " +
			"ones permitted by the runner's declared capabilities.",
	}

	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the runner loop (registers, polls, executes).",
		RunE:  runStart,
	}
	startCmd.Flags().StringVar(&configPath, "config", "/etc/squadron/action-runner.yaml", "Path to the YAML config file.")
	startCmd.Flags().BoolVar(&dryRunOnly, "dry-run-only", false, "Refuse phase=execute requests; useful for canary nodes.")
	startCmd.Flags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error).")

	root.AddCommand(startCmd)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runStart(cmd *cobra.Command, _ []string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger, err := buildLogger(logLevel)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	// The MVP executor is the real systemd one. Tests inject a fake
	// via NewRunner directly without going through main.
	exec := NewSystemdExecutor(logger)

	runner, err := NewRunner(cfg, exec, logger)
	if err != nil {
		return fmt.Errorf("build runner: %w", err)
	}
	runner.DryRunOnly = dryRunOnly

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("squadron-action-runner starting",
		zap.String("runner_id", cfg.RunnerID),
		zap.String("hostname", cfg.Hostname),
		zap.String("squadron_url", cfg.SquadronURL),
		zap.Bool("dry_run_only", dryRunOnly),
		zap.Duration("poll_interval", cfg.PollInterval),
	)
	return runner.Run(ctx)
}

func buildLogger(level string) (*zap.Logger, error) {
	zapCfg := zap.NewProductionConfig()
	if err := zapCfg.Level.UnmarshalText([]byte(level)); err != nil {
		return nil, err
	}
	zapCfg.DisableStacktrace = true
	return zapCfg.Build()
}

