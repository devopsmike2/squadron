// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// SiemHandlers serves /api/v1/siem/destinations.
//
// Plaintext secrets only travel inbound: Create and Update accept a
// plaintext_secret in the body; the response is a SiemDestinationView
// that exposes has_secret instead. List / Get never return the
// secret in any form.
//
// Added in v0.50.2.
type SiemHandlers struct {
	siemService services.SiemService
	logger      *zap.Logger
}

func NewSiemHandlers(siemService services.SiemService, logger *zap.Logger) *SiemHandlers {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &SiemHandlers{siemService: siemService, logger: logger}
}

// HandleListSiem serves GET /api/v1/siem/destinations.
func (h *SiemHandlers) HandleListSiem(c *gin.Context) {
	list, err := h.siemService.List(c.Request.Context())
	if err != nil {
		h.logger.Error("failed to list siem destinations", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list siem destinations"})
		return
	}
	if list == nil {
		list = []*services.SiemDestinationView{}
	}
	c.JSON(http.StatusOK, gin.H{"destinations": list})
}

// HandleGetSiem serves GET /api/v1/siem/destinations/:id.
func (h *SiemHandlers) HandleGetSiem(c *gin.Context) {
	id := c.Param("id")
	d, err := h.siemService.Get(c.Request.Context(), id)
	if err != nil {
		h.logger.Error("failed to get siem destination", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get siem destination"})
		return
	}
	if d == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "siem destination not found"})
		return
	}
	c.JSON(http.StatusOK, d)
}

// HandleCreateSiem serves POST /api/v1/siem/destinations.
func (h *SiemHandlers) HandleCreateSiem(c *gin.Context) {
	var input services.SiemDestinationInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "detail": err.Error()})
		return
	}
	d, err := h.siemService.Create(c.Request.Context(), input)
	if err != nil {
		// Validation errors are operator input problems; surface
		// as 400 so the UI can render them inline. The service
		// is the source of truth on what's invalid; we
		// distinguish via "is required" / "must be" substrings
		// the same way the rollouts handler does.
		if isSiemValidationError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("failed to create siem destination", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create siem destination"})
		return
	}
	c.JSON(http.StatusCreated, d)
}

// HandleUpdateSiem serves PUT /api/v1/siem/destinations/:id.
func (h *SiemHandlers) HandleUpdateSiem(c *gin.Context) {
	id := c.Param("id")
	var input services.SiemDestinationUpdate
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "detail": err.Error()})
		return
	}
	d, err := h.siemService.Update(c.Request.Context(), id, input)
	if err != nil {
		if isNotFoundError(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		if isSiemValidationError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("failed to update siem destination", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update siem destination"})
		return
	}
	c.JSON(http.StatusOK, d)
}

// HandleDeleteSiem serves DELETE /api/v1/siem/destinations/:id.
func (h *SiemHandlers) HandleDeleteSiem(c *gin.Context) {
	id := c.Param("id")
	if err := h.siemService.Delete(c.Request.Context(), id); err != nil {
		if isNotFoundError(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("failed to delete siem destination", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete siem destination"})
		return
	}
	c.Status(http.StatusNoContent)
}

// HandleTestSiem serves POST /api/v1/siem/destinations/:id/test.
//
// Sends a synthetic "squadron.test" event to the configured endpoint
// and returns the result. The destination's status row gets updated
// either way so the UI's last-error column reflects the outcome.
func (h *SiemHandlers) HandleTestSiem(c *gin.Context) {
	id := c.Param("id")
	actor := actorFromContext(c)
	if err := h.siemService.Test(c.Request.Context(), id, actor); err != nil {
		if isNotFoundError(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		// Test failures are not server-side bugs — surface as 502
		// so the UI can show "Splunk returned 401" vs Squadron
		// being broken. We don't log at error because operators
		// see these constantly while debugging configs.
		h.logger.Info("siem destination test failed",
			zap.String("id", id), zap.String("error", err.Error()))
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"result": "ok"})
}

// isSiemValidationError matches the error substrings the SiemService
// raises for bad input. Cheap heuristic since the service uses plain
// fmt.Errorf rather than a sentinel hierarchy.
func isSiemValidationError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsAny(msg,
		"is required",
		"must be",
		"cannot be empty",
		"crypter not configured",
	)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if containsSubstr(s, sub) {
			return true
		}
	}
	return false
}

func containsSubstr(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
