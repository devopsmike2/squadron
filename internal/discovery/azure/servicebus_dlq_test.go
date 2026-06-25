// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// DLQ configuration analysis slice 1 chunk 3 (v0.89.165, #807
// Stream 204) — Azure Service Bus namespace-level honest-framing
// tests per docs/proposals/dlq-configuration-analysis-slice1.md
// §3.2.
//
// The pre-revision design doc §11.9-13 referenced queue-level
// fields (forwardDeadLetteredMessagesTo,
// enableDeadLetteringOnMessageExpiration, maxDeliveryCount). These
// fields sit at the Microsoft.ServiceBus/namespaces/queues ARM
// sub-resource which the slice 1 chunk 3 scanner has not yet
// walked. The chunk 3 revision defers §11.9-13 to a future slice
// that adds the queue walk; this test file pins the namespace-level
// honest-framing axis instead.

// --- Test §3.2.A: namespace-level DLQ axes ALWAYS report scanner-coverage-gap state.

func TestDetectServiceBusDLQ_NamespaceLevel_AlwaysReturnsHonestFramingState(t *testing.T) {
	cases := []struct {
		name string
		ns   armServiceBusNamespace
	}{
		{
			name: "empty namespace",
			ns:   armServiceBusNamespace{},
		},
		{
			name: "premium namespace with SKU",
			ns: armServiceBusNamespace{
				ID:       "/subscriptions/test/resourceGroups/rg/providers/Microsoft.ServiceBus/namespaces/ns-premium",
				Name:     "ns-premium",
				Location: "eastus",
				SKU:      armServiceBusNamespaceSKU{Name: "Premium"},
			},
		},
		{
			name: "standard namespace east region",
			ns: armServiceBusNamespace{
				ID:       "/subscriptions/test/resourceGroups/rg/providers/Microsoft.ServiceBus/namespaces/ns-std",
				Name:     "ns-std",
				Location: "westus",
				SKU:      armServiceBusNamespaceSKU{Name: "Standard"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := detectServiceBusDLQ(tc.ns)
			assert.Equal(t, false, res.HasDLQQueueWalkAvailable,
				"slice 1 §3.2 invariant: namespace-level scanner has NO per-queue DLQ visibility")
			assert.Equal(t, -1, res.RetryCount,
				"absent sentinel — per-queue maxDeliveryCount unreadable from namespace-level walk")
			assert.Equal(t, false, res.RetryCountInBand,
				"band check meaningless without per-queue maxDeliveryCount")
		})
	}
}

// --- Test §3.2.B: applyServiceBusDLQDetail writes the three honest-framing keys.

func TestApplyServiceBusDLQDetail_WritesAllThreeAxisKeys(t *testing.T) {
	ns := armServiceBusNamespace{
		ID:       "/subscriptions/test/resourceGroups/rg/providers/Microsoft.ServiceBus/namespaces/ns",
		Name:     "ns",
		Location: "eastus",
	}
	snap := projectServiceBusNamespace(ns, false, false, false, "", "subscription-1")

	assert.Equal(t, false, snap.Detail["has_dlq_queue_walk_available"])
	assert.Equal(t, -1, snap.Detail["dlq_retry_count"])
	assert.Equal(t, false, snap.Detail["dlq_retry_count_in_band"])
}

// --- Test §3.2.C: cold-start parity — slice-1 + slice-2 keys preserved.
//
// The chunk 3 patch is ADDITIVE only. The namespace-level keys
// the slice-1 chunk + slice-2 chunk wrote (source_type, has_trace,
// has_log, sku) must survive byte-identically. This test pins
// those keys alongside the new chunk 3 DLQ axis keys so a future
// refactor that accidentally drops a pre-existing key triggers a
// regression test failure.

func TestApplyServiceBusDLQDetail_AdditiveOnly_PreservesPriorKeys(t *testing.T) {
	ns := armServiceBusNamespace{
		ID:       "/subscriptions/test/resourceGroups/rg/providers/Microsoft.ServiceBus/namespaces/ns",
		Name:     "ns",
		Location: "eastus",
		SKU:      armServiceBusNamespaceSKU{Name: "Premium"},
	}
	snap := projectServiceBusNamespace(ns, true, true, true, "auth rules preserved", "subscription-1")

	// Slice 1 + slice 2 namespace-level keys preserved
	// byte-identically.
	assert.Equal(t, "namespace", snap.Detail["source_type"])
	assert.Equal(t, true, snap.Detail["has_trace"])
	assert.Equal(t, true, snap.Detail["has_log"])
	assert.Equal(t, "Premium", snap.Detail["sku"])
	assert.True(t, snap.HasTraceAxis, "slice-1 HasTraceAxis preserved")
	assert.True(t, snap.HasLogAxis, "slice-1 HasLogAxis preserved")
	assert.True(t, snap.HasPropagationConfig, "slice-2 HasPropagationConfig preserved")
	assert.Equal(t, []string{"auth rules preserved"}, snap.PropagationNotes,
		"slice-2 PropagationNotes preserved")

	// Slice 1 chunk 3 honest-framing DLQ axis keys also present.
	assert.Equal(t, false, snap.Detail["has_dlq_queue_walk_available"])
	assert.Equal(t, -1, snap.Detail["dlq_retry_count"])
	assert.Equal(t, false, snap.Detail["dlq_retry_count_in_band"])
}
