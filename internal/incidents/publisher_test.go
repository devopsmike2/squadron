// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package incidents

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

func TestClipboardPublisher_NoOpReturnsEmpty(t *testing.T) {
	p := NewClipboardPublisher()
	id, url, err := p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.NoError(t, err)
	assert.Empty(t, id)
	assert.Empty(t, url)
	assert.Equal(t, "clipboard", p.Name())
}

func TestNewGitHubIssuesPublisher_RequiresFields(t *testing.T) {
	cases := []GitHubIssuesConfig{
		{Repo: "r", Token: "t"},
		{Owner: "o", Token: "t"},
		{Owner: "o", Repo: "r"},
	}
	for _, c := range cases {
		_, err := NewGitHubIssuesPublisher(c)
		assert.Error(t, err)
	}
}

func TestGitHubIssuesPublisher_HappyPath(t *testing.T) {
	var capturedPath string
	var capturedAuth string
	var capturedAccept string
	var capturedAPIVersion string
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		capturedAccept = r.Header.Get("Accept")
		capturedAPIVersion = r.Header.Get("X-GitHub-Api-Version")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &capturedBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/devopsmike2/squadron/issues/42"}`))
	}))
	defer srv.Close()

	p, err := NewGitHubIssuesPublisher(GitHubIssuesConfig{
		Owner:      "devopsmike2",
		Repo:       "squadron",
		Token:      "ghp_demo",
		Labels:     []string{"squadron", "automation"},
		APIBaseURL: srv.URL,
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	id, url, err := p.Publish(context.Background(), &types.IncidentDraft{
		Title:        "Restart nginx on web canary, success",
		BodyMarkdown: "# Test\n\nbody",
	})
	require.NoError(t, err)
	assert.Equal(t, "42", id)
	assert.Equal(t, "https://github.com/devopsmike2/squadron/issues/42", url)

	// Wire details: path, auth, headers, body payload.
	assert.Equal(t, "/repos/devopsmike2/squadron/issues", capturedPath)
	assert.Equal(t, "Bearer ghp_demo", capturedAuth)
	assert.Equal(t, "application/vnd.github+json", capturedAccept)
	assert.Equal(t, "2022-11-28", capturedAPIVersion)
	assert.Equal(t, "Restart nginx on web canary, success", capturedBody["title"])
	assert.Equal(t, "# Test\n\nbody", capturedBody["body"])
	labels, _ := capturedBody["labels"].([]any)
	require.Len(t, labels, 2)
	assert.Equal(t, "squadron", labels[0])
}

func TestGitHubIssuesPublisher_NonOKReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	p, err := NewGitHubIssuesPublisher(GitHubIssuesConfig{
		Owner: "x", Repo: "y", Token: "bad",
		APIBaseURL: srv.URL,
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.Contains(t, err.Error(), "Bad credentials")
}

func TestGitHubIssuesPublisher_EmptyTitleErrors(t *testing.T) {
	p, err := NewGitHubIssuesPublisher(GitHubIssuesConfig{Owner: "o", Repo: "r", Token: "t"})
	require.NoError(t, err)
	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: ""})
	require.Error(t, err)
}

func TestPublisherRegistry_LookupAndRegister(t *testing.T) {
	r := NewPublisherRegistry()
	assert.NotNil(t, r.Lookup("clipboard"))
	assert.Nil(t, r.Lookup("linear"))

	gh, err := NewGitHubIssuesPublisher(GitHubIssuesConfig{Owner: "o", Repo: "r", Token: "t"})
	require.NoError(t, err)
	r.Register(gh)
	assert.NotNil(t, r.Lookup("github"))
}
