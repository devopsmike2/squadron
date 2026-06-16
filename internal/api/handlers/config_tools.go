// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/devopsmike2/squadron/internal/configlint"
	"github.com/devopsmike2/squadron/internal/configtemplates"
	"github.com/devopsmike2/squadron/internal/services"
)

// LintConfigRequest is the body of POST /api/v1/configs/lint.
type LintConfigRequest struct {
	Content string `json:"content" binding:"required"`
}

// LintConfigResponse is the body of POST /api/v1/configs/lint. Findings is
// always non-nil so the UI can rely on `.length` without a null check.
type LintConfigResponse struct {
	Findings []configlint.Finding `json:"findings"`
}

// HandleLintConfig runs the structural lint engine over a YAML body and
// returns every finding. This is the endpoint the editor's debounced
// validation panel calls on every keystroke pause.
//
// The existing `/api/v1/configs/validate` endpoint focuses on whether the
// YAML parses and meets a handful of OTel-specific shape checks; this one
// goes further with anti-pattern detection (memory_limiter position,
// missing batch, localhost exporters, undefined component references).
func (h *ConfigHandlers) HandleLintConfig(c *gin.Context) {
	var req LintConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "detail": err.Error()})
		return
	}

	findings := configlint.Lint(req.Content)
	if findings == nil {
		findings = []configlint.Finding{}
	}
	// v0.51 — persist a config.lint_evaluated audit event when the
	// lint actually produced findings, so the evidence trail for
	// NIST CSF PR.PS-06 (configuration management) and SOC 2 CC8.1
	// (change management) shows that the org evaluated proposed
	// changes against guardrails before they shipped. We don't
	// persist the full YAML (sometimes operator-sensitive) — only
	// the rule IDs and severities, which is the "what did the
	// gating say" auditors actually want.
	if h.audit != nil && len(findings) > 0 {
		var errCount, warnCount int
		ruleHits := make([]string, 0, len(findings))
		for _, f := range findings {
			switch f.Severity {
			case configlint.SeverityError:
				errCount++
			case configlint.SeverityWarning:
				warnCount++
			}
			ruleHits = append(ruleHits, f.Rule)
		}
		_ = h.audit.Record(c.Request.Context(), services.AuditEntry{
			EventType:  "config.lint_evaluated",
			TargetType: "config",
			TargetID:   "ad-hoc", // editor lint runs are not yet
			// associated with a specific config row; once the editor
			// gains a "save draft" affordance we can pass the draft
			// ID through here and stop using "ad-hoc".
			Action: "lint_evaluated",
			Payload: map[string]any{
				"errors":    errCount,
				"warnings":  warnCount,
				"rule_hits": ruleHits,
				// Hash of the content so an auditor can confirm two
				// events refer to the same proposal without us
				// storing the YAML itself.
				"content_sha256": sha256Hex(req.Content),
			},
		})
	}
	c.JSON(http.StatusOK, LintConfigResponse{Findings: findings})
}

// sha256Hex returns the hex sha256 of s. Used by the lint audit
// payload so two events that referred to the same proposed config
// are correlatable without persisting the YAML body itself.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// HandleGetConfigTemplates returns the catalog of curated YAML snippets.
// The UI renders these in a "Templates" dropdown in the config editor
// header.
func (h *ConfigHandlers) HandleGetConfigTemplates(c *gin.Context) {
	templates := configtemplates.All()
	c.JSON(http.StatusOK, gin.H{"templates": templates})
}

// HandleGetConfigTemplate returns a single template by ID. Useful for the
// UI to load just the YAML body on insert without re-fetching the whole
// catalog.
func (h *ConfigHandlers) HandleGetConfigTemplate(c *gin.Context) {
	id := c.Param("id")
	template := configtemplates.Get(id)
	if template == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "template not found", "id": id})
		return
	}
	c.JSON(http.StatusOK, template)
}
