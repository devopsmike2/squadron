// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package cliapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_AttachesBearerToken(t *testing.T) {
	// Token is non-empty → every request carries it. Token empty →
	// the header is absent (so dev servers with auth.enabled=false
	// don't get a phantom "Authorization: Bearer " value).
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"recipes":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sqd_abc")
	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/api/v1/anything", nil, nil, nil))
	assert.Equal(t, "Bearer sqd_abc", gotAuth)

	c2 := New(srv.URL, "")
	require.NoError(t, c2.Do(context.Background(), http.MethodGet, "/api/v1/anything", nil, nil, nil))
	assert.Empty(t, gotAuth, "empty token must not produce an Authorization header")
}

func TestClient_DecodesJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","label":"ci-bot"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	var got APIToken
	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/api/v1/auth/tokens/abc", nil, nil, &got))
	assert.Equal(t, "abc", got.ID)
	assert.Equal(t, "ci-bot", got.Label)
}

func TestClient_401ProducesIs401True(t *testing.T) {
	// 401 must roundtrip as *APIError with Status=401 so the CLI can
	// translate it to the "set SQUADRON_TOKEN" message instead of a
	// bare stack trace.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","detail":"missing token"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sqd_bogus")
	err := c.Do(context.Background(), http.MethodGet, "/api/v1/agents", nil, nil, nil)
	require.Error(t, err)
	assert.True(t, Is401(err), "Is401 must report true on 401 errors")
	assert.Contains(t, err.Error(), "unauthorized")
	assert.Contains(t, err.Error(), "missing token")
}

func TestClient_4xxNonJSONStillDecodesAsError(t *testing.T) {
	// Servers can return non-JSON bodies (proxy errors, panic
	// recovery). The client must still return *APIError so the CLI
	// can branch on the status code without parsing the body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>upstream error</html>`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	err := c.Do(context.Background(), http.MethodGet, "/api/v1/agents", nil, nil, nil)
	require.Error(t, err)
	assert.False(t, Is401(err))
	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError; got %T", err)
	assert.Equal(t, http.StatusBadGateway, apiErr.Status)
}

func TestClient_PassesQueryParams(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	q := url.Values{}
	q.Set("group_id", "g1")
	q.Set("state", "in_progress")
	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/api/v1/rollouts", q, nil, nil))
	assert.Equal(t, "g1", gotQuery.Get("group_id"))
	assert.Equal(t, "in_progress", gotQuery.Get("state"))
}

func TestClient_MarshalsBody(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = readBody(r)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(gotBody)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	in := CreateConfigRequest{Name: "n", Content: "k: v"}
	var out CreateConfigRequest
	require.NoError(t, c.Do(context.Background(), http.MethodPost, "/api/v1/configs", nil, in, &out))
	assert.Equal(t, "n", out.Name)
	assert.Contains(t, string(gotBody), `"name":"n"`)
}

// readBody is a tiny helper to slurp a request body in tests without
// importing io/ioutil semantics. Returns ("", nil) for empty body.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 256)
	chunk := make([]byte, 64)
	for {
		n, err := r.Body.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

func TestClient_204NoBody(t *testing.T) {
	// 204 responses with no body must not produce a JSON decode
	// error even when the caller passes an out pointer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	var out APIToken
	err := c.Do(context.Background(), http.MethodPost, "/api/v1/auth/tokens/x/revoke", nil, nil, &out)
	assert.NoError(t, err)
}
