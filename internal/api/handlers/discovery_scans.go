// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Continuous-discovery — persisted scan history.
//
// slice 1 (v0.89.250) added the store + AWS persist + AWS history endpoints.
// slice 2 (v0.89.251) extends the persist + history endpoints to GCP / Azure /
// OCI via the shared writeScanList / writeScanDetail / recordScan helpers
// below — each cloud handler is a thin wrapper.
//
// ScopeID convention: it is the discovery route's own `:id` path parameter for
// that cloud — account_id for AWS, connection ID for GCP/Azure/OCI — so the
// history endpoints query by `:id` uniformly with no extra connection lookup.

// DiscoveryScanStore is the slim persistence surface the discovery handlers
// need. The application store satisfies it directly; tests substitute a fake.
type DiscoveryScanStore interface {
	SaveDiscoveryScan(ctx context.Context, rec *types.ScanRecord) error
	ListDiscoveryScans(ctx context.Context, provider, scopeID string, limit int) ([]*types.ScanRecord, error)
	GetDiscoveryScan(ctx context.Context, scanID string) (*types.ScanRecord, error)
}

// scanHistoryListLimit caps a history listing. Generous for now (no retention
// cap yet); the list endpoint never returns inventory blobs so the payload
// stays small even at the cap.
const scanHistoryListLimit = 50

// scanSummary projects the per-category counts an operator wants in a listing.
// Mirrors the mandatory scan_completed audit counts.
func scanSummary(r *scanner.Result) map[string]int {
	if r == nil {
		return map[string]int{}
	}
	return map[string]int{
		"compute":        len(r.Compute),
		"functions":      len(r.Functions),
		"databases":      len(r.Databases),
		"object_stores":  len(r.ObjectStores),
		"load_balancers": len(r.LoadBalancers),
		"clusters":       len(r.Clusters),
		"event_sources":  len(r.EventSources),
		"instrumented":   r.InstrumentedCount,
		"uninstrumented": r.UninstrumentedCount,
	}
}

// recordScan best-effort persists a completed scan under the given provider +
// scope (the route's :id). Nil store or result is a no-op; persistence failure
// logs but never propagates (the scan already succeeded for the caller).
func recordScan(ctx context.Context, store DiscoveryScanStore, logger *zap.Logger, provider, scopeID string, r *scanner.Result, resultJSON []byte) {
	if store == nil || r == nil {
		return
	}
	rec := &types.ScanRecord{
		ScanID:        r.ScanID,
		Provider:      provider,
		ScopeID:       scopeID,
		Regions:       r.Regions,
		StartedAt:     r.ScanStartedAt,
		CompletedAt:   r.ScanCompletedAt,
		Partial:       r.Partial,
		PartialReason: r.PartialReason,
		Summary:       scanSummary(r),
		ResultJSON:    string(resultJSON),
	}
	if err := store.SaveDiscoveryScan(ctx, rec); err != nil && logger != nil {
		logger.Warn("persist discovery scan failed",
			zap.Error(err), zap.String("scan_id", r.ScanID), zap.String("provider", provider))
	}
}

// writeScanList serves a newest-first scan history for (provider, scopeID).
// Shared by all four cloud list handlers.
func writeScanList(c *gin.Context, store DiscoveryScanStore, logger *zap.Logger, provider, scopeID string) {
	if scopeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": &scanner.HumanizedError{
			Code:    "ScanHistoryNotConfigured",
			Message: "Scan history isn't enabled on this deployment.",
		}})
		return
	}
	recs, err := store.ListDiscoveryScans(c.Request.Context(), provider, scopeID, scanHistoryListLimit)
	if err != nil {
		if logger != nil {
			logger.Error("list scans failed", zap.Error(err), zap.String("provider", provider), zap.String("scope_id", scopeID))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "ScanHistoryReadFailed",
			Message: "Squadron could not read scan history. The error has been logged; retry in a moment.",
		}})
		return
	}
	if recs == nil {
		recs = []*types.ScanRecord{}
	}
	c.JSON(http.StatusOK, gin.H{"scans": recs})
}

// writeScanDetail serves one scan including the full inventory. 404s when the
// scan is missing OR belongs to a different provider/scope than the path —
// prevents cross-scope ID guessing from leaking a record. Shared by all four
// cloud get handlers.
func writeScanDetail(c *gin.Context, store DiscoveryScanStore, logger *zap.Logger, provider, scopeID, scanID string) {
	if scanID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingScanID",
			Message: "Scan ID path parameter is required.",
		}})
		return
	}
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": &scanner.HumanizedError{
			Code:    "ScanHistoryNotConfigured",
			Message: "Scan history isn't enabled on this deployment.",
		}})
		return
	}
	rec, err := store.GetDiscoveryScan(c.Request.Context(), scanID)
	if err != nil {
		if logger != nil {
			logger.Error("get scan failed", zap.Error(err), zap.String("scan_id", scanID))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "ScanHistoryReadFailed",
			Message: "Squadron could not read the scan. The error has been logged; retry in a moment.",
		}})
		return
	}
	if rec == nil || rec.Provider != provider || rec.ScopeID != scopeID {
		c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
			Code:    "ScanNotFound",
			Message: "No scan with that ID exists for this connection.",
		}})
		return
	}
	out := gin.H{
		"scan_id":      rec.ScanID,
		"provider":     rec.Provider,
		"scope_id":     rec.ScopeID,
		"regions":      rec.Regions,
		"started_at":   rec.StartedAt,
		"completed_at": rec.CompletedAt,
		"partial":      rec.Partial,
		"summary":      rec.Summary,
		"created_at":   rec.CreatedAt,
	}
	if rec.PartialReason != "" {
		out["partial_reason"] = rec.PartialReason
	}
	if rec.ResultJSON != "" {
		out["result"] = json.RawMessage(rec.ResultJSON)
	}
	c.JSON(http.StatusOK, out)
}

// --- AWS history endpoints (DiscoveryHandlers) ---

// HandleAWSListScans — GET /api/v1/discovery/aws/connections/:id/scans.
func (h *DiscoveryHandlers) HandleAWSListScans(c *gin.Context) {
	writeScanList(c, h.scanStore, h.logger, "aws", strings.TrimSpace(c.Param("id")))
}

// HandleAWSGetScan — GET /api/v1/discovery/aws/connections/:id/scans/:scanID.
func (h *DiscoveryHandlers) HandleAWSGetScan(c *gin.Context) {
	writeScanDetail(c, h.scanStore, h.logger, "aws",
		strings.TrimSpace(c.Param("id")), strings.TrimSpace(c.Param("scanID")))
}
