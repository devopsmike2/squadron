// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/billing"
)

// BillingHandlers exposes /api/v1/billing/snapshot — a thin shell
// that calls the registered SnapshotProvider, caches the result for
// the cache duration, and returns it.
//
// Why cache: Splunk searches are expensive (the license report scan
// hits the _internal index over a 30d window). Polling every page
// view would push real CPU on the Splunk side. 30 minutes is the
// sweet spot — refreshes fast enough for an operator iterating, slow
// enough that finance-style usage doesn't accidentally DDoS Splunk.
//
// Added in v0.42.0 (connectors part 2).
type BillingHandlers struct {
	provider billing.SnapshotProvider
	logger   *zap.Logger

	mu       sync.Mutex
	cached   *billing.Snapshot
	cachedAt time.Time
	cacheTTL time.Duration
}

// NewBillingHandlers builds the handler. Pass a nil provider when no
// connector is configured; the handler will return 204 NoContent so
// the UI cleanly hides the billing tile.
func NewBillingHandlers(provider billing.SnapshotProvider, logger *zap.Logger) *BillingHandlers {
	return &BillingHandlers{
		provider: provider,
		logger:   logger,
		cacheTTL: 30 * time.Minute,
	}
}

// HandleSnapshot is GET /api/v1/billing/snapshot.
//
// Returns 204 NoContent when no provider is configured. Returns a
// cached snapshot when one is fresh; refreshes otherwise.
func (h *BillingHandlers) HandleSnapshot(c *gin.Context) {
	if h.provider == nil {
		// No provider wired — the UI uses 204 as the signal to hide
		// the billing tile. We could 404, but 204 keeps the URL
		// stable for future provisioning.
		c.Status(http.StatusNoContent)
		return
	}
	h.mu.Lock()
	stale := h.cached == nil || time.Since(h.cachedAt) > h.cacheTTL
	cached := h.cached
	h.mu.Unlock()
	if !stale && cached != nil {
		c.JSON(http.StatusOK, cached)
		return
	}
	snap, err := h.provider.Snapshot(c.Request.Context())
	if err != nil {
		h.logger.Warn("billing snapshot failed", zap.Error(err))
		// Don't 500 — if Splunk is having a bad day, the UI tile
		// should fall back to "actuals unavailable" rather than a
		// crash banner. 502 is the honest status code.
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	h.mu.Lock()
	h.cached = snap
	h.cachedAt = time.Now().UTC()
	h.mu.Unlock()
	c.JSON(http.StatusOK, snap)
}
