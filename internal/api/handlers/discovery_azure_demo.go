package handlers

import (
	"net/http"

	"github.com/devopsmike2/squadron/internal/discovery/azureconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// findAzureDemoConn returns the stored demo Azure connection (identified by the
// sentinel SubscriptionID), or nil if none exists.
func (h *DiscoveryAzureHandlers) findAzureDemoConn(c *gin.Context) (*azureconnstore.AzureConnection, error) {
	conns, err := h.store.List(c.Request.Context())
	if err != nil {
		return nil, err
	}
	for _, conn := range conns {
		if conn != nil && demo.IsAzureDemoSubscription(conn.SubscriptionID) {
			return conn, nil
		}
	}
	return nil, nil
}

// HandleAzureDemoEnable provisions the built-in credential-free demo Azure
// connection. Idempotent: returns the existing demo subscription rather than
// duplicating it.
func (h *DiscoveryAzureHandlers) HandleAzureDemoEnable(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreNotWired",
			Message: "Squadron's Azure connection substrate isn't configured.",
		}})
		return
	}

	if existing, err := h.findAzureDemoConn(c); err == nil && existing != nil {
		c.JSON(http.StatusOK, existing)
		return
	}

	conn := &azureconnstore.AzureConnection{
		DisplayName:    demo.AzureDisplayName,
		TenantID:       "demo-tenant",
		SubscriptionID: demo.AzureSubscriptionID,
		ClientID:       "demo-client",
		Location:       demo.AzureLocation,
		// Placeholder sealed bytes: the demo scan path never decrypts them
		// (the scan handler short-circuits on the sentinel SubscriptionID).
		SealedSecret: []byte("demo"),
	}
	if err := h.store.Create(c.Request.Context(), conn); err != nil {
		if h.logger != nil {
			h.logger.Error("azure demo enable: store create failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreWriteFailed",
			Message: "Squadron could not provision the demo subscription. The error has been logged; retry in a moment.",
		}})
		return
	}
	c.JSON(http.StatusOK, conn)
}

// HandleAzureDemoDisable removes the demo Azure connection(s). Idempotent.
func (h *DiscoveryAzureHandlers) HandleAzureDemoDisable(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreNotWired",
			Message: "Squadron's Azure connection substrate isn't configured.",
		}})
		return
	}
	conns, err := h.store.List(c.Request.Context())
	if err != nil {
		if h.logger != nil {
			h.logger.Error("azure demo disable: store list failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreReadFailed",
			Message: "Squadron could not read the connection list. The error has been logged; retry in a moment.",
		}})
		return
	}
	for _, conn := range conns {
		if conn != nil && demo.IsAzureDemoSubscription(conn.SubscriptionID) {
			_ = h.store.Delete(c.Request.Context(), conn.ID)
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "disabled"})
}
