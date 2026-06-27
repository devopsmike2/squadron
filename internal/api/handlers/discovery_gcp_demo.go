package handlers

import (
	"net/http"

	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/gcpconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// findGCPDemoConn returns the stored demo GCP connection (identified by the
// sentinel ProjectID), or nil if none exists. GCP connection IDs are generated
// at create time, so the demo is keyed on ProjectID rather than a fixed ID.
func (h *DiscoveryGCPHandlers) findGCPDemoConn(c *gin.Context) (*gcpconnstore.GCPConnection, error) {
	conns, err := h.store.List(c.Request.Context())
	if err != nil {
		return nil, err
	}
	for _, conn := range conns {
		if conn != nil && demo.IsGCPDemoProject(conn.ProjectID) {
			return conn, nil
		}
	}
	return nil, nil
}

// HandleGCPDemoEnable provisions the built-in credential-free demo GCP
// connection. Idempotent: if the demo project already exists it is returned
// rather than duplicated (GCP Create generates a fresh UUID each call).
func (h *DiscoveryGCPHandlers) HandleGCPDemoEnable(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreNotWired",
			Message: "Squadron's GCP connection substrate isn't configured.",
		}})
		return
	}

	if existing, err := h.findGCPDemoConn(c); err == nil && existing != nil {
		c.JSON(http.StatusOK, existing)
		return
	}

	conn := &gcpconnstore.GCPConnection{
		DisplayName: demo.GCPDisplayName,
		ProjectID:   demo.GCPProjectID,
		Region:      demo.GCPRegion,
		// Placeholder sealed bytes: the demo scan path never decrypts them
		// (the scan handler short-circuits on the sentinel ProjectID before
		// any SA decrypt). The store only requires non-empty bytes.
		SealedSA: []byte("demo"),
	}
	if err := h.store.Create(c.Request.Context(), conn); err != nil {
		if h.logger != nil {
			h.logger.Error("gcp demo enable: store create failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreWriteFailed",
			Message: "Squadron could not provision the demo project. The error has been logged; retry in a moment.",
		}})
		return
	}
	c.JSON(http.StatusOK, conn)
}

// HandleGCPDemoDisable removes the demo GCP connection(s). Idempotent.
func (h *DiscoveryGCPHandlers) HandleGCPDemoDisable(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreNotWired",
			Message: "Squadron's GCP connection substrate isn't configured.",
		}})
		return
	}
	conns, err := h.store.List(c.Request.Context())
	if err != nil {
		if h.logger != nil {
			h.logger.Error("gcp demo disable: store list failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreReadFailed",
			Message: "Squadron could not read the connection list. The error has been logged; retry in a moment.",
		}})
		return
	}
	for _, conn := range conns {
		if conn != nil && demo.IsGCPDemoProject(conn.ProjectID) {
			_ = h.store.Delete(c.Request.Context(), conn.ID)
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "disabled"})
}
