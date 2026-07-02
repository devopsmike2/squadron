// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Load-balancer tier — Cloud Load Balancing walk. Coverage-parity arc
// slice 2. A Cloud Load Balancing config's access-logging unit is the
// backend service (LogConfig.Enable); we list backend services across
// all scopes (global + regional) via the aggregated list and project
// each into scanner.LoadBalancerSnapshot. Instrumented axis mirrors the
// AWS ALB AccessLogsEnabled axis.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// ServiceIDGCLB is the load-balancer-tier service identifier reported
// against Result.FailedServices on a non-fatal Cloud Load Balancing
// error. compute.readonly (already in the scope union) covers the
// aggregated backend-service list, so no new OAuth scope is needed.
const ServiceIDGCLB = "gclb"

// walkLoadBalancers lists backend services (the access-logging unit of a
// Cloud Load Balancing config), projects each into a
// scanner.LoadBalancerSnapshot, and appends to result.LoadBalancers.
func (s *Scanner) walkLoadBalancers(ctx context.Context, client *compute.Service, result *scanner.Result) error {
	return client.BackendServices.AggregatedList(s.ProjectID).Pages(ctx, func(page *compute.BackendServiceAggregatedList) error {
		for scope, scoped := range page.Items {
			region := regionFromAggregatedScope(scope)
			for _, bs := range scoped.BackendServices {
				if bs == nil {
					continue
				}
				snap := scanner.LoadBalancerSnapshot{
					ResourceID: bs.SelfLink,
					Name:       bs.Name,
					Type:       bs.LoadBalancingScheme,
					Scheme:     gclbScheme(bs.LoadBalancingScheme),
					Region:     region,
					// ImportID: the backend service imports by the canonical
					// resource path (google_compute_backend_service accepts
					// "projects/{{project}}/global/backendServices/{{name}}",
					// the regional variant "projects/{{project}}/regions/{{region}}/backendServices/{{name}}").
					// The SelfLink already carries exactly that path after the
					// API host+version prefix, so slice it out; empty when the
					// SelfLink is absent so the mapper skips.
					ImportID: backendServiceImportID(bs.SelfLink),
				}
				if snap.ResourceID == "" {
					snap.ResourceID = bs.Name
				}
				if bs.LogConfig != nil && bs.LogConfig.Enable {
					snap.AccessLogsEnabled = true
				}
				result.LoadBalancers = append(result.LoadBalancers, snap)
			}
		}
		return nil
	})
}

// backendServiceImportID extracts the canonical Terraform import path from a
// backend service SelfLink. A SelfLink looks like
// "https://www.googleapis.com/compute/v1/projects/P/global/backendServices/N"
// (or ".../regions/R/backendServices/N"); the terraform import id is that same
// path from "projects/" onward. Returns "" when the SelfLink lacks the
// expected "/projects/" segment so the mapper skips rather than guess.
func backendServiceImportID(selfLink string) string {
	i := strings.Index(selfLink, "/projects/")
	if i < 0 {
		return ""
	}
	return selfLink[i+1:] // drop the leading slash -> "projects/..."
}

// gclbScheme maps a GCP loadBalancingScheme onto the AWS-style
// internet-facing/internal vocabulary the proposer reasons about.
func gclbScheme(scheme string) string {
	switch {
	case strings.HasPrefix(scheme, "INTERNAL"):
		return "internal"
	case strings.HasPrefix(scheme, "EXTERNAL"):
		return "internet-facing"
	}
	return strings.ToLower(scheme)
}

// regionFromAggregatedScope turns an aggregated-list scope key
// ("global" or "regions/us-central1") into a bare region string.
func regionFromAggregatedScope(scope string) string {
	if scope == "" || scope == "global" {
		return "global"
	}
	if i := strings.LastIndex(scope, "/"); i >= 0 {
		return scope[i+1:]
	}
	return scope
}

func classifyGCLBListError(err error) string {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusForbidden, http.StatusUnauthorized:
			return fmt.Sprintf("%s: permission denied (grant the service account roles/compute.viewer)", ServiceIDGCLB)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: project not found (verify project_id is correct)", ServiceIDGCLB)
		case http.StatusTooManyRequests:
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDGCLB)
		}
		return fmt.Sprintf("%s: backend-service list failed (HTTP %d): %s", ServiceIDGCLB, ge.Code, truncate(ge.Message, 200))
	}
	return fmt.Sprintf("%s: network error: %s", ServiceIDGCLB, truncate(err.Error(), 200))
}
