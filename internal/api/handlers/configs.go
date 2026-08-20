package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/devopsmike2/squadron/internal/api/middleware"
	"github.com/devopsmike2/squadron/internal/confignorm"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// ConfigHandlers handles config-related API endpoints
type ConfigHandlers struct {
	agentService services.AgentService
	commander    AgentCommander
	// audit is optional and wired post-construction via
	// SetAuditService. It lets the lint endpoint persist a
	// config.lint_evaluated event when severity-error findings show
	// up — that's the evidence that "we evaluated and either passed
	// or knowingly proceeded" required by NIST CSF PR.PS-06 and
	// SOC 2 CC8.1.
	audit  services.AuditService
	logger *zap.Logger
}

// NewConfigHandlers creates a new config handlers instance
func NewConfigHandlers(agentService services.AgentService, commander AgentCommander, logger *zap.Logger) *ConfigHandlers {
	return &ConfigHandlers{
		agentService: agentService,
		commander:    commander,
		logger:       logger,
	}
}

// SetAuditService wires audit fan-out post-construction. Called from
// main.go after the audit service is built, because the config
// handler is constructed earlier in the dependency graph.
func (h *ConfigHandlers) SetAuditService(a services.AuditService) {
	h.audit = a
}

// CreateConfigRequest represents the request for creating a config
type CreateConfigRequest struct {
	Name       string     `json:"name,omitempty"`
	AgentID    *uuid.UUID `json:"agent_id,omitempty"`
	GroupID    *string    `json:"group_id,omitempty"`
	ConfigHash string     `json:"config_hash,omitempty"`
	Content    string     `json:"content" binding:"required"`
	Version    int        `json:"version" binding:"required"`
}

// UpdateConfigRequest represents the request for updating a config
type UpdateConfigRequest struct {
	Name    string `json:"name,omitempty"`
	Content string `json:"content" binding:"required"`
	Version int    `json:"version" binding:"required"`
}

// handleGetConfigs handles GET /api/v1/configs
func (h *ConfigHandlers) HandleGetConfigs(c *gin.Context) {
	// Parse query parameters
	agentIDStr := c.Query("agent_id")
	groupIDStr := c.Query("group_id")
	limitStr := c.DefaultQuery("limit", "100")
	// Soft-archived configs are hidden by default so residue/superseded configs
	// don't clutter the list; ?include_archived=true surfaces them (e.g. to
	// unarchive one). Accepts the usual truthy spellings.
	includeArchived := isTruthy(c.Query("include_archived"))

	// Parse limit
	limit := 100
	if limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil {
			limit = parsedLimit
		}
	}
	if limit > 1000 {
		limit = 1000
	}

	// Parse UUIDs
	var agentUUID *uuid.UUID
	var groupID *string
	var err error

	if agentIDStr != "" {
		parsed, err := uuid.Parse(agentIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid agent ID format"})
			return
		}
		agentUUID = &parsed
	}

	if groupIDStr != "" {
		groupID = &groupIDStr
	}

	// Build filter
	filter := services.ConfigFilter{
		AgentID:         agentUUID,
		GroupID:         groupID,
		Limit:           limit,
		IncludeArchived: includeArchived,
	}

	// Get configs from service
	configs, err := h.agentService.ListConfigs(c.Request.Context(), filter)
	if err != nil {
		h.logger.Error("Failed to get configs", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch configs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"configs": configs,
		"count":   len(configs),
	})
}

// handleCreateConfig handles POST /api/v1/configs
func (h *ConfigHandlers) HandleCreateConfig(c *gin.Context) {
	var req CreateConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data", "details": err.Error()})
		return
	}

	// Generate UUID for the config
	configID := uuid.New().String()

	contentHash := hashConfig(req.Content)

	// Create config
	config := &services.Config{
		ID:         configID,
		Name:       req.Name,
		AgentID:    req.AgentID,
		GroupID:    req.GroupID,
		ConfigHash: contentHash,
		Content:    req.Content,
		Version:    req.Version,
		CreatedAt:  time.Now(),
	}

	// Save config to service
	err := h.agentService.CreateConfig(c.Request.Context(), config)
	if err != nil {
		h.logger.Error("Failed to create config", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create config"})
		return
	}

	// If this is a group config, send it to all agents in the group
	if config.GroupID != nil && *config.GroupID != "" {
		updatedAgents, errors := h.commander.SendConfigToAgentsInGroup(*config.GroupID, config.Content)

		// Log the results
		if len(errors) > 0 {
			h.logger.Warn("Some agents failed to receive group config",
				zap.String("group_id", *config.GroupID),
				zap.Int("updated", len(updatedAgents)),
				zap.Int("failed", len(errors)))
		} else {
			h.logger.Info("Group config sent to all agents",
				zap.String("group_id", *config.GroupID),
				zap.Int("updated", len(updatedAgents)))
		}
	}

	c.JSON(http.StatusCreated, config)
}

// handleGetConfig handles GET /api/v1/configs/:id
func (h *ConfigHandlers) HandleGetConfig(c *gin.Context) {
	configID := c.Param("id")
	if configID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Config ID is required"})
		return
	}

	// Get config from service
	config, err := h.agentService.GetConfig(c.Request.Context(), configID)
	if err != nil {
		h.logger.Error("Failed to get config", zap.String("config_id", configID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch config"})
		return
	}

	if config == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Config not found"})
		return
	}

	c.JSON(http.StatusOK, config)
}

// handleUpdateConfig handles PUT /api/v1/configs/:id
func (h *ConfigHandlers) HandleUpdateConfig(c *gin.Context) {
	configID := c.Param("id")
	if configID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Config ID is required"})
		return
	}

	var req UpdateConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data", "details": err.Error()})
		return
	}

	// Get existing config
	existingConfig, err := h.agentService.GetConfig(c.Request.Context(), configID)
	if err != nil {
		h.logger.Error("Failed to get config", zap.String("config_id", configID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch config"})
		return
	}

	if existingConfig == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Config not found"})
		return
	}

	// Validate YAML content
	if err := validateYAMLConfig(req.Content); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid YAML configuration", "details": err.Error()})
		return
	}

	// Create new version of config
	newConfigID := uuid.New().String()
	configHash := hashConfig(req.Content)

	// Use the new name if provided, otherwise keep the existing name
	name := req.Name
	if name == "" {
		name = existingConfig.Name
	}

	newConfig := &services.Config{
		ID:         newConfigID,
		Name:       name,
		AgentID:    existingConfig.AgentID,
		GroupID:    existingConfig.GroupID,
		ConfigHash: configHash,
		Content:    req.Content,
		Version:    req.Version,
		CreatedAt:  time.Now(),
	}

	// Save new config version
	err = h.agentService.CreateConfig(c.Request.Context(), newConfig)
	if err != nil {
		h.logger.Error("Failed to create config version", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create config version"})
		return
	}

	c.JSON(http.StatusOK, newConfig)
}

// handleDeleteConfig handles DELETE /api/v1/configs/:id
//
// Hard delete stays intentionally unimplemented: configs are versioned and
// immutable (they back the tamper-evident change/audit trail), so destroying a
// row would break that guarantee. The supported way to get a superseded or
// residue config out of the operator's way is POST /api/v1/configs/:id/archive
// — a reversible SOFT archive that hides the config from the default listing
// without deleting it. The 501 body points operators there.
func (h *ConfigHandlers) HandleDeleteConfig(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": "Config deletion not implemented - configs are versioned and immutable; use POST /api/v1/configs/:id/archive to hide a config instead",
	})
}

// HandleArchiveConfig handles POST /api/v1/configs/:id/archive. It soft-archives
// a config so the default listing hides it, WITHOUT deleting the row (configs
// stay immutable for audit). Archiving is REFUSED with 409 when the config is the
// current effective config for a live agent or group — the operator must clear
// or replace that assignment first, so we never hide a config an agent is
// actually running. Idempotent: archiving an already-archived config is a 200.
func (h *ConfigHandlers) HandleArchiveConfig(c *gin.Context) {
	configID := c.Param("id")
	if configID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Config ID is required"})
		return
	}
	ctx := c.Request.Context()

	config, err := h.agentService.GetConfig(ctx, configID)
	if err != nil {
		h.logger.Error("Failed to get config", zap.String("config_id", configID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch config"})
		return
	}
	if config == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Config not found"})
		return
	}
	if config.ArchivedAt != nil {
		// Already archived — nothing to do. Report success so the operation is
		// idempotent (re-running cleanup must not 409/500).
		c.JSON(http.StatusOK, gin.H{"status": "archived", "id": configID, "archived_at": config.ArchivedAt})
		return
	}

	// Assigned-guard: refuse to hide a config that is the CURRENT effective
	// config for a live agent or group. A superseded version (an older row for
	// the same agent/group) or a config whose agent/group no longer exists is
	// safe to archive.
	if reason, assigned, gErr := h.configCurrentlyAssigned(ctx, config); gErr != nil {
		h.logger.Error("Failed to evaluate config assignment", zap.String("config_id", configID), zap.Error(gErr))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to evaluate config assignment"})
		return
	} else if assigned {
		c.JSON(http.StatusConflict, gin.H{"error": reason})
		return
	}

	if err := h.agentService.SetConfigArchived(ctx, configID, true); err != nil {
		if errors.Is(err, applicationstore.ErrConfigNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Config not found"})
			return
		}
		h.logger.Error("Failed to archive config", zap.String("config_id", configID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to archive config"})
		return
	}

	h.recordArchiveAudit(c, config, true)
	c.JSON(http.StatusOK, gin.H{"status": "archived", "id": configID})
}

// HandleUnarchiveConfig handles POST /api/v1/configs/:id/unarchive. It clears the
// archive tombstone so the config reappears in the default listing. Restoring
// visibility is always safe, so there is no assignment guard. Idempotent:
// unarchiving an active config is a 200.
func (h *ConfigHandlers) HandleUnarchiveConfig(c *gin.Context) {
	configID := c.Param("id")
	if configID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Config ID is required"})
		return
	}
	ctx := c.Request.Context()

	config, err := h.agentService.GetConfig(ctx, configID)
	if err != nil {
		h.logger.Error("Failed to get config", zap.String("config_id", configID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch config"})
		return
	}
	if config == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Config not found"})
		return
	}
	if config.ArchivedAt == nil {
		c.JSON(http.StatusOK, gin.H{"status": "active", "id": configID})
		return
	}

	if err := h.agentService.SetConfigArchived(ctx, configID, false); err != nil {
		if errors.Is(err, applicationstore.ErrConfigNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Config not found"})
			return
		}
		h.logger.Error("Failed to unarchive config", zap.String("config_id", configID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to unarchive config"})
		return
	}

	h.recordArchiveAudit(c, config, false)
	c.JSON(http.StatusOK, gin.H{"status": "active", "id": configID})
}

// configCurrentlyAssigned reports whether config is the CURRENT effective config
// for a live agent or group — i.e. archiving it would hide a config something is
// actually running. It returns (reason, true, nil) when assigned, ("", false,
// nil) when safe, and a non-nil error only on a store failure. A config is
// considered assigned when it is BOTH the latest version for its agent/group AND
// that agent/group still exists (a config pinned to a decommissioned agent or a
// deleted group is orphaned residue and safe to archive).
func (h *ConfigHandlers) configCurrentlyAssigned(ctx context.Context, config *services.Config) (string, bool, error) {
	if config.AgentID != nil {
		agent, err := h.agentService.GetAgent(ctx, *config.AgentID)
		if err != nil {
			return "", false, err
		}
		if agent != nil {
			latest, err := h.agentService.GetLatestConfigForAgent(ctx, *config.AgentID)
			if err != nil {
				return "", false, err
			}
			if latest != nil && latest.ID == config.ID {
				return fmt.Sprintf("config is currently assigned to agent %s; clear or replace the agent's config before archiving", config.AgentID.String()), true, nil
			}
		}
	}
	if config.GroupID != nil && *config.GroupID != "" {
		group, err := h.agentService.GetGroup(ctx, *config.GroupID)
		if err != nil {
			return "", false, err
		}
		if group != nil {
			latest, err := h.agentService.GetLatestConfigForGroup(ctx, *config.GroupID)
			if err != nil {
				return "", false, err
			}
			if latest != nil && latest.ID == config.ID {
				return fmt.Sprintf("config is currently assigned to group %s; reassign the group before archiving", *config.GroupID), true, nil
			}
		}
	}
	return "", false, nil
}

// recordArchiveAudit emits a config.archived / config.unarchived audit event when
// an audit service is wired. Archiving a config is a change-management action
// (it alters what operators see as the live config inventory), so it belongs on
// the same audit timeline as config.lint_evaluated and the agent mutations.
func (h *ConfigHandlers) recordArchiveAudit(c *gin.Context, config *services.Config, archived bool) {
	if h.audit == nil {
		return
	}
	actor := middleware.ActorFromGin(c).String()
	if actor == "" {
		actor = services.AuditActorSystem
	}
	eventType, action := "config.unarchived", "unarchived"
	if archived {
		eventType, action = "config.archived", "archived"
	}
	_ = h.audit.Record(c.Request.Context(), services.AuditEntry{
		Actor:      actor,
		EventType:  eventType,
		TargetType: services.AuditTargetConfig,
		TargetID:   config.ID,
		Action:     action,
		Payload: map[string]any{
			"name":     config.Name,
			"version":  config.Version,
			"agent_id": derefUUID(config.AgentID),
			"group_id": derefOrEmpty(config.GroupID),
		},
	})
}

// isTruthy interprets the common truthy query-string spellings.
func isTruthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True", "yes", "on":
		return true
	default:
		return false
	}
}

func derefUUID(u *uuid.UUID) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// handleValidateConfig handles POST /api/v1/configs/validate
func (h *ConfigHandlers) HandleValidateConfig(c *gin.Context) {
	var req struct {
		Content string `json:"content" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data", "details": err.Error()})
		return
	}

	// Validate YAML syntax
	if err := validateYAMLConfig(req.Content); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"valid":  false,
			"errors": []string{err.Error()},
		})
		return
	}

	// Additional validation (check required fields, etc.)
	warnings, err := validateOTelConfig(req.Content)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"valid":  false,
			"errors": []string{err.Error()},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"valid":    true,
		"warnings": warnings,
	})
}

// handleGetConfigVersions handles GET /api/v1/configs/:id/versions
func (h *ConfigHandlers) HandleGetConfigVersions(c *gin.Context) {
	// Get query parameters
	agentIDStr := c.Query("agent_id")
	groupIDStr := c.Query("group_id")

	if agentIDStr == "" && groupIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Either agent_id or group_id is required"})
		return
	}

	// Build filter
	filter := services.ConfigFilter{
		Limit: 100,
	}

	if agentIDStr != "" {
		agentUUID, err := uuid.Parse(agentIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid agent ID format"})
			return
		}
		filter.AgentID = &agentUUID
	}

	if groupIDStr != "" {
		filter.GroupID = &groupIDStr
	}

	// Get config versions
	configs, err := h.agentService.ListConfigs(c.Request.Context(), filter)
	if err != nil {
		h.logger.Error("Failed to get config versions", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch config versions"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"versions": configs,
		"count":    len(configs),
	})
}

// Validation helper functions

// validateYAMLConfig validates YAML syntax
func validateYAMLConfig(content string) error {
	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(content), &config); err != nil {
		return fmt.Errorf("invalid YAML syntax: %w", err)
	}
	return nil
}

// validateOTelConfig performs OpenTelemetry-specific validation
func validateOTelConfig(content string) ([]string, error) {
	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(content), &config); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}

	var warnings []string

	// Check for required top-level sections
	requiredSections := []string{"receivers", "processors", "exporters", "service"}
	for _, section := range requiredSections {
		if _, exists := config[section]; !exists {
			warnings = append(warnings, fmt.Sprintf("missing recommended section: %s", section))
		}
	}

	// Check service.pipelines
	if service, ok := config["service"].(map[string]interface{}); ok {
		if pipelines, ok := service["pipelines"].(map[string]interface{}); !ok || len(pipelines) == 0 {
			warnings = append(warnings, "no pipelines defined in service section")
		}
	}

	return warnings, nil
}

// hashConfig creates a content fingerprint of the config. It delegates to the
// canonical confignorm.Hash so the ConfigHash stored at create time matches the
// hash drift detection recomputes from an agent's effective config — same
// normalization, same digest.
func hashConfig(content string) string {
	return confignorm.Hash(content)
}
