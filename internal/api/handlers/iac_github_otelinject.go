// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/iac/otelconfig"
)

// iacGitHubOTelInjectPRRequest carries the target collector config path
// plus the Squadron OTLP endpoint to inject. Mirrors the tfimport PR
// request shape. OTEL-agent arc slice 3.
type iacGitHubOTelInjectPRRequest struct {
	// ConfigPath is the repo-relative path of the OTel Collector config
	// to patch (e.g. modules/standalone-collector/collector.yaml).
	ConfigPath string `json:"config_path"`
	// Endpoint is the Squadron OTLP endpoint to inject (host:port).
	Endpoint string `json:"endpoint"`
	// Protocol selects the exporter type: "grpc" (otlp, default) or
	// "http" (otlphttp).
	Protocol string `json:"protocol"`
	// Insecure adds tls.insecure to the exporter (dev/self-signed).
	Insecure bool `json:"insecure"`
	// Signals optionally restricts which pipelines to wire (default
	// traces+metrics+logs).
	Signals []string `json:"signals"`
}

// HandleIaCGitHubOTelInjectPR injects a Squadron OTLP exporter into an
// existing OTel Collector config in the connected repo and opens a PR
// with the change. Idempotent: when the config already exports to the
// endpoint, no PR is opened. OTEL-agent arc slice 3.
func (h *IaCGitHubHandlers) HandleIaCGitHubOTelInjectPR(c *gin.Context) {
	connectionID := strings.TrimSpace(c.Param("id"))
	if connectionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code: "MissingConnectionID", Message: "Connection ID path parameter is required.",
		}})
		return
	}
	var req iacGitHubOTelInjectPRRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message: "Request body could not be parsed as JSON.",
		}})
		return
	}
	req.ConfigPath = strings.TrimSpace(req.ConfigPath)
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	if req.ConfigPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code: "MissingConfigPath", Message: "config_path is required (repo-relative collector config path).",
		}})
		return
	}
	if req.Endpoint == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code: "MissingEndpoint", Message: "endpoint is required (Squadron OTLP endpoint host:port).",
		}})
		return
	}
	if h.store == nil || h.credKey == nil || h.clientFor == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code: "IaCNotWired", Message: "Squadron's IaC substrate isn't fully configured.",
		}})
		return
	}

	conn, err := h.store.Get(c.Request.Context(), connectionID)
	if err != nil {
		if errors.Is(err, iacconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code: "ConnectionNotFound", Message: "No IaC connection exists with that ID.",
			}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code: "IaCStoreReadFailed", Message: "Squadron could not read the IaC connection.",
		}})
		return
	}
	creds, err := iacconnstore.UnmarshalGitHubPATCreds(conn.CredCiphertext, h.credKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code: "CredentialDecryptFailed", Message: "Squadron could not decrypt the stored token.", SuggestedStep: "save",
		}})
		return
	}
	owner, repo, ok := splitRepoFullName(conn.RepoFullName)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code: "MalformedRepoFullName", Message: "The stored connection's repo_full_name is malformed.",
		}})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), iacGitHubHandlerTimeout)
	defer cancel()
	client := h.clientForConn(conn, creds.Token)
	repoInfo, err := client.GetRepo(ctx, owner, repo)
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	defaultBranch := repoInfo.DefaultBranch

	// The collector config must already exist — the injector patches it,
	// it does not create a collector from scratch.
	fc, ferr := client.GetFileContent(ctx, owner, repo, req.ConfigPath, defaultBranch)
	if ferr != nil {
		if errors.Is(ferr, iacgithub.ErrFileNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "CollectorConfigNotFound",
				Message: fmt.Sprintf("No collector config found at %q on %s.", req.ConfigPath, defaultBranch),
			}})
			return
		}
		he := humanizeGitHubErrorForOpenPR(ferr, conn.RepoFullName)
		c.JSON(statusForGitHubError(ferr), gin.H{"error": he})
		return
	}

	res, err := otelconfig.InjectOTLPExporter(fc.DecodedContent, req.Endpoint, otelconfig.Options{
		Protocol: req.Protocol,
		Insecure: req.Insecure,
		Signals:  req.Signals,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code: "CollectorConfigInjectFailed", Message: "Squadron could not parse the collector config: " + err.Error(),
		}})
		return
	}
	if !res.Changed {
		c.JSON(http.StatusOK, gin.H{
			"changed":       false,
			"already_wired": true,
			"file_path":     req.ConfigPath,
			"message":       fmt.Sprintf("%s already exports to %s; no PR opened.", req.ConfigPath, req.Endpoint),
		})
		return
	}

	branch := fmt.Sprintf("%sotel-inject/%s", normalizedBranchPrefix(conn), sanitizeBranchSegment(req.ConfigPath))
	branchSHA, err := h.getBranchSHA(ctx, client, owner, repo, defaultBranch)
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	if err := client.CreateBranch(ctx, owner, repo, branch, branchSHA); err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	if _, err := client.PutFileContent(ctx, iacgithub.PutFileOptions{
		Owner: owner, Repo: repo, Path: req.ConfigPath, Branch: branch,
		Content: res.Bytes, FileSHA: fc.SHA,
		Message: "Squadron: inject OTLP exporter so the collector connects to Squadron",
	}); err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	pr, err := client.OpenPR(ctx, iacgithub.OpenPROptions{
		Owner: owner, Repo: repo,
		Title: "Squadron: connect OTel Collector — inject OTLP exporter",
		Body:  fmt.Sprintf("Adds a dedicated `otlp/squadron` exporter to `%s` pointing at `%s` and wires it into the collector pipelines, so this agent ships telemetry to Squadron.\n\n_Authored by Squadron._", req.ConfigPath, req.Endpoint),
		Head:  branch, Base: defaultBranch,
	})
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	if h.logger != nil {
		h.logger.Info("iac github otel-inject: PR opened",
			zap.Int("pr", pr.Number), zap.String("config_path", req.ConfigPath))
	}
	c.JSON(http.StatusOK, gin.H{
		"changed":   true,
		"pr_number": pr.Number, "pr_url": pr.HTMLURL, "branch": branch,
		"file_path": req.ConfigPath, "summary": res.Summary,
	})
}

// sanitizeBranchSegment turns a config path into a safe branch segment.
func sanitizeBranchSegment(s string) string {
	r := strings.NewReplacer("/", "-", " ", "-", "\\", "-")
	out := r.Replace(strings.TrimSpace(s))
	out = strings.TrimSuffix(out, ".yaml")
	out = strings.TrimSuffix(out, ".yml")
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}
