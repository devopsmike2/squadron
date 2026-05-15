// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// AlertHandlers handles /api/v1/alerts/rules endpoints.
type AlertHandlers struct {
	alertService services.AlertService
	logger       *zap.Logger
}

// NewAlertHandlers constructs the handler set.
func NewAlertHandlers(alertService services.AlertService, logger *zap.Logger) *AlertHandlers {
	return &AlertHandlers{alertService: alertService, logger: logger}
}

// HandleListAlertRules returns every configured rule.
func (h *AlertHandlers) HandleListAlertRules(c *gin.Context) {
	rules, err := h.alertService.ListAlertRules(c.Request.Context())
	if err != nil {
		h.logger.Error("failed to list alert rules", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list alert rules"})
		return
	}
	if rules == nil {
		// Make the JSON shape deterministic: prefer an empty array over null.
		rules = []*services.AlertRule{}
	}
	c.JSON(http.StatusOK, gin.H{"rules": rules})
}

// HandleGetAlertRule returns a single rule by ID.
func (h *AlertHandlers) HandleGetAlertRule(c *gin.Context) {
	id := c.Param("id")
	rule, err := h.alertService.GetAlertRule(c.Request.Context(), id)
	if err != nil {
		h.logger.Error("failed to get alert rule", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get alert rule"})
		return
	}
	if rule == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "alert rule not found"})
		return
	}
	c.JSON(http.StatusOK, rule)
}

// HandleCreateAlertRule creates a new rule.
func (h *AlertHandlers) HandleCreateAlertRule(c *gin.Context) {
	var input services.AlertRuleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "detail": err.Error()})
		return
	}
	rule, err := h.alertService.CreateAlertRule(c.Request.Context(), input)
	if err != nil {
		// Validation errors come back as plain errors from the service. Surface
		// them as 400 so the UI can render them inline. Other errors are 500.
		if isValidationError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("failed to create alert rule", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create alert rule"})
		return
	}
	c.JSON(http.StatusCreated, rule)
}

// HandleUpdateAlertRule updates an existing rule.
func (h *AlertHandlers) HandleUpdateAlertRule(c *gin.Context) {
	id := c.Param("id")
	var input services.AlertRuleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "detail": err.Error()})
		return
	}
	rule, err := h.alertService.UpdateAlertRule(c.Request.Context(), id, input)
	if err != nil {
		if isNotFoundError(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "alert rule not found"})
			return
		}
		if isValidationError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("failed to update alert rule", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update alert rule"})
		return
	}
	c.JSON(http.StatusOK, rule)
}

// HandleDeleteAlertRule removes a rule.
func (h *AlertHandlers) HandleDeleteAlertRule(c *gin.Context) {
	id := c.Param("id")
	if err := h.alertService.DeleteAlertRule(c.Request.Context(), id); err != nil {
		if isNotFoundError(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "alert rule not found"})
			return
		}
		h.logger.Error("failed to delete alert rule", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete alert rule"})
		return
	}
	c.Status(http.StatusNoContent)
}

// isNotFoundError reports whether the error string suggests a missing rule.
// AlertService surfaces these as plain errors today; if we add typed errors
// later, swap this for errors.Is.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such")
}

// isValidationError reports whether the error is a user-input problem.
func isValidationError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is required") ||
		strings.Contains(msg, "invalid") ||
		strings.Contains(msg, "must be")
}
