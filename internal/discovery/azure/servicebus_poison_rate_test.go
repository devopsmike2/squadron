// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Poison-message rate analysis slice 3 chunk 3 (v0.89.175,
// #817 Stream 214) — Azure Service Bus honest-framing tests
// per design doc §3.3 + §11.4.

func TestDetectServiceBusPoisonRate_AlwaysHonestFramingState(t *testing.T) {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := detectServiceBusPoisonRate(tc.ns)
			assert.Equal(t, -1, res.RatePerHour,
				"slice 3 §3.3 invariant: Azure Monitor substrate integration deferred")
			assert.Equal(t, false, res.HighBand)
		})
	}
}

func TestApplyServiceBusPoisonRateDetail_AdditiveOnly_PreservesPriorKeys(t *testing.T) {
	ns := armServiceBusNamespace{
		ID:       "/subscriptions/test/resourceGroups/rg/providers/Microsoft.ServiceBus/namespaces/ns",
		Name:     "ns",
		Location: "eastus",
		SKU:      armServiceBusNamespaceSKU{Name: "Premium"},
	}
	snap := projectServiceBusNamespace(ns, true, true, true, "auth rules preserved", "subscription-1")

	// Slice 1 + slice 2 namespace-level keys preserved.
	assert.Equal(t, "namespace", snap.Detail["source_type"])
	assert.Equal(t, "Premium", snap.Detail["sku"])
	assert.True(t, snap.HasPropagationConfig)

	// Slice 1 DLQ honest-framing keys preserved.
	assert.Equal(t, false, snap.Detail["has_dlq_queue_walk_available"])

	// Slice 2 lag honest-framing keys preserved.
	assert.Equal(t, -1, snap.Detail["lag_backlog_depth"])

	// Slice 3 poison-rate honest-framing keys also present.
	assert.Equal(t, -1, snap.Detail["poison_rate_per_hour"])
	assert.Equal(t, false, snap.Detail["poison_rate_high_band"])
}
