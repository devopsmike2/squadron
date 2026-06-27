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

// Continuous-discovery slice 1 (v0.89.250) — persisted scan history.
//
// Scans were synchronous + non-persisted: the only durable trace was the
// scan_completed audit event (summary counts, no inventory). This adds a
// whole-scan record so an operator gets scan history + a basis for drift.
// Slice 1 persists + exposes history for AWS; the store + record shape are
// provider-neutral so GCP/Azure/OCI follow mechanically in slice 2.

// DiscoveryScanStore is the slim persistence surface the discovery handlers
// need. The application store satisfies it directly; tests substitute a fake.
type DiscoveryScanStore interface {
	SaveDiscoveryScan(ctx context.Context, rec *types.ScanRecord) error
	ListDiscoveryScans(ctx context.Context, provider, scopeID string, limit int) ([]*types.ScanRecord, error)
	GetDiscoveryScan(ctx context.Context, scanID string) (*types.ScanRecord, error)
}

// scanHistoryListLimit caps a history listing. Generous for slice 1 (no
// retention cap yet); the list endpoint never returns the inventory blobs so
// the payload stays small even at the cap.
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

// recordScan best-effort persists a completed scan. Nil store or result is a
// no-op; persistence failure logs but never propagates (the scan already
// succeeded for the caller).
func recordScan(ctx context.Context, store DiscoveryScanStore, logger *zap.Logger, provider string, r *scanner.Result, resultJSON []byte) {
	if store == nil || r == nil {
		return
	}
	rec := &types.ScanRecord{
		ScanID:        r.ScanID,
		Provider:      provider,
		ScopeID:       r.AccountID,
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

// HandleAWSListScans — GET /api/v1/discovery/aws/connections/:id/scans.
// Newest-first scan history for the account (summary rows, no inventory blob).
func (h *DiscoveryHandlers) HandleAWSListScans(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	if accountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingAccountID",
			Message: "Account ID path parameter is required.",
		}})
		return
	}
	if h.scanStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": &scanner.HumanizedError{
			Code:    "ScanHistoryNotConfigured",
			Message: "Scan history isn't enabled on this deployment.",
		}})
		return
	}
	recs, err := h.scanStore.ListDiscoveryScans(c.Request.Context(), "aws", accountID, scanHistoryListLimit)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws list scans failed", zap.Error(err), zap.String("account_id", accountID))
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

// HandleAWSGetScan — GET /api/v1/discovery/aws/connections/:id/scans/:scanID.
// One scan including the full marshaled inventory.
func (h *DiscoveryHandlers) HandleAWSGetScan(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	scanID := strings.TrimSpace(c.Param("scanID"))
	if scanID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingScanID",
			Message: "Scan ID path parameter is required.",
		}})
		return
	}
	if h.scanStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": &scanner.HumanizedError{
			Code:    "ScanHistoryNotConfigured",
			Message: "Scan history isn't enabled on this deployment.",
		}})
		return
	}
	rec, err := h.scanStore.GetDiscoveryScan(c.Request.Context(), scanID)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws get scan failed", zap.Error(err), zap.String("scan_id", scanID))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "ScanHistoryReadFailed",
			Message: "Squadron could not read the scan. The error has been logged; retry in a moment.",
		}})
		return
	}
	// 404 when missing OR when the scan belongs to a different provider/scope
	// than the path — prevents cross-scope ID guessing from leaking a record.
	if rec == nil || rec.Provider != "aws" || rec.ScopeID != accountID {
		c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
			Code:    "ScanNotFound",
			Message: "No scan with that ID exists for this account.",
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
