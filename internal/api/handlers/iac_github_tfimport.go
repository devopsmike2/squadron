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
	"github.com/devopsmike2/squadron/internal/iac/tfimport"
)

const tfImportFilePath = "squadron_imports.tf"

// iacGitHubTerraformImportPRRequest carries the scan result to turn into
// import blocks. Mirrors the recommendations request shape.
type iacGitHubTerraformImportPRRequest struct {
	ScanResult awsScanResponse `json:"scan_result"`
}

// HandleIaCGitHubTerraformImportPR generates Terraform import{} blocks
// for a scan result, dedups them against any existing squadron_imports.tf
// (idempotent by cloud import ID), and opens a PR adding/appending the
// blocks. env->Terraform arc slice 2.
func (h *IaCGitHubHandlers) HandleIaCGitHubTerraformImportPR(c *gin.Context) {
	connectionID := strings.TrimSpace(c.Param("id"))
	if connectionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code: "MissingConnectionID", Message: "Connection ID path parameter is required.",
		}})
		return
	}
	var req iacGitHubTerraformImportPRRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message: "Request body could not be parsed as JSON.",
		}})
		return
	}
	if req.ScanResult.AccountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code: "MissingScanAccountID", Message: "scan_result.account_id is required.",
		}})
		return
	}
	if h.store == nil || h.credKey == nil || h.clientFor == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code: "IaCNotWired", Message: "Squadron's IaC substrate isn't fully configured.",
		}})
		return
	}

	blocks, skipped := tfimport.Generate(awsScanToImportResources(req.ScanResult))
	if len(blocks) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"block_count": 0,
			"skipped":     skipped,
			"message":     "No resources in this scan have a supported import mapping yet.",
		})
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
	client := h.clientFor(creds.Token)
	repoInfo, err := client.GetRepo(ctx, owner, repo)
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	defaultBranch := repoInfo.DefaultBranch

	// Dedup against any existing squadron_imports.tf (idempotent by
	// cloud import ID). When present we APPEND new blocks; the file's
	// SHA is needed for the update PUT.
	var existingContent []byte
	var existingSHA string
	if fc, ferr := client.GetFileContent(ctx, owner, repo, tfImportFilePath, defaultBranch); ferr == nil && fc != nil {
		existingContent = fc.DecodedContent
		existingSHA = fc.SHA
	} else if ferr != nil && !errors.Is(ferr, iacgithub.ErrFileNotFound) {
		he := humanizeGitHubErrorForOpenPR(ferr, conn.RepoFullName)
		c.JSON(statusForGitHubError(ferr), gin.H{"error": he})
		return
	}
	blocks, removed := tfimport.DedupByImportID(blocks, tfimport.ParseExistingImportIDs(existingContent))
	if len(blocks) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"block_count":      0,
			"already_imported": true,
			"message":          fmt.Sprintf("All %d candidate resource(s) are already present in %s.", removed, tfImportFilePath),
		})
		return
	}

	var finalContent []byte
	if len(existingContent) > 0 {
		finalContent = appendSnippetWithTrailingNewline(existingContent, []byte(tfimport.RenderBlocksOnly(blocks)))
	} else {
		finalContent = []byte(tfimport.Render(blocks, skipped))
	}

	branch := fmt.Sprintf("%simports/%s", normalizedBranchPrefix(conn), shortScanID(req.ScanResult.ScanID))
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
		Owner: owner, Repo: repo, Path: tfImportFilePath, Branch: branch,
		Content: finalContent, FileSHA: existingSHA,
		Message: "Squadron: import blocks to adopt un-managed resources",
	}); err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	pr, err := client.OpenPR(ctx, iacgithub.OpenPROptions{
		Owner: owner, Repo: repo,
		Title: fmt.Sprintf("Squadron: adopt %d un-managed resource(s) into Terraform", len(blocks)),
		Body: "These Terraform `import {}` blocks bring existing cloud resources under Terraform.\n\n" +
			"Run `terraform plan -generate-config-out=generated.tf` then `terraform apply` to adopt them " +
			"(requires Terraform >= 1.5). Review the generated config before applying.\n\n_Authored by Squadron._",
		Head: branch, Base: defaultBranch,
	})
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	if h.logger != nil {
		h.logger.Info("iac github terraform-import: PR opened",
			zap.Int("pr", pr.Number), zap.Int("blocks", len(blocks)), zap.Int("deduped", removed))
	}
	c.JSON(http.StatusOK, gin.H{
		"pr_number": pr.Number, "pr_url": pr.HTMLURL, "branch": branch,
		"file_path": tfImportFilePath, "block_count": len(blocks), "deduped": removed,
	})
}

// normalizedBranchPrefix returns the connection's branch prefix ending
// in "/" (mirrors the open-PR handler's prefix handling).
func normalizedBranchPrefix(conn *iacconnstore.IaCConnection) string {
	prefix := conn.BranchPrefix
	if prefix == "" {
		prefix = iacconnstore.DefaultBranchPrefix
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix
}
