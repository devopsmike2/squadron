// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScan_OCIObjectStorage covers the object-store tier (coverage-
// parity slice 4): buckets project into ObjectStoreSnapshot, and a
// bucket with objectEventsEnabled counts as instrumented.
func TestScan_OCIObjectStorage(t *testing.T) {
	root := "ocid1.tenancy.oc1..aaa"
	fake := &fakeOCI{
		Namespace: "myns",
		BucketsByCompartment: map[string][]ociBucket{
			root: {
				{Name: "logged-bucket", FreeformTags: map[string]string{"env": "prod"}},
				{Name: "plain-bucket"},
			},
		},
		// Slice 6: coverage is resolved from the OCI Logging service.
		// An enabled object-storage service log references logged-bucket;
		// plain-bucket has none.
		LogsByResource: map[string][]ociLogResource{
			"logged-bucket": {
				{Configuration: ociLogConfiguration{Source: ociLogSource{Resource: "logged-bucket", Category: "write"}}},
			},
		},
	}
	res, err := newScannerWithFake(t, fake, "us-phoenix-1").Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.ObjectStores, 2)
	var sawLogged, sawPlain bool
	for _, o := range res.ObjectStores {
		switch o.ResourceID {
		case "logged-bucket":
			sawLogged = true
			assert.True(t, o.ServerAccessLoggingEnabled, "object-storage service log => covered")
			assert.Equal(t, "us-phoenix-1", o.Region)
			assert.Equal(t, "prod", o.Tags["env"])
		case "plain-bucket":
			sawPlain = true
			assert.False(t, o.ServerAccessLoggingEnabled, "no service log => uncovered")
		}
	}
	assert.True(t, sawLogged && sawPlain, "both buckets present")
}

// TestScan_OCILoadBalancers covers the load-balancer tier: LBs project
// into LoadBalancerSnapshot with scheme from isPrivate. AccessLogsEnabled
// is left false (OCI LB access logs are detected via the Logging
// service — deferred).
func TestScan_OCILoadBalancers(t *testing.T) {
	root := "ocid1.tenancy.oc1..aaa"
	fake := &fakeOCI{
		LoadBalancersByCompartment: map[string][]ociLoadBalancer{
			root: {
				{ID: "ocid1.loadbalancer..lb1", DisplayName: "public-lb", IsPrivate: false},
				{ID: "ocid1.loadbalancer..lb2", DisplayName: "private-lb", IsPrivate: true},
			},
		},
		// Slice 6: an OCI service log references lb1; lb2 has none.
		LogsByResource: map[string][]ociLogResource{
			"ocid1.loadbalancer..lb1": {
				{Configuration: ociLogConfiguration{Source: ociLogSource{Resource: "ocid1.loadbalancer..lb1", Category: "access"}}},
			},
		},
	}
	res, err := newScannerWithFake(t, fake, "us-phoenix-1").Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.LoadBalancers, 2)
	var sawPublic, sawPrivate bool
	for _, lb := range res.LoadBalancers {
		switch lb.Name {
		case "public-lb":
			sawPublic = true
			assert.Equal(t, "internet-facing", lb.Scheme)
			assert.True(t, lb.AccessLogsEnabled, "service log referencing the LB OCID => covered")
		case "private-lb":
			sawPrivate = true
			assert.Equal(t, "internal", lb.Scheme)
			assert.False(t, lb.AccessLogsEnabled, "no service log => uncovered")
		}
		assert.Equal(t, "us-phoenix-1", lb.Region)
	}
	assert.True(t, sawPublic && sawPrivate, "both LBs present")
}
