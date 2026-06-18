// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactSecrets_Anthropic(t *testing.T) {
	in := "Authorization: Bearer sk-ant-api03-AAAAAAAAAAAAAAAAAAAA"
	out := RedactSecrets(in)
	assert.NotContains(t, out, "sk-ant-api03")
	assert.Contains(t, out, "<redacted:anthropic_key>")
}

func TestRedactSecrets_GitHub(t *testing.T) {
	in := "token: ghp_1234567890abcdefABCDEF1234567890"
	out := RedactSecrets(in)
	assert.NotContains(t, out, "ghp_1234567890")
	assert.Contains(t, out, "<redacted:github_token>")
}

func TestRedactSecrets_Linear(t *testing.T) {
	in := "lin_api_xxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	out := RedactSecrets(in)
	assert.NotContains(t, out, "lin_api_x")
	assert.Contains(t, out, "<redacted:linear_key>")
}

func TestRedactSecrets_JWT(t *testing.T) {
	// Bare JWT (no Bearer prefix) so the jwt pattern wins. When a
	// JWT shows up inside an Authorization header the bearer_token
	// pattern fires first and the whole header gets redacted as
	// "bearer_token" — that's a separate test, both are valid scrubs.
	in := "session: eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NSJ9.SflKxwRJSMeKKF2QT4f"
	out := RedactSecrets(in)
	assert.NotContains(t, out, "eyJhbGciOiJIUzI1Ni")
	assert.Contains(t, out, "<redacted:jwt>")
}

func TestRedactSecrets_InternalHostnames(t *testing.T) {
	cases := []string{
		"web-canary.example.internal",
		"db01.corp",
		"laptop.local",
	}
	for _, c := range cases {
		out := RedactSecrets(c)
		assert.Contains(t, out, "<redacted:internal_hostname>", "input: %s", c)
	}
}

func TestRedactSecrets_IPv4(t *testing.T) {
	in := "host=10.0.0.42 listener=192.168.1.1"
	out := RedactSecrets(in)
	assert.NotContains(t, out, "10.0.0.42")
	assert.NotContains(t, out, "192.168.1.1")
}

func TestRedactSecrets_Idempotent(t *testing.T) {
	in := "key sk-ant-api03-AAAAAAAAAAAAAAAAAAAAA more text"
	once := RedactSecrets(in)
	twice := RedactSecrets(once)
	assert.Equal(t, once, twice, "second pass should be a no-op")
}

func TestRedactSecrets_EmptyAndPlainText(t *testing.T) {
	assert.Equal(t, "", RedactSecrets(""))
	plain := "Squadron rolled out the web-prod-canary stage at 14:23 UTC."
	assert.Equal(t, plain, RedactSecrets(plain))
}

func TestRedactMap_NestedValues(t *testing.T) {
	in := map[string]any{
		"actor": "operator:alice@example.com",
		"payload": map[string]any{
			"token":   "ghp_1234567890ABCDEFabcdef1234567890",
			"runner":  "fleet01.internal",
			"healthy": true,
			"count":   42,
		},
		"tags": []any{"safe", "192.168.1.1"},
	}
	out := RedactMap(in)
	payload := out["payload"].(map[string]any)
	assert.Equal(t, "<redacted:github_token>", payload["token"])
	assert.Contains(t, payload["runner"].(string), "<redacted:internal_hostname>")
	assert.Equal(t, true, payload["healthy"], "bool passes through")
	assert.Equal(t, 42, payload["count"], "int passes through")
	tags := out["tags"].([]any)
	assert.Equal(t, "safe", tags[0])
	assert.NotEqual(t, "192.168.1.1", tags[1], "ipv4 in slice gets redacted")
}

func TestSummarizeRedactionPlaceholders_CountsByCategory(t *testing.T) {
	s := "x <redacted:github_token> y <redacted:internal_hostname> <redacted:internal_hostname>"
	summary := SummarizeRedactionPlaceholders(s)
	assert.Contains(t, summary, "github_token x1")
	assert.Contains(t, summary, "internal_hostname x2")
}

func TestSummarizeRedactionPlaceholders_EmptyWhenNoMatches(t *testing.T) {
	assert.Empty(t, SummarizeRedactionPlaceholders("no secrets here"))
}

func TestRedactSecrets_PreservesShape(t *testing.T) {
	// Sanity test: a sentence with a credential mid-stream should
	// still parse as English on the other side, just with a
	// placeholder in the credential's spot.
	in := "Action runner registered on host fleet01.internal at 14:23."
	out := RedactSecrets(in)
	assert.True(t, strings.Contains(out, "Action runner registered on host"))
	assert.True(t, strings.Contains(out, "at 14:23"))
}
