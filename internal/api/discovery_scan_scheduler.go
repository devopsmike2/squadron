// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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
func (s *Server) StartDiscoveryScanScheduler(ctx context.Context, interval, driftCooldown time.Duration) {
	if interval <= 0 {
		return
	}
	emitter := newDriftEmitter(driftCooldown)

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
			ScanAccount: s.scanAccountWithDrift(emitter, "aws", h.RunScanForAccount),
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
			ScanAccount: s.scanAccountWithDrift(emitter, "gcp", func(ctx context.Context, id string) error {
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
			ScanAccount: s.scanAccountWithDrift(emitter, "azure", func(ctx context.Context, id string) error {
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
			ScanAccount: s.scanAccountWithDrift(emitter, "oci", func(ctx context.Context, id string) error {
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
func (s *Server) scanAccountWithDrift(emitter *driftEmitter, provider string, scan func(context.Context, string) error) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		if err := scan(ctx, id); err != nil {
			return err
		}
		emitDriftIfChanged(ctx, s.appStore, s.auditService, s.logger, emitter, provider, id)
		return nil
	}
}

// driftEmitter rate-limits drift events per scope. With a positive cooldown it
// suppresses a scope's drift events that land within the window of the last
// emitted one — letting an operator scan frequently but be alerted less often.
// A zero cooldown disables suppression (every changed sweep emits). In-memory:
// rate-limiting that resets on restart is acceptable.
type driftEmitter struct {
	cooldown time.Duration
	mu       sync.Mutex
	last     map[string]time.Time
}

func newDriftEmitter(cooldown time.Duration) *driftEmitter {
	return &driftEmitter{cooldown: cooldown, last: map[string]time.Time{}}
}

// allow reports whether a drift event for key may emit now, recording the time
// when it may. Called only when there IS a change, so the cooldown advances on
// real emits, never on quiet sweeps.
func (e *driftEmitter) allow(key string, now time.Time) bool {
	if e == nil || e.cooldown <= 0 {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.last[key]; ok && now.Sub(t) < e.cooldown {
		return false
	}
	e.last[key] = now
	return true
}

// cappedIDs flattens a category diff field across categories, capped.
func cappedIDs(lists [][]string, cap int) []string {
	out := []string{}
	for _, l := range lists {
		for _, id := range l {
			if len(out) >= cap {
				return out
			}
			out = append(out, id)
		}
	}
	return out
}

// driftWarn logs a drift-path failure. These are best-effort/fail-open (a lost
// drift event never fails the scan), but silently swallowing the error made a
// missing drift signal impossible to diagnose — so the store/parse failure
// modes get a Warn with the scope for context.
func driftWarn(logger *zap.Logger, msg, provider, scopeID string, err error) {
	if logger == nil {
		return
	}
	logger.Warn("discovery scan drift: "+msg,
		zap.String("provider", provider), zap.String("scope_id", scopeID), zap.Error(err))
}

// emitDriftIfChanged diffs the two most recent persisted scans for
// (provider, scopeID) and records a discovery.scan_drift_detected audit event
// when something changed. No-op when fewer than two scans exist (the first
// scheduled scan has nothing to compare against, so it does not emit an
// everything-is-new event), or when the store / audit service is unwired.
// instrumentation_regressions surfaces the highest-signal subset: resources
// whose OTel instrumentation turned OFF between scans.
func emitDriftIfChanged(ctx context.Context, store handlers.DiscoveryScanStore, audit services.AuditService, logger *zap.Logger, emitter *driftEmitter, provider, scopeID string) {
	if store == nil || audit == nil {
		return
	}
	// Distinguish genuine failures (log at Warn so a lost drift signal is
	// diagnosable) from the legitimately-quiet "fewer than two scans" case (the
	// first scheduled scan has nothing to compare against — no log, no event).
	recs, err := store.ListDiscoveryScans(ctx, provider, scopeID, 2)
	if err != nil {
		driftWarn(logger, "list scans failed", provider, scopeID, err)
		return
	}
	if len(recs) < 2 {
		return
	}
	newer, err := store.GetDiscoveryScan(ctx, recs[0].ScanID)
	if err != nil || newer == nil {
		driftWarn(logger, "fetch newer scan failed", provider, scopeID, err)
		return
	}
	older, err := store.GetDiscoveryScan(ctx, recs[1].ScanID)
	if err != nil || older == nil {
		driftWarn(logger, "fetch older scan failed", provider, scopeID, err)
		return
	}
	// A partial scan is not a trustworthy inventory: a tier/IAM failure (e.g.
	// AccessDenied on ec2:DescribeInstances) sets Partial=true and yields an
	// empty or short category, which the diff would read as "everything
	// removed" — not because resources disappeared but because they were never
	// scanned. Worse, once a full scan follows a persisted partial one, the
	// partial becomes the older side and the same absence reads as "everything
	// added". Either side being partial makes the before/after untrustworthy,
	// so skip until two consecutive full scans give a real comparison. (The
	// scan itself is still persisted for history; only the drift signal waits.)
	if newer.Partial || older.Partial {
		if logger != nil {
			logger.Debug("discovery scan drift skipped: partial scan in comparison window",
				zap.String("provider", provider), zap.String("scope_id", scopeID),
				zap.Bool("newer_partial", newer.Partial), zap.Bool("older_partial", older.Partial))
		}
		return
	}
	diff, err := discoverydrift.Between(older.ResultJSON, newer.ResultJSON)
	if err != nil {
		// Malformed persisted scan JSON: the diff can't be trusted, so no event
		// — but log it, otherwise the drift signal vanishes with no trace.
		driftWarn(logger, "diff computation failed", provider, scopeID, err)
		return
	}
	if diff.TotalAdded == 0 && diff.TotalRemoved == 0 && diff.TotalInstrumentationChanged == 0 {
		return
	}
	if !emitter.allow(provider+"/"+scopeID, time.Now()) {
		if logger != nil {
			logger.Debug("discovery scan drift suppressed by cooldown",
				zap.String("provider", provider), zap.String("scope_id", scopeID))
		}
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
			"added":                         cappedIDs([][]string{diff.Compute.Added, diff.Functions.Added, diff.Databases.Added, diff.Clusters.Added}, 20),
			"removed":                       cappedIDs([][]string{diff.Compute.Removed, diff.Functions.Removed, diff.Databases.Removed, diff.Clusters.Removed}, 20),
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
