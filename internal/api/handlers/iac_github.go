// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/iac"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/iac/hclpatch"
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

	// v0.89.43 (#663 Stream 61, slice 1 chunk 2 of the GitHub Checks
	// API back-signal arc). All four fields are optional: when any
	// one is nil/empty the chunk-2 check-run follow-up on the PR open
	// path is a no-op (fail-open per design doc §5). This is the
	// slice-1 posture for deployments that haven't upgraded their PAT
	// scope or wired the checks integration. ChecksAPI + CheckRunStore
	// are the slim interfaces defined in iac_github_checkrun.go.
	checksClient    ChecksAPI
	checkRunStore   CheckRunStore
	squadronHost    string
	checkRunNameVal string
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

// WithChecksClient wires the Checks API client used by the chunk-2
// PR-open follow-up. Nil keeps the follow-up dormant — the existing
// recommendation.pr_opened path completes normally with no check-run
// side-effects. See design doc §5 fail-open posture.
//
// v0.89.43 (#663 Stream 61, slice 1 chunk 2).
func (h *IaCGitHubHandlers) WithChecksClient(c ChecksAPI) *IaCGitHubHandlers {
	h.checksClient = c
	return h
}

// WithCheckRunStore wires the application-store surface the chunk-2
// follow-up uses to persist the durable check-run state per
// recommendation. Nil keeps the storage write a no-op — the live
// check run on GitHub will reconcile on the next event (chunks 3+).
//
// v0.89.43 (#663 Stream 61, slice 1 chunk 2).
func (h *IaCGitHubHandlers) WithCheckRunStore(s CheckRunStore) *IaCGitHubHandlers {
	h.checkRunStore = s
	return h
}

// WithSquadronHost configures the base URL the check-run summary's
// "View in Squadron" link targets. Empty value suppresses the link
// line rather than emitting a broken (/) href.
//
// v0.89.43 (#663 Stream 61, slice 1 chunk 2).
func (h *IaCGitHubHandlers) WithSquadronHost(host string) *IaCGitHubHandlers {
	h.squadronHost = host
	return h
}

// WithCheckRunName overrides the slice-1 default check-run name
// ("Squadron recommendation" per design doc §11 Q2). Operator
// override via env var SQUADRON_CHECK_RUN_NAME flows here; an empty
// value keeps the default.
//
// v0.89.43 (#663 Stream 61, slice 1 chunk 2).
func (h *IaCGitHubHandlers) WithCheckRunName(name string) *IaCGitHubHandlers {
	h.checkRunNameVal = name
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

// iacGitHubUpdatePlacementMapRequest is the v0.89.4 #610 placement-map
// edit payload. Operators reach this through the deep-linked wizard
// (DiscoveryAWS's NoPlacementMapping / State-B link target):
// /discovery/iac/github?connection_id=<uuid>&step=placement&kind=<kind>.
// The wizard pre-fills with the connection's existing rows, the
// operator edits, and Save here mutates the substrate row in place.
//
// Only the placement_map is mutable through this endpoint — branch
// prefix, reviewer team, token, repo are owned by the create-time
// wizard. Slice 1 design doc §6 invariant: credential rotation is
// Delete + re-create, not in-place edit.
type iacGitHubUpdatePlacementMapRequest struct {
	PlacementMap []iacGitHubPlacementEntryReq `json:"placement_map"`
}

// HandleIaCGitHubUpdatePlacementMap — PATCH
// /api/v1/iac/github/connections/:id/placement-map.
//
// Replaces the placement_map column on the connection identified by
// :id. 404 if no such connection exists. Emits
// iac.github.placement_map_updated on success (design doc §8 +
// v0.89.4 audit-event-registry addendum). Token bytes NEVER touch this
// flow — the substrate's cred_ciphertext is preserved untouched.
func (h *IaCGitHubHandlers) HandleIaCGitHubUpdatePlacementMap(c *gin.Context) {
	connectionID := strings.TrimSpace(c.Param("id"))
	if connectionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}

	var req iacGitHubUpdatePlacementMapRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message:       "Request body could not be parsed as JSON.",
			SuggestedStep: "placement-map",
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

	// Look the connection up before the write so we can a) 404 cleanly
	// (separate from "store down") and b) carry repo_full_name into
	// the audit payload — the timeline humanizer keys off it.
	conn, err := h.store.Get(c.Request.Context(), connectionID)
	if err != nil {
		if errors.Is(err, iacconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No IaC connection exists with that ID. The connection may have been deleted; re-run the connect wizard.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("iac github placement-map update: store get failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "IaCStoreReadFailed",
			Message: "Squadron could not read the IaC connection. The error has been logged.",
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

	if err := h.store.UpdatePlacementMap(c.Request.Context(), connectionID, placement); err != nil {
		if errors.Is(err, iacconnstore.ErrConnectionNotFound) {
			// Race: row vanished between the Get above and the Update.
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "The IaC connection was deleted while the update was in flight.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("iac github placement-map update: store write failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "IaCStoreWriteFailed",
			Message:       "Squadron could not persist the placement-map update. The error has been logged; retry in a moment.",
			SuggestedStep: "placement-map",
		}})
		return
	}

	// Audit. Token bytes NEVER in payload (design doc §8 invariant);
	// the row's CredCiphertext is untouched by this call so there's
	// nothing token-shaped to leak.
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
			EventType:  services.AuditEventIaCGitHubPlacementMapUpdated,
			TargetType: services.AuditTargetIaCConnection,
			TargetID:   conn.ConnectionID,
			Action:     "placement_map_updated",
			Payload: map[string]any{
				"connection_id":  conn.ConnectionID,
				"repo_full_name": conn.RepoFullName,
				"placement_map":  placementForAudit,
				"recorded_at":    time.Now().UTC(),
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"connection_id":  conn.ConnectionID,
		"repo_full_name": conn.RepoFullName,
		"placement_map":  placement,
	})
}

// iacGitHubUpdateConnectionRequest is the v0.89.28 (#643 slice 1)
// PATCH /api/v1/iac/github/connections/:id payload. Slice 1 carries
// exactly one mutable field: LearnFromAcceptedRecommendations, the
// per-connection opt-in for the discovery proposer's accepted-
// examples feedback loop. The shape mirrors v0.89.18's
// LearnFromVerdicts pointer-bool pattern so partial-update semantics
// match across both proposer surfaces:
//
//   - nil → leave the column untouched (no-op the PATCH).
//   - explicit false → opt out (the proposer prompt block goes
//     silent for this connection).
//   - explicit true → opt back in (default for new connections).
type iacGitHubUpdateConnectionRequest struct {
	LearnFromAcceptedRecommendations *bool `json:"learn_from_accepted_recommendations,omitempty"`

	// WebhookSecret is the v0.89.31 (#650) per-connection inbound
	// webhook HMAC secret. Pointer semantics:
	//   - nil               → leave the column untouched (no-op).
	//   - "" (empty string) → clear the column (fall back to the
	//                         env-var global SQUADRON_GITHUB_WEBHOOK_SECRET
	//                         at HMAC-verify time).
	//   - any other value   → sealed via credstore.SealWebhookSecret
	//                         and stored. Replacing a previous secret
	//                         simply overwrites; rotation is just a
	//                         second PATCH with a new value.
	//
	// The response never echoes the plaintext (or the sealed bytes)
	// back — the response carries only {connection_id, status}.
	WebhookSecret *string `json:"webhook_secret,omitempty"`
}

// HandleIaCGitHubUpdateConnection — PATCH
// /api/v1/iac/github/connections/:id. v0.89.28 (#643 slice 1).
//
// Mutates non-credential, non-placement-map fields on the connection.
// Slice 1 limits the surface to LearnFromAcceptedRecommendations;
// future slices append fields here without breaking the contract.
// Returns 404 when no such connection exists. Does NOT emit an audit
// event in slice 1 — the flag flip is operator-visible UI state, not
// a state change that an SOC consumer needs to correlate against
// (per spec §8 the audit events list does not include the toggle).
func (h *IaCGitHubHandlers) HandleIaCGitHubUpdateConnection(c *gin.Context) {
	connectionID := strings.TrimSpace(c.Param("id"))
	if connectionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}

	var req iacGitHubUpdateConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message: "Request body could not be parsed as JSON.",
		}})
		return
	}

	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "IaCStoreNotWired",
			Message: "Squadron's IaC connection substrate isn't configured.",
		}})
		return
	}

	// Pre-check 404 cleanly when the row doesn't exist.
	if _, err := h.store.Get(c.Request.Context(), connectionID); err != nil {
		if errors.Is(err, iacconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No IaC connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("iac github update connection: store get failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "IaCStoreReadFailed",
			Message: "Squadron could not read the IaC connection. The error has been logged.",
		}})
		return
	}

	// Partial-update semantics: nil pointer → leave the column
	// untouched. An explicit value (true or false) flips it.
	if req.LearnFromAcceptedRecommendations != nil {
		if err := h.store.UpdateLearnFromAcceptedRecommendations(
			c.Request.Context(), connectionID, *req.LearnFromAcceptedRecommendations,
		); err != nil {
			if errors.Is(err, iacconnstore.ErrConnectionNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
					Code:    "ConnectionNotFound",
					Message: "The IaC connection was deleted while the update was in flight.",
				}})
				return
			}
			if h.logger != nil {
				h.logger.Error("iac github update connection: write failed", zap.Error(err))
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
				Code:    "IaCStoreWriteFailed",
				Message: "Squadron could not persist the connection update.",
			}})
			return
		}
	}

	// v0.89.31 (#650) — per-connection webhook secret. Same partial-
	// update posture as the learn flag:
	//   nil          → leave untouched.
	//   "" (empty)   → clear the column (fall back to env-var global).
	//   non-empty    → seal via credstore.SealWebhookSecret and store.
	// The plaintext bytes never leave this branch — no log line, no
	// error message, no audit payload carries them.
	if req.WebhookSecret != nil {
		if *req.WebhookSecret == "" {
			if err := h.store.SetWebhookSecret(c.Request.Context(), connectionID, nil); err != nil {
				if errors.Is(err, iacconnstore.ErrConnectionNotFound) {
					c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
						Code:    "ConnectionNotFound",
						Message: "The IaC connection was deleted while the update was in flight.",
					}})
					return
				}
				if h.logger != nil {
					h.logger.Error("iac github update connection: webhook secret clear failed", zap.Error(err))
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
					Code:    "IaCStoreWriteFailed",
					Message: "Squadron could not clear the per-connection webhook secret.",
				}})
				return
			}
		} else {
			if h.credKey == nil {
				if h.logger != nil {
					// No token bytes in the log line — the
					// no-token-in-errors invariant covers webhook
					// secrets the same way it covers PATs.
					h.logger.Error("iac github update connection: credstore key not wired; cannot seal webhook secret")
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
					Code:    "CredstoreKeyNotWired",
					Message: "Squadron's secrets key isn't configured. Verify SQUADRON_SECRETS_KEY is set and retry.",
				}})
				return
			}
			sealed, err := credstore.SealWebhookSecret(h.credKey, []byte(*req.WebhookSecret))
			if err != nil {
				if h.logger != nil {
					// Critically: do NOT include the plaintext in
					// the log line. The seal failure mode is a
					// crypto-substrate failure, not a secret-shape
					// failure — surface the error type only.
					h.logger.Error("iac github update connection: webhook secret seal failed", zap.Error(err))
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
					Code:    "WebhookSecretEncryptFailed",
					Message: "Squadron could not encrypt the per-connection webhook secret. The error has been logged.",
				}})
				return
			}
			if err := h.store.SetWebhookSecret(c.Request.Context(), connectionID, sealed); err != nil {
				if errors.Is(err, iacconnstore.ErrConnectionNotFound) {
					c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
						Code:    "ConnectionNotFound",
						Message: "The IaC connection was deleted while the update was in flight.",
					}})
					return
				}
				if h.logger != nil {
					h.logger.Error("iac github update connection: webhook secret write failed", zap.Error(err))
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
					Code:    "IaCStoreWriteFailed",
					Message: "Squadron could not persist the per-connection webhook secret.",
				}})
				return
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"connection_id": connectionID,
		"status":        "updated",
	})
}

// iacGitHubOpenPRRequest is the Recommendations-tab Open-PR payload.
// Mirrors the design doc §5: scan_id + step_idx pin down which
// recommendation; resource_kind keys the placement-map lookup;
// snippet is the proposer-emitted Terraform; proposer_reasoning is
// the overall narrative; affected_resources is the list rendered in
// the PR body.
//
// v0.89.28 (#643 slice 1) adds Region alongside AccountID so the branch
// name can carry both segments — the discovery proposer's accepted-
// recommendations lookup is scoped by (connection_id, account_id,
// region) and the only way that scope round-trips from the original
// recommendation through the PR merge is via the branch name.
type iacGitHubOpenPRRequest struct {
	ScanID            string   `json:"scan_id"`
	StepIdx           int      `json:"step_idx"`
	ResourceKind      string   `json:"resource_kind"`
	Snippet           string   `json:"snippet"`
	ProposerReasoning string   `json:"proposer_reasoning"`
	AffectedResources []string `json:"affected_resources"`
	AccountID         string   `json:"account_id"`
	Region            string   `json:"region"`

	// HCLPatch — v0.89.12 #628 Stream 29 (slice 2) — structured
	// per-attribute edit description for patch_existing kinds. When
	// present AND the file parses AND the target resource address
	// resolves cleanly, the handler routes to the HCL-aware merge
	// path (clean drop-in PR; no manual-merge label). When nil OR
	// the merge fails for any reason, the handler falls back to the
	// slice-1.5 append-only path with the manual-merge label — the
	// recommendation is never lost.
	//
	// Empty / nil on new_file kinds (the disposition path doesn't
	// take a patch input) and on patch_existing recommendations
	// produced by a pre-v0.89.12 proposer prompt.
	HCLPatch *hclpatch.Patch `json:"hcl_patch,omitempty"`

	// RecommendationID — v0.89.43 (#663 Stream 61, slice 1 chunk 2)
	// — the proposer-emitted ID for this recommendation. Stamped
	// onto the check-run summary's View-in-Squadron deep link and
	// onto the durable iac_recommendation_verdicts row the chunk-2
	// follow-up writes via SetCheckRunForRecommendation. Optional:
	// when empty the link omits the anchor and the storage write is
	// skipped (the live check run on GitHub still creates).
	RecommendationID string `json:"recommendation_id,omitempty"`

	// VerdictExamplesUsedByState — v0.89.43 (#663 Stream 61, slice 1
	// chunk 2) — the per-state bucket map carried forward from the
	// discovery_proposal.created audit event (chunk 6 of #531 slice
	// 2, v0.89.37). The discovery proposer's
	// verdict_examples_used_by_state shape feeds the check-run
	// summary's "Verdict learning context" section verbatim.
	// Optional: empty / nil triggers the cold-start path inside
	// checkrunprompt.ComposeCreateSummary — the entire learning-
	// context section is omitted.
	VerdictExamplesUsedByState map[string][]string `json:"verdict_examples_used_by_state,omitempty"`
}

// iacGitHubOpenPRResponse is the success-path body.
//
// v0.89.11 (#626 Stream 27) — slice 1.5 — extends the response with
// the per-PR disposition. For "new_file" PRs, FilePath is the newly
// CREATED sibling file (squadron_<resource_kind>.tf in the placement
// file's directory) and ManualMergeRequired is false. For
// "patch_existing" PRs, FilePath is the placement file Squadron
// appended to and ManualMergeRequired is true — the UI's success
// card surfaces the same "[needs manual merge]" badge the PR title
// carries.
type iacGitHubOpenPRResponse struct {
	PRNumber            int    `json:"pr_number"`
	PRURL               string `json:"pr_url"`
	Branch              string `json:"branch"`
	CommitSHA           string `json:"commit_sha"`
	FilePath            string `json:"file_path"`
	RepoFullName        string `json:"repo_full_name"`
	Disposition         string `json:"disposition"`
	ManualMergeRequired bool   `json:"manual_merge_required"`

	// DispositionActual — v0.89.12 #628 Stream 29 (slice 2) —
	// refines Disposition with the path the handler actually took:
	//
	//   - "new_file" — slice-1.5 sibling-file write (unchanged).
	//   - "patch_existing_hcl_merged" — slice-2 HCL-aware merge
	//     completed cleanly; PR has no manual-merge label.
	//   - "patch_existing_fell_back_to_append" — slice-2 fallback
	//     to slice-1.5 append-only behavior after the HCL merge
	//     refused (parse error, resource not found, etc).
	//
	// The UI reads this to render the green "HCL-merged" checkmark
	// vs the amber "Needs manual merge" badge on the success card.
	// Empty on pre-v0.89.12 server responses.
	DispositionActual string `json:"disposition_actual,omitempty"`

	// LifecycleIgnored — v0.89.12 — true when the HCL merger
	// detected lifecycle.ignore_changes on the target resource
	// referencing a patched attribute. Surfaces as a PR-body
	// warning. Always false on the new_file and fallback paths.
	LifecycleIgnored bool `json:"lifecycle_ignored,omitempty"`

	// HCLPatchFailureReason — v0.89.12 — set only when
	// DispositionActual = "patch_existing_fell_back_to_append".
	// One of: "parse_error", "resource_not_found",
	// "ambiguous_resource", "unknown_op", "invalid_value_type",
	// "other". The UI's success card surfaces this so the operator
	// understands WHY the manual-merge banner is back.
	HCLPatchFailureReason string `json:"hcl_patch_failure_reason,omitempty"`
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
	//
	// v0.89.28 (#643 slice 1) — the branch shape now encodes the
	// recommendation_kind + account_id + region so the v0.89.23
	// webhook receiver can fill those fields onto the
	// recommendation.pr_merged audit payload. The discovery proposer's
	// accepted-examples lookup uses (connection_id, account_id, region)
	// as the scope tuple; only the branch name can round-trip the
	// scan-time context all the way through the merge.
	//
	// New shape: <prefix>/<kind>/<account_id>/<region>/<short_id>
	// Old shape: <prefix>-<scan>-<step> (slice-1 webhook tests already
	// use the slash variant; this release finishes the transition).
	// When account_id OR region is missing the handler falls back to
	// the kind-only shape <prefix>/<kind>/<short_id> so a caller that
	// hasn't yet plumbed the scan context can still open a PR.
	scanIDShort := shortScanID(req.ScanID)
	prefix := conn.BranchPrefix
	if prefix == "" {
		prefix = iacconnstore.DefaultBranchPrefix
	}
	// Normalize prefix to end in "/" so the resulting branch uses
	// path-separator semantics consistent with the webhook parser.
	prefixSlash := prefix
	if !strings.HasSuffix(prefixSlash, "/") {
		prefixSlash = prefix + "/"
	}
	shortID := fmt.Sprintf("%s-%d", scanIDShort, req.StepIdx)
	branchKind := strings.TrimSpace(req.ResourceKind)
	branchAccount := strings.TrimSpace(req.AccountID)
	branchRegion := strings.TrimSpace(req.Region)
	var branchName string
	switch {
	case branchKind != "" && branchAccount != "" && branchRegion != "":
		branchName = fmt.Sprintf("%s%s/%s/%s/%s",
			prefixSlash, branchKind, branchAccount, branchRegion, shortID)
	case branchKind != "":
		branchName = fmt.Sprintf("%s%s/%s", prefixSlash, branchKind, shortID)
	default:
		// No resource_kind in scope (pre-classify path). Fall back to
		// a kind-less branch so we still emit a deterministic name.
		branchName = fmt.Sprintf("%s%s", prefixSlash, shortID)
	}
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

	// v0.89.11 #626 Stream 27 — slice 1.5 hybrid PR disposition.
	// The disposition for the resource_kind is a STRUCTURAL fact
	// (per internal/iac.KindDispositions); the handler ALWAYS
	// overrides whatever the proposer emitted. Trust-but-verify:
	// the prompt teaches the model, the handler enforces.
	disposition := iac.DispositionFor(req.ResourceKind)

	// targetFilePath is what the PUT writes to and what shows up
	// in the audit payload + the success response. For new_file,
	// it's a sibling file in the placement file's directory; for
	// patch_existing, it's the placement file itself.
	var targetFilePath string
	var existingContent []byte
	var existingFileSHA string

	switch disposition {
	case iac.DispositionNewFile:
		// new_file path: write a sibling file
		// squadron_<resource_kind>.tf in the placement file's
		// directory. The file MUST NOT already exist — if a prior
		// Squadron PR for the same kind already created it (and was
		// merged), the next Open-PR for the same kind would
		// collide. Slice 2 will replace this with HCL-aware merge;
		// slice 1.5 surfaces the collision as
		// SquadronFileAlreadyExists.
		placementDir := path.Dir(placement.FilePath)
		if placementDir == "." {
			placementDir = ""
		}
		targetFilePath = path.Join(placementDir, "squadron_"+req.ResourceKind+".tf")

		// Pre-flight collision check against the default branch.
		// GetFileContent returns ErrFileNotFound → expected (good);
		// nil → file exists → SquadronFileAlreadyExists; other →
		// pass through.
		existingFC, ferr := client.GetFileContent(ctx, owner, repo, targetFilePath, defaultBranch)
		switch {
		case errors.Is(ferr, iacgithub.ErrFileNotFound):
			// Good — no collision.
		case ferr == nil && existingFC != nil:
			he := &scanner.HumanizedError{
				Code: "SquadronFileAlreadyExists",
				Message: fmt.Sprintf(
					"A previous Squadron PR already created %s in this repo. The proposer's snippet for this scan would conflict. Close the existing file's last open PR or merge it, then re-scan.",
					targetFilePath,
				),
				SuggestedStep: "open-pr",
			}
			h.emitPROpenFailed(c.Request.Context(), conn, &req, he, "", 0)
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": he})
			return
		default:
			he := humanizeGitHubErrorForOpenPR(ferr, conn.RepoFullName)
			h.emitPROpenFailed(c.Request.Context(), conn, &req, he, "", 0)
			c.JSON(statusForGitHubError(ferr), gin.H{"error": he})
			return
		}
		// Leave existingContent / existingFileSHA empty — PUT is a
		// create.

	case iac.DispositionPatchExisting:
		// patch_existing path: by slice 2 the handler attempts
		// HCL-aware merging when the proposer emitted a structured
		// hcl_patch payload AND the file parses cleanly. The
		// fallback in every error path is the slice-1.5 append-
		// only behavior — the operator never loses a recommendation.
		// The actual path taken is captured in dispositionActual
		// and threaded into the audit payload + the PR body banner.
		targetFilePath = placement.FilePath
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

	// Step 5: build the file content + decide on the actual
	// disposition path. v0.89.12 #628 Stream 29 (slice 2) tracks
	// the actual path taken with the dispositionActual /
	// hclPatchFailureReason / lifecycleIgnored / ignoredAttrPath
	// locals so they can be threaded into the audit payload, the
	// PR body banner, and the response envelope below.
	var finalContent []byte
	dispositionActual := disposition // refined for patch_existing below
	var hclPatchFailureReason string
	var lifecycleIgnored bool
	var ignoredAttrPath string
	switch disposition {
	case iac.DispositionNewFile:
		finalContent = buildNewFileContent(req.ResourceKind, scanIDShort, []byte(req.Snippet))
	case iac.DispositionPatchExisting:
		// Slice 2: try HCL-aware merge first when the proposer
		// emitted a structured patch. Any failure → fall back to
		// the slice-1.5 append-only behavior. The fallback
		// preserves the v0.89.11 invariant: a recommendation is
		// never dropped because the parser refused.
		if req.HCLPatch != nil {
			merged, applyResult, mergeErr := hclpatch.ApplyPatch(existingContent, req.HCLPatch)
			if mergeErr == nil {
				finalContent = merged
				dispositionActual = dispositionPatchExistingHCLMerged
				if applyResult != nil && applyResult.LifecycleIgnoresPatchedAttr {
					lifecycleIgnored = true
					ignoredAttrPath = applyResult.IgnoredAttrPath
				}
			} else {
				// Slice-2 fallback. Stamp the failure reason for
				// the audit payload + the operator-visible banner.
				dispositionActual = dispositionPatchExistingFellBackToAppend
				hclPatchFailureReason = hclPatchFailureReasonOf(mergeErr)
				if h.logger != nil {
					h.logger.Warn("iac github open-pr: HCL merge fell back to append",
						zap.String("resource_kind", req.ResourceKind),
						zap.String("reason", hclPatchFailureReason),
						zap.Error(mergeErr),
					)
				}
				finalContent = appendSnippetWithTrailingNewline(existingContent, []byte(req.Snippet))
			}
		} else {
			// No structured patch — slice-1.5 era recommendation
			// (or a proposer prompt that didn't emit one). Same
			// fallback path as a merge failure, with a distinct
			// audit reason so an auditor can tell them apart.
			dispositionActual = dispositionPatchExistingFellBackToAppend
			hclPatchFailureReason = "no_patch_emitted"
			finalContent = appendSnippetWithTrailingNewline(existingContent, []byte(req.Snippet))
		}
	}

	// Step 6: PUT file content to new branch.
	commitMsg := fmt.Sprintf("Squadron: instrument %s for scan %s step %d", req.ResourceKind, scanIDShort, req.StepIdx)
	putRes, err := client.PutFileContent(ctx, iacgithub.PutFileOptions{
		Owner:   owner,
		Repo:    repo,
		Path:    targetFilePath,
		Branch:  branchName,
		Content: finalContent,
		Message: commitMsg,
		FileSHA: existingFileSHA,
	})
	if err != nil {
		// ErrFileAlreadyExists on the new_file path is a TOCTOU
		// surfacing of the same collision the pre-flight check
		// would have caught — surface as SquadronFileAlreadyExists.
		// On patch_existing the sentinel is unreachable (FileSHA
		// is set, so the create-branch decoder doesn't fire it),
		// but the fallback statusForGitHubError handles it.
		if errors.Is(err, iacgithub.ErrFileAlreadyExists) && disposition == iac.DispositionNewFile {
			he := &scanner.HumanizedError{
				Code: "SquadronFileAlreadyExists",
				Message: fmt.Sprintf(
					"A previous Squadron PR already created %s in this repo. The proposer's snippet for this scan would conflict. Close the existing file's last open PR or merge it, then re-scan.",
					targetFilePath,
				),
				SuggestedStep: "open-pr",
			}
			h.emitPROpenFailed(c.Request.Context(), conn, &req, he, branchName, 0)
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": he})
			return
		}
		he := humanizeGitHubErrorForOpenPR(err, conn.RepoFullName)
		h.emitPROpenFailed(c.Request.Context(), conn, &req, he, branchName, 0)
		c.JSON(statusForGitHubError(err), gin.H{"error": he})
		return
	}

	// Step 7: open PR. patch_existing PRs that fell back to
	// append-only carry a "[needs manual merge]" title prefix + a
	// loud merge-warning section. patch_existing PRs that the HCL
	// merger handled cleanly carry NEITHER — the absence of the
	// banner IS the slice-2 signal that the PR is merge-clean.
	prTitle := buildPRTitle(req.ResourceKind, len(req.AffectedResources), scanIDShort)
	if dispositionActual == dispositionPatchExistingFellBackToAppend {
		prTitle = "[needs manual merge] " + prTitle
	}
	prBody := buildPRBody(buildPRBodyOptions{
		Reasoning:             req.ProposerReasoning,
		Snippet:               req.Snippet,
		Affected:              req.AffectedResources,
		Disposition:           disposition,
		DispositionActual:     dispositionActual,
		TargetFilePath:        targetFilePath,
		LifecycleIgnored:      lifecycleIgnored,
		IgnoredAttrPath:       ignoredAttrPath,
		HCLPatchFailureReason: hclPatchFailureReason,
	})
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

	// Step 8: labels per design doc §7, plus
	// "squadron/needs-manual-merge" only when the actual path was
	// the slice-1.5 append-only fallback. Successful slice-2 HCL
	// merges drop the label entirely so operator's auto-merge
	// tools see the PR as the same shape as a new_file PR.
	labels := []string{"squadron", "squadron/" + req.ResourceKind}
	if dispositionActual == dispositionPatchExistingFellBackToAppend {
		labels = append(labels, "squadron/needs-manual-merge")
	}
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
	//
	// v0.89.11 (#626 Stream 27): payload gains `disposition` and
	// `manual_merge_required`. For new_file the `created_file_path`
	// names the sibling file Squadron wrote; `file_path` always
	// names the file the PR commit touches (same value as
	// created_file_path on new_file, the placement file on
	// patch_existing) so existing humanizers and SIEM forwarders
	// that key off `file_path` keep working.
	// v0.89.12 #628 Stream 29: manualMergeRequired tracks the
	// ACTUAL path the handler took, not the structural disposition.
	// A successful slice-2 HCL merge drops the marker; a fallback
	// keeps it. The UI's success card uses this signal to render
	// either the green checkmark or the amber banner.
	manualMergeRequired := dispositionActual == dispositionPatchExistingFellBackToAppend
	if h.auditService != nil {
		payload := map[string]any{
			"scan_id":               req.ScanID,
			"step_idx":              req.StepIdx,
			"resource_kind":         req.ResourceKind,
			"repo_full_name":        conn.RepoFullName,
			"pr_number":             pr.Number,
			"pr_url":                pr.HTMLURL,
			"branch":                branchName,
			"commit_sha":            putRes.CommitSHA,
			"file_path":             targetFilePath,
			"disposition":           disposition,
			"disposition_actual":    dispositionActual,
			"manual_merge_required": manualMergeRequired,
			"actor":                 services.AuditActorSystem,
			"recorded_at":           time.Now().UTC(),
		}
		if lifecycleIgnored {
			payload["lifecycle_ignored"] = true
			if ignoredAttrPath != "" {
				payload["lifecycle_ignored_attr"] = ignoredAttrPath
			}
		}
		if hclPatchFailureReason != "" {
			payload["hcl_patch_failure_reason"] = hclPatchFailureReason
		}
		if disposition == iac.DispositionNewFile {
			payload["created_file_path"] = targetFilePath
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

	// v0.89.43 (#663 Stream 61, slice 1 chunk 2 of the GitHub Checks
	// API back-signal arc). Follow up on the recommendation.pr_opened
	// emit by creating a GitHub check run on the PR's head commit
	// with the verdict-learning summary. Fail-open per design doc §5:
	// any error inside the helper emits iac.check_run.failed and
	// returns — the PR open and its audit event have already
	// completed, the check run is value-add. The helper short-circuits
	// silently when h.checksClient is unwired (operator hasn't enabled
	// the integration yet).
	h.emitCheckRunForOpenedPR(c.Request.Context(), checkRunOpenedPRArgs{
		Connection:                 conn,
		Request:                    &req,
		PRURL:                      pr.HTMLURL,
		HeadSHA:                    putRes.CommitSHA,
		Owner:                      owner,
		Repo:                       repo,
		PAT:                        creds.Token,
		VerdictExamplesUsedByState: req.VerdictExamplesUsedByState,
	})

	c.JSON(http.StatusOK, iacGitHubOpenPRResponse{
		PRNumber:              pr.Number,
		PRURL:                 pr.HTMLURL,
		Branch:                branchName,
		CommitSHA:             putRes.CommitSHA,
		FilePath:              targetFilePath,
		RepoFullName:          conn.RepoFullName,
		Disposition:           disposition,
		ManualMergeRequired:   manualMergeRequired,
		DispositionActual:     dispositionActual,
		LifecycleIgnored:      lifecycleIgnored,
		HCLPatchFailureReason: hclPatchFailureReason,
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
	// v0.89.11 #626 Stream 27: stamp the disposition Squadron WOULD
	// HAVE used so an auditor reading a failed event can correlate
	// against the success-event payload schema. Empty resource_kind
	// → empty disposition (the failure pre-dates the structural
	// lookup).
	if req.ResourceKind != "" {
		payload["disposition"] = iac.DispositionFor(req.ResourceKind)
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
	case errors.Is(err, iacgithub.ErrFileAlreadyExists):
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

// Disposition-actual constants for v0.89.12 #628 Stream 29 (slice 2).
// These refine the structural Disposition with the path the handler
// actually took at PR open time:
//
//   - dispositionPatchExistingHCLMerged — slice-2 HCL-aware merge
//     completed cleanly. PR body has no manual-merge banner; PR
//     labels do not include squadron/needs-manual-merge.
//   - dispositionPatchExistingFellBackToAppend — slice-2 fallback
//     to the slice-1.5 append-only path after the HCL merger
//     refused (parse error, resource not found, unknown op,
//     invalid value type, or no patch emitted by the proposer).
//     PR body carries the slice-1.5 manual-merge banner; the
//     manual-merge label is applied.
const (
	dispositionPatchExistingHCLMerged        = "patch_existing_hcl_merged"
	dispositionPatchExistingFellBackToAppend = "patch_existing_fell_back_to_append"
)

// hclPatchFailureReasonOf maps the hclpatch sentinel errors to a
// stable audit string. The handler keys off these in the
// recommendation.pr_opened payload's `hcl_patch_failure_reason`
// field so an auditor can correlate the fallback against the
// hclpatch error class without parsing the wrapped error text.
func hclPatchFailureReasonOf(err error) string {
	switch {
	case errors.Is(err, hclpatch.ErrParseFailed):
		return "parse_error"
	case errors.Is(err, hclpatch.ErrResourceNotFound):
		return "resource_not_found"
	case errors.Is(err, hclpatch.ErrAmbiguousResource):
		return "ambiguous_resource"
	case errors.Is(err, hclpatch.ErrUnknownOp):
		return "unknown_op"
	case errors.Is(err, hclpatch.ErrInvalidValueType):
		return "invalid_value_type"
	default:
		return "other"
	}
}

// buildPRBodyOptions bundles the v0.89.12 (#628 Stream 29) inputs
// to buildPRBody. The struct was promoted from positional args
// when slice 2 added the lifecycle-warning + fallback-reason
// surfaces; further additions land here without churning every
// caller.
type buildPRBodyOptions struct {
	Reasoning             string
	Snippet               string
	Affected              []string
	Disposition           string // structural: "new_file" | "patch_existing"
	DispositionActual     string // refined: see the disposition_actual constants above
	TargetFilePath        string
	LifecycleIgnored      bool
	IgnoredAttrPath       string
	HCLPatchFailureReason string
}

// buildPRBody assembles the design-doc §7 PR body: proposer
// reasoning, affected resources, the snippet in a fenced block, the
// disposition-aware merge-posture note, and the orchestrator-not-
// executor footer.
//
// v0.89.12 (#628 Stream 29) — slice 2 — the banner choice now
// depends on DispositionActual rather than the structural
// Disposition:
//
//   - new_file — clean drop-in note (unchanged from slice 1.5).
//   - patch_existing_hcl_merged — green "Clean HCL merge" note;
//     no duplicate-resource warning. When LifecycleIgnored is true
//     a SECOND note warns the operator that terraform apply will
//     no-op the patched attribute because of an existing
//     lifecycle.ignore_changes entry.
//   - patch_existing_fell_back_to_append — the slice-1.5 loud
//     "manual merge required" warning, plus an extra line naming
//     the HCLPatchFailureReason so the operator understands why
//     the slice-2 path didn't run.
func buildPRBody(opts buildPRBodyOptions) string {
	var b strings.Builder

	switch opts.DispositionActual {
	case dispositionPatchExistingFellBackToAppend:
		b.WriteString("> [!WARNING]\n")
		b.WriteString("> **Manual merge required.** Squadron appended this snippet to ")
		b.WriteString("`" + opts.TargetFilePath + "` because it modifies an existing Terraform ")
		b.WriteString("resource block AND the slice-2 HCL-aware merge could not run ")
		b.WriteString("(reason: `" + opts.HCLPatchFailureReason + "`). ")
		b.WriteString("Merging this PR as-is will cause `terraform plan` to fail with a ")
		b.WriteString("duplicate-resource error. Hand-integrate the highlighted attributes ")
		b.WriteString("into the existing resource block, then merge. See ")
		b.WriteString("`docs/proposals/603-slice-2-hcl-aware-merging.md` §9 for the ")
		b.WriteString("fallback-reason catalog.\n\n")
	case dispositionPatchExistingHCLMerged:
		b.WriteString("> [!NOTE]\n")
		b.WriteString("> **Clean HCL merge.** Squadron parsed `" + opts.TargetFilePath + "` ")
		b.WriteString("and applied the proposer's structured patch to the existing ")
		b.WriteString("resource block in place (slice 2 disposition: ")
		b.WriteString("`patch_existing_hcl_merged`). No duplicate-resource conflict; ")
		b.WriteString("review the diff and merge.\n\n")
		if opts.LifecycleIgnored {
			b.WriteString("> [!WARNING]\n")
			b.WriteString("> **`lifecycle.ignore_changes` affects this patch.** The target resource ")
			b.WriteString("carries `lifecycle { ignore_changes = [...] }` referencing the ")
			b.WriteString("`" + opts.IgnoredAttrPath + "` attribute. The file change Squadron wrote ")
			b.WriteString("is real, but `terraform apply` will no-op the corresponding update ")
			b.WriteString("until the operator removes that entry from `ignore_changes`. If the ")
			b.WriteString("ignore entry was a staging step for a prior change, this is ")
			b.WriteString("expected; otherwise consider editing the resource's lifecycle block ")
			b.WriteString("alongside this merge.\n\n")
		}
	case iac.DispositionNewFile:
		b.WriteString("> [!NOTE]\n")
		b.WriteString("> **Clean drop-in.** Squadron created a new sibling file ")
		b.WriteString("`" + opts.TargetFilePath + "` because this snippet defines a net-new top-level ")
		b.WriteString("Terraform resource (disposition: `new_file`). No manual ")
		b.WriteString("integration is needed — review the file and merge.\n\n")
	}

	if strings.TrimSpace(opts.Reasoning) != "" {
		b.WriteString("## Why\n\n")
		b.WriteString(strings.TrimSpace(opts.Reasoning))
		b.WriteString("\n\n")
	}
	if len(opts.Affected) > 0 {
		b.WriteString("## Affected resources\n\n")
		for _, r := range opts.Affected {
			b.WriteString("- ")
			b.WriteString(r)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("## Proposed change\n\n")
	b.WriteString("```hcl\n")
	b.WriteString(strings.TrimRight(opts.Snippet, "\n"))
	b.WriteString("\n```\n\n")
	b.WriteString("---\n")
	b.WriteString("Authored by Squadron as orchestrator, not executor: this PR awaits ")
	b.WriteString("your review and is gated by your branch protection. Your CI runs ")
	b.WriteString("`terraform plan` / `apply` on merge; Squadron does not run Terraform. ")
	b.WriteString("Squadron will not push to this branch again.\n")
	return b.String()
}

// buildNewFileContent assembles the body of a new sibling-file write
// for the v0.89.11 (#626 Stream 27) new_file disposition. The file
// opens with a two-line provenance header comment so a reviewer
// opening the file alone (outside the PR view) sees which Squadron
// scan + resource_kind produced it. Below the header, the snippet
// is written verbatim with exactly one trailing newline.
func buildNewFileContent(resourceKind, scanIDShort string, snippet []byte) []byte {
	header := fmt.Sprintf(
		"# Authored by Squadron (resource_kind=%s, scan=%s).\n# This file is a net-new Terraform module Squadron created; merge-clean.\n\n",
		resourceKind, scanIDShort,
	)
	out := make([]byte, 0, len(header)+len(snippet)+1)
	out = append(out, header...)
	trimmed := bytesTrimRightNewlines(snippet)
	out = append(out, trimmed...)
	out = append(out, '\n')
	return out
}
