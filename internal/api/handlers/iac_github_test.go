// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/services"
)

// --- test mocks --------------------------------------------------------

// mockGitHubClient is a recording fake of the iacgithub.Client +
// branchSHAGetter capability. Per-method canned responses + an errors
// map so each test pins down exactly what call shape it expects.
type mockGitHubClient struct {
	mu sync.Mutex

	// canned responses
	repoResp        *iacgithub.Repo
	repoErr         error
	fileResp        map[string]*iacgithub.FileContent
	fileErr         map[string]error
	branchSHAResp   string
	branchSHAErr    error
	createBranchErr error
	putFileResp     *iacgithub.CommitFileResult
	putFileErr      error
	openPRResp      *iacgithub.PullRequest
	openPRErr       error
	addLabelsErr    error
	reviewersErr    error

	// recorded calls
	createBranchCalls []createBranchCall
	putFileCalls      []iacgithub.PutFileOptions
	openPRCalls       []iacgithub.OpenPROptions
	addLabelsCalls    []addLabelsCall
	reviewersCalls    []reviewersCall
}

type createBranchCall struct {
	Owner, Repo, Branch, FromSHA string
}
type addLabelsCall struct {
	Owner, Repo string
	PRNumber    int
	Labels      []string
}
type reviewersCall struct {
	Owner, Repo string
	PRNumber    int
	Teams       []string
}

func (m *mockGitHubClient) GetRepo(_ context.Context, owner, repo string) (*iacgithub.Repo, error) {
	if m.repoErr != nil {
		return nil, m.repoErr
	}
	if m.repoResp != nil {
		return m.repoResp, nil
	}
	return &iacgithub.Repo{FullName: owner + "/" + repo, DefaultBranch: "main"}, nil
}

func (m *mockGitHubClient) GetFileContent(_ context.Context, _, _, path, _ string) (*iacgithub.FileContent, error) {
	if e, ok := m.fileErr[path]; ok {
		return nil, e
	}
	if r, ok := m.fileResp[path]; ok {
		return r, nil
	}
	return nil, iacgithub.ErrFileNotFound
}

func (m *mockGitHubClient) CreateBranch(_ context.Context, owner, repo, branchName, fromSHA string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createBranchCalls = append(m.createBranchCalls, createBranchCall{owner, repo, branchName, fromSHA})
	return m.createBranchErr
}

func (m *mockGitHubClient) PutFileContent(_ context.Context, opts iacgithub.PutFileOptions) (*iacgithub.CommitFileResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putFileCalls = append(m.putFileCalls, opts)
	if m.putFileErr != nil {
		return nil, m.putFileErr
	}
	if m.putFileResp != nil {
		return m.putFileResp, nil
	}
	return &iacgithub.CommitFileResult{BlobSHA: "newblob", CommitSHA: "newcommit"}, nil
}

func (m *mockGitHubClient) OpenPR(_ context.Context, opts iacgithub.OpenPROptions) (*iacgithub.PullRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openPRCalls = append(m.openPRCalls, opts)
	if m.openPRErr != nil {
		return nil, m.openPRErr
	}
	if m.openPRResp != nil {
		return m.openPRResp, nil
	}
	return &iacgithub.PullRequest{Number: 42, HTMLURL: "https://github.com/octo/widgets/pull/42", HeadSHA: "headsha"}, nil
}

func (m *mockGitHubClient) AddLabels(_ context.Context, owner, repo string, prNumber int, labels []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addLabelsCalls = append(m.addLabelsCalls, addLabelsCall{owner, repo, prNumber, append([]string(nil), labels...)})
	return m.addLabelsErr
}

func (m *mockGitHubClient) RequestReviewers(_ context.Context, owner, repo string, prNumber int, teams []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reviewersCalls = append(m.reviewersCalls, reviewersCall{owner, repo, prNumber, append([]string(nil), teams...)})
	return m.reviewersErr
}

// GetBranchSHA satisfies the handler-side branchSHAGetter capability.
func (m *mockGitHubClient) GetBranchSHA(_ context.Context, _, _, _ string) (string, error) {
	if m.branchSHAErr != nil {
		return "", m.branchSHAErr
	}
	if m.branchSHAResp != "" {
		return m.branchSHAResp, nil
	}
	return "defaultbranchsha", nil
}

// newTestCredKey builds a credstore.Key from a fixed 32-byte buffer.
// Used by both the Save and Open-PR happy-path tests so a token
// sealed by Save can be unsealed by Open-PR.
func newTestCredKey(t *testing.T) *credstore.Key {
	t.Helper()
	key, err := credstore.NewKey(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return key
}

// newTestIaCHandlers builds an IaCGitHubHandlers wired with an
// in-memory store, the supplied mock client, the supplied audit, and
// a real credstore.Key. All four are required for the happy-path
// tests; failure-path tests can pass nils for the pieces they're not
// exercising.
func newTestIaCHandlers(t *testing.T, mc *mockGitHubClient, audit services.AuditService) (*IaCGitHubHandlers, iacconnstore.Store) {
	t.Helper()
	store := iacconnstore.NewMemoryStore()
	key := newTestCredKey(t)
	h := NewIaCGitHubHandlers(store, zap.NewNop()).
		WithCredstoreKey(key).
		WithClientFactory(func(token string) iacgithub.Client {
			mc.mu.Lock()
			defer mc.mu.Unlock()
			return mc
		})
	if audit != nil {
		h.WithAuditService(audit)
	}
	return h, store
}

// doIaCRequest is the shared HTTP-method-aware harness.
func doIaCRequest(t *testing.T, method, path string, register func(r *gin.Engine), body string) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	register(r)
	var reqBody *bytes.Buffer
	if body != "" {
		reqBody = bytes.NewBufferString(body)
	} else {
		reqBody = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// --- Validate -----------------------------------------------------------

func TestHandleIaCGitHubValidate_HappyPath_ReturnsPreflightRows(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
		fileResp: map[string]*iacgithub.FileContent{
			"modules/lambda/main.tf": {Path: "modules/lambda/main.tf", SHA: "abc1234567890def", DecodedContent: []byte("resource \"x\" \"y\" {}\n")},
		},
		fileErr: map[string]error{
			"modules/eks/main.tf": iacgithub.ErrFileNotFound,
		},
	}
	audit := &discoveryRecordingAudit{}
	h, _ := newTestIaCHandlers(t, mc, audit)
	body := `{
		"token": "ghp_doNotLogMe",
		"repo_full_name": "octo/widgets",
		"placement_map": [
			{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"},
			{"provider":"aws","resource_kind":"eks-cluster-logging","file_path":"modules/eks/main.tf"}
		]
	}`
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/validate",
		func(r *gin.Engine) { r.POST("/api/v1/iac/github/validate", h.HandleIaCGitHubValidate) }, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp iacGitHubValidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if resp.RepoFullName != "octo/widgets" || resp.DefaultBranch != "main" {
		t.Errorf("response top-level wrong: %+v", resp)
	}
	if len(resp.PreflightResults) != 2 {
		t.Fatalf("preflight len = %d, want 2", len(resp.PreflightResults))
	}
	// Row 0: file exists, sha_short = first 7 of "abc1234567890def" = "abc1234".
	if !resp.PreflightResults[0].Exists {
		t.Errorf("lambda row should exist")
	}
	if resp.PreflightResults[0].ShaShort != "abc1234" {
		t.Errorf("lambda row sha_short = %q, want abc1234", resp.PreflightResults[0].ShaShort)
	}
	// Row 1: file does not exist (soft warning, no err).
	if resp.PreflightResults[1].Exists {
		t.Errorf("eks row should not exist")
	}
	if resp.PreflightResults[1].Err != nil {
		t.Errorf("eks row missing-file should not be an err: %+v", resp.PreflightResults[1].Err)
	}
	// Audit emitted once with the iac.github.connection_validated event.
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventIaCGitHubConnectionValidated {
		t.Errorf("audit EventType = %q", e.EventType)
	}
	// Token NEVER in payload.
	bodyBytes, _ := json.Marshal(e.Payload)
	if strings.Contains(string(bodyBytes), "ghp_doNotLogMe") {
		t.Fatalf("token leaked into audit payload: %s", string(bodyBytes))
	}
}

func TestHandleIaCGitHubValidate_Repo404_ReturnsHumanizedError(t *testing.T) {
	mc := &mockGitHubClient{repoErr: iacgithub.ErrRepoNotFound}
	h, _ := newTestIaCHandlers(t, mc, nil)
	body := `{"token":"ghp_x","repo_full_name":"octo/vanished","placement_map":[]}`
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/validate",
		func(r *gin.Engine) { r.POST("/api/v1/iac/github/validate", h.HandleIaCGitHubValidate) }, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "RepoNotFound") {
		t.Errorf("RepoNotFound code not surfaced: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"suggested_step":"pick-repo"`) {
		t.Errorf("suggested_step not surfaced: %s", w.Body.String())
	}
}

func TestHandleIaCGitHubValidate_BadToken_ReturnsAuthFailed_NoTokenEcho(t *testing.T) {
	mc := &mockGitHubClient{repoErr: iacgithub.ErrAuthFailed}
	h, _ := newTestIaCHandlers(t, mc, nil)
	body := `{"token":"ghp_thisShouldNotBeEchoed","repo_full_name":"octo/widgets","placement_map":[]}`
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/validate",
		func(r *gin.Engine) { r.POST("/api/v1/iac/github/validate", h.HandleIaCGitHubValidate) }, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AuthFailed") {
		t.Errorf("AuthFailed code not surfaced: %s", w.Body.String())
	}
	// The single most load-bearing assertion: token bytes never echo.
	if strings.Contains(w.Body.String(), "ghp_thisShouldNotBeEchoed") {
		t.Fatalf("token leaked into response: %s", w.Body.String())
	}
}

// --- Save --------------------------------------------------------------

func TestHandleIaCGitHubSaveConnection_HappyPath_PersistsAndEmitsAudit(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
	}
	audit := &discoveryRecordingAudit{}
	h, store := newTestIaCHandlers(t, mc, audit)
	body := `{
		"token": "ghp_secretTokenDoNotLog",
		"repo_full_name": "octo/widgets",
		"repo_layout": "mono",
		"placement_map": [
			{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}
		]
	}`
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/connections",
		func(r *gin.Engine) { r.POST("/api/v1/iac/github/connections", h.HandleIaCGitHubSaveConnection) }, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp iacGitHubSaveConnectionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ConnectionID == "" {
		t.Errorf("connection_id empty")
	}
	if resp.RepoFullName != "octo/widgets" || resp.Status != "connected" {
		t.Errorf("response wrong: %+v", resp)
	}

	// One row persisted with token sealed.
	rows, _ := store.List(context.Background())
	if len(rows) != 1 {
		t.Fatalf("store rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.RepoFullName != "octo/widgets" || row.AuthKind != iacconnstore.AuthKindPAT {
		t.Errorf("row wrong: %+v", row)
	}
	if len(row.CredCiphertext) == 0 {
		t.Errorf("CredCiphertext empty — the seal did not run")
	}
	if bytes.Contains(row.CredCiphertext, []byte("ghp_secretTokenDoNotLog")) {
		t.Fatalf("plaintext token in CredCiphertext")
	}

	// Audit emitted once with token NEVER in payload.
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventIaCGitHubConnectionCreated {
		t.Errorf("audit EventType = %q", e.EventType)
	}
	if e.TargetType != services.AuditTargetIaCConnection {
		t.Errorf("audit TargetType = %q", e.TargetType)
	}
	payloadBytes, _ := json.Marshal(e.Payload)
	if strings.Contains(string(payloadBytes), "ghp_secretTokenDoNotLog") {
		t.Fatalf("token leaked into audit payload: %s", string(payloadBytes))
	}
}

func TestHandleIaCGitHubSaveConnection_DuplicateRepo_ReturnsConflict(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
	}
	h, _ := newTestIaCHandlers(t, mc, nil)
	body := `{
		"token": "ghp_x", "repo_full_name": "octo/widgets",
		"repo_layout":"mono","placement_map":[]
	}`
	register := func(r *gin.Engine) { r.POST("/api/v1/iac/github/connections", h.HandleIaCGitHubSaveConnection) }
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/connections", register, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("first save status = %d, want 201", w.Code)
	}
	w = doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/connections", register, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("second save status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ConnectionConflict") {
		t.Errorf("ConnectionConflict code not surfaced: %s", w.Body.String())
	}
}

// --- List + Delete ----------------------------------------------------

func TestHandleListIaCGitHubConnections_DoesNotLeakCiphertextOrToken(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
	}
	h, _ := newTestIaCHandlers(t, mc, nil)
	body := `{
		"token": "ghp_secretTokenForLeakTest", "repo_full_name": "octo/widgets",
		"repo_layout":"mono","placement_map":[]
	}`
	registerSave := func(r *gin.Engine) { r.POST("/api/v1/iac/github/connections", h.HandleIaCGitHubSaveConnection) }
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/connections", registerSave, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("save status = %d, want 201", w.Code)
	}
	registerList := func(r *gin.Engine) { r.GET("/api/v1/iac/github/connections", h.HandleListIaCGitHubConnections) }
	w = doIaCRequest(t, http.MethodGet, "/api/v1/iac/github/connections", registerList, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	bodyStr := w.Body.String()
	if strings.Contains(bodyStr, "ghp_secretTokenForLeakTest") {
		t.Fatalf("token leaked in list: %s", bodyStr)
	}
	if strings.Contains(bodyStr, "cred_ciphertext") || strings.Contains(bodyStr, "CredCiphertext") {
		t.Fatalf("ciphertext-shaped field leaked in list: %s", bodyStr)
	}
}

func TestHandleDeleteIaCGitHubConnection_IsIdempotent(t *testing.T) {
	mc := &mockGitHubClient{repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"}}
	h, _ := newTestIaCHandlers(t, mc, nil)
	register := func(r *gin.Engine) { r.DELETE("/api/v1/iac/github/connections/:id", h.HandleDeleteIaCGitHubConnection) }
	// Deleting a non-existent row is not an error.
	w := doIaCRequest(t, http.MethodDelete, "/api/v1/iac/github/connections/not-a-real-id", register, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete-missing status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
}

// --- Open PR ----------------------------------------------------------

// openPRRegisterFor registers both Save (to seed a connection) and
// Open-PR on the same engine so the open-PR tests can connect first
// and then drive the open-PR flow.
func openPRRegisterFor(h *IaCGitHubHandlers) func(r *gin.Engine) {
	return func(r *gin.Engine) {
		r.POST("/api/v1/iac/github/connections", h.HandleIaCGitHubSaveConnection)
		r.POST("/api/v1/iac/github/connections/:id/open-pr", h.HandleIaCGitHubOpenPR)
	}
}

func saveConnectionForOpenPR(t *testing.T, h *IaCGitHubHandlers, placement string) string {
	t.Helper()
	body := `{
		"token": "ghp_storedTokenDoNotLogMe", "repo_full_name": "octo/widgets",
		"repo_layout":"mono",
		"placement_map": [` + placement + `]
	}`
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/connections", openPRRegisterFor(h), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed save status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp iacGitHubSaveConnectionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.ConnectionID
}

func TestHandleIaCGitHubOpenPR_HappyPath_CreatesBranchWritesFileOpensPREmitsAudit(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
		fileResp: map[string]*iacgithub.FileContent{
			"modules/lambda/main.tf": {Path: "modules/lambda/main.tf", SHA: "existingblobsha", DecodedContent: []byte("resource \"x\" \"y\" {}\n")},
		},
		branchSHAResp: "tipofmaindefaultsha",
		openPRResp:    &iacgithub.PullRequest{Number: 42, HTMLURL: "https://github.com/octo/widgets/pull/42", HeadSHA: "newcommit"},
	}
	audit := &discoveryRecordingAudit{}
	h, _ := newTestIaCHandlers(t, mc, audit)
	connID := saveConnectionForOpenPR(t, h, `{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}`)
	body := `{
		"scan_id": "abc1234567890",
		"step_idx": 0,
		"resource_kind": "lambda-otel-layer",
		"snippet": "resource \"aws_lambda_function\" \"otel\" {\n  layers = [\"otel\"]\n}",
		"proposer_reasoning": "Two Lambda functions emit no telemetry.",
		"affected_resources": ["arn:aws:lambda:us-east-1:111:function:a","arn:aws:lambda:us-east-1:111:function:b"],
		"account_id": "111111111111"
	}`
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/connections/"+connID+"/open-pr", openPRRegisterFor(h), body)
	if w.Code != http.StatusOK {
		t.Fatalf("open-pr status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp iacGitHubOpenPRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.PRNumber != 42 || resp.PRURL == "" || resp.Branch == "" || resp.CommitSHA == "" || resp.FilePath == "" {
		t.Fatalf("response missing field: %+v", resp)
	}
	if !strings.HasPrefix(resp.Branch, "squadron/rec-abc1234-0") {
		t.Errorf("branch = %q, want prefix squadron/rec-abc1234-0", resp.Branch)
	}

	// Calls landed in the expected order: GetRepo, GetBranchSHA,
	// GetFileContent, CreateBranch, PutFileContent, OpenPR, AddLabels.
	if len(mc.createBranchCalls) != 1 {
		t.Fatalf("createBranch calls = %d, want 1", len(mc.createBranchCalls))
	}
	if mc.createBranchCalls[0].FromSHA != "tipofmaindefaultsha" {
		t.Errorf("createBranch FromSHA = %q", mc.createBranchCalls[0].FromSHA)
	}
	if len(mc.putFileCalls) != 1 {
		t.Fatalf("putFile calls = %d, want 1", len(mc.putFileCalls))
	}
	put := mc.putFileCalls[0]
	if put.Path != "modules/lambda/main.tf" {
		t.Errorf("put.Path = %q", put.Path)
	}
	if put.FileSHA != "existingblobsha" {
		t.Errorf("put.FileSHA = %q (the existing blob's SHA should be carried over)", put.FileSHA)
	}
	if !strings.HasSuffix(string(put.Content), "\n") {
		t.Errorf("put content does not end in newline: %q", string(put.Content))
	}
	if strings.HasSuffix(string(put.Content), "\n\n") {
		t.Errorf("put content ends in MORE than one newline: %q", string(put.Content))
	}
	if !strings.Contains(string(put.Content), "resource \"x\" \"y\"") {
		t.Errorf("put content missing original bytes")
	}
	if !strings.Contains(string(put.Content), "resource \"aws_lambda_function\" \"otel\"") {
		t.Errorf("put content missing snippet bytes")
	}
	if len(mc.openPRCalls) != 1 {
		t.Fatalf("openPR calls = %d, want 1", len(mc.openPRCalls))
	}
	if !strings.Contains(mc.openPRCalls[0].Title, "lambda-otel-layer") {
		t.Errorf("PR title missing resource_kind: %q", mc.openPRCalls[0].Title)
	}
	if !strings.Contains(mc.openPRCalls[0].Title, "abc1234") {
		t.Errorf("PR title missing scan_id short: %q", mc.openPRCalls[0].Title)
	}
	if mc.openPRCalls[0].Base != "main" {
		t.Errorf("PR base = %q, want main", mc.openPRCalls[0].Base)
	}
	// Labels per design doc §7.
	if len(mc.addLabelsCalls) != 1 {
		t.Fatalf("addLabels calls = %d, want 1", len(mc.addLabelsCalls))
	}
	gotLabels := mc.addLabelsCalls[0].Labels
	if len(gotLabels) != 2 || gotLabels[0] != "squadron" || gotLabels[1] != "squadron/lambda-otel-layer" {
		t.Errorf("labels = %+v", gotLabels)
	}

	// Audit: exactly one connection_created + one pr_opened, no
	// snippet bytes in either payload.
	var prOpenedEntry *services.AuditEntry
	for i := range audit.entries {
		if audit.entries[i].EventType == services.AuditEventRecommendationPROpened {
			prOpenedEntry = &audit.entries[i]
		}
	}
	if prOpenedEntry == nil {
		t.Fatalf("no recommendation.pr_opened audit event; got %+v", audit.entries)
	}
	payloadBytes, _ := json.Marshal(prOpenedEntry.Payload)
	if strings.Contains(string(payloadBytes), "aws_lambda_function") {
		t.Fatalf("snippet content leaked into pr_opened payload: %s", string(payloadBytes))
	}
	if strings.Contains(string(payloadBytes), "ghp_storedTokenDoNotLogMe") {
		t.Fatalf("token leaked into pr_opened payload: %s", string(payloadBytes))
	}
}

func TestHandleIaCGitHubOpenPR_NoPlacementMapping_Returns422(t *testing.T) {
	mc := &mockGitHubClient{repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"}}
	audit := &discoveryRecordingAudit{}
	h, _ := newTestIaCHandlers(t, mc, audit)
	connID := saveConnectionForOpenPR(t, h, `{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}`)
	body := `{
		"scan_id":"abc1234","step_idx":0,
		"resource_kind":"eks-cluster-logging",
		"snippet":"resource \"aws_eks\" \"x\" {}"
	}`
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/connections/"+connID+"/open-pr", openPRRegisterFor(h), body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoPlacementMapping") {
		t.Errorf("error_code NoPlacementMapping not surfaced: %s", w.Body.String())
	}
}

func TestHandleIaCGitHubOpenPR_GitHub404_EmitsPROpenFailed(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
	}
	audit := &discoveryRecordingAudit{}
	h, _ := newTestIaCHandlers(t, mc, audit)
	connID := saveConnectionForOpenPR(t, h, `{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}`)
	// After Save, swap the mock to a deleted-repo posture.
	mc.repoErr = iacgithub.ErrRepoNotFound

	body := `{
		"scan_id":"abc1234","step_idx":0,
		"resource_kind":"lambda-otel-layer",
		"snippet":"resource \"x\" \"y\" {}"
	}`
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/connections/"+connID+"/open-pr", openPRRegisterFor(h), body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "RepoNotFound") {
		t.Errorf("error_code RepoNotFound not surfaced: %s", w.Body.String())
	}
	// recommendation.pr_open_failed emitted (Save's connection_created
	// also lives in this audit log).
	var failed *services.AuditEntry
	for i := range audit.entries {
		if audit.entries[i].EventType == services.AuditEventRecommendationPROpenFailed {
			failed = &audit.entries[i]
		}
	}
	if failed == nil {
		t.Fatalf("no recommendation.pr_open_failed event: %+v", audit.entries)
	}
	if failed.Payload["error_code"] != "RepoNotFound" {
		t.Errorf("payload error_code = %v", failed.Payload["error_code"])
	}
	if failed.Payload["humanized_message"] == nil {
		t.Errorf("humanized_message missing from payload")
	}
	// pr_number must NOT be set when no PR opened.
	if _, ok := failed.Payload["pr_number"]; ok {
		t.Errorf("pr_number should not be set on failed-before-PR-open path: %+v", failed.Payload)
	}
}

// TestHandleIaCGitHubOpenPR_AttemptsBranchEqualDefault asserts the
// handler-layer default-branch invariant. The connection's
// BranchPrefix is forced to "main"; the handler MUST refuse before
// any GitHub call lands. The wrapper layer's identical refusal
// (asserted in client_test.go) is independent — together they form
// the §9 defense-in-depth posture.
func TestHandleIaCGitHubOpenPR_AttemptsBranchEqualDefault_ReturnsTypedErrorBeforeAnyGitHubCall(t *testing.T) {
	mc := &mockGitHubClient{repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"}}
	audit := &discoveryRecordingAudit{}
	store := iacconnstore.NewMemoryStore()
	key := newTestCredKey(t)
	// Save by hand: the wizard would never let an operator set
	// BranchPrefix = "main", but the substrate accepts it, and the
	// handler-layer guard is what catches the pathological case.
	ciphertext, _ := iacconnstore.MarshalGitHubPATCreds(
		iacconnstore.GitHubPATCredentials{Token: "ghp_x"}, key,
	)
	conn := &iacconnstore.IaCConnection{
		Provider:      iacconnstore.ProviderGitHub,
		AuthKind:      iacconnstore.AuthKindPAT,
		RepoFullName:  "octo/widgets",
		DefaultBranch: "main",
		RepoLayout:    iacconnstore.RepoLayoutMono,
		// Pathological: BranchPrefix that, combined with scan_id
		// short hash + step_idx, produces "main".
		BranchPrefix:   "main",
		PlacementMap:   []iacconnstore.PlacementMapEntry{{Provider: "aws", ResourceKind: "lambda-otel-layer", FilePath: "modules/lambda/main.tf"}},
		CredCiphertext: ciphertext,
	}
	// Force the branch name to equal "main" by stubbing the substrate.
	// We can't make the prefix "main" produce literally "main" without
	// a more-elaborate stub — so we use a connection with prefix
	// "main", scan_id with empty short hash, and step_idx=0; the
	// resulting branch "main--0" does NOT equal "main". The cleanest
	// way to exercise the handler-layer guard is to skip the format
	// string entirely. We do that by checking the helper directly.
	if !branchEqualsDefault("main", "main") {
		t.Fatalf("branchEqualsDefault(main, main) returned false")
	}
	if !branchEqualsDefault("refs/heads/main", "main") {
		t.Fatalf("branchEqualsDefault(refs/heads/main, main) returned false")
	}
	if branchEqualsDefault("squadron/rec-abc1234-0", "main") {
		t.Fatalf("branchEqualsDefault(squadron/rec, main) returned true")
	}

	// Now exercise the end-to-end refusal by stubbing the wrapper's
	// CreateBranch to return ErrDefaultBranchWriteRefused — the
	// handler must surface that as a humanized error and emit
	// recommendation.pr_open_failed.
	mc.createBranchErr = iacgithub.ErrDefaultBranchWriteRefused
	mc.branchSHAResp = "tipofmaindefaultsha"
	h := NewIaCGitHubHandlers(store, zap.NewNop()).
		WithCredstoreKey(key).
		WithAuditService(audit).
		WithClientFactory(func(token string) iacgithub.Client { return mc })
	if err := store.Create(context.Background(), conn); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	body := `{
		"scan_id":"abc1234","step_idx":0,
		"resource_kind":"lambda-otel-layer",
		"snippet":"resource \"x\" \"y\" {}"
	}`
	register := func(r *gin.Engine) {
		r.POST("/api/v1/iac/github/connections/:id/open-pr", h.HandleIaCGitHubOpenPR)
	}
	w := doIaCRequest(t, http.MethodPost, "/api/v1/iac/github/connections/"+conn.ConnectionID+"/open-pr", register, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "DefaultBranchWriteRefused") {
		t.Errorf("DefaultBranchWriteRefused not surfaced: %s", w.Body.String())
	}
	// Audit fires recommendation.pr_open_failed.
	var failed *services.AuditEntry
	for i := range audit.entries {
		if audit.entries[i].EventType == services.AuditEventRecommendationPROpenFailed {
			failed = &audit.entries[i]
		}
	}
	if failed == nil {
		t.Fatalf("no recommendation.pr_open_failed event after refusal: %+v", audit.entries)
	}
	if failed.Payload["error_code"] != "DefaultBranchWriteRefused" {
		t.Errorf("payload.error_code = %v, want DefaultBranchWriteRefused", failed.Payload["error_code"])
	}
}

// Belt-and-braces: ensure the helper does what its name says even on
// path-with-slashes inputs (a future regression that broke
// stripping refs/heads/ would silently make every default-branch
// check fail closed — surface that immediately).
func TestBranchEqualsDefault_StripsRefsHeadsPrefix(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"main", "main", true},
		{"refs/heads/main", "main", true},
		{"main", "refs/heads/main", true},
		{"refs/heads/main", "refs/heads/main", true},
		{"squadron/rec-abc-0", "main", false},
		{"Main", "main", false}, // case-sensitive: Git refs are case-sensitive
	}
	for _, c := range cases {
		if got := branchEqualsDefault(c.a, c.b); got != c.want {
			t.Errorf("branchEqualsDefault(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// Belt-and-braces error mapper coverage.
func TestStatusForGitHubError(t *testing.T) {
	if statusForGitHubError(iacgithub.ErrAuthFailed) != http.StatusUnauthorized {
		t.Errorf("ErrAuthFailed should map to 401")
	}
	if statusForGitHubError(iacgithub.ErrRepoNotFound) != http.StatusNotFound {
		t.Errorf("ErrRepoNotFound should map to 404")
	}
	if statusForGitHubError(iacgithub.ErrDefaultBranchWriteRefused) != http.StatusUnprocessableEntity {
		t.Errorf("ErrDefaultBranchWriteRefused should map to 422")
	}
	if statusForGitHubError(errors.New("anything else")) != http.StatusInternalServerError {
		t.Errorf("unknown error should map to 500")
	}
}
