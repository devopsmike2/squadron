// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// AutomationHandlers handles /api/v1/automations endpoints (ADR 0038).
type AutomationHandlers struct {
	automationService services.AutomationService
	logger            *zap.Logger
}

// NewAutomationHandlers constructs the handler set.
func NewAutomationHandlers(automationService services.AutomationService, logger *zap.Logger) *AutomationHandlers {
	return &AutomationHandlers{automationService: automationService, logger: logger}
}

// HandleListAutomations returns every configured automation.
func (h *AutomationHandlers) HandleListAutomations(c *gin.Context) {
	autos, err := h.automationService.ListAutomations(c.Request.Context())
	if err != nil {
		h.logger.Error("failed to list automations", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list automations"})
		return
	}
	if autos == nil {
		autos = []*services.Automation{}
	}
	c.JSON(http.StatusOK, gin.H{"automations": autos})
}

// HandleGetAutomation returns a single automation by ID.
func (h *AutomationHandlers) HandleGetAutomation(c *gin.Context) {
	id := c.Param("id")
	a, err := h.automationService.GetAutomation(c.Request.Context(), id)
	if err != nil {
		h.logger.Error("failed to get automation", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get automation"})
		return
	}
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "automation not found"})
		return
	}
	c.JSON(http.StatusOK, a)
}

// HandleCreateAutomation creates a new automation.
func (h *AutomationHandlers) HandleCreateAutomation(c *gin.Context) {
	var input services.AutomationInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "detail": err.Error()})
		return
	}
	a, err := h.automationService.CreateAutomation(c.Request.Context(), input)
	if err != nil {
		if isValidationError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("failed to create automation", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create automation"})
		return
	}
	c.JSON(http.StatusCreated, a)
}

// HandleUpdateAutomation updates an existing automation.
func (h *AutomationHandlers) HandleUpdateAutomation(c *gin.Context) {
	id := c.Param("id")
	var input services.AutomationInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "detail": err.Error()})
		return
	}
	a, err := h.automationService.UpdateAutomation(c.Request.Context(), id, input)
	if err != nil {
		if isNotFoundError(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "automation not found"})
			return
		}
		if isValidationError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("failed to update automation", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update automation"})
		return
	}
	c.JSON(http.StatusOK, a)
}

// enabledBody is the payload for the enable/disable toggle.
type enabledBody struct {
	Enabled bool `json:"enabled"`
}

// HandleSetAutomationEnabled flips the enabled flag (POST /:id/enable). The
// instant enable/disable guardrail: an operator can disarm a rule without
// editing it.
func (h *AutomationHandlers) HandleSetAutomationEnabled(c *gin.Context) {
	id := c.Param("id")
	var body enabledBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "detail": err.Error()})
		return
	}
	a, err := h.automationService.SetAutomationEnabled(c.Request.Context(), id, body.Enabled)
	if err != nil {
		if isNotFoundError(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "automation not found"})
			return
		}
		h.logger.Error("failed to set automation enabled", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set automation enabled"})
		return
	}
	c.JSON(http.StatusOK, a)
}

// HandleDeleteAutomation removes an automation.
func (h *AutomationHandlers) HandleDeleteAutomation(c *gin.Context) {
	id := c.Param("id")
	if err := h.automationService.DeleteAutomation(c.Request.Context(), id); err != nil {
		if isNotFoundError(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "automation not found"})
			return
		}
		h.logger.Error("failed to delete automation", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete automation"})
		return
	}
	c.Status(http.StatusNoContent)
}
