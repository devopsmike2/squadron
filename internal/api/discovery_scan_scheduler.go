// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

	"github.com/devopsmike2/squadron/internal/api/handlers"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/scanscheduler"
)

// StartDiscoveryScanScheduler launches the opt-in continuous-discovery
// scheduler (slice 3a) in a background goroutine. It re-runs + persists AWS
// scans on the given interval so scan history accrues automatically.
//
// Prerequisites mirror the AWS scan path: the credstore + its encryption key
// must be wired (the key also installs the production AWS scanner factory via
// WithCredstoreKey). When either is missing, the scheduler logs and does not
// start — the on-demand scan endpoints are unaffected.
//
// The scheduler reuses runAWSScan (audit + persistence included) via the
// exported RunScanForAccount; it adds no new scan logic. The demo account is
// excluded — its scan short-circuits before persistence and would be wasted
// work.
//
// The goroutine stops when ctx is cancelled (wire it to process shutdown).
func (s *Server) StartDiscoveryScanScheduler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	if s.discoveryCredStore == nil || s.discoveryCredKey == nil {
		if s.logger != nil {
			s.logger.Warn("discovery scan scheduler not started: credstore or key not wired")
		}
		return
	}

	h := handlers.NewDiscoveryHandlers(s.discoveryCredStore, s.logger).
		WithCredstoreKey(s.discoveryCredKey)
	if s.auditService != nil {
		h.WithAuditService(s.auditService)
	}
	if s.appStore != nil {
		h.WithScanStore(s.appStore)
	}

	credStore := s.discoveryCredStore
	logger := s.logger
	sched := &scanscheduler.Scheduler{
		Interval: interval,
		Logger:   logger,
		ListAccounts: func(ctx context.Context) ([]string, error) {
			conns, err := credStore.ListConnections(ctx, credstore.ListFilter{Provider: credstore.ProviderAWS})
			if err != nil {
				return nil, err
			}
			ids := make([]string, 0, len(conns))
			for _, conn := range conns {
				if conn == nil || conn.AccountID == "" || demo.IsDemo(conn.AccountID) {
					continue
				}
				ids = append(ids, conn.AccountID)
			}
			return ids, nil
		},
		ScanAccount: h.RunScanForAccount,
	}
	go sched.Run(ctx)
}
