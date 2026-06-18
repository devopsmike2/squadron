// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package incidents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

func TestNewJiraPublisher_RequiresFields(t *testing.T) {
	cases := []JiraConfig{
		{Email: "e", APIToken: "t", ProjectKey: "P"},
		{BaseURL: "https://acme.atlassian.net", APIToken: "t", ProjectKey: "P"},
		{BaseURL: "https://acme.atlassian.net", Email: "e", ProjectKey: "P"},
		{BaseURL: "https://acme.atlassian.net", Email: "e", APIToken: "t"},
	}
	for _, c := range cases {
		_, err := NewJiraPublisher(c)
		assert.Error(t, err)
	}
}

func TestJiraPublisher_HappyPath(t *testing.T) {
	var capturedPath string
	var capturedAuth string
	var capturedAccept string
	var capturedContentType string
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		capturedAccept = r.Header.Get("Accept")
		capturedContentType = r.Header.Get("Content-Type")

		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"id": "10042",
			"key": "SQUAD-42",
			"self": "https://acme.atlassian.net/rest/api/3/issue/10042"
		}`))
	}))
	defer srv.Close()

	p, err := NewJiraPublisher(JiraConfig{
		BaseURL:    srv.URL,
		Email:      "ops@example.com",
		APIToken:   "atlassian_token_demo",
		ProjectKey: "SQUAD",
		IssueType:  "Incident",
		Labels:     []string{"squadron", "automation"},
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	id, url, err := p.Publish(context.Background(), &types.IncidentDraft{
		Title:        "Restart nginx on web canary, success",
		BodyMarkdown: "First paragraph of context.\n\nSecond paragraph with the resolution.",
	})
	require.NoError(t, err)
	assert.Equal(t, "SQUAD-42", id)
	assert.Equal(t, srv.URL+"/browse/SQUAD-42", url)

	// Wire details: path, Basic auth shape, headers.
	assert.Equal(t, "/rest/api/3/issue", capturedPath)
	expectedCreds := base64.StdEncoding.EncodeToString([]byte("ops@example.com:atlassian_token_demo"))
	assert.Equal(t, "Basic "+expectedCreds, capturedAuth)
	assert.Equal(t, "application/json", capturedAccept)
	assert.Equal(t, "application/json", capturedContentType)

	// Fields shape: project, summary, issuetype, labels at the top
	// level under "fields", plus a description that is a minimal
	// ADF document split into two paragraph nodes (one per
	// markdown chunk).
	fields, _ := capturedBody["fields"].(map[string]any)
	require.NotNil(t, fields)
	project, _ := fields["project"].(map[string]any)
	assert.Equal(t, "SQUAD", project["key"])
	assert.Equal(t, "Restart nginx on web canary, success", fields["summary"])
	issueType, _ := fields["issuetype"].(map[string]any)
	assert.Equal(t, "Incident", issueType["name"])
	labels, _ := fields["labels"].([]any)
	require.Len(t, labels, 2)
	assert.Equal(t, "squadron", labels[0])

	description, _ := fields["description"].(map[string]any)
	require.NotNil(t, description)
	assert.Equal(t, "doc", description["type"])
	paragraphs, _ := description["content"].([]any)
	require.Len(t, paragraphs, 2,
		"two markdown chunks should produce two ADF paragraph nodes")
	first, _ := paragraphs[0].(map[string]any)
	assert.Equal(t, "paragraph", first["type"])
	firstContent, _ := first["content"].([]any)
	require.Len(t, firstContent, 1)
	firstText, _ := firstContent[0].(map[string]any)
	assert.Equal(t, "text", firstText["type"])
	assert.Equal(t, "First paragraph of context.", firstText["text"])
}

func TestJiraPublisher_DefaultIssueTypeIsTask(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"1","key":"X-1","self":""}`))
	}))
	defer srv.Close()

	p, err := NewJiraPublisher(JiraConfig{
		BaseURL: srv.URL, Email: "e", APIToken: "t", ProjectKey: "X",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{
		Title: "t", BodyMarkdown: "body",
	})
	require.NoError(t, err)

	fields, _ := capturedBody["fields"].(map[string]any)
	issueType, _ := fields["issuetype"].(map[string]any)
	assert.Equal(t, "Task", issueType["name"])
}

func TestJiraPublisher_AuthFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{
			"errorMessages": ["Login required for the supplied email."],
			"errors": {}
		}`))
	}))
	defer srv.Close()

	p, err := NewJiraPublisher(JiraConfig{
		BaseURL: srv.URL, Email: "e", APIToken: "bad", ProjectKey: "X",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.Contains(t, err.Error(), "Login required")
}

func TestJiraPublisher_ValidationFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{
			"errorMessages": [],
			"errors": {
				"project": "project is required"
			}
		}`))
	}))
	defer srv.Close()

	p, err := NewJiraPublisher(JiraConfig{
		BaseURL: srv.URL, Email: "e", APIToken: "t", ProjectKey: "MISSING",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, strings.ToLower(err.Error()), "project")
}

func TestJiraPublisher_ServerErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`upstream timeout`))
	}))
	defer srv.Close()

	p, err := NewJiraPublisher(JiraConfig{
		BaseURL: srv.URL, Email: "e", APIToken: "t", ProjectKey: "X",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Contains(t, err.Error(), "upstream timeout")
}

func TestJiraPublisher_EmptyTitleErrors(t *testing.T) {
	p, err := NewJiraPublisher(JiraConfig{
		BaseURL: "https://acme.atlassian.net", Email: "e", APIToken: "t", ProjectKey: "X",
	})
	require.NoError(t, err)
	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: ""})
	require.Error(t, err)
}
