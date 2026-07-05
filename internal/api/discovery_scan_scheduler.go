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

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/api/handlers"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/driftnotify"
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
	emitter := driftnotify.NewEmitter(driftCooldown)

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
			// ADR 0013 §D6-b: re-Get the connection and stamp the owning
			// tenant onto ctx BEFORE scanAccountWithDrift runs, so BOTH the
			// scan's SaveDiscoveryScan write AND the drift emit inherit the
			// tenant (see stampOwnerTenant). The scheduler's top-level
			// ListAccounts stays system-scoped (sees every tenant's
			// connections); only this per-connection write ctx is
			// re-stamped. Inert in OSS — every connection's tenant is
			// "default".
			ScanAccount: func(ctx context.Context, id string) error {
				ctx = stampOwnerTenant(ctx, id, func(ctx context.Context, id string) string {
					if conn, err := credStore.GetConnection(ctx, id); err == nil && conn != nil {
						return conn.TenantID
					}
					return ""
				})
				return s.scanAccountWithDrift(emitter, "aws", h.RunScanForAccount)(ctx, id)
			},
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
			// ADR 0013 §D6-b: stamp the owning tenant onto ctx before
			// scanAccountWithDrift so both the scan write and the drift
			// emit land in the connection's tenant (see stampOwnerTenant).
			// Inert in OSS.
			ScanAccount: func(ctx context.Context, id string) error {
				ctx = stampOwnerTenant(ctx, id, func(ctx context.Context, id string) string {
					if conn, err := store.Get(ctx, id); err == nil && conn != nil {
						return conn.TenantID
					}
					return ""
				})
				return s.scanAccountWithDrift(emitter, "gcp", func(ctx context.Context, id string) error {
					return invokeScanHandler(ctx, id, h.HandleScanGCPConnection)
				})(ctx, id)
			},
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
			// ADR 0013 §D6-b: stamp the owning Squadron tenant onto ctx
			// before scanAccountWithDrift (see stampOwnerTenant). Uses
			// SquadronTenantID — the Squadron owner tenant, DISTINCT from
			// the Azure-AD TenantID on the connection. Inert in OSS.
			ScanAccount: func(ctx context.Context, id string) error {
				ctx = stampOwnerTenant(ctx, id, func(ctx context.Context, id string) string {
					if conn, err := store.Get(ctx, id); err == nil && conn != nil {
						return conn.SquadronTenantID
					}
					return ""
				})
				return s.scanAccountWithDrift(emitter, "azure", func(ctx context.Context, id string) error {
					return invokeScanHandler(ctx, id, h.HandleScanAzureConnection)
				})(ctx, id)
			},
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
			// ADR 0013 §D6-b: stamp the owning Squadron tenant onto ctx
			// before scanAccountWithDrift (see stampOwnerTenant). Uses
			// OwnerTenantID — the Squadron owner tenant, DISTINCT from the
			// OCI TenancyOCID on the connection. Inert in OSS.
			ScanAccount: func(ctx context.Context, id string) error {
				ctx = stampOwnerTenant(ctx, id, func(ctx context.Context, id string) string {
					if conn, err := store.Get(ctx, id); err == nil && conn != nil {
						return conn.OwnerTenantID
					}
					return ""
				})
				return s.scanAccountWithDrift(emitter, "oci", func(ctx context.Context, id string) error {
					return invokeScanHandler(ctx, id, h.HandleScanOCIConnection)
				})(ctx, id)
			},
		}
		go sched.Run(ctx)
	}
}

// stampOwnerTenant re-Gets a connection's Squadron owner tenant (via the
// per-cloud ownerOf lookup) and, when non-empty, stamps it onto ctx (ADR
// 0013 §D6-b). It is called at the ScanAccount-closure boundary — BEFORE
// scanAccountWithDrift runs — so BOTH the scan's SaveDiscoveryScan write
// AND the drift emit inherit the tenant'd ctx. Go passes ctx by value, so
// stamping inside the inner scan fn would leave the drift emit (which uses
// the ctx that entered scanAccountWithDrift) on the un-stamped ctx; hence
// this boundary. ownerOf returns "" when the connection is missing or has
// no owner tenant, in which case ctx is returned unchanged — and the
// downstream store's tenantScope falls back to DefaultTenant. Inert in
// OSS: every connection's owner tenant is "default", so the stamp is a
// no-op relative to today's behavior.
func stampOwnerTenant(ctx context.Context, id string, ownerOf func(context.Context, string) string) context.Context {
	if owner := ownerOf(ctx, id); owner != "" {
		return identity.WithTenant(ctx, owner)
	}
	return ctx
}

// scanAccountWithDrift wraps a scan function so that, after a successful
// scheduled scan, drift versus the previous scan is computed and (when anything
// changed) recorded as an audit event. This turns the continuous engine from
// "history accrues" into a proactive "what changed" signal that rides the
// existing audit timeline + SIEM forwarding — no polling required.
//
// The drift computation itself lives in internal/discovery/driftnotify (SDK-free
// so it can be unit-tested without this package's cloud-SDK tree). s.appStore
// satisfies driftnotify.ScanStore structurally; s.auditService is adapted via
// driftAuditRecorder. When the audit service is unwired, a nil recorder is
// passed so EmitIfChanged's own guard no-ops (preserving prior behavior).
func (s *Server) scanAccountWithDrift(emitter *driftnotify.Emitter, provider string, scan func(context.Context, string) error) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		if err := scan(ctx, id); err != nil {
			return err
		}
		var rec driftnotify.AuditRecorder
		if s.auditService != nil {
			rec = driftAuditRecorder{svc: s.auditService}
		}
		driftnotify.EmitIfChanged(ctx, s.appStore, rec, s.logger, emitter, provider, id)
		return nil
	}
}

// driftAuditRecorder adapts the Server's AuditService to driftnotify.
// AuditRecorder, mapping the SDK-free driftnotify.AuditEntry onto the real
// services.AuditEntry. Keeping this seam here (where both types are visible)
// lets internal/discovery/driftnotify stay free of internal/services, which
// transitively drags the duckdb driver.
type driftAuditRecorder struct{ svc services.AuditService }

func (a driftAuditRecorder) Record(ctx context.Context, e driftnotify.AuditEntry) error {
	return a.svc.Record(ctx, services.AuditEntry{
		Actor:      e.Actor,
		EventType:  e.EventType,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Action:     e.Action,
		Payload:    e.Payload,
	})
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
