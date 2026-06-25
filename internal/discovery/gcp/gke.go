// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Slice-2 (kubernetes-tier-slice2.md, v0.89.70) GKE extension.
//
// This file ships the GKE Container API walk that the GCP scanner runs
// after the slice-1 Compute Engine walk + slice-2 Cloud SQL walk
// complete. All three walks share the same per-scan SA credential and
// the same partial-failure accumulator on the Result; GKE surfaces
// independently in Result.Clusters with Provider="gcp" so the proposer
// routes its findings to the gke-mp-enable recommendation kind (see
// proposal §3.1).
//
// Library choice mirrors the compute + Cloud SQL walks:
// google.golang.org/api/container/v1beta1 (the REST client). The
// httptest mock surface that already shape-tests the earlier walks
// extends to the GKE path by adding /v1beta1/projects/.../clusters
// handling — see scanner_test.go::fakeGCP.handler.
//
// API surface used:
//   - GET https://container.googleapis.com/v1beta1/projects/{project}/locations/-/clusters
//
// The "-" location wildcard returns clusters across all regions/zones,
// so a single list call covers the whole project — pagination is not
// exposed on this endpoint (clusters per project is bounded; the
// design doc §5.1 pins this assumption explicitly).
//
// OAuth scope: cloud-platform.read-only (see consts.go ContainerReadonlyScope
// godoc for why we don't use a more-targeted container.readonly
// constant). The runbook documents roles/container.viewer as the
// project-level IAM grant — the scope on the token and the role on the
// principal are independent axes; the role is the least-privilege ask
// either way.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	container "google.golang.org/api/container/v1beta1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// walkGKE lists GKE clusters in the configured project, projects each
// into a ClusterSnapshot, and appends the result to result.Clusters.
// Errors are surfaced to the caller for recording as a partial-failure
// entry against the gke service identifier — same pattern as the
// Cloud SQL walk's error surfacing.
//
// Pagination: the GKE Container API's clusters.list endpoint does NOT
// expose a NextPageToken on ListClustersResponse — the response carries
// every cluster the project owns in a single call. This is consistent
// with operator scale (the design doc §5.1 pins the assumption that
// per-project cluster count stays bounded; large fleets typically
// distribute across projects rather than packing one project).
//
// Region filter: the scanner's s.Region field is applied client-side
// after the response arrives — the Container API supports a parent
// path that names a specific region, but the slice-2 scanner uses the
// "-" wildcard so a single call covers the project, and any region
// filter applies post-projection. This matches the Cloud SQL walk's
// region-filter posture (the Cloud SQL list call also has no
// server-side region filter; both walks filter client-side on the
// projected snapshot's Region).
func (s *Scanner) walkGKE(ctx context.Context, client *container.Service, result *scanner.Result) error {
	// The "-" location wildcard fans across every region and zone the
	// project's clusters live in. See container/v1beta1 godoc on
	// ProjectsLocationsClustersService.List for the parent format.
	parent := fmt.Sprintf("projects/%s/locations/-", s.ProjectID)
	resp, err := client.Projects.Locations.Clusters.List(parent).Context(ctx).Do()
	if err != nil {
		return err
	}
	for _, cluster := range resp.Clusters {
		if cluster == nil {
			continue
		}
		if s.Region != "" && cluster.Location != s.Region {
			continue
		}
		result.Clusters = append(result.Clusters, projectGKECluster(cluster))
	}
	return nil
}

// projectGKECluster maps a container.Cluster into the provider-agnostic
// ClusterSnapshot. The mapping is the slice-2 (kubernetes-tier-slice2.md
// §3.1) contract:
//
//   - ResourceID: cluster.SelfLink (the canonical
//     "https://container.googleapis.com/v1beta1/projects/.../clusters/<name>"
//     URL — globally unique per cluster). Mirrors the AWS EKS slice 1
//     ResourceID-as-ARN convention.
//   - Name: cluster.Name.
//   - KubernetesVersion: extractMajorMinor(cluster.CurrentMasterVersion).
//     "1.29.4-gke.1043000" → "1.29".
//   - Status: cluster.Status (raw GKE enum: RUNNING / PROVISIONING /
//     STOPPING / ERROR / DEGRADED / etc).
//   - Region: cluster.Location (regional clusters return the region
//     name e.g. "us-central1"; zonal clusters return the zone e.g.
//     "us-central1-a" — both shapes pass through unchanged so the
//     Inventory tab can render either honestly).
//   - Tags: cluster.ResourceLabels — defensively copied.
//   - Provider: "gcp" — the discriminator the proposer reads to route
//     to gke-mp-enable.
//   - ManagedPrometheusEnabled: a three-axis nil-safe traversal of
//     monitoringConfig.managedPrometheusConfig.enabled. Any nil
//     intermediate node OR an explicit enabled=false reads as false
//     (the design doc §3.1 detection rule pins this explicitly:
//     missing config is treated as opt-out, not as "we don't know,"
//     because the proposer's negative-case branch is the right
//     recommendation either way — the cluster has no managed
//     observability primitive on).
//
// The AWS-specific fields (ControlPlaneLogging, Addons, NodegroupCount,
// FargateProfileCount) stay zero on GCP-projected snapshots —
// scanner.ClusterSnapshot's godoc pins backward-compat: AWS readers
// inspect those fields only when Provider=="aws" (empty Provider also
// defaults to "aws" per the substrate contract, but every GCP-projected
// snapshot stamps Provider="gcp" explicitly so the proposer's routing
// fires on the right axis).
func projectGKECluster(cluster *container.Cluster) scanner.ClusterSnapshot {
	snap := scanner.ClusterSnapshot{
		ResourceID:               cluster.SelfLink,
		Name:                     cluster.Name,
		KubernetesVersion:        extractMajorMinor(cluster.CurrentMasterVersion),
		Status:                   cluster.Status,
		Region:                   cluster.Location,
		Tags:                     copyLabels(cluster.ResourceLabels),
		Provider:                 ProviderGCP,
		ManagedPrometheusEnabled: managedPrometheusEnabled(cluster.MonitoringConfig),
	}
	return snap
}

// managedPrometheusEnabled implements the slice-2 detection rule for
// GKE managed observability with strict nil-safety. The rule:
//
//	cluster.MonitoringConfig != nil
//	  AND cluster.MonitoringConfig.ManagedPrometheusConfig != nil
//	  AND cluster.MonitoringConfig.ManagedPrometheusConfig.Enabled
//
// Any nil intermediate node reads as false. Operators on GKE clusters
// that pre-date the managed observability surface entirely return
// MonitoringConfig=nil from the API; clusters that have monitoring on
// but managed Prometheus off return ManagedPrometheusConfig=nil (or
// Enabled=false). Both shapes map to the same proposer outcome — the
// gke-mp-enable recommendation fires when this returns false.
func managedPrometheusEnabled(cfg *container.MonitoringConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.ManagedPrometheusConfig == nil {
		return false
	}
	return cfg.ManagedPrometheusConfig.Enabled
}

// extractMajorMinor reduces a GKE master-version string to its bare
// "major.minor" form. GKE versions arrive as e.g. "1.29.4-gke.1043000";
// the proposer keys per-version guidance off the "1.29" portion (matches
// the operator's mental model — they ask "are we on 1.29 yet" not
// "are we on 1.29.4-gke.1043000 yet"). The slice-2 contract is:
//
//	"1.29.4-gke.1043000" -> "1.29"
//	"1.30.1-gke.500"     -> "1.30"
//	"1.28"               -> "1.28"
//	"1.29.4"             -> "1.29"
//	""                   -> ""
//	"weirdshape"         -> "weirdshape"
//
// Implementation: split off any "-gke.*" suffix first, then take the
// first two dot-separated components. Defensive fallthrough to the
// raw input when the shape doesn't parse (the API guarantees the GKE
// suffix on currentMasterVersion but slice 2 shouldn't crash on an
// unexpected shape — see design doc §11 acceptance test 1).
func extractMajorMinor(version string) string {
	if version == "" {
		return ""
	}
	// Strip "-gke.NNN" (or any "-*") suffix.
	if i := strings.Index(version, "-"); i >= 0 {
		version = version[:i]
	}
	parts := strings.Split(version, ".")
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return parts[0] + "." + parts[1]
	}
}

// classifyGKEListError maps an Clusters.List failure into the
// operator-visible PartialReason string. Same shape as
// classifyZonesListError + classifyCloudSQLListError but scoped to
// the GKE Container service so the error message points at the right
// IAM grant (roles/container.viewer per the design doc §12 threat
// model) and not at the compute or Cloud SQL ones.
//
// Error mappings (per brief Step 4):
//
//   - 403 -> permission denied with remediation hint pointing at
//     roles/container.viewer (the slice-2 new IAM grant the runbook
//     documents).
//   - 404 -> project not found (same remediation hint as the compute
//   - Cloud SQL paths — verify project_id).
//   - 429 -> rate limit (GKE has its own quota separate from compute
//   - Cloud SQL; same recovery story).
//   - Transport / network -> network-error with the underlying
//     err.Error() truncated to keep audit payloads bounded.
//   - Other 4xx/5xx -> truncated message with the HTTP code surfaced
//     so support agents can pattern-match against the Container API
//     documentation.
func classifyGKEListError(err error) string {
	if err == nil {
		return ""
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service account has roles/container.viewer)", ServiceIDGKE)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: project not found (verify project_id is correct)", ServiceIDGKE)
		case http.StatusTooManyRequests:
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDGKE)
		default:
			return fmt.Sprintf("%s: clusters list failed (HTTP %d): %s", ServiceIDGKE, ge.Code, truncate(ge.Message, 200))
		}
	}
	return fmt.Sprintf("%s: network error: %s", ServiceIDGKE, truncate(err.Error(), 200))
}

// buildContainerClient constructs a container.Service using the test
// httpClient + endpoint (no auth) or the SA-JSON-backed oauth2 client.
// Production callers reach the latter path; tests the former.
//
// Note: the OAuth scope union (compute.readonly + sqlservice.admin +
// cloud-platform.read-only) is set on the SHARED JWT config that
// buildComputeClient + buildCloudSQLClient also read. See
// buildOAuthHTTPClient for the scope union — the scope for the
// Container API surface is the read-only platform scope (the
// container/v1beta1 client library does not expose a more-targeted
// container.readonly constant; see consts.go ContainerReadonlyScope
// godoc for the rationale).
func (s *Scanner) buildContainerClient(ctx context.Context, oauthClient *http.Client) (*container.Service, error) {
	if s.httpClient != nil {
		// Test path. The httpClient already points at the test server;
		// option.WithoutAuthentication stops the container client from
		// wrapping the test transport in another oauth2 layer.
		opts := []option.ClientOption{
			option.WithHTTPClient(s.httpClient),
			option.WithoutAuthentication(),
		}
		if s.endpoint != "" {
			opts = append(opts, option.WithEndpoint(s.endpoint))
		}
		return container.NewService(ctx, opts...)
	}
	// Production path: reuse the shared oauth-backed client built by
	// buildOAuthHTTPClient so the SA JSON is parsed once per scan
	// regardless of how many APIs the scan walks.
	return container.NewService(ctx, option.WithHTTPClient(oauthClient))
}
