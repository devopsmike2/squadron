// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// AuditHandlers serves the audit log endpoints. Read-only — audit entries
// are only created via the service layer, never through HTTP.
type AuditHandlers struct {
	auditService services.AuditService
	logger       *zap.Logger
}

func NewAuditHandlers(auditService services.AuditService, logger *zap.Logger) *AuditHandlers {
	return &AuditHandlers{auditService: auditService, logger: logger}
}

// HandleListAuditEvents serves GET /api/v1/audit/events.
//
// Query parameters (all optional):
//   - target_type=agent|group|config|rule
//   - target_id=<uuid|string>
//   - since=<RFC3339 timestamp>
//   - limit=<int, default 100, max 1000>
//
// Returns {events: [...]} sorted newest-first.
func (h *AuditHandlers) HandleListAuditEvents(c *gin.Context) {
	filter := services.AuditEventFilter{
		TargetType: c.Query("target_type"),
		TargetID:   c.Query("target_id"),
	}

	if raw := c.Query("since"); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid `since` — expected RFC3339",
				"detail": err.Error(),
			})
			return
		}
		filter.Since = ts
	}

	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "invalid `limit` — expected positive integer",
			})
			return
		}
		filter.Limit = n
	}

	events, err := h.auditService.List(c.Request.Context(), filter)
	if err != nil {
		h.logger.Error("failed to list audit events", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list audit events"})
		return
	}
	if events == nil {
		events = []*services.AuditEvent{}
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}
