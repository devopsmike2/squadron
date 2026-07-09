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

func TestNewServiceNowPublisher_RequiresFields(t *testing.T) {
	cases := []ServiceNowConfig{
		{Username: "svc", Password: "p"},
		{Instance: "acme", Password: "p"},
		{Instance: "acme", Username: "svc"},
	}
	for _, c := range cases {
		_, err := NewServiceNowPublisher(c)
		assert.Error(t, err)
	}
}

func TestServiceNowPublisher_HappyPath(t *testing.T) {
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
			"result": {
				"sys_id": "abc123def456",
				"number": "INC0010023"
			}
		}`))
	}))
	defer srv.Close()

	p, err := NewServiceNowPublisher(ServiceNowConfig{
		Instance:       "acme",
		Username:       "ops@example.com",
		Password:       "servicenow_token_demo",
		DefaultUrgency: "1",
		DefaultImpact:  "2",
		BaseURL:        srv.URL,
		HTTPClient:     srv.Client(),
	})
	require.NoError(t, err)

	id, url, err := p.Publish(context.Background(), &types.IncidentDraft{
		Title:        "Restart nginx on web canary, success",
		BodyMarkdown: "First paragraph of context.\n\nSecond paragraph with the resolution.",
	})
	require.NoError(t, err)
	assert.Equal(t, "INC0010023", id)
	assert.Equal(t, srv.URL+"/nav_to.do?uri=incident.do?sys_id=abc123def456", url)

	// Wire details: path, Basic auth shape, headers.
	assert.Equal(t, "/api/now/table/incident", capturedPath)
	expectedCreds := base64.StdEncoding.EncodeToString([]byte("ops@example.com:servicenow_token_demo"))
	assert.Equal(t, "Basic "+expectedCreds, capturedAuth)
	assert.Equal(t, "application/json", capturedAccept)
	assert.Equal(t, "application/json", capturedContentType)

	// Field mapping: draft title -> short_description, markdown body
	// -> description, and the configured urgency / impact defaults.
	assert.Equal(t, "Restart nginx on web canary, success", capturedBody["short_description"])
	assert.Equal(t, "First paragraph of context.\n\nSecond paragraph with the resolution.", capturedBody["description"])
	assert.Equal(t, "1", capturedBody["urgency"])
	assert.Equal(t, "2", capturedBody["impact"])
}

func TestServiceNowPublisher_DefaultUrgencyAndImpact(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"result":{"sys_id":"s1","number":"INC0000001"}}`))
	}))
	defer srv.Close()

	p, err := NewServiceNowPublisher(ServiceNowConfig{
		Instance: "acme", Username: "svc", Password: "p",
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{
		Title: "t", BodyMarkdown: "body",
	})
	require.NoError(t, err)

	assert.Equal(t, "3", capturedBody["urgency"])
	assert.Equal(t, "3", capturedBody["impact"])
}

func TestServiceNowPublisher_AuthFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{
			"error": {"message": "User Not Authenticated", "detail": "Required to provide Auth information"},
			"status": "failure"
		}`))
	}))
	defer srv.Close()

	p, err := NewServiceNowPublisher(ServiceNowConfig{
		Instance: "acme", Username: "svc", Password: "bad",
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.Contains(t, err.Error(), "User Not Authenticated")
}

func TestServiceNowPublisher_ValidationFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{
			"error": {"message": "Invalid table", "detail": "incident_bogus"},
			"status": "failure"
		}`))
	}))
	defer srv.Close()

	p, err := NewServiceNowPublisher(ServiceNowConfig{
		Instance: "acme", Username: "svc", Password: "p",
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, strings.ToLower(err.Error()), "invalid table")
}

func TestServiceNowPublisher_ServerErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`upstream timeout`))
	}))
	defer srv.Close()

	p, err := NewServiceNowPublisher(ServiceNowConfig{
		Instance: "acme", Username: "svc", Password: "p",
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Contains(t, err.Error(), "upstream timeout")
}

func TestServiceNowPublisher_MissingNumberErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"result":{"sys_id":"s1","number":""}}`))
	}))
	defer srv.Close()

	p, err := NewServiceNowPublisher(ServiceNowConfig{
		Instance: "acme", Username: "svc", Password: "p",
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing incident number")
}

func TestServiceNowPublisher_EmptyTitleErrors(t *testing.T) {
	p, err := NewServiceNowPublisher(ServiceNowConfig{
		Instance: "acme", Username: "svc", Password: "p",
	})
	require.NoError(t, err)
	_, _, err = p.Publish(context.Background(), &types.IncidentDraft{Title: ""})
	require.Error(t, err)
}

func TestServiceNowPublisher_DerivesBaseURLFromInstance(t *testing.T) {
	p, err := NewServiceNowPublisher(ServiceNowConfig{
		Instance: "acme", Username: "svc", Password: "p",
	})
	require.NoError(t, err)
	assert.Equal(t, "https://acme.service-now.com", p.cfg.BaseURL)
}
