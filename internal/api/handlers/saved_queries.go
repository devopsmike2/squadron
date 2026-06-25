package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// SavedQueryHandlers manages CRUD operations for saved Squadron QL queries.
type SavedQueryHandlers struct {
	service services.SavedQueryService
	logger  *zap.Logger
}

// NewSavedQueryHandlers wires HTTP handlers around the SavedQueryService.
func NewSavedQueryHandlers(service services.SavedQueryService, logger *zap.Logger) *SavedQueryHandlers {
	return &SavedQueryHandlers{service: service, logger: logger}
}

// listSavedQueriesResponse wraps the saved queries payload.
type listSavedQueriesResponse struct {
	SavedQueries []*services.SavedQuery `json:"saved_queries"`
}

// HandleListSavedQueries returns all saved queries.
func (h *SavedQueryHandlers) HandleListSavedQueries(c *gin.Context) {
	queries, err := h.service.ListSavedQueries(c.Request.Context())
	if err != nil {
		h.logger.Error("failed to list saved queries", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list saved queries"})
		return
	}

	c.JSON(http.StatusOK, listSavedQueriesResponse{SavedQueries: queries})
}

// HandleCreateSavedQuery persists a new saved query definition.
func (h *SavedQueryHandlers) HandleCreateSavedQuery(c *gin.Context) {
	var input services.SavedQueryInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	query, err := h.service.CreateSavedQuery(c.Request.Context(), input)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, query)
}

// HandleUpdateSavedQuery updates an existing saved query by ID.
func (h *SavedQueryHandlers) HandleUpdateSavedQuery(c *gin.Context) {
	id := c.Param("id")
	var input services.SavedQueryInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	query, err := h.service.UpdateSavedQuery(c.Request.Context(), id, input)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, query)
}

// HandleDeleteSavedQuery removes a saved query by ID.
func (h *SavedQueryHandlers) HandleDeleteSavedQuery(c *gin.Context) {
	id := c.Param("id")
	if err := h.service.DeleteSavedQuery(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
