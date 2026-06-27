// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import "testing"

// TestSQSPoisonDepth covers the depth/presence poison signal (#156): a source
// queue reads its DLQ's current message count from the pass-1 ARN->depth map,
// surfacing poison_dlq_depth + poison_dlq_nonempty honestly.
func TestSQSPoisonDepth(t *testing.T) {
	const dlqARN = "arn:aws:sqs:us-east-1:123456789012:orders-dlq"
	srcWithRedrive := func() queueAttributes {
		return queueAttributes{
			URL:              "https://sqs.us-east-1.amazonaws.com/123456789012/orders",
			ARN:              "arn:aws:sqs:us-east-1:123456789012:orders",
			Attributes:       map[string]string{},
			HasRedrivePolicy: true,
			RedrivePolicy:    &redrivePolicyShape{DeadLetterTargetArn: dlqARN, MaxReceiveCount: 5},
		}
	}

	cases := []struct {
		name         string
		qa           queueAttributes
		arnDepth     map[string]int
		wantDepth    int
		wantNonempty bool
	}{
		{
			name:         "reachable DLQ with messages -> poison present",
			qa:           srcWithRedrive(),
			arnDepth:     map[string]int{dlqARN: 7, "arn:aws:sqs:us-east-1:123456789012:orders": 0},
			wantDepth:    7,
			wantNonempty: true,
		},
		{
			name:         "reachable empty DLQ -> no poison",
			qa:           srcWithRedrive(),
			arnDepth:     map[string]int{dlqARN: 0},
			wantDepth:    0,
			wantNonempty: false,
		},
		{
			name:         "cross-account / dangling DLQ -> absent (-1)",
			qa:           srcWithRedrive(),
			arnDepth:     map[string]int{}, // DLQ ARN not in this scan
			wantDepth:    -1,
			wantNonempty: false,
		},
		{
			name:         "no redrive policy -> absent (-1)",
			qa:           queueAttributes{URL: "https://sqs.us-east-1.amazonaws.com/123456789012/plain", ARN: "arn:aws:sqs:us-east-1:123456789012:plain", Attributes: map[string]string{}},
			arnDepth:     map[string]int{},
			wantDepth:    -1,
			wantNonempty: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := buildSQSSnapshot("123456789012", "us-east-1", tc.qa, tc.arnDepth)
			if got := snap.Detail["poison_dlq_depth"]; got != tc.wantDepth {
				t.Errorf("poison_dlq_depth = %v, want %d", got, tc.wantDepth)
			}
			if got := snap.Detail["poison_dlq_nonempty"]; got != tc.wantNonempty {
				t.Errorf("poison_dlq_nonempty = %v, want %v", got, tc.wantNonempty)
			}
			// The honest absent rate keys remain (additive change).
			if _, ok := snap.Detail["poison_rate_per_hour"]; !ok {
				t.Errorf("poison_rate_per_hour key dropped (parity break)")
			}
		})
	}
}
