package handlers

import (
	"net/http"

	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/ociconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// findOCIDemoConn returns the stored demo OCI connection (identified by the
// sentinel TenancyOCID), or nil if none exists.
func (h *DiscoveryOCIHandlers) findOCIDemoConn(c *gin.Context) (*ociconnstore.OCIConnection, error) {
	conns, err := h.store.List(c.Request.Context())
	if err != nil {
		return nil, err
	}
	for _, conn := range conns {
		if conn != nil && demo.IsOCIDemoTenancy(conn.TenancyOCID) {
			return conn, nil
		}
	}
	return nil, nil
}

// HandleOCIDemoEnable provisions the built-in credential-free demo OCI
// connection. Idempotent: returns the existing demo tenancy rather than
// duplicating it.
func (h *DiscoveryOCIHandlers) HandleOCIDemoEnable(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreNotWired",
			Message: "Squadron's OCI connection substrate isn't configured.",
		}})
		return
	}

	if existing, err := h.findOCIDemoConn(c); err == nil && existing != nil {
		c.JSON(http.StatusOK, existing)
		return
	}

	conn := &ociconnstore.OCIConnection{
		DisplayName: demo.OCIDisplayName,
		TenancyOCID: demo.OCITenancyOCID,
		UserOCID:    "ocid1.user.oc1..demo",
		Fingerprint: "00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00",
		Region:      demo.OCIRegion,
		// Placeholder sealed bytes: the demo scan path never decrypts them
		// (the scan handler short-circuits on the sentinel TenancyOCID).
		SealedPrivateKey: []byte("demo"),
	}
	if err := h.store.Create(c.Request.Context(), conn); err != nil {
		if h.logger != nil {
			h.logger.Error("oci demo enable: store create failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreWriteFailed",
			Message: "Squadron could not provision the demo tenancy. The error has been logged; retry in a moment.",
		}})
		return
	}
	c.JSON(http.StatusOK, conn)
}

// HandleOCIDemoDisable removes the demo OCI connection(s). Idempotent.
func (h *DiscoveryOCIHandlers) HandleOCIDemoDisable(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreNotWired",
			Message: "Squadron's OCI connection substrate isn't configured.",
		}})
		return
	}
	conns, err := h.store.List(c.Request.Context())
	if err != nil {
		if h.logger != nil {
			h.logger.Error("oci demo disable: store list failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreReadFailed",
			Message: "Squadron could not read the connection list. The error has been logged; retry in a moment.",
		}})
		return
	}
	for _, conn := range conns {
		if conn != nil && demo.IsOCIDemoTenancy(conn.TenancyOCID) {
			_ = h.store.Delete(c.Request.Context(), conn.ID)
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "disabled"})
}
