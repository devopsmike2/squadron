// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Event source tier slice 5 chunk 1 (v0.89.144, #784 Stream 182) GCP
// Cloud Tasks queue scanner + two-way ScanEventSources dispatcher
// extension.
//
// Slice 1 chunk 2 (v0.89.101) shipped the Pub/Sub scanner as the GCP
// scanner's only event-source surface; ScanEventSources returned
// Pub/Sub topics directly. Slice 5 adds Cloud Tasks queues as the
// second GCP event-source surface and extends the dispatcher to fan
// out across BOTH surfaces with a two-way partial-scan posture
// mirroring the AWS slice 4 three-way dispatcher
// (internal/discovery/aws/eventbridge.go::ScanEventSources): each
// surface may fail independently; only when BOTH fail does the
// dispatcher return a non-nil error.
//
// Library choice — raw HTTP rather than
// cloud.google.com/go/cloudtasks/apiv2. The chunk-1 walk mirrors the
// pubsub.go raw-HTTP path (v0.89.101) so the slice's surface coverage
// stays consistent with the rest of the GCP scanner pack. The
// generated SDK would pull in the gRPC stack just to read queue
// metadata Squadron already retrieves via a single GET; the
// raw-HTTP path also keeps the httptest fake surface uniform across
// the GCP scanners.
//
// API surface: GET /v2/projects/{project}/locations/{location}/queues
// (paginated via pageToken). Cloud Tasks is regional — queues live
// per-location, so the walk iterates the configured region list (or
// the GCP standard region set when none is pinned). OAuth scope:
// cloud-platform.read-only (same as the Pub/Sub + Workflows walks).
// IAM grant: roles/cloudtasks.viewer at the project level. Required
// permissions per design doc §3:
//   - cloudtasks.queues.list
//   - cloudtasks.queues.get

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// CloudTasksSurface is the Surface discriminator string for GCP Cloud
// Tasks snapshots. The proposer's recommendation-kind prefix routing
// switches on "eventbridge" → AWS, "pubsub" / "cloudtasks" → GCP,
// "servicebus" → Azure, "streaming" → OCI.
const CloudTasksSurface = "cloudtasks"

// CloudTasksSourceTypeQueue is the SourceType discriminator string for
// Cloud Tasks queues. Mirrors the per-cloud "bus" / "topic" / "queue" /
// "namespace" / "stream" SourceType convention documented on
// scanner.EventSourceInstanceSnapshot.
const CloudTasksSourceTypeQueue = "queue"

// CloudTasksMaxAttemptsUnlimited is the sentinel value Cloud Tasks
// uses to indicate unlimited retry attempts. Slice 5 treats both
// maxAttempts > 0 AND maxAttempts == -1 (unlimited) as configured
// retry per docs/proposals/event-source-tier-slice5.md §3:
//
//   "Has retry config | retryConfig.maxAttempts > 0
//    (or -1 for unlimited; slice 5 treats both as configured)"
//
// A queue with maxAttempts == 0 (or no retryConfig at all) drops
// tasks SILENTLY on the first HTTP target failure — the GCP equivalent
// of an SQS queue without a redrive policy. Slice 5's
// cloudtasks-retry-policy-enable recommendation kind fires on
// HasTraceAxis == false.
const CloudTasksMaxAttemptsUnlimited = -1

// ServiceIDCloudTasks is the event-source-tier-slice5.md §3 service
// identifier the scanner reports against Result.FailedServices when
// the Cloud Tasks walk produces a non-fatal error. Same unprefixed
// shape as ServiceIDPubSub / ServiceIDWorkflows / ServiceIDCloudRun.
const ServiceIDCloudTasks = "cloudtasks"

// CloudTasksReadonlyScope is the OAuth scope the Cloud Tasks API walk
// is authorized against. cloud-platform.read-only — same posture as
// Pub/Sub / Workflows / Cloud Functions. Runbook (chunk 2) documents
// roles/cloudtasks.viewer as the project-level IAM grant.
const CloudTasksReadonlyScope = "https://www.googleapis.com/auth/cloud-platform.read-only"

// cloudTasksAPIBaseURL is the production Cloud Tasks REST API root.
// Tests override via s.endpoint.
const cloudTasksAPIBaseURL = "https://cloudtasks.googleapis.com"

// gcpStandardCloudTasksRegions is the fallback region list the Cloud
// Tasks walk iterates when neither scope.Regions nor s.Region pins a
// region. Cloud Tasks is regional (no "-" wildcard, like Cloud Run);
// queues must be listed per-location. The list is deliberately the
// canonical set of GCP general-availability regions for Cloud Tasks
// per https://cloud.google.com/tasks/docs/dual-overview; slice 5
// expects most operators to pin a region via scope.Regions or
// s.Region, and the fallback only fires when neither is set.
//
// Operators with queues in regions outside this list should configure
// scope.Regions explicitly. The fallback is intentionally NOT a full
// projects.locations.list call: a List failure on the locations
// enumeration would block the entire Cloud Tasks walk, whereas the
// hard-coded list lets per-region failures degrade independently per
// the slice 5 partial-scan posture.
var gcpStandardCloudTasksRegions = []string{
	"us-central1",
	"us-east1",
	"us-east4",
	"us-west1",
	"us-west2",
	"europe-west1",
	"europe-west2",
	"europe-west3",
	"europe-west4",
	"asia-east1",
	"asia-northeast1",
	"asia-south1",
	"asia-southeast1",
	"australia-southeast1",
}

// cloudTasksQueue is the chunk-1 raw-HTTP decode target for the Cloud
// Tasks REST API's Queue resource. Pointer-typed sub-structs let the
// scanner distinguish absent (nil) from explicit zero values, which
// matters for the maxAttempts axis per design doc §3 (maxAttempts == 0
// AND nil retryConfig both fail HasTraceAxis; maxAttempts == -1 passes
// per the unlimited sentinel).
type cloudTasksQueue struct {
	// Name is the fully-qualified queue resource path
	// "projects/{project}/locations/{location}/queues/{queue}".
	// Mapped to ResourceARN verbatim; trailing segment becomes
	// ResourceName.
	Name string `json:"name"`
	// State is the queue's lifecycle state: "RUNNING" / "PAUSED" /
	// "DISABLED". Surfaced raw in the Detail bag per design doc §3.
	State string `json:"state,omitempty"`
	// PurgeTime is RFC 3339 — present when the queue has ever been
	// purged. Detail-only per design doc §3 informational axis.
	PurgeTime *string `json:"purgeTime,omitempty"`
	// RetryConfig drives the HasTraceAxis detection per design doc
	// §3 axis 1.
	RetryConfig *cloudTasksRetryConfig `json:"retryConfig,omitempty"`
	// RateLimits — Detail bag entries when MaxDispatchesPerSecond
	// or MaxConcurrentDispatches are positive. Informational only
	// per design doc §3.
	RateLimits *cloudTasksRateLimits `json:"rateLimits,omitempty"`
	// StackdriverLoggingConfig drives the HasLogAxis detection per
	// design doc §3 axis 2.
	StackdriverLoggingConfig *cloudTasksStackdriverLoggingConfig `json:"stackdriverLoggingConfig,omitempty"`
}

// cloudTasksRetryConfig mirrors retryConfig. MaxAttempts is the axis
// input: > 0 OR == -1 (unlimited) → HasTraceAxis flips per design doc
// §3. == 0 (or absent retryConfig) → HasTraceAxis stays false.
type cloudTasksRetryConfig struct {
	MaxAttempts      int32  `json:"maxAttempts"`
	MaxRetryDuration string `json:"maxRetryDuration,omitempty"`
	MinBackoff       string `json:"minBackoff,omitempty"`
	MaxBackoff       string `json:"maxBackoff,omitempty"`
	MaxDoublings     int32  `json:"maxDoublings,omitempty"`
}

// cloudTasksRateLimits mirrors rateLimits. Informational only per
// design doc §3 — does NOT drive an axis, but the positive values
// surface in the Detail bag so the Inventory drilldown shows
// throughput-shape alongside the trace/log axes.
type cloudTasksRateLimits struct {
	MaxDispatchesPerSecond  float64 `json:"maxDispatchesPerSecond,omitempty"`
	MaxConcurrentDispatches int32   `json:"maxConcurrentDispatches,omitempty"`
	MaxBurstSize            int32   `json:"maxBurstSize,omitempty"`
}

// cloudTasksStackdriverLoggingConfig mirrors stackdriverLoggingConfig.
// SamplingRatio is the float in [0, 1.0] that controls per-task
// audit-log emission. HasLogAxis flips when SamplingRatio > 0 per
// design doc §3 axis 2. Absent stackdriverLoggingConfig OR explicit
// samplingRatio = 0 both leave HasLogAxis false.
type cloudTasksStackdriverLoggingConfig struct {
	SamplingRatio float64 `json:"samplingRatio"`
}

// cloudTasksListQueuesResponse mirrors projects.locations.queues.list.
// NextPageToken drives the pagination loop.
type cloudTasksListQueuesResponse struct {
	Queues        []*cloudTasksQueue `json:"queues,omitempty"`
	NextPageToken string             `json:"nextPageToken,omitempty"`
}

// ScanCloudTasksQueues walks every configured region's Cloud Tasks
// queues and returns the mapped event source snapshots. Slice 5 chunk
// 1 of the event-source-tier arc (v0.89.144, #784 Stream 182).
//
// Detection per docs/proposals/event-source-tier-slice5.md §3:
//
//   - HasTraceAxis ← retryConfig is non-nil AND
//     (maxAttempts > 0 OR maxAttempts == CloudTasksMaxAttemptsUnlimited).
//     A queue with maxAttempts == 0 OR no retryConfig at all leaves the
//     axis false (recommendation cloudtasks-retry-policy-enable fires).
//   - HasLogAxis ← stackdriverLoggingConfig is non-nil AND
//     samplingRatio > 0. Absent stackdriverLoggingConfig OR explicit
//     samplingRatio == 0 leaves the axis false (recommendation
//     cloudtasks-logging-enable fires).
//
// Detail bag fields (per §3): max_attempts (raw value preserves the
// drilldown's distinction between 0 / -1 / positive) /
// max_retry_duration / stackdriver_sampling_ratio (only when
// stackdriverLoggingConfig is non-nil, preserving the absent-vs-zero
// distinction at the drilldown layer) / max_dispatches_per_second /
// max_concurrent_dispatches / state / purge_time.
//
// Per-region failures are non-fatal: the walk continues with the next
// region after recording the failure into the partial-failure surface.
// A single-region IAM gap doesn't drop the queues in regions the SA
// CAN reach.
//
// IAM contract per design doc §3: cloudtasks.queues.list,
// cloudtasks.queues.get. Read-only.
func (s *Scanner) ScanCloudTasksQueues(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	if s.ProjectID == "" {
		return nil, errors.New("gcp: ProjectID is required")
	}
	if len(s.SAJSON) == 0 && s.httpClient == nil {
		return nil, errors.New("gcp: SAJSON is required")
	}
	client, err := s.buildCloudTasksHTTPClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: build cloudtasks http client: %w", err)
	}
	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.ProjectID
	}
	regions := s.cloudTasksRegions(scope)
	if len(regions) == 0 {
		// Defensive: no regions to walk. Returns an empty slice; the
		// dispatcher's partial-scan posture treats this as a successful
		// (vacuous) walk.
		return nil, nil
	}

	var all []scanner.EventSourceInstanceSnapshot
	var lastErr error
	successes := 0
	for _, region := range regions {
		queues, err := s.walkCloudTasksRegion(ctx, client, region)
		if err != nil {
			// Per-region failure is non-fatal: continue with the next
			// region. The error is captured into lastErr so the
			// caller's partial-failure recording can surface ONE
			// representative cause, but the walk keeps producing
			// queues from regions the SA can reach. Mirrors the AWS
			// scanner's per-region partial-failure posture.
			lastErr = err
			continue
		}
		successes++
		for _, q := range queues {
			if q == nil || q.Name == "" {
				continue
			}
			all = append(all, buildCloudTasksSnapshot(accountID, region, q))
		}
	}
	// If every configured region failed, surface the last error so the
	// dispatcher's two-way posture (Pub/Sub OK + Cloud Tasks all-regions-
	// fail) records a partial failure against the cloudtasks surface.
	// When at least one region succeeded, the walk returns nil error
	// even when other regions failed.
	// Poison-rate substrate slice 4 chunk 2 (v0.89.178, #820 Stream
	// 217) — enrich the honest-framing poison-rate Detail keys with
	// real Cloud Monitoring readings (failed task_attempt_count over a
	// trailing 1h window). Nil-tolerant on metricsClient: deployments
	// without the Cloud Monitoring wiring see this as a no-op and keep
	// the slice-3 §3.3 absent sentinels (cold-start parity). The
	// poison rate is measured on each queue itself, so no DLQ /
	// reachability gate is needed (unlike AWS SQS).
	// See docs/proposals/poison-rate-substrate-slice4.md §3-§5.
	s.enrichCloudTasksPoisonRate(ctx, all)

	// Consumer-lag substrate slice 5 chunk 1 (v0.89.182, #824 Stream
	// 221) — enrich the honest-framing BACKLOG lag keys with real
	// Cloud Monitoring readings (queue/depth peak over the trailing
	// window). Nil-tolerant on metricsClient (no-op → cold-start
	// parity). Silence keys stay honest-framed.
	// See docs/proposals/consumer-lag-substrate-slice5.md §3-§5.
	s.enrichCloudTasksLag(ctx, all)

	if successes == 0 && lastErr != nil {
		return all, lastErr
	}
	return all, nil
}

// cloudTasksRegions resolves the list of regions to walk. Precedence:
// scope.Regions (when non-empty, after empty-string trimming) → s.Region
// (when set) → gcpStandardCloudTasksRegions. Mirrors the workflows.go
// region-resolution pattern from v0.89.96.
func (s *Scanner) cloudTasksRegions(scope scanner.ScanScope) []string {
	if len(scope.Regions) > 0 {
		out := make([]string, 0, len(scope.Regions))
		for _, r := range scope.Regions {
			if strings.TrimSpace(r) != "" {
				out = append(out, r)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if s.Region != "" {
		return []string{s.Region}
	}
	// Defensive copy so callers can't mutate the package-level fallback.
	out := make([]string, len(gcpStandardCloudTasksRegions))
	copy(out, gcpStandardCloudTasksRegions)
	return out
}

// walkCloudTasksRegion lists queues in a single region, paginating via
// the standard nextPageToken, and returns the raw queue slice. Returns
// a non-nil error on the underlying list call's failure; the caller
// swallows the error and continues with the next region per the slice
// 5 partial-scan posture.
func (s *Scanner) walkCloudTasksRegion(ctx context.Context, client *http.Client, region string) ([]*cloudTasksQueue, error) {
	var (
		out       []*cloudTasksQueue
		pageToken string
	)
	for {
		listURL := s.cloudTasksListQueuesURL(region, pageToken)
		resp, err := s.cloudTasksGet(ctx, client, listURL)
		if err != nil {
			return out, err
		}
		out = append(out, resp.Queues...)
		if resp.NextPageToken == "" {
			return out, nil
		}
		pageToken = resp.NextPageToken
	}
}

// cloudTasksListQueuesURL constructs the full URL for
// projects.locations.queues.list. Uses s.endpoint when set (tests) or
// cloudTasksAPIBaseURL (production). pageToken is added as a query
// parameter when non-empty. Mirrors pubsubListTopicsURL's shape.
func (s *Scanner) cloudTasksListQueuesURL(region, pageToken string) string {
	base := s.endpoint
	if base == "" {
		base = cloudTasksAPIBaseURL
	}
	u := fmt.Sprintf("%s/v2/projects/%s/locations/%s/queues",
		strings.TrimRight(base, "/"), s.ProjectID, region)
	if pageToken != "" {
		u = u + "?pageToken=" + url.QueryEscape(pageToken)
	}
	return u
}

// cloudTasksGet issues a single GET against the supplied URL and
// decodes the JSON body. Non-2xx responses surface as an error with the
// operator-readable classification (403 / 404 / 429 / other); mirrors
// pubsubGet's shape with the cloudtasks.viewer remediation hint per
// design doc §12.
func (s *Scanner) cloudTasksGet(ctx context.Context, client *http.Client, listURL string) (*cloudTasksListQueuesResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %s", ServiceIDCloudTasks, err.Error())
	}
	req.Header.Set("Accept", "application/json")
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", ServiceIDCloudTasks, classifyCloudTasksTransportError(err))
	}
	defer httpResp.Body.Close()
	body, readErr := io.ReadAll(httpResp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("%s: read response body: %s", ServiceIDCloudTasks, truncate(readErr.Error(), 200))
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", ServiceIDCloudTasks, classifyCloudTasksListStatus(httpResp.StatusCode, body))
	}
	var resp cloudTasksListQueuesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%s: decode response: %s", ServiceIDCloudTasks, truncate(err.Error(), 200))
	}
	return &resp, nil
}

// buildCloudTasksSnapshot maps a cloudTasksQueue into the provider-
// agnostic EventSourceInstanceSnapshot per design doc §3 + §5:
//
//   - Provider="gcp" / Surface="cloudtasks" / SourceType="queue"
//   - AccountID = project id; Region = the per-region walker's region
//   - ResourceARN = queue.Name verbatim; ResourceName = trailing segment
//   - HasTraceAxis ← retryConfig is non-nil AND
//     (maxAttempts > 0 OR maxAttempts == CloudTasksMaxAttemptsUnlimited)
//   - HasLogAxis ← stackdriverLoggingConfig is non-nil AND
//     samplingRatio > 0
//   - Detail bag carries max_attempts / max_retry_duration /
//     stackdriver_sampling_ratio / max_dispatches_per_second /
//     max_concurrent_dispatches / state / purge_time per the §3
//     informational axes.
func buildCloudTasksSnapshot(accountID, region string, q *cloudTasksQueue) scanner.EventSourceInstanceSnapshot {
	snap := scanner.EventSourceInstanceSnapshot{
		Provider:     string(credstore.ProviderGCP),
		Surface:      CloudTasksSurface,
		AccountID:    accountID,
		Region:       region,
		ResourceARN:  q.Name,
		ResourceName: cloudTasksQueueNameFromResourceID(q.Name),
		SourceType:   CloudTasksSourceTypeQueue,
	}
	detail := map[string]any{}

	// §3 axis 1: retry config presence drives HasTraceAxis. Both
	// MaxAttempts > 0 AND MaxAttempts == -1 (unlimited) flip the axis;
	// MaxAttempts == 0 leaves it false.
	if q.RetryConfig != nil {
		if q.RetryConfig.MaxAttempts > 0 || q.RetryConfig.MaxAttempts == CloudTasksMaxAttemptsUnlimited {
			snap.HasTraceAxis = true
		}
		// Raw max_attempts surfaces even at 0 so the drilldown can
		// distinguish "absent retryConfig" (no entry) from "explicit
		// zero" (entry == 0). HasTraceAxis collapses both per §3; the
		// Detail bag preserves the distinction honestly.
		detail["max_attempts"] = q.RetryConfig.MaxAttempts
		if q.RetryConfig.MaxRetryDuration != "" {
			detail["max_retry_duration"] = q.RetryConfig.MaxRetryDuration
		}
	}

	// §3 axis 2: Stackdriver Logging samplingRatio > 0 drives
	// HasLogAxis. Absent stackdriverLoggingConfig OR explicit
	// samplingRatio == 0 leaves the axis false.
	if q.StackdriverLoggingConfig != nil {
		if q.StackdriverLoggingConfig.SamplingRatio > 0 {
			snap.HasLogAxis = true
		}
		// Raw samplingRatio surfaces even at 0 — drilldown layer
		// preserves absent-vs-zero distinction.
		detail["stackdriver_sampling_ratio"] = q.StackdriverLoggingConfig.SamplingRatio
	}

	// §3 informational axes — rate limits, state, purge time.
	if q.RateLimits != nil {
		if q.RateLimits.MaxDispatchesPerSecond > 0 {
			detail["max_dispatches_per_second"] = q.RateLimits.MaxDispatchesPerSecond
		}
		if q.RateLimits.MaxConcurrentDispatches > 0 {
			detail["max_concurrent_dispatches"] = q.RateLimits.MaxConcurrentDispatches
		}
		if q.RateLimits.MaxBurstSize > 0 {
			detail["max_burst_size"] = q.RateLimits.MaxBurstSize
		}
	}
	if q.State != "" {
		detail["state"] = q.State
	}
	if q.PurgeTime != nil && *q.PurgeTime != "" {
		detail["purge_time"] = *q.PurgeTime
	}
	if len(detail) > 0 {
		snap.Detail = detail
	}

	// DLQ configuration analysis slice 1 chunk 2 (v0.89.164, #806
	// Stream 203) — adds the three Cloud Tasks DLQ axis Detail keys
	// (has_dlq_pattern_likely, dlq_retry_count,
	// dlq_retry_count_in_band) per
	// docs/proposals/dlq-configuration-analysis-slice1.md §3.1.
	// ADDITIVE only — none of the slice-5 keys above are modified
	// here, so callers that have not yet adopted the DLQ axis keys
	// see byte-identical output to v0.89.163.
	applyCloudTasksDLQDetail(&snap, q)

	// Consumer lag detection slice 2 chunk 2 (v0.89.169, #811
	// Stream 208) — adds the four GCP Cloud Tasks lag axis Detail
	// keys (lag_backlog_depth, lag_backlog_depth_high,
	// lag_consumer_silence_seconds, lag_consumer_silence_high) per
	// docs/proposals/consumer-lag-detection-slice2.md §3.3 honest
	// framing (admin API does not surface task count as a metric;
	// always returns absent sentinels). ADDITIVE only — none of
	// the slice-5 + slice-1 (DLQ) keys above are modified here, so
	// callers that have not yet adopted the lag axis keys see
	// byte-identical output to v0.89.168.
	applyCloudTasksLagDetail(&snap, q)

	// Poison-message rate analysis slice 3 chunk 2 (v0.89.174,
	// #816 Stream 213) — adds the two GCP Cloud Tasks
	// poison-rate axis Detail keys (poison_rate_per_hour,
	// poison_rate_high_band) per
	// docs/proposals/poison-message-rate-slice3.md §3.3 honest
	// framing. ADDITIVE only — none of the slice-5 +
	// slice-1-DLQ + slice-2-lag keys above are modified here,
	// so callers that have not yet adopted the poison-rate
	// axis keys see byte-identical output to v0.89.173.
	applyCloudTasksPoisonRateDetail(&snap, q)

	return snap
}

// cloudTasksQueueNameFromResourceID extracts the queue name from a
// Cloud Tasks resource path:
//
//	projects/my-project/locations/us-central1/queues/my-queue
//	  → "my-queue"
//
// Empty input returns empty. Inputs without slashes return verbatim
// (defensive — the API guarantees the format but the helper shouldn't
// panic on an unexpected shape).
func cloudTasksQueueNameFromResourceID(name string) string {
	if name == "" {
		return ""
	}
	if i := strings.LastIndex(name, "/"); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return name
}

// buildCloudTasksHTTPClient returns the *http.Client the chunk-1 walk
// issues raw GETs against. Test path: s.httpClient verbatim.
// Production: reuses the shared oauth-backed client from
// buildOAuthHTTPClient so the SA JSON parse + scope union is computed
// once per scan. Error message NEVER embeds SA JSON bytes per the
// credstore substrate invariant. Mirrors buildPubSubHTTPClient.
func (s *Scanner) buildCloudTasksHTTPClient(ctx context.Context) (*http.Client, error) {
	if s.httpClient != nil {
		return s.httpClient, nil
	}
	oauthClient, err := s.buildOAuthHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	if oauthClient == nil {
		return nil, errors.New("gcp: nil oauth client (no SA JSON and no test client)")
	}
	return oauthClient, nil
}

// classifyCloudTasksListStatus maps a non-2xx response to an operator-
// readable PartialReason string. Same shape as
// classifyPubSubListStatus but with the cloudtasks.viewer remediation
// hint per design doc §12.
//
// 403 → permission denied (roles/cloudtasks.viewer hint).
// 404 → project / location not found.
// 429 → rate limit.
// Other → truncated message with HTTP code.
func classifyCloudTasksListStatus(code int, body []byte) string {
	switch code {
	case http.StatusForbidden:
		return "permission denied (verify the service account has roles/cloudtasks.viewer)"
	case http.StatusNotFound:
		return "project or location not found (verify project_id and region are correct)"
	case http.StatusTooManyRequests:
		return "rate limit exceeded mid-scan"
	default:
		msg := extractCloudTasksErrorMessage(body)
		return fmt.Sprintf("queues list failed (HTTP %d): %s", code, truncate(msg, 200))
	}
}

// classifyCloudTasksTransportError maps a transport / network failure
// (DNS, TCP, TLS, context cancellation) into the operator-readable
// PartialReason string. Mirrors classifyPubSubTransportError's shape.
func classifyCloudTasksTransportError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("network error: %s", truncate(err.Error(), 200))
}

// extractCloudTasksErrorMessage best-effort decodes the standard GCP
// error envelope. Falls back to the raw body when the envelope doesn't
// match (httptest fakes may return plain bodies). Mirrors
// extractPubSubErrorMessage.
func extractCloudTasksErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return string(body)
}
