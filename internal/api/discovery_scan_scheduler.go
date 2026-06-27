// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/api/handlers"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/discoverydrift"
	"github.com/devopsmike2/squadron/internal/discovery/scanscheduler"
	"github.com/devopsmike2/squadron/internal/services"
)

// StartDiscoveryScanScheduler launches the opt-in continuous-discovery
// scheduler (slice 3) in background goroutines — one per cloud whose substrate
// is wired. It re-runs + persists discovery scans on the given interval so scan
// history accrues automatically.
//
// AWS (slice 3a) uses the cleanly-extracted RunScanForAccount entry. GCP /
// Azure / OCI (slice 3b) reuse their existing scan handlers verbatim by
// invoking them through an internal synthetic gin context — this preserves full
// parity with on-demand scans (audit events, tier dispatch, persistence)
// without a risky extraction of three large gin-coupled handlers. Replacing the
// synthetic-context dispatch with a handler-extracted core is a noted future
// cleanup; the behavior is identical either way.
//
// Each goroutine stops when ctx is cancelled (wire it to process shutdown).
func (s *Server) StartDiscoveryScanScheduler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	// --- AWS (slice 3a) ---
	if s.discoveryCredStore != nil && s.discoveryCredKey != nil {
		h := handlers.NewDiscoveryHandlers(s.discoveryCredStore, s.logger).
			WithCredstoreKey(s.discoveryCredKey)
		if s.auditService != nil {
			h.WithAuditService(s.auditService)
		}
		if s.appStore != nil {
			h.WithScanStore(s.appStore)
		}
		credStore := s.discoveryCredStore
		sched := &scanscheduler.Scheduler{
			Interval: interval,
			Logger:   s.logger,
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
			ScanAccount: s.scanAccountWithDrift("aws", h.RunScanForAccount),
		}
		go sched.Run(ctx)
	} else if s.logger != nil {
		s.logger.Warn("discovery scan scheduler: AWS not started (credstore or key not wired)")
	}

	// --- GCP (slice 3b) ---
	if s.discoveryGCPStore != nil && s.discoveryCredKey != nil && s.discoveryGCPScannerFactory != nil {
		h := handlers.NewDiscoveryGCPHandlers(s.discoveryGCPStore, s.logger).
			WithGCPCredstoreKey(s.discoveryCredKey).
			WithGCPScannerFactory(s.discoveryGCPScannerFactory)
		if s.auditService != nil {
			h.WithGCPAuditService(s.auditService)
		}
		if s.appStore != nil {
			h.WithGCPScanStore(s.appStore)
		}
		store := s.discoveryGCPStore
		sched := &scanscheduler.Scheduler{
			Interval: interval,
			Logger:   s.logger,
			ListAccounts: func(ctx context.Context) ([]string, error) {
				conns, err := store.List(ctx)
				if err != nil {
					return nil, err
				}
				ids := make([]string, 0, len(conns))
				for _, conn := range conns {
					if conn == nil || conn.ID == "" || demo.IsGCPDemoProject(conn.ProjectID) {
						continue
					}
					ids = append(ids, conn.ID)
				}
				return ids, nil
			},
			ScanAccount: s.scanAccountWithDrift("gcp", func(ctx context.Context, id string) error {
				return invokeScanHandler(ctx, id, h.HandleScanGCPConnection)
			}),
		}
		go sched.Run(ctx)
	}

	// --- Azure (slice 3b) ---
	if s.discoveryAzureStore != nil && s.discoveryCredKey != nil && s.discoveryAzureScannerFactory != nil {
		h := handlers.NewDiscoveryAzureHandlers(s.discoveryAzureStore, s.logger).
			WithAzureCredstoreKey(s.discoveryCredKey).
			WithAzureScannerFactory(s.discoveryAzureScannerFactory)
		if s.auditService != nil {
			h.WithAzureAuditService(s.auditService)
		}
		if s.appStore != nil {
			h.WithAzureScanStore(s.appStore)
		}
		store := s.discoveryAzureStore
		sched := &scanscheduler.Scheduler{
			Interval: interval,
			Logger:   s.logger,
			ListAccounts: func(ctx context.Context) ([]string, error) {
				conns, err := store.List(ctx)
				if err != nil {
					return nil, err
				}
				ids := make([]string, 0, len(conns))
				for _, conn := range conns {
					if conn == nil || conn.ID == "" || demo.IsAzureDemoSubscription(conn.SubscriptionID) {
						continue
					}
					ids = append(ids, conn.ID)
				}
				return ids, nil
			},
			ScanAccount: s.scanAccountWithDrift("azure", func(ctx context.Context, id string) error {
				return invokeScanHandler(ctx, id, h.HandleScanAzureConnection)
			}),
		}
		go sched.Run(ctx)
	}

	// --- OCI (slice 3b) ---
	if s.discoveryOCIStore != nil && s.discoveryCredKey != nil && s.discoveryOCIScannerFactory != nil {
		h := handlers.NewDiscoveryOCIHandlers(s.discoveryOCIStore, s.logger).
			WithOCICredstoreKey(s.discoveryCredKey).
			WithOCIScannerFactory(s.discoveryOCIScannerFactory)
		if s.auditService != nil {
			h.WithOCIAuditService(s.auditService)
		}
		if s.appStore != nil {
			h.WithOCIScanStore(s.appStore)
		}
		store := s.discoveryOCIStore
		sched := &scanscheduler.Scheduler{
			Interval: interval,
			Logger:   s.logger,
			ListAccounts: func(ctx context.Context) ([]string, error) {
				conns, err := store.List(ctx)
				if err != nil {
					return nil, err
				}
				ids := make([]string, 0, len(conns))
				for _, conn := range conns {
					if conn == nil || conn.ID == "" || demo.IsOCIDemoTenancy(conn.TenancyOCID) {
						continue
					}
					ids = append(ids, conn.ID)
				}
				return ids, nil
			},
			ScanAccount: s.scanAccountWithDrift("oci", func(ctx context.Context, id string) error {
				return invokeScanHandler(ctx, id, h.HandleScanOCIConnection)
			}),
		}
		go sched.Run(ctx)
	}
}

// scanAccountWithDrift wraps a scan function so that, after a successful
// scheduled scan, drift versus the previous scan is computed and (when anything
// changed) recorded as an audit event. This turns the continuous engine from
// "history accrues" into a proactive "what changed" signal that rides the
// existing audit timeline + SIEM forwarding — no polling required.
func (s *Server) scanAccountWithDrift(provider string, scan func(context.Context, string) error) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		if err := scan(ctx, id); err != nil {
			return err
		}
		emitDriftIfChanged(ctx, s.appStore, s.auditService, s.logger, provider, id)
		return nil
	}
}

// emitDriftIfChanged diffs the two most recent persisted scans for
// (provider, scopeID) and records a discovery.scan_drift_detected audit event
// when something changed. No-op when fewer than two scans exist (the first
// scheduled scan has nothing to compare against, so it does not emit an
// everything-is-new event), or when the store / audit service is unwired.
// instrumentation_regressions surfaces the highest-signal subset: resources
// whose OTel instrumentation turned OFF between scans.
func emitDriftIfChanged(ctx context.Context, store handlers.DiscoveryScanStore, audit services.AuditService, logger *zap.Logger, provider, scopeID string) {
	if store == nil || audit == nil {
		return
	}
	recs, err := store.ListDiscoveryScans(ctx, provider, scopeID, 2)
	if err != nil || len(recs) < 2 {
		return
	}
	newer, err := store.GetDiscoveryScan(ctx, recs[0].ScanID)
	if err != nil || newer == nil {
		return
	}
	older, err := store.GetDiscoveryScan(ctx, recs[1].ScanID)
	if err != nil || older == nil {
		return
	}
	diff, err := discoverydrift.Between(older.ResultJSON, newer.ResultJSON)
	if err != nil {
		return
	}
	if diff.TotalAdded == 0 && diff.TotalRemoved == 0 && diff.TotalInstrumentationChanged == 0 {
		return
	}
	regressions := []string{}
	for _, f := range diff.Compute.InstrumentationChanged {
		if !f.Now {
			regressions = append(regressions, f.ResourceID)
		}
	}
	for _, f := range diff.Functions.InstrumentationChanged {
		if !f.Now {
			regressions = append(regressions, f.ResourceID)
		}
	}
	_ = audit.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  "discovery.scan_drift_detected",
		TargetType: credstore.TargetTypeCloudConnection,
		TargetID:   scopeID,
		Action:     "scan_drift_detected",
		Payload: map[string]any{
			"provider":                      provider,
			"scope_id":                      scopeID,
			"from_scan_id":                  older.ScanID,
			"to_scan_id":                    newer.ScanID,
			"total_added":                   diff.TotalAdded,
			"total_removed":                 diff.TotalRemoved,
			"total_instrumentation_changed": diff.TotalInstrumentationChanged,
			"instrumentation_regressions":   regressions,
			"recorded_at":                   time.Now().UTC(),
		},
	})
	if logger != nil {
		logger.Info("discovery scan drift detected",
			zap.String("provider", provider), zap.String("scope_id", scopeID),
			zap.Int("added", diff.TotalAdded), zap.Int("removed", diff.TotalRemoved),
			zap.Int("instrumentation_changed", diff.TotalInstrumentationChanged))
	}
}

// invokeScanHandler drives a per-cloud scan handler outside the HTTP path by
// building a synthetic gin context (connection id as the :id param, empty body
// so the handler's tier parse falls back to defaults, the scheduler's context
// for cancellation). The handler's persistence + audit side effects run
// exactly as on-demand; the recorded response is discarded. A >=400 status
// surfaces as an error the scheduler counts + logs.
func invokeScanHandler(ctx context.Context, connID string, handler func(*gin.Context)) error {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	c.Params = gin.Params{{Key: "id", Value: connID}}
	handler(c)
	if rec.Code >= http.StatusBadRequest {
		return fmt.Errorf("scan %s: http %d", connID, rec.Code)
	}
	return nil
}
