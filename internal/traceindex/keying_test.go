// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestComputeResourceKey_CloudResourceID_StrongMatch — tier 1.
// cloud.resource_id wins outright and the key projects verbatim.
// Acceptance test 1 in design doc §11.
func TestComputeResourceKey_CloudResourceID_StrongMatch(t *testing.T) {
	attrs := map[string]string{
		"cloud.provider":    "aws",
		"cloud.account.id":  "123456789012",
		"cloud.resource_id": "arn:aws:ec2:us-east-1:123456789012:instance/i-0abc",
		"service.name":      "checkout",
	}
	key, provider, scope, hint, svc, conf, ok := ComputeResourceKey(attrs)
	assert.True(t, ok)
	assert.Equal(t, "arn:aws:ec2:us-east-1:123456789012:instance/i-0abc", key)
	assert.Equal(t, "aws", provider)
	assert.Equal(t, "123456789012", scope)
	assert.Equal(t, "arn:aws:ec2:us-east-1:123456789012:instance/i-0abc", hint)
	assert.Equal(t, "checkout", svc)
	assert.Equal(t, MatchConfidenceStrong, conf)
}

// TestComputeResourceKey_HostIDAndAccount_StrongMatch — tier 2.
// Verifies the "<provider>:<account>:<host_id>" key shape and the
// strong-confidence flag. Acceptance test 2 in design doc §11.
func TestComputeResourceKey_HostIDAndAccount_StrongMatch(t *testing.T) {
	attrs := map[string]string{
		"cloud.provider":   "aws",
		"cloud.account.id": "12345",
		"host.id":          "i-0abc",
		"service.name":     "checkout",
	}
	key, provider, scope, hint, svc, conf, ok := ComputeResourceKey(attrs)
	assert.True(t, ok)
	assert.Equal(t, "aws:12345:i-0abc", key)
	assert.Equal(t, "aws", provider)
	assert.Equal(t, "12345", scope)
	assert.Equal(t, "", hint, "host.id projection leaves resource_id_hint empty")
	assert.Equal(t, "checkout", svc)
	assert.Equal(t, MatchConfidenceStrong, conf)
}

// TestComputeResourceKey_K8sClusterAndAccount_StrongMatch — tier 3.
func TestComputeResourceKey_K8sClusterAndAccount_StrongMatch(t *testing.T) {
	attrs := map[string]string{
		"cloud.provider":   "gcp",
		"cloud.account.id": "my-project",
		"k8s.cluster.name": "prod-cluster",
		"service.name":     "ingest",
	}
	key, provider, scope, _, svc, conf, ok := ComputeResourceKey(attrs)
	assert.True(t, ok)
	assert.Equal(t, "gcp:my-project:k8s:prod-cluster", key)
	assert.Equal(t, "gcp", provider)
	assert.Equal(t, "my-project", scope)
	assert.Equal(t, "ingest", svc)
	assert.Equal(t, MatchConfidenceStrong, conf)
}

// TestComputeResourceKey_DBSystemAndName_StrongMatch — tier 4.
func TestComputeResourceKey_DBSystemAndName_StrongMatch(t *testing.T) {
	attrs := map[string]string{
		"cloud.provider":   "azure",
		"cloud.account.id": "sub-uuid",
		"db.system":        "postgresql",
		"db.name":          "orders",
		"service.name":     "orders-db",
	}
	key, provider, _, _, svc, conf, ok := ComputeResourceKey(attrs)
	assert.True(t, ok)
	assert.Equal(t, "azure:sub-uuid:db:postgresql:orders", key)
	assert.Equal(t, "azure", provider)
	assert.Equal(t, "orders-db", svc)
	assert.Equal(t, MatchConfidenceStrong, conf)
}

// TestComputeResourceKey_HostNameOnly_WeakMatch — tier 5.
// Acceptance test 3 in design doc §11. No cloud identifiers; the
// match drops to host.name with the weak flag.
func TestComputeResourceKey_HostNameOnly_WeakMatch(t *testing.T) {
	attrs := map[string]string{
		"host.name":    "db-prod",
		"service.name": "orders",
	}
	key, provider, _, _, svc, conf, ok := ComputeResourceKey(attrs)
	assert.True(t, ok)
	assert.Equal(t, "host:db-prod", key)
	assert.Equal(t, "unknown", provider)
	assert.Equal(t, "orders", svc)
	assert.Equal(t, MatchConfidenceWeak, conf)
}

// TestComputeResourceKey_ServiceNameOnly_WeakMatch — tier 6.
// service.name as the last-resort weak identifier.
func TestComputeResourceKey_ServiceNameOnly_WeakMatch(t *testing.T) {
	attrs := map[string]string{
		"service.name": "checkout-worker",
	}
	key, provider, _, _, svc, conf, ok := ComputeResourceKey(attrs)
	assert.True(t, ok)
	assert.Equal(t, "service:checkout-worker", key)
	assert.Equal(t, "unknown", provider)
	assert.Equal(t, "checkout-worker", svc)
	assert.Equal(t, MatchConfidenceWeak, conf)
}

// TestComputeResourceKey_NoIdentifiers_DropsObservation — the bail.
// An attribute map with no usable identifier returns ok=false and
// the caller (Index.Observe) drops silently. Design doc §13 Q4.
func TestComputeResourceKey_NoIdentifiers_DropsObservation(t *testing.T) {
	attrs := map[string]string{
		"cloud.provider": "aws",
		// No host.id / host.name / k8s.cluster.name / db.* / service.name.
	}
	_, _, _, _, _, _, ok := ComputeResourceKey(attrs)
	assert.False(t, ok)

	// Empty map fast-path.
	_, _, _, _, _, _, ok = ComputeResourceKey(nil)
	assert.False(t, ok)
}

// TestComputeResourceKey_PrecedenceOrder — when MULTIPLE identifiers
// are present the higher-priority tier wins. Verifies cloud.resource_id
// (tier 1) over host.id (tier 2) over k8s.cluster.name (tier 3).
func TestComputeResourceKey_PrecedenceOrder(t *testing.T) {
	// Tier 1 over tier 2: resource_id beats host.id.
	attrs := map[string]string{
		"cloud.provider":    "aws",
		"cloud.account.id":  "12345",
		"cloud.resource_id": "arn:aws:ec2:us-east-1:12345:instance/i-0abc",
		"host.id":           "i-0abc",
	}
	key, _, _, _, _, _, ok := ComputeResourceKey(attrs)
	assert.True(t, ok)
	assert.Equal(t, "arn:aws:ec2:us-east-1:12345:instance/i-0abc", key,
		"tier 1 (cloud.resource_id) must win over tier 2 (host.id)")

	// Tier 2 over tier 3: host.id beats k8s.cluster.name.
	attrs = map[string]string{
		"cloud.provider":   "gcp",
		"cloud.account.id": "my-project",
		"host.id":          "gce-vm-123",
		"k8s.cluster.name": "prod",
	}
	key, _, _, _, _, _, ok = ComputeResourceKey(attrs)
	assert.True(t, ok)
	assert.Equal(t, "gcp:my-project:gce-vm-123", key,
		"tier 2 (host.id) must win over tier 3 (k8s.cluster.name)")

	// Tier 5 over tier 6: host.name beats service.name.
	attrs = map[string]string{
		"host.name":    "vm-1",
		"service.name": "checkout",
	}
	key, _, _, _, _, _, ok = ComputeResourceKey(attrs)
	assert.True(t, ok)
	assert.Equal(t, "host:vm-1", key,
		"tier 5 (host.name) must win over tier 6 (service.name)")
}

// TestComputeResourceKey_HostIDMissingAccount_FallsThrough — tier 2
// requires BOTH host.id AND cloud.account.id; without the account
// the chain falls through to a weaker tier rather than producing an
// ambiguous "::host" key.
func TestComputeResourceKey_HostIDMissingAccount_FallsThrough(t *testing.T) {
	attrs := map[string]string{
		"host.id":   "i-0abc",
		"host.name": "vm-1",
	}
	key, _, _, _, _, conf, ok := ComputeResourceKey(attrs)
	assert.True(t, ok)
	assert.Equal(t, "host:vm-1", key, "tier 2 without account falls through to tier 5")
	assert.Equal(t, MatchConfidenceWeak, conf)
}
