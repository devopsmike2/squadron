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

// HandleListTargets is GET /api/v1/deploy/targets. v0.35 augments
// each target with a `last_run` summary so the card grid can show
// "Last deployed: 2h ago — Success" without a per-card fetch.
func (h *DeployHandlers) HandleListTargets(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	targets, err := h.service.ListTargets(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Pull the most recent run per target. List once and bucket in
	// memory rather than N+1 round-trips. Caps at 500 most-recent
	// runs across all targets; if a target's last run is older than
	// that cap, the card just won't show a recency badge — degrades
	// gracefully.
	runs, _ := h.service.ListRuns(c.Request.Context(), apptypes.DeployRunFilter{Limit: 500})
	latest := map[string]*apptypes.DeployRun{}
	for _, r := range runs {
		if _, seen := latest[r.TargetID]; !seen {
			latest[r.TargetID] = r
		}
	}
	type cardItem struct {
		*apptypes.DeployTarget
		LastRun *apptypes.DeployRun `json:"last_run,omitempty"`
	}
	out := make([]cardItem, 0, len(targets))
	for _, t := range targets {
		out = append(out, cardItem{DeployTarget: t, LastRun: latest[t.ID]})
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "count": len(out), "enabled": true})
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

// adoptBody is the JSON shape POSTed to /deploy/targets/:id/adopt.
// The handler resolves each hostname against the inventory store to
// pick up any labels the CI/CD pipeline registered with the host
// when it was discovered, so the snippet's agent_description block
// carries through the pipeline-set metadata.
type adoptBody struct {
	Hostnames []string `json:"hostnames"`
	Notes     string   `json:"notes,omitempty"`
	// OpAMPServerURL lets the caller override the URL baked into
	// each snippet. Empty falls back to ws://<request-host>:<opamp-port>/v1/opamp
	// which is right for dev but wrong for production where the
	// adoption pipeline runs from a CI runner that can't reach
	// localhost. Operators should explicitly set this in prod.
	OpAMPServerURL string `json:"opamp_server_url,omitempty"`
}

// HandleAdopt is POST /api/v1/deploy/targets/:id/adopt.
//
// Fires the configured adoption pipeline with a single
// adoption_payload input containing one per-host snippet block per
// requested hostname. Used by the v0.46 bulk adoption flow from the
// Inventory page.
//
// The hostnames in the body are the source of truth for who gets
// adopted. The handler enriches each one with labels from any
// matching expected_agents row so the snippet carries the same
// metadata the CI/CD pipeline originally registered.
//
// Added in v0.46.0.
func (h *DeployHandlers) HandleAdopt(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	var body adoptBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(body.Hostnames) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hostnames required"})
		return
	}
	// Resolve labels via the deploy service's inventory accessor.
	// We don't pull this from the store directly — the deploy.Service
	// owns its store handle, so we ask it.
	hosts := h.service.ResolveAdoptionHosts(c.Request.Context(), body.Hostnames)
	opampURL := body.OpAMPServerURL
	if opampURL == "" {
		// Best-effort fallback: use the host from the request and
		// the standard OpAMP port. Works for dev; production
		// operators should set this explicitly.
		opampURL = "ws://" + c.Request.Host + "/v1/opamp"
	}
	actor := actorFromContext(c)
	run, err := h.service.TriggerAdoption(c.Request.Context(), deploy.AdoptRequest{
		TargetID:       id,
		Hosts:          hosts,
		RequestedBy:    actor,
		Notes:          body.Notes,
		OpAMPServerURL: opampURL,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, run)
}

// HandleMetrics is GET /api/v1/deploy/metrics?window=30d.
//
// Returns the DORA-style summary (deploy frequency, change failure
// rate, MTTR, lead time) computed in-process from the deploy_runs
// ledger. Window defaults to 30d if missing or unrecognized.
//
// Added in v0.39.0 — directors and SRE leads asked for the standard
// DORA dashboard. Computing in-handler from existing ledger data
// avoids any new schema, and the result is cheap to recompute on
// every poll because the underlying slice is bounded.
func (h *DeployHandlers) HandleMetrics(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	windowParam := c.DefaultQuery("window", "30d")
	window := deploy.DORAWindow(windowParam)
	metrics, err := h.service.Metrics(c.Request.Context(), window)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, metrics)
}

// HandleValidate is POST /api/v1/deploy/targets/:id/validate. The
// v0.35 pre-flight: tests GitHub auth, workflow existence, inventory
// readability, and configlint without firing a workflow_dispatch.
// Always returns 200 with the result payload — individual checks
// report their own status — so the UI can render a checklist.
func (h *DeployHandlers) HandleValidate(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	result, err := h.service.Validate(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// HandleRedeploy is POST /api/v1/deploy/runs/:id/redeploy. Replays
// a past run's inputs as a new deploy. Operator's "incident
// response" panic button — known-good config + known-good inputs,
// one click.
func (h *DeployHandlers) HandleRedeploy(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	prev, err := h.service.GetRun(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if prev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	req := deploy.TriggerRequest{
		TargetID:    prev.TargetID,
		RequestedBy: actorFromContext(c),
		Inputs:      prev.Inputs,
		Notes:       "redeploy of " + prev.ID,
	}
	// Don't pass ExpectedHosts — if the target has an inventory file
	// the service will repopulate from the current file at trigger
	// time (which may have changed since the previous run).
	run, err := h.service.Trigger(c.Request.Context(), req)
	if err != nil {
		var lerr *deploy.LintGateError
		if errors.As(err, &lerr) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":         "config lint blocked redeploy",
				"lint_findings": lerr.Findings,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
	path, statuses, err := h.service.HostsWithLiveStatus(c.Request.Context(), id)
	out := gin.H{
		"path":  path,
		"hosts": statuses,
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
