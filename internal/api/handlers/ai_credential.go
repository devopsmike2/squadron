// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/aicredstore"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// AICredentialHandlers backs the admin-only AI-provider-key management
// routes (PUT/DELETE /api/v1/ai/credential). It seals the operator's
// plaintext key with the credstore.Key (SQUADRON_SECRETS_KEY), persists
// the ciphertext to the app-global aicredstore, and applies the key to
// the live ai.Service so AI assist works without a restart.
//
// The plaintext key is write-only: it enters here on PUT, is sealed and
// stored, and is NEVER returned by any route. The status surface exposes
// only the non-secret key_source + key_last4 hints.
type AICredentialHandlers struct {
	svc    *ai.Service
	store  aicredstore.Store
	key    *credstore.Key
	logger *zap.Logger
}

// NewAICredentialHandlers constructs the handler. key may be nil when
// SQUADRON_SECRETS_KEY is absent — the PUT path degrades gracefully
// (sealing unavailable) rather than storing plaintext.
func NewAICredentialHandlers(svc *ai.Service, store aicredstore.Store, key *credstore.Key, logger *zap.Logger) *AICredentialHandlers {
	return &AICredentialHandlers{svc: svc, store: store, key: key, logger: logger}
}

// setAICredentialRequest is the PUT body. provider is optional.
type setAICredentialRequest struct {
	APIKey   string `json:"api_key"`
	Provider string `json:"provider"`
}

// HandleSetCredential — PUT /api/v1/ai/credential
//
// Seals the supplied key, persists it, then rotates the live service's
// key. Requires ScopeAIWrite (enforced by the route middleware).
func (h *AICredentialHandlers) HandleSetCredential(c *gin.Context) {
	var req setAICredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key is required"})
		return
	}
	// Sealing requires the credstore key. When SQUADRON_SECRETS_KEY is
	// absent the feature degrades like every other sealed surface —
	// refuse rather than persist plaintext.
	if h.key == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "cannot store the AI key: sealing is unavailable (set SQUADRON_SECRETS_KEY and restart)",
		})
		return
	}
	sealed, nonce, err := h.key.Seal([]byte(key))
	if err != nil {
		// The error never carries the plaintext (credstore invariant).
		h.logger.Warn("ai credential: seal failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to seal the AI key"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if err := h.store.Set(c.Request.Context(), sealed, nonce, provider); err != nil {
		h.logger.Warn("ai credential: persist failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store the AI key"})
		return
	}
	// Apply to the live service so AI works immediately. LIMITATION: the
	// provider backend is recorded in the store but not switched live —
	// switching backends re-derives base URL + model defaults, which
	// happens cleanly at startup (main.go reads the stored provider), so a
	// provider change takes effect on the next restart.
	h.svc.SetAPIKey(key)
	h.logger.Info("ai credential set", zap.String("provider", provider), zap.String("key_source", ai.KeySourceStored))
	c.JSON(http.StatusOK, h.svc.Capabilities())
}

// HandleClearCredential — DELETE /api/v1/ai/credential
//
// Removes the stored credential and clears the live key. Requires
// ScopeAIWrite. Idempotent — clearing when nothing is stored succeeds.
func (h *AICredentialHandlers) HandleClearCredential(c *gin.Context) {
	if err := h.store.Clear(c.Request.Context()); err != nil {
		h.logger.Warn("ai credential: clear failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to clear the AI key"})
		return
	}
	h.svc.SetAPIKey("")
	h.logger.Info("ai credential cleared")
	c.JSON(http.StatusOK, h.svc.Capabilities())
}
