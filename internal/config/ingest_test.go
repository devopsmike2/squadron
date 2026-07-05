// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"
)

// ADR 0012 §1 — ingest.otlp.tenant_id binds this instance's OTLP ingest to a
// single tenant. Empty (the default) is inert: the worker stamps
// identity.DefaultTenant, so OSS behavior is unchanged.

// --- default: an omitted block leaves the tenant empty (=> DefaultTenant) ---

func TestIngestOTLPTenant_DefaultEmpty(t *testing.T) {
	var cfg Config
	assert.NoError(t, yaml.Unmarshal([]byte("server:\n  http_port: 8080\n"), &cfg))
	assert.Equal(t, "", cfg.Ingest.OTLP.TenantID, "no ingest block => empty tenant (inert; worker stamps default)")
}

// --- explicit tenant binding parses through ---------------------------------

func TestIngestOTLPTenant_ExplicitTenant(t *testing.T) {
	yamlSrc := "ingest:\n  otlp:\n    tenant_id: acme\n"
	var cfg Config
	assert.NoError(t, yaml.Unmarshal([]byte(yamlSrc), &cfg))
	assert.Equal(t, "acme", cfg.Ingest.OTLP.TenantID)
}
