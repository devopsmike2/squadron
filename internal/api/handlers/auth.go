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

// AuthHandlers serves /api/v1/auth/tokens.
type AuthHandlers struct {
	authService services.AuthService
	logger      *zap.Logger
}

func NewAuthHandlers(authService services.AuthService, logger *zap.Logger) *AuthHandlers {
	return &AuthHandlers{authService: authService, logger: logger}
}

// CreateTokenRequest is the body of POST /api/v1/auth/tokens. Label is
// required so issued tokens are operator-readable in the list view and
// in audit log actors.
type CreateTokenRequest struct {
	Label string `json:"label" binding:"required"`
}

// CreateTokenResponse is the body of POST /api/v1/auth/tokens. Plaintext
// is included here and ONLY here — subsequent List calls return the
// metadata without the plaintext. The UI shows this once and warns
// operators to copy it now.
type CreateTokenResponse struct {
	Token     *services.APIToken `json:"token"`
	Plaintext string             `json:"plaintext"`
}

// HandleCreateToken serves POST /api/v1/auth/tokens.
func (h *AuthHandlers) HandleCreateToken(c *gin.Context) {
	var req CreateTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "detail": err.Error()})
		return
	}
	token, plaintext, err := h.authService.Issue(c.Request.Context(), req.Label)
	if err != nil {
		if strings.Contains(err.Error(), "label") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("failed to issue api token", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue token"})
		return
	}
	c.JSON(http.StatusCreated, CreateTokenResponse{Token: token, Plaintext: plaintext})
}

// HandleListTokens serves GET /api/v1/auth/tokens.
//
// Returns every issued token — active and revoked — newest first.
// Plaintext is NOT included; operators who lose a token must issue a
// new one rather than re-fetching the original.
func (h *AuthHandlers) HandleListTokens(c *gin.Context) {
	tokens, err := h.authService.List(c.Request.Context())
	if err != nil {
		h.logger.Error("failed to list api tokens", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tokens"})
		return
	}
	if tokens == nil {
		tokens = []*services.APIToken{}
	}
	c.JSON(http.StatusOK, gin.H{"tokens": tokens})
}

// HandleRevokeToken serves POST /api/v1/auth/tokens/:id/revoke.
func (h *AuthHandlers) HandleRevokeToken(c *gin.Context) {
	id := c.Param("id")
	if err := h.authService.Revoke(c.Request.Context(), id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("failed to revoke api token",
			zap.String("token_id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke token"})
		return
	}
	c.JSON(http.StatusNoContent, nil)
}
