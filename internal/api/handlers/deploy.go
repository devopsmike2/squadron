// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/deploy"
	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// DeployHandlers serves v0.34 GitHub Actions deploy endpoints.
type DeployHandlers struct {
	service *deploy.Service
	logger  *zap.Logger
}

func NewDeployHandlers(svc *deploy.Service, logger *zap.Logger) *DeployHandlers {
	return &DeployHandlers{service: svc, logger: logger}
}

// targetBody is the JSON shape for create/update. PAT comes through
// the same body but is dropped server-side after encryption —
// never echoed back.
type targetBody struct {
	Name           string            `json:"name" binding:"required"`
	GitHubOwner    string            `json:"github_owner"`
	GitHubRepo     string            `json:"github_repo"`
	GitHubWorkflow string            `json:"github_workflow"`
	GitHubBranch   string            `json:"github_branch"`
	DefaultInputs  map[string]string `json:"default_inputs,omitempty"`
	ConfigID       string            `json:"config_id,omitempty"`
	InventoryPath  string            `json:"inventory_path,omitempty"`
	PAT            string            `json:"pat,omitempty"`
}

func (h *DeployHandlers) guard(c *gin.Context) bool {
	if h == nil || h.service == nil || !h.service.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "Deploy feature is not configured — set SQUADRON_DEPLOY_KEY (base64-encoded 32-byte key) and restart.",
			"enabled": false,
		})
		return false
	}
	return true
}

// HandleListTargets is GET /api/v1/deploy/targets.
func (h *DeployHandlers) HandleListTargets(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	targets, err := h.service.ListTargets(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": targets, "count": len(targets), "enabled": true})
}

// HandleGetTarget is GET /api/v1/deploy/targets/:id.
func (h *DeployHandlers) HandleGetTarget(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	t, err := h.service.GetTarget(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if t == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "target not found"})
		return
	}
	c.JSON(http.StatusOK, t)
}

// HandleCreateTarget is POST /api/v1/deploy/targets.
func (h *DeployHandlers) HandleCreateTarget(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	var body targetBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	t := &apptypes.DeployTarget{
		Name:           body.Name,
		Provider:       "github",
		GitHubOwner:    body.GitHubOwner,
		GitHubRepo:     body.GitHubRepo,
		GitHubWorkflow: body.GitHubWorkflow,
		GitHubBranch:   body.GitHubBranch,
		DefaultInputs:  body.DefaultInputs,
		ConfigID:       body.ConfigID,
		InventoryPath:  body.InventoryPath,
	}
	if err := h.service.CreateTarget(c.Request.Context(), t, body.PAT); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Refresh so the response carries has_credential.
	out, _ := h.service.GetTarget(c.Request.Context(), t.ID)
	c.JSON(http.StatusCreated, out)
}

// HandleUpdateTarget is PUT /api/v1/deploy/targets/:id.
func (h *DeployHandlers) HandleUpdateTarget(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	existing, err := h.service.GetTarget(c.Request.Context(), id)
	if err != nil || existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "target not found"})
		return
	}
	var body targetBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	existing.Name = body.Name
	existing.GitHubOwner = body.GitHubOwner
	existing.GitHubRepo = body.GitHubRepo
	existing.GitHubWorkflow = body.GitHubWorkflow
	existing.GitHubBranch = body.GitHubBranch
	existing.DefaultInputs = body.DefaultInputs
	existing.ConfigID = body.ConfigID
	existing.InventoryPath = body.InventoryPath
	if err := h.service.UpdateTarget(c.Request.Context(), existing, body.PAT); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out, _ := h.service.GetTarget(c.Request.Context(), existing.ID)
	c.JSON(http.StatusOK, out)
}

// HandleDeleteTarget is DELETE /api/v1/deploy/targets/:id.
func (h *DeployHandlers) HandleDeleteTarget(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	if err := h.service.DeleteTarget(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// triggerBody is the JSON shape for POST /api/v1/deploy/runs.
type triggerBody struct {
	TargetID      string            `json:"target_id" binding:"required"`
	Inputs        map[string]string `json:"inputs,omitempty"`
	ExpectedHosts []string          `json:"expected_hosts,omitempty"`
	Notes         string            `json:"notes,omitempty"`
}

// HandleTriggerRun is POST /api/v1/deploy/runs. The hot path.
// Lint failures return 422 with the findings attached so the UI
// can render them inline.
func (h *DeployHandlers) HandleTriggerRun(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	var body triggerBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	requestedBy := actorFromContext(c)
	req := deploy.TriggerRequest{
		TargetID:      body.TargetID,
		RequestedBy:   requestedBy,
		Inputs:        body.Inputs,
		ExpectedHosts: body.ExpectedHosts,
		Notes:         body.Notes,
	}
	run, err := h.service.Trigger(c.Request.Context(), req)
	if err != nil {
		var lerr *deploy.LintGateError
		if errors.As(err, &lerr) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":         "config lint blocked deploy",
				"lint_findings": lerr.Findings,
			})
			return
		}
		h.logger.Warn("deploy trigger failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, run)
}

// HandleListRuns is GET /api/v1/deploy/runs?target_id=…&status=…&limit=…
func (h *DeployHandlers) HandleListRuns(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	filter := apptypes.DeployRunFilter{
		TargetID: c.Query("target_id"),
		Status:   c.Query("status"),
		Limit:    50,
	}
	runs, err := h.service.ListRuns(c.Request.Context(), filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": runs, "count": len(runs)})
}

// HandleGetRun is GET /api/v1/deploy/runs/:id. Calls SyncRun
// inline so the response reflects the freshest GitHub status —
// avoids the "is this run done yet?" polling-from-the-UI dance.
func (h *DeployHandlers) HandleGetRun(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	run, err := h.service.SyncRun(c.Request.Context(), id)
	if err != nil {
		h.logger.Debug("deploy run sync failed (returning cached)", zap.Error(err))
	}
	if run == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	c.JSON(http.StatusOK, run)
}

// HandleInventoryPreview is GET /api/v1/deploy/targets/:id/inventory.
// Returns the parsed host list from the target's configured
// inventory.ini. UI uses this to render the trigger sheet's
// "deploys to these hosts" read-only display before the operator
// fires the deploy.
//
// Returns 200 with an empty hosts list when no inventory_path is
// configured — caller's UI falls back to the manual host textarea.
// Returns 200 with a fetch_error message (and empty hosts) when the
// path is set but the fetch failed, so the trigger sheet can show
// a "couldn't read inventory.ini: 404" tooltip without blowing up.
func (h *DeployHandlers) HandleInventoryPreview(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	path, hosts, err := h.service.FetchInventory(c.Request.Context(), id)
	out := gin.H{
		"path":  path,
		"hosts": hosts,
	}
	if err != nil {
		// Don't 500 — surface the error in the body so the UI can
		// render it next to the (empty) host list.
		out["fetch_error"] = err.Error()
		h.logger.Debug("inventory preview fetch error",
			zap.String("target_id", id),
			zap.Error(err))
	}
	c.JSON(http.StatusOK, out)
}

// HandleLintConfig is POST /api/v1/deploy/targets/:id/lint. UI uses
// this to preview the lint result before clicking Deploy.
func (h *DeployHandlers) HandleLintConfig(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	t, err := h.service.GetTarget(c.Request.Context(), id)
	if err != nil || t == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "target not found"})
		return
	}
	if t.ConfigID == "" {
		c.JSON(http.StatusOK, gin.H{"findings": []any{}, "config_id": ""})
		return
	}
	findings, err := h.service.LintConfig(c.Request.Context(), t.ConfigID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"findings": findings, "config_id": t.ConfigID})
}

// actorFromContext extracts the auth actor for audit trails. Falls
// back to "anonymous" when auth is disabled.
func actorFromContext(c *gin.Context) string {
	if v, ok := c.Get("auth_actor"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return "anonymous"
}
