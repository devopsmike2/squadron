// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import "testing"

// TestProductionEndpointHosts pins the per-region service hosts. OCI is
// inconsistent about the ".oci" infix across services, and a wrong host
// surfaces only as a DNS failure at scan time (caught live during the
// slice-6 OCI validation: the load-balancer host was missing the
// ".oci" infix). These assertions lock the resolved hosts so the
// regression cannot recur silently.
func TestProductionEndpointHosts(t *testing.T) {
	s := &Scanner{Region: "us-ashburn-1"} // ociEndpoint empty => production hosts
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"objectstorage", s.objectStorageEndpoint(), "https://objectstorage.us-ashburn-1.oraclecloud.com"},
		{"loadbalancer", s.lbEndpoint(), "https://iaas.us-ashburn-1.oraclecloud.com"},
		{"logging", s.loggingEndpoint(), "https://logging.us-ashburn-1.oci.oraclecloud.com"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s endpoint = %q, want %q", c.name, c.got, c.want)
		}
	}
}
