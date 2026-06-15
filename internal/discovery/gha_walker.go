// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/deploy"
	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// DefaultGHAWalkInterval is how often the walker polls. 6 hours is
// generous — deploy histories don't change that fast, and frequent
// walks burn GitHub rate-limit budget.
const DefaultGHAWalkInterval = 6 * time.Hour

// DefaultGHALookback is the lookback window. 30 days catches the
// active inventory without backfilling years of stale hosts that
// have probably been retired by now.
const DefaultGHALookback = 30 * 24 * time.Hour

// GHAWalkerStore is the slice of the application store the walker
// needs. Targets give it the workflow to enumerate and the PAT
// (encrypted, decrypted via the deploy service); the expected-
// agents methods are how it materializes discoveries.
type GHAWalkerStore interface {
	ListDeployTargets(ctx context.Context) ([]*apptypes.DeployTarget, error)
	GetDeployTarget(ctx context.Context, id string) (*apptypes.DeployTarget, error)
	UpsertExpectedAgent(ctx context.Context, e *apptypes.ExpectedAgent) error
}

// GHAWalker periodically replays a deploy target's GitHub Actions
// history. For each successful past run, it fetches the
// inventory.ini at that commit and registers the parsed hosts as
// expected_agents with source "gha-history:<target-id>".
//
// Why this exists: with v0.32 your operator has to either manually
// maintain the expected list or PUT it from a CI step. With v0.36.1
// the deploy history IS the inventory ledger — Squadron auto-derives
// the expected set from authoritative source-of-truth (the actual
// commit SHAs your team has deployed against).
//
// Each walker run is idempotent: replaying the same window over
// gives the same expected_agents set.
type GHAWalker struct {
	store    GHAWalkerStore
	deploy   GHAWalkerDeployBridge
	provider deploy.Provider
	logger   *zap.Logger
	interval time.Duration
	lookback time.Duration

	stop chan struct{}
}

// GHAWalkerDeployBridge is the small slice of internal/deploy.Service
// the walker needs — specifically the credential-decrypt path. We
// declare it as a local interface so the walker package doesn't
// take a hard dep on the full deploy service in tests.
type GHAWalkerDeployBridge interface {
	DecryptedPAT(ctx context.Context, target *apptypes.DeployTarget) (string, error)
}

// NewGHAWalker constructs the walker. Pass zero for interval /
// lookback to get the defaults.
func NewGHAWalker(
	store GHAWalkerStore,
	bridge GHAWalkerDeployBridge,
	provider deploy.Provider,
	interval time.Duration,
	lookback time.Duration,
	logger *zap.Logger,
) *GHAWalker {
	if interval <= 0 {
		interval = DefaultGHAWalkInterval
	}
	if lookback <= 0 {
		lookback = DefaultGHALookback
	}
	return &GHAWalker{
		store:    store,
		deploy:   bridge,
		provider: provider,
		logger:   logger,
		interval: interval,
		lookback: lookback,
		stop:     make(chan struct{}),
	}
}

// Run blocks until Stop is called. Walks every target on each tick.
// The first walk runs immediately on startup so an operator
// configuring a new target sees results within seconds, not hours.
func (w *GHAWalker) Run(ctx context.Context) {
	w.logger.Info("GHA history walker started",
		zap.Duration("interval", w.interval),
		zap.Duration("lookback", w.lookback))
	// Initial walk.
	if err := w.WalkAll(ctx); err != nil {
		w.logger.Warn("initial GHA walk failed", zap.Error(err))
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.WalkAll(ctx); err != nil {
				w.logger.Warn("GHA walk failed", zap.Error(err))
			}
		}
	}
}

// Stop signals Run to exit at its next tick.
func (w *GHAWalker) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
}

// WalkAll iterates every deploy target with an inventory_path set
// and walks its workflow history. Targets without inventory_path
// are skipped — there's nothing to extract.
func (w *GHAWalker) WalkAll(ctx context.Context) error {
	targets, err := w.store.ListDeployTargets(ctx)
	if err != nil {
		return fmt.Errorf("list targets: %w", err)
	}
	for _, t := range targets {
		if t.InventoryPath == "" {
			continue
		}
		// Refresh the target so we get the encrypted_credential
		// bytes that ListDeployTargets strips for security.
		full, err := w.store.GetDeployTarget(ctx, t.ID)
		if err != nil || full == nil {
			continue
		}
		if err := w.walkTarget(ctx, full); err != nil {
			w.logger.Warn("GHA walk target failed",
				zap.String("target", full.Name), zap.Error(err))
		}
	}
	return nil
}

// walkTarget walks one target's history. Pulls the PAT, lists
// successful runs in the lookback window, fetches inventory.ini at
// each unique commit SHA, parses hosts, registers each as expected.
//
// Dedup: hosts are upserted by hostname, so the same host appearing
// in multiple runs just refreshes the row. We don't bother
// deduping inside this function — UpsertExpectedAgent is idempotent.
func (w *GHAWalker) walkTarget(ctx context.Context, target *apptypes.DeployTarget) error {
	pat, err := w.deploy.DecryptedPAT(ctx, target)
	if err != nil {
		return fmt.Errorf("decrypt PAT: %w", err)
	}
	since := time.Now().Add(-w.lookback)
	runs, err := w.provider.ListSuccessfulRuns(ctx, target, pat, since)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	if len(runs) == 0 {
		return nil
	}

	// Dedup by SHA — multiple workflow runs against the same
	// commit produce the same inventory snapshot, so we'd fetch
	// the same file repeatedly otherwise.
	seenSHAs := map[string]struct{}{}
	source := "gha-history:" + target.ID
	totalHosts := 0
	for _, r := range runs {
		if r.HeadSHA == "" {
			continue
		}
		if _, dup := seenSHAs[r.HeadSHA]; dup {
			continue
		}
		seenSHAs[r.HeadSHA] = struct{}{}

		raw, ferr := w.provider.FetchFileAtRef(ctx, target, pat, target.InventoryPath, r.HeadSHA)
		if ferr != nil {
			w.logger.Debug("fetch inventory at ref failed",
				zap.String("sha", r.HeadSHA), zap.Error(ferr))
			continue
		}
		hosts := ParseInventoryHostsForGHA(raw)
		for _, host := range hosts {
			notes := fmt.Sprintf("from %s run #%d (sha %s)",
				target.Name, r.RunID, shortSHA(r.HeadSHA))
			_ = w.store.UpsertExpectedAgent(ctx, &apptypes.ExpectedAgent{
				Hostname:      host,
				Source:        source,
				ExpectedSince: r.CreatedAt,
				Notes:         notes,
			})
			totalHosts++
		}
	}
	w.logger.Info("GHA walk done",
		zap.String("target", target.Name),
		zap.Int("runs", len(runs)),
		zap.Int("unique_shas", len(seenSHAs)),
		zap.Int("hosts_upserted", totalHosts))
	return nil
}

// ParseInventoryHostsForGHA wraps deploy.ParseInventoryHosts so this
// package can use it without importing deploy directly in the test
// surface. (Kept as its own function for documentation discoverability;
// the underlying parser is the same one used at trigger time.)
func ParseInventoryHostsForGHA(content []byte) []string {
	return deploy.ParseInventoryHosts(content)
}

// shortSHA truncates to the conventional 7-char display form.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// hashSource is used internally if we ever want a stable cache key
// per target. Not exposed; kept for future use.
//
//nolint:unused
func hashSource(s string) string {
	h := sha256.Sum256([]byte(strings.ToLower(s)))
	return hex.EncodeToString(h[:8])
}
