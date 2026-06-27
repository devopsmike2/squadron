package handlers

import (
	"net/http"

	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// HandleDemoEnable provisions the built-in demo connection (v0.89.239,
// first-user onboarding arc). It persists the reserved demo CloudConnection so
// it appears in the AWS connections list; the operator then runs a scan against
// it like any other connection and gets the canned sample inventory (runAWSScan
// short-circuits on the demo sentinel). No cloud credentials are involved.
//
// Idempotent: StoreConnection upserts, so repeated enables are harmless.
func (h *DiscoveryHandlers) HandleDemoEnable(c *gin.Context) {
	if h.credStore == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredStoreNotWired",
			Message:       "Squadron's credential substrate isn't configured.",
			SuggestedStep: "save",
		}})
		return
	}

	conn := demo.Connection()
	if err := h.credStore.StoreConnection(c.Request.Context(), conn); err != nil {
		if h.logger != nil {
			h.logger.Error("demo enable: credstore write failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredStoreWriteFailed",
			Message:       "Squadron could not provision the demo connection. The error has been logged; retry in a moment.",
			SuggestedStep: "save",
		}})
		return
	}

	// Re-read so CreatedAt reflects what the store stamped. Fall back to the
	// in-memory record if the read hiccups — the connection is already saved.
	stored, err := h.credStore.GetConnection(c.Request.Context(), demo.SentinelAccountID)
	if err != nil || stored == nil {
		stored = &conn
	}

	c.JSON(http.StatusOK, awsConnectionRow{
		ConnectionID: stored.AccountID,
		AccountID:    stored.AccountID,
		DisplayName:  stored.DisplayName,
		Regions:      stored.Regions,
		CreatedAt:    stored.CreatedAt,
	})
}

// HandleDemoDisable removes the built-in demo connection. Idempotent:
// DeleteConnection treats a missing row as success.
func (h *DiscoveryHandlers) HandleDemoDisable(c *gin.Context) {
	if h.credStore == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredStoreNotWired",
			Message:       "Squadron's credential substrate isn't configured.",
			SuggestedStep: "save",
		}})
		return
	}

	if err := h.credStore.DeleteConnection(c.Request.Context(), demo.SentinelAccountID); err != nil {
		if h.logger != nil {
			h.logger.Error("demo disable: credstore delete failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "CredStoreDeleteFailed",
			Message: "Squadron could not remove the demo connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "disabled"})
}
