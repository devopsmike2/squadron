// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"strings"
	"time"

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
// in audit log actors. Scopes is required — operators must opt into
// the permissions they're granting. Pass ["*"] for full access (still
// recorded explicitly so the audit log shows it).
//
// ExpiresAt is optional. When non-nil and in the future, Squadron
// rejects the token after that time. Nil = never expires (the default,
// matching pre-v0.11 behavior). Operators should set expiries on
// human-issued tokens; long-lived automation tokens are OK without
// them but should be rotated by hand.
type CreateTokenRequest struct {
	Label     string     `json:"label" binding:"required"`
	Scopes    []string   `json:"scopes" binding:"required"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
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
	// ADR 0013 D1 — the public token-create API must not mint a token whose
	// label is reserved for break-glass admin. The enterprise RBAC edition
	// keys its bootstrap-admin grant on the token LABEL, so an `auth:write`
	// holder minting a `bootstrap`-labeled token here would self-escalate to
	// cross-tenant admin. The reserved set is EMPTY in OSS (inert); the
	// enterprise wire populates it with its bootstrap labels. The internal
	// first-start bootstrapAuthToken calls Issue directly (not this handler),
	// so it can still mint the label. Match is case-insensitive on the
	// trimmed value so `Bootstrap` can't bypass `bootstrap`.
	if services.IsReservedTokenLabel(req.Label) {
		c.JSON(http.StatusForbidden, gin.H{"error": "label is reserved"})
		return
	}
	token, plaintext, err := h.authService.Issue(c.Request.Context(), req.Label, req.Scopes, req.ExpiresAt)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "label") || strings.Contains(msg, "scopes") ||
			strings.Contains(msg, "unknown scope") || strings.Contains(msg, "expires_at") {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
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
