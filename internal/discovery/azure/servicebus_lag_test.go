// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Consumer lag detection slice 2 chunk 3 (v0.89.170, #812
// Stream 209) — Azure Service Bus honest-framing tests per design
// doc §3.4 (§3.2-inherited scanner-coverage-gap pattern).

// --- Test §3.4.A: namespace-shape invariance — all namespaces return absent state.

func TestDetectServiceBusLag_AlwaysReturnsHonestFramingAbsentState(t *testing.T) {
	cases := []struct {
		name string
		ns   armServiceBusNamespace
	}{
		{name: "empty namespace", ns: armServiceBusNamespace{}},
		{
			name: "premium namespace",
			ns: armServiceBusNamespace{
				ID:       "/subscriptions/test/resourceGroups/rg/providers/Microsoft.ServiceBus/namespaces/ns-premium",
				Name:     "ns-premium",
				Location: "eastus",
				SKU:      armServiceBusNamespaceSKU{Name: "Premium"},
			},
		},
		{
			name: "standard namespace west region",
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
			res := detectServiceBusLag(tc.ns)
			assert.Equal(t, -1, res.BacklogDepth,
				"slice 2 §3.4 invariant: namespace-level scanner has NO per-queue activeMessageCount visibility")
			assert.Equal(t, false, res.BacklogDepthHigh)
			assert.Equal(t, -1, res.ConsumerSilenceSeconds)
			assert.Equal(t, false, res.ConsumerSilenceHigh)
		})
	}
}

// --- Test §3.4.B: applyServiceBusLagDetail writes all four honest-framing keys.

func TestApplyServiceBusLagDetail_WritesAllFourAxisKeys(t *testing.T) {
	ns := armServiceBusNamespace{
		ID:       "/subscriptions/test/resourceGroups/rg/providers/Microsoft.ServiceBus/namespaces/ns",
		Name:     "ns",
		Location: "eastus",
	}
	snap := projectServiceBusNamespace(ns, false, false, false, "", "subscription-1")

	assert.Equal(t, -1, snap.Detail["lag_backlog_depth"])
	assert.Equal(t, false, snap.Detail["lag_backlog_depth_high"])
	assert.Equal(t, -1, snap.Detail["lag_consumer_silence_seconds"])
	assert.Equal(t, false, snap.Detail["lag_consumer_silence_high"])
}

// --- Test §3.4.C: cold-start parity — slice-1 + slice-2 + slice-1-DLQ keys preserved.
//
// The chunk 3 patch is ADDITIVE only. Every namespace-level key
// written by prior slices must survive byte-identically alongside
// the new chunk 3 lag axis keys.

func TestApplyServiceBusLagDetail_AdditiveOnly_PreservesPriorKeys(t *testing.T) {
	ns := armServiceBusNamespace{
		ID:       "/subscriptions/test/resourceGroups/rg/providers/Microsoft.ServiceBus/namespaces/ns",
		Name:     "ns",
		Location: "eastus",
		SKU:      armServiceBusNamespaceSKU{Name: "Premium"},
	}
	snap := projectServiceBusNamespace(ns, true, true, true, "auth rules preserved", "subscription-1")

	// Slice 1 + slice 2 namespace-level keys preserved.
	assert.Equal(t, "namespace", snap.Detail["source_type"])
	assert.Equal(t, true, snap.Detail["has_trace"])
	assert.Equal(t, true, snap.Detail["has_log"])
	assert.Equal(t, "Premium", snap.Detail["sku"])
	assert.True(t, snap.HasTraceAxis)
	assert.True(t, snap.HasLogAxis)
	assert.True(t, snap.HasPropagationConfig)
	assert.Equal(t, []string{"auth rules preserved"}, snap.PropagationNotes)

	// Slice 1 DLQ honest-framing keys preserved.
	assert.Equal(t, false, snap.Detail["has_dlq_queue_walk_available"])
	assert.Equal(t, -1, snap.Detail["dlq_retry_count"])
	assert.Equal(t, false, snap.Detail["dlq_retry_count_in_band"])

	// Slice 2 lag honest-framing keys also present.
	assert.Equal(t, -1, snap.Detail["lag_backlog_depth"])
	assert.Equal(t, false, snap.Detail["lag_backlog_depth_high"])
	assert.Equal(t, -1, snap.Detail["lag_consumer_silence_seconds"])
	assert.Equal(t, false, snap.Detail["lag_consumer_silence_high"])
}
