// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
)

// validationWorkflowPath is the fixed repo path for the Squadron-
// authored terraform-validate GitHub Action.
const validationWorkflowPath = ".github/workflows/squadron-validate.yml"

// validationWorkflowBranch is the dedicated setup branch (distinct from
// the squadron/rec/* remediation branches so it never collides).
const validationWorkflowBranch = "squadron/setup/terraform-validate"

// validationWorkflowYAML is the canonical workflow Squadron commits.
// It runs `terraform validate` on every PR across all module + env
// dirs with no backend and no credentials (safe: parse-only, no apply,
// no cloud calls). Making "Squadron Terraform Validate / validate" a
// required status check turns this into the merge-ready gate — a
// remediation snippet that wouldn't apply is caught before merge.
const validationWorkflowYAML = `name: Squadron Terraform Validate

# Added by Squadron (context-aware-merge-ready-prs arc, slice 3).
# Gates auto-generated remediation PRs: every pull request is checked
# with ` + "`terraform validate`" + ` so a snippet that would not apply is
# caught BEFORE merge. To ENFORCE merge-readiness, mark
# "Squadron Terraform Validate / validate" as a required status check
# in this repo's branch protection rules.
#
# Safe by construction: -backend=false, no credentials, no ` + "`apply`" + ` —
# this only parses + type-checks your Terraform.

on:
  pull_request:

permissions:
  contents: read

jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: hashicorp/setup-terraform@v3
      - name: terraform validate (all module + env dirs)
        shell: bash
        run: |
          set -uo pipefail
          dirs=$(find . -type f -name '*.tf' -not -path '*/.terraform/*' -exec dirname {} \; | sort -u)
          if [ -z "$dirs" ]; then echo "no terraform files found"; exit 0; fi
          rc=0
          while IFS= read -r d; do
            echo "== validating $d =="
            terraform -chdir="$d" init -backend=false -input=false >/dev/null || { rc=1; continue; }
            terraform -chdir="$d" validate || rc=1
          done <<< "$dirs"
          exit $rc
`

// HandleIaCGitHubSetupValidation opens a one-time PR that adds the
// Squadron terraform-validate GitHub Action to the connected repo. The
// merge-ready gate (slice 3) runs in the operator's CI; Squadron's job
// is to author the workflow so the operator gets it with one click.
func (h *IaCGitHubHandlers) HandleIaCGitHubSetupValidation(c *gin.Context) {
	connectionID := strings.TrimSpace(c.Param("id"))
	if connectionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	if h.store == nil || h.credKey == nil || h.clientFor == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "IaCNotWired",
			Message: "Squadron's IaC substrate isn't fully configured.",
		}})
		return
	}

	conn, err := h.store.Get(c.Request.Context(), connectionID)
	if err != nil {
		if errors.Is(err, iacconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No IaC connection exists with that ID. Connect a repo from the wizard first.",
			}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "IaCStoreReadFailed",
			Message: "Squadron could not read the IaC connection. The error has been logged.",
		}})
		if h.logger != nil {
			h.logger.Error("iac github setup-validation: store get failed", zap.Error(err))
		}
		return
	}

	creds, err := iacconnstore.UnmarshalGitHubPATCreds(conn.CredCiphertext, h.credKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredentialDecryptFailed",
			Message:       "Squadron could not decrypt the stored token. Re-run the connect wizard to refresh credentials.",
			SuggestedStep: "save",
		}})
		return
	}
	owner, repo, ok := splitRepoFullName(conn.RepoFullName)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "MalformedRepoFullName",
			Message: "The stored connection's repo_full_name is malformed.",
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

	// Idempotency: if the workflow already exists on the default branch,
	// there's nothing to do — report it cleanly rather than opening a
	// no-op PR.
	if fc, ferr := client.GetFileContent(ctx, owner, repo, validationWorkflowPath, defaultBranch); ferr == nil && fc != nil {
		c.JSON(http.StatusOK, gin.H{
			"already_configured": true,
			"file_path":          validationWorkflowPath,
			"message":            "The Squadron validation workflow is already present on the default branch.",
		})
		return
	} else if ferr != nil && !errors.Is(ferr, iacgithub.ErrFileNotFound) {
		he := humanizeGitHubErrorForOpenPR(ferr, conn.RepoFullName)
		c.JSON(statusForGitHubError(ferr), gin.H{"error": he})
		return
	}

	branchSHA, err := h.getBranchSHA(ctx, client, owner, repo, defaultBranch)
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	if err := client.CreateBranch(ctx, owner, repo, validationWorkflowBranch, branchSHA); err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	if _, err := client.PutFileContent(ctx, iacgithub.PutFileOptions{
		Owner:   owner,
		Repo:    repo,
		Path:    validationWorkflowPath,
		Branch:  validationWorkflowBranch,
		Content: []byte(validationWorkflowYAML),
		Message: "Squadron: add terraform-validate workflow (merge-ready gate)",
	}); err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	pr, err := client.OpenPR(ctx, iacgithub.OpenPROptions{
		Owner: owner, Repo: repo,
		Title: "Squadron: add terraform-validate workflow (merge-ready gate)",
		Body: "This PR adds a GitHub Action that runs `terraform validate` on every pull request " +
			"(no backend, no credentials, no apply — parse + type-check only).\n\n" +
			"Once merged, make **Squadron Terraform Validate / validate** a required status check in " +
			"branch protection to ensure Squadron's auto-generated remediation PRs can't be merged " +
			"unless they actually validate.\n\n_Authored by Squadron._",
		Head: validationWorkflowBranch,
		Base: defaultBranch,
	})
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"pr_number": pr.Number,
		"pr_url":    pr.HTMLURL,
		"branch":    validationWorkflowBranch,
		"file_path": validationWorkflowPath,
	})
}
