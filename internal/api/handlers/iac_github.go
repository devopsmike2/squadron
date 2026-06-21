// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/services"
)

// IaCGitHubClientFactory builds an iacgithub.Client from a PAT. The
// production wire constructs a *iacgithub.PATClient; tests inject a
// mock that records calls without touching real GitHub.
//
// The token is consumed inside the factory and not held by the
// handler — the handler never keeps plaintext PAT bytes past the
// factory call. (Mirrors the AWSCredMarshaller posture in
// discovery.go: the secret is held by the smallest possible code
// surface, and never by the handler.)
type IaCGitHubClientFactory func(token string) iacgithub.Client

// iacGitHubHandlerTimeout caps any single IaC-GitHub handler's total
// wall-clock budget. Open-PR walks four to six GitHub calls; 60s
// comfortably exceeds the happy path while bounding the worst case
// — same defense-in-depth posture as the AWS handlers'
// validateHandlerTimeout.
const iacGitHubHandlerTimeout = 60 * time.Second

// IaCGitHubHandlers serves the connect-IaC-repo surface — slice 1
// ships GitHub only; the design doc §2 names GitLab, Bitbucket and
// Azure DevOps as explicit non-goals.
//
// The handler is constructed per-request by the server.go
// trampoline so the iacconnstore.Store + credstore.Key + audit
// service are picked up at call time. Mirrors the AWS
// DiscoveryHandlers shape.
type IaCGitHubHandlers struct {
	store        iacconnstore.Store
	credKey      *credstore.Key
	clientFor    IaCGitHubClientFactory
	auditService services.AuditService
	logger       *zap.Logger
}

// NewIaCGitHubHandlers constructs an IaCGitHubHandlers. store may be
// nil at construction (the trampoline 503s in that case); credKey
// may be nil (Save / Open-PR 500 with a humanized "key not wired"
// error; Validate keeps working because Validate never touches the
// store). logger must be non-nil.
func NewIaCGitHubHandlers(store iacconnstore.Store, logger *zap.Logger) *IaCGitHubHandlers {
	return &IaCGitHubHandlers{
		store:     store,
		clientFor: defaultIaCGitHubClientFactory,
		logger:    logger,
	}
}

// defaultIaCGitHubClientFactory is the production GitHub client
// factory. Wraps the operator's PAT in a PATClient.
func defaultIaCGitHubClientFactory(token string) iacgithub.Client {
	return iacgithub.NewPATClient(token)
}

// WithCredstoreKey wires the credstore Key used to seal/unseal the
// PAT. Production callers pass the key the discovery substrate was
// opened with — that's the only way the Open-PR handler can decrypt
// what the Save handler encrypted.
func (h *IaCGitHubHandlers) WithCredstoreKey(key *credstore.Key) *IaCGitHubHandlers {
	h.credKey = key
	return h
}

// WithAuditService wires the audit recorder. Optional — a nil
// auditService means "no audit emission" rather than a 500. The
// test_server.go path relies on this.
func (h *IaCGitHubHandlers) WithAuditService(a services.AuditService) *IaCGitHubHandlers {
	h.auditService = a
	return h
}

// WithClientFactory overrides the GitHub client factory. Tests use
// this to inject a mock that records calls without round-tripping
// real GitHub; production callers let the default factory build a
// PATClient.
func (h *IaCGitHubHandlers) WithClientFactory(f IaCGitHubClientFactory) *IaCGitHubHandlers {
	h.clientFor = f
	return h
}

// iacGitHubPlacementEntryReq is the request-side placement-map row
// shape. The wire shape matches the substrate's PlacementMapEntry
// (snake_case provider/resource_kind/file_path).
type iacGitHubPlacementEntryReq struct {
	Provider     string `json:"provider"`
	ResourceKind string `json:"resource_kind"`
	FilePath     string `json:"file_path"`
}

// iacGitHubValidateRequest is the wizard's test-before-commit
// payload. Mirrors awsValidateRequest's shape — minimum fields the
// substrate row needs to be valid, the placement map declared
// up-front so Validate can preflight each declared file.
type iacGitHubValidateRequest struct {
	Token         string                       `json:"token"`
	RepoFullName  string                       `json:"repo_full_name"`
	DefaultBranch string                       `json:"default_branch"` // optional; server fetches if empty
	PlacementMap  []iacGitHubPlacementEntryReq `json:"placement_map"`
}

// iacGitHubPreflightRow is one preflight result, one per placement-map
// row. Sha is empty when the file does not exist (Validate treats
// missing files as a soft warning — PutFileContent will create them
// on the new branch at PR time).
type iacGitHubPreflightRow struct {
	Provider     string                  `json:"provider"`
	ResourceKind string                  `json:"resource_kind"`
	FilePath     string                  `json:"file_path"`
	Exists       bool                    `json:"exists"`
	ShaShort     string                  `json:"sha_short,omitempty"`
	Err          *scanner.HumanizedError `json:"err,omitempty"`
}

// iacGitHubValidateResponse is the wire shape the wizard renders into
// the "what just happened" panel. RepoFullName + DefaultBranch land
// even on the success path so the operator can confirm the wizard's
// state without re-fetching.
type iacGitHubValidateResponse struct {
	RepoFullName     string                   `json:"repo_full_name"`
	DefaultBranch    string                   `json:"default_branch"`
	RepoErr          *scanner.HumanizedError  `json:"repo_err,omitempty"`
	PreflightResults []iacGitHubPreflightRow  `json:"preflight_results"`
	Errors           []scanner.HumanizedError `json:"errors,omitempty"`
}

// HandleIaCGitHubValidate — POST /api/v1/iac/github/validate.
//
// Per the design doc §6, this handler:
//   - validates the request body shape;
//   - opens the GitHub client with the wizard-supplied PAT;
//   - GETs the repo to verify access + read default_branch;
//   - walks the placement map, GET-ing each declared file at the
//     repo's default branch — exists / sha_short / humanized error
//     surface per row.
//
// ZERO records created. The token is NEVER persisted, NEVER logged,
// NEVER echoed in the response. Emits iac.github.connection_validated
// per design doc §8.
func (h *IaCGitHubHandlers) HandleIaCGitHubValidate(c *gin.Context) {
	var req iacGitHubValidateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message:       "Request body could not be parsed as JSON. Check the wizard's payload shape.",
			SuggestedStep: "validate",
		}})
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingToken",
			Message:       "A GitHub personal access token is required. Paste the value from the wizard's PAT step (Advanced disclosure).",
			SuggestedStep: "pat",
		}})
		return
	}
	if strings.TrimSpace(req.RepoFullName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingRepoFullName",
			Message:       "Repository full name is required (e.g. \"octo/infra\"). Pick the repo from the wizard's Step 3.",
			SuggestedStep: "pick-repo",
		}})
		return
	}
	owner, repo, ok := splitRepoFullName(req.RepoFullName)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MalformedRepoFullName",
			Message:       "Repository full name must be in \"owner/repo\" form.",
			SuggestedStep: "pick-repo",
		}})
		return
	}
	if h.clientFor == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "IaC GitHub client factory not wired"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), iacGitHubHandlerTimeout)
	defer cancel()
	client := h.clientFor(req.Token)

	// Step 1: GetRepo. Surfaces both auth failure (typed
	// ErrAuthFailed → "Bad credentials" humanized) and missing-repo
	// (typed ErrRepoNotFound → "the repo is no longer reachable"
	// humanized). Either is operator-recoverable — 200 with errors[]
	// rather than 4xx so the wizard can render the per-step pointer.
	repoInfo, err := client.GetRepo(ctx, owner, repo)
	if err != nil {
		resp := iacGitHubValidateResponse{
			RepoFullName:     req.RepoFullName,
			DefaultBranch:    req.DefaultBranch,
			PreflightResults: []iacGitHubPreflightRow{},
		}
		resp.RepoErr = humanizeGitHubErrorForValidate(err)
		resp.Errors = []scanner.HumanizedError{*resp.RepoErr}
		c.JSON(http.StatusOK, resp)
		return
	}
	defaultBranch := repoInfo.DefaultBranch
	if strings.TrimSpace(req.DefaultBranch) != "" && req.DefaultBranch != defaultBranch {
		// The wizard's UI may have cached an out-of-date default
		// branch (operator renamed it server-side after install). Use
		// the server's view and let the wizard correct itself on the
		// response.
		defaultBranch = repoInfo.DefaultBranch
	}

	// Step 2: per-row preflight. Each row's file is read at the
	// default branch. Missing files are NOT a hard failure (exists =
	// false, no err); the PR builder will create them.
	rows := make([]iacGitHubPreflightRow, 0, len(req.PlacementMap))
	for _, e := range req.PlacementMap {
		row := iacGitHubPreflightRow{
			Provider:     e.Provider,
			ResourceKind: e.ResourceKind,
			FilePath:     e.FilePath,
		}
		if strings.TrimSpace(e.FilePath) == "" {
			row.Err = &scanner.HumanizedError{
				Code:          "MissingFilePath",
				Message:       "This placement row is missing a file path.",
				SuggestedStep: "placement-map",
			}
			rows = append(rows, row)
			continue
		}
		fc, ferr := client.GetFileContent(ctx, owner, repo, e.FilePath, defaultBranch)
		switch {
		case ferr == nil:
			row.Exists = true
			row.ShaShort = shortSHA(fc.SHA)
		case errors.Is(ferr, iacgithub.ErrFileNotFound):
			row.Exists = false
		case errors.Is(ferr, iacgithub.ErrAuthFailed):
			row.Err = &scanner.HumanizedError{
				Code:          "AuthFailed",
				Message:       "GitHub rejected the token while reading this file. Re-paste the token; ensure the `repo` scope is checked.",
				SuggestedStep: "pat",
			}
		case errors.Is(ferr, iacgithub.ErrRepoNotFound):
			row.Err = &scanner.HumanizedError{
				Code:          "RepoNotFound",
				Message:       fmt.Sprintf("The repo %q is no longer reachable. Re-pick the repo or re-run the connect wizard.", req.RepoFullName),
				SuggestedStep: "pick-repo",
			}
		default:
			row.Err = &scanner.HumanizedError{
				Code:          "FilePreflightFailed",
				Message:       fmt.Sprintf("Squadron could not read %q: %s", e.FilePath, ferr.Error()),
				SuggestedStep: "placement-map",
			}
		}
		rows = append(rows, row)
	}

	resp := iacGitHubValidateResponse{
		RepoFullName:     req.RepoFullName,
		DefaultBranch:    defaultBranch,
		PreflightResults: rows,
	}
	for _, r := range rows {
		if r.Err != nil {
			resp.Errors = append(resp.Errors, *r.Err)
		}
	}

	// Audit. Token is NEVER in the payload — neither is any per-row
	// content, only the structural results.
	if h.auditService != nil {
		auditRows := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			row := map[string]any{
				"provider":      r.Provider,
				"resource_kind": r.ResourceKind,
				"file_path":     r.FilePath,
				"exists":        r.Exists,
			}
			if r.ShaShort != "" {
				row["sha_short"] = r.ShaShort
			}
			if r.Err != nil {
				row["error_code"] = r.Err.Code
			}
			auditRows = append(auditRows, row)
		}
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventIaCGitHubConnectionValidated,
			TargetType: services.AuditTargetIaCConnection,
			TargetID:   req.RepoFullName,
			Action:     "validated",
			Payload: map[string]any{
				"repo_full_name":    req.RepoFullName,
				"default_branch":    defaultBranch,
				"preflight_results": auditRows,
				"recorded_at":       time.Now().UTC(),
			},
		})
	}

	c.JSON(http.StatusOK, resp)
}

// iacGitHubSaveConnectionRequest is the wizard's final Save step
// payload. Mirrors validate but adds the connection-shape fields the
// substrate row needs: repo_layout (mono|multi), optional
// branch_prefix and reviewer_team_handle.
type iacGitHubSaveConnectionRequest struct {
	Token              string                       `json:"token"`
	RepoFullName       string                       `json:"repo_full_name"`
	DefaultBranch      string                       `json:"default_branch"`
	RepoLayout         string                       `json:"repo_layout"`
	BranchPrefix       string                       `json:"branch_prefix"`
	ReviewerTeamHandle string                       `json:"reviewer_team_handle"`
	PlacementMap       []iacGitHubPlacementEntryReq `json:"placement_map"`
}

// iacGitHubSaveConnectionResponse is the success shape.
type iacGitHubSaveConnectionResponse struct {
	ConnectionID string `json:"connection_id"`
	RepoFullName string `json:"repo_full_name"`
	Status       string `json:"status"`
}

// HandleIaCGitHubSaveConnection — POST /api/v1/iac/github/connections.
//
// Defense-in-depth Validate: the substrate write only happens if the
// repo is still reachable. The token is sealed via
// MarshalGitHubPATCreds; the substrate row carries opaque ciphertext.
// Emits iac.github.connection_created on success.
func (h *IaCGitHubHandlers) HandleIaCGitHubSaveConnection(c *gin.Context) {
	var req iacGitHubSaveConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message:       "Request body could not be parsed as JSON. Check the wizard's payload shape.",
			SuggestedStep: "save",
		}})
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingToken",
			Message:       "A GitHub personal access token is required.",
			SuggestedStep: "pat",
		}})
		return
	}
	if strings.TrimSpace(req.RepoFullName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingRepoFullName",
			Message:       "Repository full name is required.",
			SuggestedStep: "pick-repo",
		}})
		return
	}
	owner, repo, ok := splitRepoFullName(req.RepoFullName)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MalformedRepoFullName",
			Message:       "Repository full name must be in \"owner/repo\" form.",
			SuggestedStep: "pick-repo",
		}})
		return
	}
	if req.RepoLayout == "" {
		req.RepoLayout = iacconnstore.RepoLayoutMono
	}
	if req.RepoLayout != iacconnstore.RepoLayoutMono && req.RepoLayout != iacconnstore.RepoLayoutMulti {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidRepoLayout",
			Message:       "repo_layout must be \"mono\" or \"multi\".",
			SuggestedStep: "repo-layout",
		}})
		return
	}

	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "IaCStoreNotWired",
			Message:       "Squadron's IaC connection substrate isn't configured. Restart the server with SQUADRON_SECRETS_KEY set.",
			SuggestedStep: "save",
		}})
		return
	}
	if h.credKey == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredKeyNotWired",
			Message:       "Squadron's credential encryption key isn't configured. Save cannot seal the token without it.",
			SuggestedStep: "save",
		}})
		return
	}
	if h.clientFor == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "IaC GitHub client factory not wired"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), iacGitHubHandlerTimeout)
	defer cancel()

	// Defense-in-depth Validate. Don't trust that the wizard already
	// passed Validate — the repo may have been deleted in the window
	// between Validate and Save.
	client := h.clientFor(req.Token)
	repoInfo, err := client.GetRepo(ctx, owner, repo)
	if err != nil {
		he := humanizeGitHubErrorForValidate(err)
		c.JSON(http.StatusBadRequest, gin.H{"error": he})
		return
	}
	defaultBranch := repoInfo.DefaultBranch
	if strings.TrimSpace(req.DefaultBranch) != "" {
		// The wizard's UI typically passes the value it cached at
		// Validate time. The server's view wins when they disagree —
		// the operator may have renamed the branch server-side.
		defaultBranch = repoInfo.DefaultBranch
	}

	// Seal the PAT. The plaintext token never lives past this call.
	ciphertext, err := iacconnstore.MarshalGitHubPATCreds(
		iacconnstore.GitHubPATCredentials{Token: req.Token},
		h.credKey,
	)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("iac github save: cred marshal failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredentialEncryptFailed",
			Message:       "Squadron could not encrypt the access token. Verify SQUADRON_SECRETS_KEY is set and retry.",
			SuggestedStep: "save",
		}})
		return
	}

	placement := make([]iacconnstore.PlacementMapEntry, 0, len(req.PlacementMap))
	for _, e := range req.PlacementMap {
		placement = append(placement, iacconnstore.PlacementMapEntry{
			Provider:     e.Provider,
			ResourceKind: e.ResourceKind,
			FilePath:     e.FilePath,
		})
	}

	conn := &iacconnstore.IaCConnection{
		Provider:           iacconnstore.ProviderGitHub,
		AuthKind:           iacconnstore.AuthKindPAT,
		RepoFullName:       req.RepoFullName,
		DefaultBranch:      defaultBranch,
		RepoLayout:         req.RepoLayout,
		BranchPrefix:       strings.TrimSpace(req.BranchPrefix),
		ReviewerTeamHandle: strings.TrimSpace(req.ReviewerTeamHandle),
		PlacementMap:       placement,
		CredCiphertext:     ciphertext,
	}
	if err := h.store.Create(c.Request.Context(), conn); err != nil {
		if errors.Is(err, iacconnstore.ErrConnectionConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": &scanner.HumanizedError{
				Code:          "ConnectionConflict",
				Message:       fmt.Sprintf("An IaC connection already exists for %q. Delete the existing connection before re-running the wizard.", req.RepoFullName),
				SuggestedStep: "list-connections",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("iac github save: store write failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "IaCStoreWriteFailed",
			Message:       "Squadron could not persist the IaC connection. The error has been logged; retry in a moment.",
			SuggestedStep: "save",
		}})
		return
	}

	// Audit. Token is NEVER in the payload (design doc §8 invariant).
	if h.auditService != nil {
		placementForAudit := make([]map[string]string, 0, len(placement))
		for _, e := range placement {
			placementForAudit = append(placementForAudit, map[string]string{
				"provider":      e.Provider,
				"resource_kind": e.ResourceKind,
				"file_path":     e.FilePath,
			})
		}
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventIaCGitHubConnectionCreated,
			TargetType: services.AuditTargetIaCConnection,
			TargetID:   conn.ConnectionID,
			Action:     "created",
			Payload: map[string]any{
				"connection_id":  conn.ConnectionID,
				"repo_full_name": conn.RepoFullName,
				"default_branch": conn.DefaultBranch,
				"auth_kind":      conn.AuthKind,
				"placement_map":  placementForAudit,
				"recorded_at":    time.Now().UTC(),
			},
		})
	}

	c.JSON(http.StatusCreated, iacGitHubSaveConnectionResponse{
		ConnectionID: conn.ConnectionID,
		RepoFullName: conn.RepoFullName,
		Status:       "connected",
	})
}

// iacGitHubConnectionRow is the redacted view of a stored
// IaCConnection. NEVER carries CredCiphertext, NEVER carries the
// token, NEVER carries internal substrate IDs beyond ConnectionID.
type iacGitHubConnectionRow struct {
	ConnectionID       string                       `json:"connection_id"`
	Provider           string                       `json:"provider"`
	AuthKind           string                       `json:"auth_kind"`
	RepoFullName       string                       `json:"repo_full_name"`
	DefaultBranch      string                       `json:"default_branch"`
	RepoLayout         string                       `json:"repo_layout"`
	BranchPrefix       string                       `json:"branch_prefix,omitempty"`
	ReviewerTeamHandle string                       `json:"reviewer_team_handle,omitempty"`
	PlacementMap       []iacGitHubPlacementEntryReq `json:"placement_map"`
	CreatedAt          time.Time                    `json:"created_at"`
}

// iacGitHubListConnectionsResponse — same empty-array posture as the
// AWS list endpoint. The UI keys off .length === 0.
type iacGitHubListConnectionsResponse struct {
	Connections []iacGitHubConnectionRow `json:"connections"`
}

// HandleListIaCGitHubConnections — GET /api/v1/iac/github/connections.
//
// Returns the redacted display fields of every stored IaC connection.
// CredCiphertext NEVER leaves the substrate.
func (h *IaCGitHubHandlers) HandleListIaCGitHubConnections(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "IaCStoreNotWired",
			Message:       "Squadron's IaC connection substrate isn't configured.",
			SuggestedStep: "save",
		}})
		return
	}
	conns, err := h.store.List(c.Request.Context())
	if err != nil {
		if h.logger != nil {
			h.logger.Error("iac github list: store read failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "IaCStoreReadFailed",
			Message:       "Squadron could not read the IaC connection list. The error has been logged.",
			SuggestedStep: "save",
		}})
		return
	}
	rows := make([]iacGitHubConnectionRow, 0, len(conns))
	for _, conn := range conns {
		if conn == nil {
			continue
		}
		entries := make([]iacGitHubPlacementEntryReq, 0, len(conn.PlacementMap))
		for _, e := range conn.PlacementMap {
			entries = append(entries, iacGitHubPlacementEntryReq{
				Provider:     e.Provider,
				ResourceKind: e.ResourceKind,
				FilePath:     e.FilePath,
			})
		}
		rows = append(rows, iacGitHubConnectionRow{
			ConnectionID:       conn.ConnectionID,
			Provider:           conn.Provider,
			AuthKind:           conn.AuthKind,
			RepoFullName:       conn.RepoFullName,
			DefaultBranch:      conn.DefaultBranch,
			RepoLayout:         conn.RepoLayout,
			BranchPrefix:       conn.BranchPrefix,
			ReviewerTeamHandle: conn.ReviewerTeamHandle,
			PlacementMap:       entries,
			CreatedAt:          conn.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, iacGitHubListConnectionsResponse{Connections: rows})
}

// HandleDeleteIaCGitHubConnection — DELETE
// /api/v1/iac/github/connections/:id.
//
// Idempotent: deleting a non-existent row returns 204 (mirrors the
// substrate's Delete contract). Slice 1 does not audit deletes —
// design doc §8 enumerates four events and delete is not among them.
func (h *IaCGitHubHandlers) HandleDeleteIaCGitHubConnection(c *gin.Context) {
	connectionID := strings.TrimSpace(c.Param("id"))
	if connectionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "IaCStoreNotWired",
			Message:       "Squadron's IaC connection substrate isn't configured.",
			SuggestedStep: "save",
		}})
		return
	}
	if err := h.store.Delete(c.Request.Context(), connectionID); err != nil {
		if h.logger != nil {
			h.logger.Error("iac github delete: store delete failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "IaCStoreDeleteFailed",
			Message:       "Squadron could not delete the IaC connection. The error has been logged.",
			SuggestedStep: "list-connections",
		}})
		return
	}
	c.Status(http.StatusNoContent)
}

// iacGitHubOpenPRRequest is the Recommendations-tab Open-PR payload.
// Mirrors the design doc §5: scan_id + step_idx pin down which
// recommendation; resource_kind keys the placement-map lookup;
// snippet is the proposer-emitted Terraform; proposer_reasoning is
// the overall narrative; affected_resources is the list rendered in
// the PR body.
type iacGitHubOpenPRRequest struct {
	ScanID            string   `json:"scan_id"`
	StepIdx           int      `json:"step_idx"`
	ResourceKind      string   `json:"resource_kind"`
	Snippet           string   `json:"snippet"`
	ProposerReasoning string   `json:"proposer_reasoning"`
	AffectedResources []string `json:"affected_resources"`
	AccountID         string   `json:"account_id"`
}

// iacGitHubOpenPRResponse is the success-path body.
type iacGitHubOpenPRResponse struct {
	PRNumber     int    `json:"pr_number"`
	PRURL        string `json:"pr_url"`
	Branch       string `json:"branch"`
	CommitSHA    string `json:"commit_sha"`
	FilePath     string `json:"file_path"`
	RepoFullName string `json:"repo_full_name"`
}

// HandleIaCGitHubOpenPR — POST
// /api/v1/iac/github/connections/:id/open-pr.
//
// Per the design doc §5 + §7:
//  1. Load the connection by ConnectionID.
//  2. Find the placement-map row for resource_kind. 422 with
//     NoPlacementMapping if missing.
//  3. Unseal the PAT via UnmarshalGitHubPATCreds.
//  4. Branch name: <BranchPrefix or DefaultBranchPrefix>-<scan_id_
//     short>-<step_idx>.
//  5. Refuse to write the default branch — both wrapper-layer and
//     handler-layer enforcement (defense in depth).
//  6. Create branch off default. Read current file content (empty
//     bytes on ErrFileNotFound). Append snippet with EXACTLY one
//     trailing newline. PUT file content to new branch.
//  7. Open PR title + body + labels per design doc §7. Request
//     reviewers if reviewer_team_handle is non-empty.
//  8. Emit recommendation.pr_opened on success;
//     recommendation.pr_open_failed on any GitHub-side error.
//     Snippet content is NEVER in either audit payload.
func (h *IaCGitHubHandlers) HandleIaCGitHubOpenPR(c *gin.Context) {
	connectionID := strings.TrimSpace(c.Param("id"))
	if connectionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}

	var req iacGitHubOpenPRRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message:       "Request body could not be parsed as JSON.",
			SuggestedStep: "open-pr",
		}})
		return
	}
	if strings.TrimSpace(req.ScanID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingScanID",
			Message: "scan_id is required.",
		}})
		return
	}
	if strings.TrimSpace(req.ResourceKind) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingResourceKind",
			Message: "resource_kind is required.",
		}})
		return
	}
	if strings.TrimSpace(req.Snippet) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingSnippet",
			Message: "snippet is required.",
		}})
		return
	}

	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "IaCStoreNotWired",
			Message:       "Squadron's IaC connection substrate isn't configured.",
			SuggestedStep: "save",
		}})
		return
	}
	if h.credKey == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredKeyNotWired",
			Message:       "Squadron's credential encryption key isn't configured.",
			SuggestedStep: "save",
		}})
		return
	}
	if h.clientFor == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "IaC GitHub client factory not wired"})
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
		if h.logger != nil {
			h.logger.Error("iac github open-pr: store get failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "IaCStoreReadFailed",
			Message: "Squadron could not read the IaC connection. The error has been logged.",
		}})
		return
	}

	// Find the placement-map row. 422 with NoPlacementMapping when
	// no row matches — the UI maps this to the "configure a path for
	// this kind" tooltip.
	var placement *iacconnstore.PlacementMapEntry
	for i, e := range conn.PlacementMap {
		if e.ResourceKind == req.ResourceKind {
			placement = &conn.PlacementMap[i]
			break
		}
	}
	if placement == nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": &scanner.HumanizedError{
			Code:          "NoPlacementMapping",
			Message:       fmt.Sprintf("No placement-map row exists for resource_kind %q. Add a row in the IaC connection's placement map and retry.", req.ResourceKind),
			SuggestedStep: "placement-map",
		}})
		return
	}

	// Unseal the PAT. Plaintext lives in `creds.Token` for the rest
	// of this function only — never logged, never echoed.
	creds, err := iacconnstore.UnmarshalGitHubPATCreds(conn.CredCiphertext, h.credKey)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("iac github open-pr: unmarshal creds failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredentialDecryptFailed",
			Message:       "Squadron could not decrypt the stored token. Re-run the connect wizard to refresh credentials.",
			SuggestedStep: "save",
		}})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), iacGitHubHandlerTimeout)
	defer cancel()

	client := h.clientFor(creds.Token)
	owner, repo, ok := splitRepoFullName(conn.RepoFullName)
	if !ok {
		// Substrate row's RepoFullName failed the parse — would only
		// happen if a future change relaxed the unique-index input
		// validation. Surface as 500.
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "MalformedRepoFullName",
			Message: "The stored connection's repo_full_name is malformed.",
		}})
		return
	}

	// Branch name. Handler-layer default-branch refusal — belt-and-
	// braces with the wrapper's own refusal so a future regression
	// at either layer is caught.
	scanIDShort := shortScanID(req.ScanID)
	prefix := conn.BranchPrefix
	if prefix == "" {
		prefix = iacconnstore.DefaultBranchPrefix
	}
	branchName := fmt.Sprintf("%s-%s-%d", prefix, scanIDShort, req.StepIdx)
	if branchEqualsDefault(branchName, conn.DefaultBranch) {
		// This should never happen — the prefix is "squadron/rec",
		// not "main" — but the handler-layer guard catches the
		// pathological case where an operator sets BranchPrefix =
		// their default branch name.
		he := &scanner.HumanizedError{
			Code:          "DefaultBranchWriteRefused",
			Message:       "Squadron will not push to the repo's default branch. Change the connection's branch_prefix.",
			SuggestedStep: "save",
		}
		h.emitPROpenFailed(c.Request.Context(), conn, &req, he, "", 0)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": he})
		return
	}

	// Step 1: GetRepo for the latest default-branch SHA. We need
	// it as the parent SHA for the new branch ref.
	repoInfo, err := client.GetRepo(ctx, owner, repo)
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		h.emitPROpenFailed(c.Request.Context(), conn, &req, he, "", 0)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}
	// The substrate row's DefaultBranch may be stale (operator
	// renamed it). The server's view wins for the write target.
	defaultBranch := repoInfo.DefaultBranch

	// Get the parent SHA: the tip of default. Read the placement-map
	// file from default first — gives us both the parent SHA (via
	// the file's commit chain on the server side) and the current
	// file blob SHA (needed for the PutFileContent update path).
	//
	// GitHub's git/ref endpoint is the more correct way to get the
	// branch tip SHA, but reading the file's content also yields
	// what we need with one fewer call: GetFileContent's response
	// carries the blob SHA, which we use as the PUT's `sha` field.
	// We use the repo's branch ref endpoint for the parent SHA.
	branchSHA, err := h.getBranchSHA(ctx, client, owner, repo, defaultBranch)
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		h.emitPROpenFailed(c.Request.Context(), conn, &req, he, "", 0)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}

	// Read current file content (empty bytes on ErrFileNotFound — the
	// file may not exist yet; PutFileContent will create it).
	var existingContent []byte
	var existingFileSHA string
	fc, ferr := client.GetFileContent(ctx, owner, repo, placement.FilePath, defaultBranch)
	switch {
	case ferr == nil:
		existingContent = fc.DecodedContent
		existingFileSHA = fc.SHA
	case errors.Is(ferr, iacgithub.ErrFileNotFound):
		// new file — leave both empty
	default:
		he := humanizeGitHubErrorForOpenPR(ferr, conn.RepoFullName)
		h.emitPROpenFailed(c.Request.Context(), conn, &req, he, "", 0)
		c.JSON(statusForGitHubError(ferr), gin.H{"error": he})
		return
	}

	// Step 4: create branch off default. A typed
	// ErrDefaultBranchWriteRefused from the wrapper should be
	// unreachable here — the handler already refused above — but the
	// statusForGitHubError map (422) catches it via the same path as
	// every other GitHub failure mode.
	if err := client.CreateBranch(ctx, owner, repo, branchName, branchSHA); err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		h.emitPROpenFailed(c.Request.Context(), conn, &req, he, branchName, 0)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}

	// Step 5: append snippet with exactly one trailing newline. The
	// invariant: the snippet block ends in EXACTLY one '\n'. If the
	// snippet already carries its own trailing newline we strip
	// duplicates; if it carries none we add one.
	finalContent := appendSnippetWithTrailingNewline(existingContent, []byte(req.Snippet))

	// Step 6: PUT file content to new branch.
	commitMsg := fmt.Sprintf("Squadron: instrument %s for scan %s step %d", req.ResourceKind, scanIDShort, req.StepIdx)
	putRes, err := client.PutFileContent(ctx, iacgithub.PutFileOptions{
		Owner:   owner,
		Repo:    repo,
		Path:    placement.FilePath,
		Branch:  branchName,
		Content: finalContent,
		Message: commitMsg,
		FileSHA: existingFileSHA,
	})
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		h.emitPROpenFailed(c.Request.Context(), conn, &req, he, branchName, 0)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}

	// Step 7: open PR.
	prTitle := buildPRTitle(req.ResourceKind, len(req.AffectedResources), scanIDShort)
	prBody := buildPRBody(req.ProposerReasoning, req.Snippet, req.AffectedResources)
	pr, err := client.OpenPR(ctx, iacgithub.OpenPROptions{
		Owner: owner, Repo: repo,
		Title: prTitle,
		Body:  prBody,
		Head:  branchName,
		Base:  defaultBranch,
	})
	if err != nil {
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		h.emitPROpenFailed(c.Request.Context(), conn, &req, he, branchName, 0)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}

	// Step 8: labels per design doc §7.
	labels := []string{"squadron", "squadron/" + req.ResourceKind}
	if err := client.AddLabels(ctx, owner, repo, pr.Number, labels); err != nil {
		// Labels failing isn't a hard failure — the PR is open and
		// the operator can label by hand. Log but don't fail the
		// flow.
		if h.logger != nil {
			h.logger.Warn("iac github open-pr: AddLabels failed (PR is open, continuing)",
				zap.Error(err), zap.Int("pr_number", pr.Number))
		}
	}

	if strings.TrimSpace(conn.ReviewerTeamHandle) != "" {
		teamSlug := teamSlugFromHandle(conn.ReviewerTeamHandle)
		if teamSlug != "" {
			if err := client.RequestReviewers(ctx, owner, repo, pr.Number, []string{teamSlug}); err != nil {
				if h.logger != nil {
					h.logger.Warn("iac github open-pr: RequestReviewers failed (PR is open, continuing)",
						zap.Error(err), zap.Int("pr_number", pr.Number))
				}
			}
		}
	}

	// Success audit. The snippet content is NEVER in the payload.
	if h.auditService != nil {
		payload := map[string]any{
			"scan_id":        req.ScanID,
			"step_idx":       req.StepIdx,
			"resource_kind":  req.ResourceKind,
			"repo_full_name": conn.RepoFullName,
			"pr_number":      pr.Number,
			"pr_url":         pr.HTMLURL,
			"branch":         branchName,
			"commit_sha":     putRes.CommitSHA,
			"file_path":      placement.FilePath,
			"actor":          services.AuditActorSystem,
			"recorded_at":    time.Now().UTC(),
		}
		if strings.TrimSpace(req.AccountID) != "" {
			payload["account_id"] = req.AccountID
		}
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventRecommendationPROpened,
			TargetType: services.AuditTargetIaCRecommendation,
			TargetID:   conn.ConnectionID,
			Action:     "pr_opened",
			Payload:    payload,
		})
	}

	c.JSON(http.StatusOK, iacGitHubOpenPRResponse{
		PRNumber:     pr.Number,
		PRURL:        pr.HTMLURL,
		Branch:       branchName,
		CommitSHA:    putRes.CommitSHA,
		FilePath:     placement.FilePath,
		RepoFullName: conn.RepoFullName,
	})
}

// emitPROpenFailed records the recommendation.pr_open_failed event.
// The snippet content is NEVER in the payload (design doc §8).
func (h *IaCGitHubHandlers) emitPROpenFailed(
	ctx context.Context,
	conn *iacconnstore.IaCConnection,
	req *iacGitHubOpenPRRequest,
	he *scanner.HumanizedError,
	branch string,
	prNumber int,
) {
	if h.auditService == nil {
		return
	}
	payload := map[string]any{
		"scan_id":           req.ScanID,
		"step_idx":          req.StepIdx,
		"resource_kind":     req.ResourceKind,
		"repo_full_name":    conn.RepoFullName,
		"error_code":        he.Code,
		"humanized_message": he.Message,
		"actor":             services.AuditActorSystem,
		"recorded_at":       time.Now().UTC(),
	}
	if branch != "" {
		payload["branch"] = branch
	}
	if prNumber > 0 {
		payload["pr_number"] = prNumber
	}
	if strings.TrimSpace(req.AccountID) != "" {
		payload["account_id"] = req.AccountID
	}
	_ = h.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  services.AuditEventRecommendationPROpenFailed,
		TargetType: services.AuditTargetIaCRecommendation,
		TargetID:   conn.ConnectionID,
		Action:     "pr_open_failed",
		Payload:    payload,
	})
}

// getBranchSHA returns the SHA the named branch tip points at via
// GitHub's /repos/.../git/ref/heads/<branch> endpoint. The handler
// uses it as the parent SHA for the new branch CreateBranch creates.
//
// Asserts the concrete branchSHAGetter capability on the client. The
// production PATClient implements it; tests inject a mock that
// implements it directly. A Client implementation that does not
// satisfy the assertion is a wiring bug and surfaces as a clear
// 500-class error.
func (h *IaCGitHubHandlers) getBranchSHA(
	ctx context.Context,
	client iacgithub.Client,
	owner, repo, branch string,
) (string, error) {
	bsg, ok := client.(branchSHAGetter)
	if !ok {
		return "", fmt.Errorf("iac github: client implementation does not support GetBranchSHA")
	}
	return bsg.GetBranchSHA(ctx, owner, repo, branch)
}

// branchSHAGetter is the slice-1-internal extension interface for
// retrieving a branch tip SHA. It lives here (not in the public
// iacgithub.Client) so adding the capability does not break callers
// in slice 2; the handler asks for it via type assertion.
type branchSHAGetter interface {
	GetBranchSHA(ctx context.Context, owner, repo, branch string) (string, error)
}

// humanizeGitHubErrorForValidate maps wrapper-layer errors to the
// scanner.HumanizedError shape the wizard renders. Validate has a
// softer posture than Open-PR — most failures are operator-
// recoverable through "re-paste the token" / "pick a different
// repo".
func humanizeGitHubErrorForValidate(err error) *scanner.HumanizedError {
	switch {
	case errors.Is(err, iacgithub.ErrAuthFailed):
		return &scanner.HumanizedError{
			Code:          "AuthFailed",
			Message:       "GitHub rejected the token. Re-paste the value; ensure the `repo` scope is checked.",
			SuggestedStep: "pat",
		}
	case errors.Is(err, iacgithub.ErrRepoNotFound):
		return &scanner.HumanizedError{
			Code:          "RepoNotFound",
			Message:       "The repo could not be reached. Verify the owner/repo spelling and the token's access.",
			SuggestedStep: "pick-repo",
		}
	default:
		return &scanner.HumanizedError{
			Code:          "GitHubRequestFailed",
			Message:       "Squadron could not reach GitHub: " + err.Error(),
			SuggestedStep: "validate",
		}
	}
}

// humanizeGitHubErrorForOpenPR is the Open-PR variant. Differs from
// validate's mapping mostly in the SuggestedStep — Open-PR jumps
// back to the Recommendations tab, not the wizard.
func humanizeGitHubErrorForOpenPR(err error, repoFullName string) *scanner.HumanizedError {
	switch {
	case errors.Is(err, iacgithub.ErrAuthFailed):
		return &scanner.HumanizedError{
			Code:          "AuthFailed",
			Message:       "GitHub rejected the stored token. Re-run the IaC connect wizard to refresh credentials.",
			SuggestedStep: "save",
		}
	case errors.Is(err, iacgithub.ErrRepoNotFound):
		return &scanner.HumanizedError{
			Code:          "RepoNotFound",
			Message:       fmt.Sprintf("The repo %q is no longer reachable. Re-run the IaC connect wizard.", repoFullName),
			SuggestedStep: "save",
		}
	case errors.Is(err, iacgithub.ErrDefaultBranchWriteRefused):
		return &scanner.HumanizedError{
			Code:    "DefaultBranchWriteRefused",
			Message: "Squadron will not push to the repo's default branch.",
		}
	default:
		return &scanner.HumanizedError{
			Code:    "GitHubRequestFailed",
			Message: "Squadron could not complete the GitHub call: " + err.Error(),
		}
	}
}

// statusForGitHubError picks the HTTP status code for an Open-PR
// failure. 401/404 are operator-recoverable through wizard re-run;
// everything else is 500.
func statusForGitHubError(err error) int {
	switch {
	case errors.Is(err, iacgithub.ErrAuthFailed):
		return http.StatusUnauthorized
	case errors.Is(err, iacgithub.ErrRepoNotFound):
		return http.StatusNotFound
	case errors.Is(err, iacgithub.ErrFileNotFound):
		return http.StatusNotFound
	case errors.Is(err, iacgithub.ErrDefaultBranchWriteRefused):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// appendSnippetWithTrailingNewline appends snippet to existing with
// EXACTLY one trailing newline. Design doc §7 invariant.
//
// Behavior:
//   - existing's trailing newlines are preserved if the file ends in
//     one (typical Terraform); a missing trailing newline before the
//     snippet is added so the new resource block starts on its own
//     line.
//   - any trailing whitespace at the end of snippet is normalized to
//     EXACTLY one '\n'.
func appendSnippetWithTrailingNewline(existing, snippet []byte) []byte {
	out := make([]byte, 0, len(existing)+len(snippet)+1)
	out = append(out, existing...)
	// If existing is non-empty and doesn't end with '\n', add one to
	// separate it from the appended block.
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		out = append(out, '\n')
	}
	// Strip any trailing newlines/whitespace from snippet, then add
	// one '\n'.
	trimmed := bytesTrimRightNewlines(snippet)
	out = append(out, trimmed...)
	out = append(out, '\n')
	return out
}

func bytesTrimRightNewlines(b []byte) []byte {
	i := len(b)
	for i > 0 && (b[i-1] == '\n' || b[i-1] == '\r') {
		i--
	}
	return b[:i]
}

// splitRepoFullName splits "owner/repo" into ("owner","repo",true);
// returns ("","",false) on any other shape. Owners and repos may
// contain dashes, dots, underscores — but not slashes — so the
// single-split check is sufficient.
func splitRepoFullName(s string) (string, string, bool) {
	s = strings.TrimSpace(s)
	idx := strings.IndexByte(s, '/')
	if idx <= 0 || idx == len(s)-1 {
		return "", "", false
	}
	if strings.IndexByte(s[idx+1:], '/') >= 0 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// shortSHA truncates a SHA to 7 chars for display. Mirrors `git log
// --abbrev-commit` defaults.
func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// shortScanID truncates a scan_id to 7 chars for the branch name.
// Mirrors the design doc's "scan_id short hash" framing.
func shortScanID(s string) string {
	if len(s) <= 7 {
		return s
	}
	return s[:7]
}

// branchEqualsDefault is the handler-layer default-branch refusal
// guard. Mirrors the wrapper's check.
func branchEqualsDefault(branch, defaultBranch string) bool {
	a := strings.TrimPrefix(branch, "refs/heads/")
	b := strings.TrimPrefix(defaultBranch, "refs/heads/")
	return a == b
}

// teamSlugFromHandle parses "org/team" into "team". Returns "" on
// any other shape — the caller skips the request in that case.
func teamSlugFromHandle(h string) string {
	parts := strings.Split(h, "/")
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// buildPRTitle assembles the design-doc §7 title shape:
// `Squadron: instrument <resource_kind> for <count> resources (scan
// <scan_id_short>)`.
func buildPRTitle(resourceKind string, count int, scanIDShort string) string {
	plural := "resources"
	if count == 1 {
		plural = "resource"
	}
	return fmt.Sprintf("Squadron: instrument %s for %d %s (scan %s)",
		resourceKind, count, plural, scanIDShort)
}

// buildPRBody assembles the design-doc §7 PR body: proposer
// reasoning, affected resources, the snippet in a fenced block, and
// the orchestrator-not-executor footer.
func buildPRBody(reasoning, snippet string, affected []string) string {
	var b strings.Builder
	if strings.TrimSpace(reasoning) != "" {
		b.WriteString("## Why\n\n")
		b.WriteString(strings.TrimSpace(reasoning))
		b.WriteString("\n\n")
	}
	if len(affected) > 0 {
		b.WriteString("## Affected resources\n\n")
		for _, r := range affected {
			b.WriteString("- ")
			b.WriteString(r)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("## Proposed change\n\n")
	b.WriteString("```hcl\n")
	b.WriteString(strings.TrimRight(snippet, "\n"))
	b.WriteString("\n```\n\n")
	b.WriteString("---\n")
	b.WriteString("Authored by Squadron as orchestrator, not executor: this PR awaits ")
	b.WriteString("your review and is gated by your branch protection. Your CI runs ")
	b.WriteString("`terraform plan` / `apply` on merge; Squadron does not run Terraform. ")
	b.WriteString("Squadron will not push to this branch again.\n")
	return b.String()
}
