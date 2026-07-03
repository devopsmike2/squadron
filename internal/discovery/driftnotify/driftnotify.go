// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package driftnotify computes and records "what changed" between two persisted
// discovery scans of the same scope. It was extracted from internal/api so the
// drift/notification logic can be unit-tested in isolation: that package links
// the entire cloud-SDK + gin + duckdb tree, which made its test binary too heavy
// to compile in constrained environments even though the drift code itself has
// no cloud dependency of its own.
//
// To stay SDK-free the package imports only discoverydrift (pure stdlib), the
// light applicationstore/types (struct-only), zap, and stdlib. The store and
// audit-recorder couplings are expressed as LOCAL minimal interfaces —
// internal/api's real appStore satisfies ScanStore structurally, and a tiny
// adapter there maps this package's AuditEntry onto the real services.AuditEntry.
// The two audit constants are duplicated (rather than importing internal/services
// and internal/discovery/credstore, both of which drag the duckdb/sqlite drivers).
package driftnotify

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/discoverydrift"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Audit field values, duplicated from internal/services.AuditActorSystem and
// internal/discovery/credstore.TargetTypeCloudConnection so this package needn't
// import those (they transitively pull the duckdb + sqlite cgo drivers). These
// strings are a stable wire contract; a change here must track those originals.
const (
	auditActorSystem        = "system"
	targetTypeCloudConn     = "cloud_connection"
	eventScanDriftDetected  = "discovery.scan_drift_detected"
	actionScanDriftDetected = "scan_drift_detected"
)

// ScanStore is the minimal persisted-scan read surface the drift notifier needs.
// It is a subset of internal/api/handlers.DiscoveryScanStore (List + Get only —
// the notifier never writes), so the api Server's appStore satisfies it
// structurally with no adapter.
type ScanStore interface {
	ListDiscoveryScans(ctx context.Context, provider, scopeID string, limit int) ([]*types.ScanRecord, error)
	GetDiscoveryScan(ctx context.Context, scanID string) (*types.ScanRecord, error)
}

// AuditEntry mirrors the fields of internal/services.AuditEntry that a drift
// event populates. Kept local so this package doesn't import internal/services.
type AuditEntry struct {
	Actor      string
	EventType  string
	TargetType string
	TargetID   string
	Action     string
	Payload    map[string]any
}

// AuditRecorder records a drift audit entry. internal/api adapts its real
// AuditService to this one-method interface via a closure that maps AuditEntry
// onto services.AuditEntry, keeping the type seam where both types are visible.
type AuditRecorder interface {
	Record(ctx context.Context, e AuditEntry) error
}

// Emitter rate-limits drift events per scope. With a positive cooldown it
// suppresses a scope's drift events that land within the window of the last
// emitted one — letting an operator scan frequently but be alerted less often.
// A zero cooldown disables suppression (every changed sweep emits). In-memory:
// rate-limiting that resets on restart is acceptable.
type Emitter struct {
	cooldown time.Duration
	mu       sync.Mutex
	last     map[string]time.Time
}

// NewEmitter builds an Emitter with the given per-scope cooldown.
func NewEmitter(cooldown time.Duration) *Emitter {
	return &Emitter{cooldown: cooldown, last: map[string]time.Time{}}
}

// allow reports whether a drift event for key may emit now, recording the time
// when it may. Called only when there IS a change, so the cooldown advances on
// real emits, never on quiet sweeps.
func (e *Emitter) allow(key string, now time.Time) bool {
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

// cappedIDs flattens a category diff field across categories, capped at limit.
func cappedIDs(lists [][]string, limit int) []string {
	out := []string{}
	for _, l := range lists {
		for _, id := range l {
			if len(out) >= limit {
				return out
			}
			out = append(out, id)
		}
	}
	return out
}

// warn logs a drift-path failure. These are best-effort/fail-open (a lost drift
// event never fails the scan), but silently swallowing the error made a missing
// drift signal impossible to diagnose — so the store/parse failure modes get a
// Warn with the scope for context.
func warn(logger *zap.Logger, msg, provider, scopeID string, err error) {
	if logger == nil {
		return
	}
	logger.Warn("discovery scan drift: "+msg,
		zap.String("provider", provider), zap.String("scope_id", scopeID), zap.Error(err))
}

// EmitIfChanged diffs the two most recent persisted scans for (provider,
// scopeID) and records a discovery.scan_drift_detected audit event when
// something changed. No-op when fewer than two scans exist (the first scheduled
// scan has nothing to compare against, so it does not emit an everything-is-new
// event), or when the store / audit recorder is unwired. instrumentation_
// regressions surfaces the highest-signal subset: resources whose OTel
// instrumentation turned OFF between scans.
func EmitIfChanged(ctx context.Context, store ScanStore, audit AuditRecorder, logger *zap.Logger, emitter *Emitter, provider, scopeID string) {
	if store == nil || audit == nil {
		return
	}
	// Distinguish genuine failures (log at Warn so a lost drift signal is
	// diagnosable) from the legitimately-quiet "fewer than two scans" case (the
	// first scheduled scan has nothing to compare against — no log, no event).
	recs, err := store.ListDiscoveryScans(ctx, provider, scopeID, 2)
	if err != nil {
		warn(logger, "list scans failed", provider, scopeID, err)
		return
	}
	if len(recs) < 2 {
		return
	}
	newer, err := store.GetDiscoveryScan(ctx, recs[0].ScanID)
	if err != nil || newer == nil {
		warn(logger, "fetch newer scan failed", provider, scopeID, err)
		return
	}
	older, err := store.GetDiscoveryScan(ctx, recs[1].ScanID)
	if err != nil || older == nil {
		warn(logger, "fetch older scan failed", provider, scopeID, err)
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
		warn(logger, "diff computation failed", provider, scopeID, err)
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
	_ = audit.Record(ctx, AuditEntry{
		Actor:      auditActorSystem,
		EventType:  eventScanDriftDetected,
		TargetType: targetTypeCloudConn,
		TargetID:   scopeID,
		Action:     actionScanDriftDetected,
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
