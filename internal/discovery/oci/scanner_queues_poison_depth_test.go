// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// TestParseOCIQueueDLQDepth covers the GetStats data-plane body parse
// (#159): dlqStats.visibleMessages is the poison-present signal;
// an absent dlqStats block or an unparseable body yields -1, never a
// fabricated zero. A present dlqStats with visibleMessages==0 is a
// REAL "DLQ currently empty" reading.
func TestParseOCIQueueDLQDepth(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantDepth int
		wantErr   bool
	}{
		{
			name:      "dlq with messages -> depth",
			body:      `{"queueId":"ocid1.queue.x","stats":{"visibleMessages":3},"dlqStats":{"visibleMessages":7}}`,
			wantDepth: 7,
		},
		{
			name:      "dlq present but empty -> real zero",
			body:      `{"stats":{"visibleMessages":12},"dlqStats":{"visibleMessages":0}}`,
			wantDepth: 0,
		},
		{
			name:      "dlqStats absent -> absent sentinel",
			body:      `{"stats":{"visibleMessages":4}}`,
			wantDepth: -1,
		},
		{
			name:      "dlqStats null -> absent sentinel",
			body:      `{"stats":{"visibleMessages":4},"dlqStats":null}`,
			wantDepth: -1,
		},
		{
			name:      "unparseable -> absent sentinel + error",
			body:      `not json`,
			wantDepth: -1,
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOCIQueueDLQDepth([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.wantDepth {
				t.Errorf("depth = %d, want %d", got, tc.wantDepth)
			}
		})
	}
}

// TestApplyOCIQueuePoisonDepthDetail covers the two depth Detail keys
// and the additive (cold-start parity) invariant — the prior
// poison-rate keys must survive untouched.
func TestApplyOCIQueuePoisonDepthDetail(t *testing.T) {
	cases := []struct {
		name         string
		depth        int
		wantDepth    int
		wantNonempty bool
	}{
		{name: "nonempty DLQ -> poison present", depth: 7, wantDepth: 7, wantNonempty: true},
		{name: "empty DLQ -> no poison", depth: 0, wantDepth: 0, wantNonempty: false},
		{name: "absent (-1) -> no poison", depth: -1, wantDepth: -1, wantNonempty: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &scanner.EventSourceInstanceSnapshot{Detail: map[string]any{}}
			// Seed the prior poison-rate axis keys to assert parity.
			applyOCIQueuePoisonRateDetail(snap, ociQueue{})
			applyOCIQueuePoisonDepthDetail(snap, tc.depth)

			if got := snap.Detail["poison_dlq_depth"]; got != tc.wantDepth {
				t.Errorf("poison_dlq_depth = %v, want %d", got, tc.wantDepth)
			}
			if got := snap.Detail["poison_dlq_nonempty"]; got != tc.wantNonempty {
				t.Errorf("poison_dlq_nonempty = %v, want %v", got, tc.wantNonempty)
			}
			// Additive invariant: the honest-absent rate keys remain.
			if _, ok := snap.Detail["poison_rate_per_hour"]; !ok {
				t.Errorf("poison_rate_per_hour key dropped (parity break)")
			}
			if _, ok := snap.Detail["poison_rate_high_band"]; !ok {
				t.Errorf("poison_rate_high_band key dropped (parity break)")
			}
		})
	}
}

// TestQueueDLQDepth_NoDLQConfigured covers the gate: a queue with
// deadLetterQueueDeliveryCount==0 has no DLQ to measure, so the depth
// read short-circuits to the absent sentinel without a network call.
func TestQueueDLQDepth_NoDLQConfigured(t *testing.T) {
	s := &Scanner{}
	q := ociQueue{ID: "ocid1.queue.x", MessagesEndpoint: "https://cell-1.queue.messages.us-ashburn-1.oci.oraclecloud.com"}
	if got := s.queueDLQDepth(context.TODO(), nil, q); got != -1 {
		t.Errorf("no-DLQ queue depth = %d, want -1", got)
	}
}

// TestQueueDLQDepth_NoMessagesEndpoint covers the data-plane
// unreachable branch: a DLQ-configured queue whose messagesEndpoint
// is unknown safe-degrades to the absent sentinel.
func TestQueueDLQDepth_NoMessagesEndpoint(t *testing.T) {
	s := &Scanner{}
	q := ociQueue{ID: "ocid1.queue.x", DeadLetterQueueDeliveryCount: 5}
	if got := s.queueDLQDepth(context.TODO(), nil, q); got != -1 {
		t.Errorf("no-endpoint queue depth = %d, want -1", got)
	}
}
