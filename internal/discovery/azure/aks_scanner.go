// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// scanAKS walks the subscription's
// Microsoft.ContainerService/managedClusters surface and appends
// ClusterSnapshot entries to result.Clusters. The walk is one
// subscription-scoped REST call per scan (following nextLink
// pagination defensively).
//
// Partial-failure semantics mirror the slice-1 VM walk and the
// slice-2 SQL walk: a 4xx/5xx on the list call records a partial
// failure under the "aks" service id and the walk returns. An
// empty managed-cluster list is valid (zero AKS surface in the
// subscription) — the walk appends zero ClusterSnapshot entries
// without recording a partial failure.
//
// The accessToken parameter is the same OAuth2 bearer the VM walk
// acquired. Azure Reader at the subscription scope already covers
// Microsoft.ContainerService reads, so the kubernetes-tier-slice-2
// path does not re-issue a token — see
// docs/proposals/kubernetes-tier-slice2.md §5.2.
func (s *Scanner) scanAKS(ctx context.Context, accessToken string, result *scanner.Result) {
	clusters, listErr := s.listAKSClusters(ctx, accessToken)
	if listErr != nil {
		recordPartialFailure(result, ServiceIDAKS, classifyAKSError(listErr))
		return
	}
	// Zero clusters is a valid response — operator has no AKS
	// surface in this subscription. Append nothing; record no
	// partial failure. Symmetric with the SQL walker's empty-server
	// branch.
	if len(clusters) == 0 {
		return
	}
	for _, cluster := range clusters {
		result.Clusters = append(result.Clusters, projectAKSCluster(cluster))
	}
}

// listAKSClusters walks the subscription-scope
// Microsoft.ContainerService/managedClusters list endpoint,
// following nextLink pagination, and returns the accumulated
// managed clusters. Errors are returned as *armCallError so the
// caller can dispatch on StatusCode / IsNetwork in
// classifyAKSError.
func (s *Scanner) listAKSClusters(ctx context.Context, accessToken string) ([]armAKSCluster, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.ContainerService/managedClusters?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		armAKSAPIVersion,
	)

	var out []armAKSCluster
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armAKSListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("aks list parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, nil
}

// projectAKSCluster maps an armAKSCluster into the
// provider-agnostic ClusterSnapshot. Mapping decisions per the
// kubernetes-tier-slice-2 design doc §3.2 / §4 and the chunk-3
// brief:
//
//   - ResourceID: the full ARM resource id. Operator-hostile
//     (long, slash-laden) but the canonical identifier the
//     proposer's evidence list and the recommendation envelope's
//     AffectedResources field both reference. Name is kept as a
//     separate field so the Inventory tab can render the
//     operator-readable label without parsing the id.
//   - Name: cluster.Name verbatim.
//   - KubernetesVersion: major.minor (e.g. "1.29" from "1.29.4")
//     for cross-cloud consistency with the GKE/GCP slice-2 chunk-2
//     scanner. Operators reasoning about ADOT operator
//     compatibility floors care about the minor; the patch level
//     drifts under managed upgrades and would create noise in the
//     Inventory tab's row-diff between scans. Falls back to
//     currentKubernetesVersion when kubernetesVersion is empty
//     (mid-upgrade clusters can transiently elide one).
//   - Status: "RUNNING" when provisioningState=="Succeeded" AND
//     powerState.code=="Running"; otherwise the raw
//     provisioningState verbatim ("Creating" / "Updating" /
//     "Deleting" / "Failed"). The two-axis healthy gate mirrors
//     EKS's "ACTIVE" projection — only fully-up clusters count as
//     RUNNING; any mid-lifecycle state surfaces so the Inventory
//     tab can dim the row and the proposer can decline to
//     recommend against it.
//   - Region: cluster.Location.
//   - Tags: defensive copy via copyTags.
//   - Provider: "azure" (drives the proposer's recommendation-kind
//     dispatch — see ClusterSnapshot.Provider godoc on the empty
//     defaults-to-AWS backward-compat note).
//   - AzureMonitorEnabled: the §3.2 three-way disjunction —
//     omsagent enabled OR azureMonitorProfile.metrics enabled OR
//     azureMonitorProfile.containerInsights enabled. Absent
//     profiles short-circuit to false rather than panicking on a
//     nil dereference.
func projectAKSCluster(cluster armAKSCluster) scanner.ClusterSnapshot {
	props := cluster.Properties
	return scanner.ClusterSnapshot{
		ResourceID:          cluster.ID,
		Name:                cluster.Name,
		KubernetesVersion:   extractMajorMinor(pickKubernetesVersion(props)),
		Status:              normalizeAKSStatus(props),
		Region:              cluster.Location,
		Tags:                copyTags(cluster.Tags),
		Provider:            azureProviderID,
		AzureMonitorEnabled: hasAzureMonitor(props),
	}
}

// pickKubernetesVersion returns the most diagnostic version string
// available on the cluster properties. AKS exposes two version
// fields: kubernetesVersion (operator-requested) and
// currentKubernetesVersion (currently-running across the control
// plane and node pools). They are typically identical; during a
// managed upgrade they diverge. The slice-2 contract prefers
// kubernetesVersion (the operator's intent) and falls back to
// currentKubernetesVersion only when kubernetesVersion is empty
// — that fallback covers the rare transient case where a cluster
// mid-creation hasn't populated the requested version yet.
func pickKubernetesVersion(props armAKSProperties) string {
	if props.KubernetesVersion != "" {
		return props.KubernetesVersion
	}
	return props.CurrentKubernetesVersion
}

// extractMajorMinor reduces a Kubernetes version string ("1.29.4")
// to its major.minor prefix ("1.29"). Empty input projects to
// empty output. Inputs already in major.minor form pass through
// unchanged. Inputs without any dot pass through unchanged (the
// only known shape with no dot is the empty string; the
// pass-through preserves safety against unforeseen wire shapes
// from a future API version).
//
// Cross-cloud consistency: the GKE/GCP slice-2 chunk-2 scanner
// uses the same reduction, so a multi-cloud Inventory tab renders
// the same level of detail across providers.
func extractMajorMinor(v string) string {
	if v == "" {
		return ""
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// normalizeAKSStatus maps the AKS lifecycle two-axis signal into
// the canonical ClusterSnapshot.Status string. AKS exposes
// provisioningState (the most recent management-plane operation
// outcome) and powerState.code (whether the cluster is started or
// stopped). A cluster is operator-actionable for observability
// recommendations only when BOTH are healthy.
//
// Rule:
//   - provisioningState=="Succeeded" AND powerState.code=="Running"
//     → "RUNNING" (parallel to the EKS "ACTIVE" / GKE "RUNNING"
//     conventions; the canonical healthy-status string).
//   - Any other combination → the raw provisioningState verbatim
//     ("Creating" / "Updating" / "Deleting" / "Failed" / etc.).
//     The raw value carries the most operator-actionable signal —
//     "Failed" tells the operator something's broken,
//     "Updating" tells them an upgrade is in flight, and the
//     proposer's "skip non-RUNNING" branch fires uniformly.
//   - When provisioningState itself is empty (the API can elide
//     it on partially-populated responses), the projection falls
//     back to "UNKNOWN" so the Inventory tab has a non-empty value
//     to render. Empty Status would otherwise be ambiguous with
//     uninitialized struct fields elsewhere in the pipeline.
func normalizeAKSStatus(props armAKSProperties) string {
	powerCode := ""
	if props.PowerState != nil {
		powerCode = props.PowerState.Code
	}
	if props.ProvisioningState == aksProvisioningSucceeded && powerCode == aksRunningPowerState {
		return aksStatusRunning
	}
	if props.ProvisioningState == "" {
		return "UNKNOWN"
	}
	return props.ProvisioningState
}

// hasAzureMonitor implements the slice-2 §3.2 three-way disjunction
// detection rule for AKS clusters. The cluster counts as having
// Azure Monitor observability iff ANY of:
//
//  1. addonProfiles["omsagent"].Enabled is true (legacy Container
//     Insights addon).
//  2. azureMonitorProfile.metrics.Enabled is true (newer Managed
//     Prometheus on AKS).
//  3. azureMonitorProfile.containerInsights.Enabled is true (newer
//     Container Insights via the azureMonitorProfile, replacing
//     the omsagent addon).
//
// Mirrors EKS's "ADOT OR CloudWatch observability" disjunction —
// operators on either the legacy or newer addon get credit. All
// three checks defend against nil/absent sub-structures: an absent
// addonProfiles map, an absent omsagent entry, an absent
// azureMonitorProfile block, or absent metrics/containerInsights
// sub-blocks all short-circuit to the false branch rather than
// panicking on a missing field.
//
// Order of checks is irrelevant for correctness (disjunction is
// commutative) but the omsagent legacy path runs first so the
// short-circuit behavior matches the order operators encounter in
// the documentation — older clusters reach the legacy branch and
// stop; newer clusters fall through to the azureMonitorProfile
// branch.
func hasAzureMonitor(props armAKSProperties) bool {
	if props.AddonProfiles != nil {
		if oms, ok := props.AddonProfiles[aksOMSAgentAddonName]; ok && oms.Enabled {
			return true
		}
	}
	if props.AzureMonitorProfile != nil {
		if props.AzureMonitorProfile.Metrics != nil && props.AzureMonitorProfile.Metrics.Enabled {
			return true
		}
		if props.AzureMonitorProfile.ContainerInsights != nil && props.AzureMonitorProfile.ContainerInsights.Enabled {
			return true
		}
	}
	return false
}

// classifyAKSError maps a Microsoft.ContainerService/managedClusters
// walk failure into the operator-visible PartialReason string
// under the "aks" service identifier. Mirrors the slice-1
// classifyARMError and slice-2 classifyAzureSQLError shapes
// (network / rate-limit / permission_denied / subscription_not_found
// / credentials / generic-tail) so the proposer-side consumer sees
// identical structure across services in the same scan.
//
// Mappings: network → "network error"; 429 OR Retry-After →
// rate_limit; 403 → permission_denied (existing Reader role at
// subscription scope already covers Microsoft.ContainerService
// reads — a 403 here means the SP role assignment is wrong, NOT
// that a separate scope is needed); 404 → subscription_not_found
// (defended for symmetry with VM/SQL — the upstream VM walk
// usually catches this first); 401 → credentials_invalid; other
// 4xx/5xx → generic-tail.
func classifyAKSError(err error) string {
	if err == nil {
		return ""
	}
	var ace *armCallError
	if errors.As(err, &ace) {
		if ace.IsNetwork {
			wrapped := ""
			if ace.Wrapped != nil {
				wrapped = ace.Wrapped.Error()
			}
			return fmt.Sprintf("%s: network error: %s", ServiceIDAKS, truncate(wrapped, 200))
		}
		if ace.StatusCode == http.StatusTooManyRequests || ace.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDAKS)
		}
		switch ace.StatusCode {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service principal has Reader role on the subscription)", ServiceIDAKS)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: subscription not found (verify subscription_id is correct)", ServiceIDAKS)
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check tenant_id, client_id, client_secret)", ServiceIDAKS)
		default:
			msg := ace.Message
			if msg == "" {
				msg = ace.BodyHint
			}
			return fmt.Sprintf("%s: AKS list failed (HTTP %d): %s", ServiceIDAKS, ace.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDAKS, truncate(err.Error(), 200))
}
