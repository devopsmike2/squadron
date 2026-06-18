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

func TestNewLinearPublisher_RequiresFields(t *testing.T) {
	cases := []LinearConfig{
		{TeamID: "tid"},
		{APIKey: "lin_api_xxx"},
	}
	for _, c := range cases {
		_, err := NewLinearPublisher(c)
		assert.Error(t, err)
	}
}

func TestLinearPublisher_HappyPath(t *testing.T) {
	var capturedPath string
	var capturedAuth string
	var capturedContentType string
	var capturedQuery string
	var capturedInput map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		capturedContentType = r.Header.Get("Content-Type")

		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.Unmarshal(raw, &body)
		capturedQuery = body.Query
		if v, ok := body.Variables["input"].(map[string]interface{}); ok {
			capturedInput = v
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"data": {
				"issueCreate": {
					"success": true,
					"issue": {
						"id": "uuid-here",
						"identifier": "ENG-123",
						"url": "https://linear.app/squadron/issue/ENG-123/restart-nginx"
					}
				}
			}
		}`))
	}))
	defer srv.Close()

	p, err := NewLinearPublisher(LinearConfig{
		APIKey:      "lin_api_demo",
		TeamID:      "team_xyz",
		LabelIDs:    []string{"label_squadron", "label_automation"},
		APIEndpoint: srv.URL,
		HTTPClient:  srv.Client(),
	})
	require.NoError(t, err)

	id, url, err := p.Publish(context.Background(), &types.IncidentDraft{
		Title:        "Restart nginx on web canary, success",
		BodyMarkdown: "# Test\n\nbody",
	})
	require.NoError(t, err)
	assert.Equal(t, "ENG-123", id)
	assert.Equal(t, "https://linear.app/squadron/issue/ENG-123/restart-nginx", url)

	// Wire details: path, auth (no Bearer prefix), GraphQL mutation
	// + variables shape.
	assert.Equal(t, "/", capturedPath)
	assert.Equal(t, "lin_api_demo", capturedAuth)
	assert.Equal(t, "application/json", capturedContentType)
	assert.Contains(t, capturedQuery, "issueCreate")
	assert.Equal(t, "team_xyz", capturedInput["teamId"])
	assert.Equal(t, "Restart nginx on web canary, success", capturedInput["title"])
	assert.Equal(t, "# Test\n\nbody", capturedInput["description"])
	labels, _ := capturedInput["labelIds"].([]interface{})
	require.Len(t, labels, 2)
	assert.Equal(t, "label_squadron", labels[0])
}

func TestLinearPublisher_GraphQLErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"errors": [
				{"message": "Team not found"}
			]
		}`))
	}))
	defer srv.Close()

	p, err := NewLinearPublisher(LinearConfig{
		APIKey: "lin_api_bad", TeamID: "missing",
		APIEndpoint: srv.URL,
		HTTPClient:  srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Team not found")
}

func TestLinearPublisher_SuccessFalseReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"data": {
				"issueCreate": {
					"success": false,
					"issue": {"id": "", "identifier": "", "url": ""}
				}
			}
		}`))
	}))
	defer srv.Close()

	p, err := NewLinearPublisher(LinearConfig{
		APIKey: "lin_api_x", TeamID: "team",
		APIEndpoint: srv.URL,
		HTTPClient:  srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "success=false")
}

func TestLinearPublisher_NonOKReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid API key"}`))
	}))
	defer srv.Close()

	p, err := NewLinearPublisher(LinearConfig{
		APIKey: "lin_api_bad", TeamID: "team",
		APIEndpoint: srv.URL,
		HTTPClient:  srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestLinearPublisher_EmptyTitleErrors(t *testing.T) {
	p, err := NewLinearPublisher(LinearConfig{
		APIKey: "lin_api_x", TeamID: "team",
	})
	require.NoError(t, err)
	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: ""})
	require.Error(t, err)
}
