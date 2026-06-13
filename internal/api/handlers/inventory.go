// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/inventory"
	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// InventoryHandlers serves v0.32 expected-vs-actual reconciliation
// endpoints. Construct with NewInventoryHandlers; the service does
// the work, the handler is HTTP plumbing.
type InventoryHandlers struct {
	service *inventory.Service
	logger  *zap.Logger
}

func NewInventoryHandlers(svc *inventory.Service, logger *zap.Logger) *InventoryHandlers {
	return &InventoryHandlers{service: svc, logger: logger}
}

// ExpectedAgentBody is the request shape for individual upserts +
// the bulk replace endpoint. Hostname is required; Labels and Notes
// are free-form. Source is required on the bulk path; the upsert
// endpoint takes it from the body too so a single CLI call can add
// one row.
type ExpectedAgentBody struct {
	Hostname string            `json:"hostname" binding:"required"`
	Labels   map[string]string `json:"labels,omitempty"`
	Source   string            `json:"source,omitempty"`
	Notes    string            `json:"notes,omitempty"`
}

// ReplaceExpectedBody is the bulk-rotate request shape. Source is
// the pipeline identifier (e.g. "gha-otel-deploy") that owns this
// inventory slice; entries replace any rows previously tagged with
// the same source.
type ReplaceExpectedBody struct {
	Source  string              `json:"source" binding:"required"`
	Entries []ExpectedAgentBody `json:"entries"`
}

// HandleReconcile is GET /api/v1/inventory/reconciliation. The
// optional `source` query parameter narrows the report to one
// pipeline's view; without it the report includes unexpected hosts
// (since the operator has no scoped sense of "unexpected").
func (h *InventoryHandlers) HandleReconcile(c *gin.Context) {
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory service not configured"})
		return
	}
	source := c.Query("source")
	report, err := h.service.Reconcile(c.Request.Context(), source)
	if err != nil {
		h.logger.Warn("inventory reconcile failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, report)
}

// HandleListExpected is GET /api/v1/inventory/expected. Returns
// every expected entry, optionally filtered by source.
func (h *InventoryHandlers) HandleListExpected(c *gin.Context) {
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory service not configured"})
		return
	}
	source := c.Query("source")
	entries, err := h.service.List(c.Request.Context(), source)
	if err != nil {
		h.logger.Warn("inventory list expected failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"items": entries,
		"count": len(entries),
	})
}

// HandleUpsertExpected is POST /api/v1/inventory/expected. Adds or
// updates a single row. The body's source field is required so the
// row can be removed cleanly by the bulk-rotate path later.
func (h *InventoryHandlers) HandleUpsertExpected(c *gin.Context) {
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory service not configured"})
		return
	}
	var body ExpectedAgentBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.Source == "" {
		body.Source = "manual"
	}
	entry := &apptypes.ExpectedAgent{
		Hostname:      body.Hostname,
		Labels:        body.Labels,
		Source:        body.Source,
		Notes:         body.Notes,
		ExpectedSince: time.Now().UTC(),
	}
	if err := h.service.Upsert(c.Request.Context(), entry); err != nil {
		h.logger.Warn("inventory upsert failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, entry)
}

// HandleDeleteExpected is DELETE /api/v1/inventory/expected/:hostname.
func (h *InventoryHandlers) HandleDeleteExpected(c *gin.Context) {
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory service not configured"})
		return
	}
	hostname := c.Param("hostname")
	if hostname == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hostname required"})
		return
	}
	if err := h.service.Delete(c.Request.Context(), hostname); err != nil {
		h.logger.Warn("inventory delete failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "hostname": hostname})
}

// HandleReplaceExpected is PUT /api/v1/inventory/expected. This is
// the GHA-friendly bulk-rotate endpoint: every existing row tagged
// with `source` is dropped, and the new list is inserted in its
// place. Idempotent on the wire so CI can retry without duplicating
// entries.
func (h *InventoryHandlers) HandleReplaceExpected(c *gin.Context) {
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory service not configured"})
		return
	}
	var body ReplaceExpectedBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(body.Source) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source required"})
		return
	}
	entries := make([]*apptypes.ExpectedAgent, 0, len(body.Entries))
	for _, e := range body.Entries {
		if e.Hostname == "" {
			continue
		}
		entries = append(entries, &apptypes.ExpectedAgent{
			Hostname: e.Hostname,
			Labels:   e.Labels,
			Source:   body.Source,
			Notes:    e.Notes,
		})
	}
	if err := h.service.ReplaceExpected(c.Request.Context(), body.Source, entries); err != nil {
		h.logger.Warn("inventory replace failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "source": body.Source, "count": len(entries)})
}
