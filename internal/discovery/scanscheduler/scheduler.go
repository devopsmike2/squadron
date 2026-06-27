// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package scanscheduler runs discovery scans on a fixed interval so that scan
// history accrues automatically — the "continuous" half of continuous
// discovery (slice 3a). It is deliberately dependency-free: the caller injects
// a ListAccounts function (which connections to scan) and a ScanAccount
// function (run + persist one scan), so the scheduler has no knowledge of
// clouds, stores, or HTTP. This keeps it unit-testable with plain fakes.
package scanscheduler

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// MinInterval is the floor for the scan interval. A tighter cadence would
// hammer the cloud provider APIs (and the operator's bill); a configured value
// below this is raised to it with a log line.
const MinInterval = 15 * time.Minute

// defaultConcurrency bounds how many accounts are scanned in parallel per
// sweep. Scans are individually heavy (minutes for large accounts), so a small
// bound keeps memory + API pressure sane.
const defaultConcurrency = 2

// Scheduler periodically runs ListAccounts + ScanAccount.
type Scheduler struct {
	// Interval between sweeps. Values below MinInterval are raised to it.
	Interval time.Duration
	// Concurrency bounds parallel per-account scans within a sweep. <=0 uses
	// defaultConcurrency.
	Concurrency int
	// ListAccounts returns the connection scope ids to scan this sweep.
	ListAccounts func(ctx context.Context) ([]string, error)
	// ScanAccount runs + persists one scan. A returned error is logged and
	// counted but never aborts the sweep — one bad account must not starve the
	// others.
	ScanAccount func(ctx context.Context, accountID string) error
	Logger      *zap.Logger
}

// effectiveInterval applies the MinInterval floor.
func (s *Scheduler) effectiveInterval() time.Duration {
	iv := s.Interval
	if iv < MinInterval {
		if s.Logger != nil {
			s.Logger.Warn("discovery scan scheduler: interval below floor, raising",
				zap.Duration("configured", iv), zap.Duration("floor", MinInterval))
		}
		iv = MinInterval
	}
	return iv
}

func (s *Scheduler) concurrency() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return defaultConcurrency
}

// Run loops RunOnce on a ticker until ctx is cancelled. The first sweep fires
// after one interval (not immediately) to avoid a surprise scan at startup.
func (s *Scheduler) Run(ctx context.Context) {
	iv := s.effectiveInterval()
	if s.Logger != nil {
		s.Logger.Info("discovery scan scheduler started", zap.Duration("interval", iv))
	}
	ticker := time.NewTicker(iv)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if s.Logger != nil {
				s.Logger.Info("discovery scan scheduler stopped")
			}
			return
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// RunOnce performs a single sweep: list the accounts and scan each with bounded
// concurrency. Returns (scanned, failed) for observability + tests. Per-account
// failures are logged, not fatal.
func (s *Scheduler) RunOnce(ctx context.Context) (scanned int, failed int) {
	if s.ListAccounts == nil || s.ScanAccount == nil {
		return 0, 0
	}
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Error("discovery scan scheduler: list accounts failed", zap.Error(err))
		}
		return 0, 0
	}
	if len(accounts) == 0 {
		return 0, 0
	}

	sem := make(chan struct{}, s.concurrency())
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, acct := range accounts {
		if ctx.Err() != nil {
			break
		}
		acct := acct
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.ScanAccount(ctx, acct); err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				if s.Logger != nil {
					s.Logger.Warn("discovery scan scheduler: scan failed",
						zap.String("account_id", acct), zap.Error(err))
				}
				return
			}
			mu.Lock()
			scanned++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if s.Logger != nil {
		s.Logger.Info("discovery scan scheduler sweep complete",
			zap.Int("scanned", scanned), zap.Int("failed", failed), zap.Int("total", len(accounts)))
	}
	return scanned, failed
}
