// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
)

func otelInjectPRRegisterFor(h *IaCGitHubHandlers) func(r *gin.Engine) {
	return func(r *gin.Engine) {
		r.POST("/api/v1/iac/github/connections", h.HandleIaCGitHubSaveConnection)
		r.POST("/api/v1/iac/github/connections/:id/otel-inject-pr", h.HandleIaCGitHubOTelInjectPR)
	}
}

func TestHandleIaCGitHubOTelInjectPR_OpensPR(t *testing.T) {
	placeholder := "receivers:\n  otlp:\n    protocols:\n      grpc:\n" +
		"exporters:\n  otlp:\n    endpoint: REPLACE_WITH_SQUADRON_OTLP\n" +
		"service:\n  pipelines:\n    traces:\n      receivers: [otlp]\n      exporters: [otlp]\n"
	mc := &mockGitHubClient{
		repoResp:      &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
		branchSHAResp: "tip",
		fileResp: map[string]*iacgithub.FileContent{
			"modules/standalone-collector/collector.yaml": {
				Path: "modules/standalone-collector/collector.yaml", SHA: "sha1",
				DecodedContent: []byte(placeholder),
			},
		},
		openPRResp: &iacgithub.PullRequest{Number: 88, HTMLURL: "https://github.com/octo/widgets/pull/88"},
	}
	h, _ := newTestIaCHandlers(t, mc, &discoveryRecordingAudit{})
	connID := saveConnectionForOpenPR(t, h,
		`{"provider":"aws","resource_kind":"otel-collector","file_path":"modules/standalone-collector/collector.yaml"}`)
	body := `{"config_path":"modules/standalone-collector/collector.yaml","endpoint":"squadron:4317","insecure":true}`
	w := doIaCRequest(t, http.MethodPost,
		"/api/v1/iac/github/connections/"+connID+"/otel-inject-pr",
		otelInjectPRRegisterFor(h), body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/pull/88") {
		t.Errorf("missing PR url: %s", w.Body.String())
	}
	if len(mc.putFileCalls) != 1 {
		t.Fatalf("putFile calls = %d, want 1", len(mc.putFileCalls))
	}
	put := mc.putFileCalls[0]
	if put.Path != "modules/standalone-collector/collector.yaml" {
		t.Errorf("path = %q", put.Path)
	}
	for _, want := range []string{"otlp/squadron", "endpoint: squadron:4317"} {
		if !strings.Contains(string(put.Content), want) {
			t.Errorf("content missing %q:\n%s", want, put.Content)
		}
	}
	// operator's own placeholder exporter must survive.
	if !strings.Contains(string(put.Content), "REPLACE_WITH_SQUADRON_OTLP") {
		t.Errorf("operator's otlp exporter was clobbered:\n%s", put.Content)
	}
}

func TestHandleIaCGitHubOTelInjectPR_Idempotent_NoPR(t *testing.T) {
	already := "exporters:\n  otlp/squadron:\n    endpoint: squadron:4317\n    tls:\n      insecure: true\n" +
		"service:\n  pipelines:\n    traces:\n      exporters: [otlp/squadron]\n" +
		"    metrics:\n      exporters: [otlp/squadron]\n    logs:\n      exporters: [otlp/squadron]\n"
	mc := &mockGitHubClient{
		repoResp:      &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
		branchSHAResp: "tip",
		fileResp: map[string]*iacgithub.FileContent{
			"c.yaml": {Path: "c.yaml", SHA: "s", DecodedContent: []byte(already)},
		},
	}
	h, _ := newTestIaCHandlers(t, mc, &discoveryRecordingAudit{})
	connID := saveConnectionForOpenPR(t, h,
		`{"provider":"aws","resource_kind":"otel-collector","file_path":"c.yaml"}`)
	body := `{"config_path":"c.yaml","endpoint":"squadron:4317","insecure":true}`
	w := doIaCRequest(t, http.MethodPost,
		"/api/v1/iac/github/connections/"+connID+"/otel-inject-pr",
		otelInjectPRRegisterFor(h), body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "already_wired") {
		t.Errorf("expected already_wired no-op: %s", w.Body.String())
	}
	if len(mc.putFileCalls) != 0 {
		t.Errorf("no PUT expected when already wired, got %d", len(mc.putFileCalls))
	}
}

func TestHandleIaCGitHubOTelInjectPR_ConfigNotFound_404(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp:      &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
		branchSHAResp: "tip",
		// no fileResp -> GetFileContent returns ErrFileNotFound.
	}
	h, _ := newTestIaCHandlers(t, mc, &discoveryRecordingAudit{})
	connID := saveConnectionForOpenPR(t, h,
		`{"provider":"aws","resource_kind":"otel-collector","file_path":"c.yaml"}`)
	body := `{"config_path":"missing.yaml","endpoint":"squadron:4317"}`
	w := doIaCRequest(t, http.MethodPost,
		"/api/v1/iac/github/connections/"+connID+"/otel-inject-pr",
		otelInjectPRRegisterFor(h), body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleIaCGitHubOTelInjectPR_MissingFields_400(t *testing.T) {
	mc := &mockGitHubClient{repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"}}
	h, _ := newTestIaCHandlers(t, mc, &discoveryRecordingAudit{})
	connID := saveConnectionForOpenPR(t, h,
		`{"provider":"aws","resource_kind":"otel-collector","file_path":"c.yaml"}`)
	// missing endpoint
	w := doIaCRequest(t, http.MethodPost,
		"/api/v1/iac/github/connections/"+connID+"/otel-inject-pr",
		otelInjectPRRegisterFor(h), `{"config_path":"c.yaml"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
