// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Event source tier slice 9 chunk 1 — OCI Queue Service scanner.
//
// Slice 9 (#797 design doc) brings OCI to parity with AWS + Azure on
// the event source tier at 3 surfaces by adding Queue Service as the
// third OCI event source surface alongside Streaming and Notification
// Service. Queue Service is the transactional FIFO queue primitive
// analogous to AWS SQS — distinct from ONS pub/sub fan-out (one
// consumer per message vs. many-consumer fan-out).
//
// The Logging detection axis mirrors the slice 1 Streaming and slice 7
// ONS patterns: same OCI Logging /logs endpoint, same searchTerm
// optimization, same defensive Source.Resource side-check fallback.
//
// See docs/proposals/event-source-tier-slice9.md.

// queuesListAPIVersion pins the OCI Queue Service /queues list API
// path version. Queue Service uses a different version date than
// Streaming (20180418) and ONS (20181201) — the constant keeps the
// per-surface version pin explicit.
const queuesListAPIVersion = "20210201"

// queuesEventSourceSurface is the Surface discriminator string for
// OCI Queue Service snapshots. The proposer's recommendation-kind
// prefix routing switches on "queues" -> OCI (slice 9 chunk 2).
const queuesEventSourceSurface = "queues"

// queuesSourceTypeQueue is the SourceType discriminator string for
// OCI Queue snapshots. Per-resource discriminator analogous to
// streamingSourceTypeStream / notificationsSourceTypeTopic across the
// OCI event-source surfaces.
const queuesSourceTypeQueue = "queue"

// ServiceIDQueue is the slice 9 (event source tier — OCI Queue
// Service) service identifier the scanner reports against
// Result.FailedServices when the Queue walk produces a non-fatal
// error. Mirrors ServiceIDEventSource (streaming) and ServiceIDONS
// (notifications); the per-provider connection model carries the
// provider discriminator separately, so the identifier is unprefixed.
//
// See docs/proposals/event-source-tier-slice9.md §11 acceptance tests.
const ServiceIDQueue = "queues"

// ociQueue is the bare JSON shape of an OCI Queue returned by the
// /20210201/queues list call. Slice 9 chunk 1 reads:
//
//   - ID: OCID for the per-queue Logging /logs lookup AND for
//     ResourceARN on the snapshot.
//   - DisplayName: human-readable name, used for ResourceName.
//   - CompartmentID: for per-queue Logging searchTerm construction.
//   - LifecycleState: surfaced in Detail.lifecycle_state per design
//     doc §3 (informational only).
//   - VisibilityInSeconds + RetentionInSeconds: surfaced in Detail
//     (informational only per design doc §3) — operationally
//     meaningful for substrate analysis but not load-bearing for
//     slice 9's Logging axis.
//   - DeadLetterQueueDeliveryCount: surfaced in Detail as the
//     dead_letter_queue_delivery_count signal (informational only —
//     DLQ configuration analysis is slice 10+ territory per design
//     doc §13).
//   - CustomEncryptionKeyID: surfaced in Detail as the
//     kms_key_id_set boolean (informational only; the raw OCID does
//     NOT surface to the UI).
//
// Pagination follows the opc-next-page response header (see
// listQueuesAll below), same convention as Streaming + ONS.
type ociQueue struct {
	ID                           string `json:"id"`
	DisplayName                  string `json:"displayName"`
	CompartmentID                string `json:"compartmentId"`
	LifecycleState               string `json:"lifecycleState"`
	VisibilityInSeconds          int    `json:"visibilityInSeconds,omitempty"`
	RetentionInSeconds           int    `json:"retentionInSeconds,omitempty"`
	DeadLetterQueueDeliveryCount int    `json:"deadLetterQueueDeliveryCount,omitempty"`
	CustomEncryptionKeyID        string `json:"customEncryptionKeyId,omitempty"`
	// MessagesEndpoint is the per-queue data-plane base URL
	// (QueueSummary.messagesEndpoint). It is the host the GetStats /
	// GetMessages / PutMessages calls target — distinct from the
	// control-plane queues endpoint. Poison-DEPTH detection (#159,
	// v0.89.305) signs a GET against
	// {messagesEndpoint}/20210201/queues/{id}/stats to read the DLQ's
	// visibleMessages. Empty when OCI omits it (older API) → the depth
	// signal safe-degrades to the absent sentinel.
	MessagesEndpoint string `json:"messagesEndpoint,omitempty"`
	// Consumer lag detection slice 2 chunk 4 (v0.89.171, #813
	// Stream 210) — runtimeMetadata carries the lag axis source
	// fields (visibleMessages + timeStateLastChanged). Optional in
	// the list response; absent values surface as the absent
	// sentinel through detectOCIQueueLag.
	RuntimeMetadata *ociQueueRuntimeMetadata `json:"runtimeMetadata,omitempty"`
}

// ociQueueRuntimeMetadata is the OCI Queue Service per-queue
// runtimeMetadata block. Slice 2 chunk 4 reads visibleMessages
// (backlog depth surrogate) + timeStateLastChanged (consumer
// silence surrogate). Other runtimeMetadata fields are not yet
// used; if OCI extends the payload they can be added here without
// breaking existing parsing (additive shape).
type ociQueueRuntimeMetadata struct {
	VisibleMessages      int    `json:"visibleMessages,omitempty"`
	TimeStateLastChanged string `json:"timeStateLastChanged,omitempty"`
}

// ociQueueList is the JSON envelope returned by the Queue Service
// /queues list call. OCI returns the list directly as a JSON array,
// matching the Streaming + ONS conventions.
type ociQueueList = []ociQueue

// ScanQueues walks the OCI Queue Service surface for the configured
// scope. Two-pass walk: per compartment list queues (paginated via
// opc-next-page), then per queue call the OCI Logging /logs endpoint
// with searchTerm=<queue.id> to detect whether a log group attaches
// to the queue.
//
// Detection per docs/proposals/event-source-tier-slice9.md §3:
//
//   - HasLogAxis  <- the OCI Logging /logs call returns at least one
//     resource whose Configuration.Source.Resource equals the queue's
//     OCID. A queue with no Logging entry leaves the axis false.
//   - HasTraceAxis <- same as HasLogAxis. Queue Service does not have
//     a first-class OCI APM integration; OCI Logging is the closest
//     observability proxy and carries both axes per the established
//     slice 1 + slice 7 pattern.
//
// Scope semantics: scope.AccountID overrides the per-snapshot
// AccountID stamped on every row; empty falls back to the scanner's
// configured TenancyOCID. scope.CompartmentIDs narrows the walk to a
// subset of compartments; empty defaults to "tenancy root + first-
// level children" via the shared compartmentsForEventSource helper
// (shared with ScanStreams + ScanNotificationTopics per slice 9
// design doc §5).
//
// IAM contract per docs/proposals/event-source-tier-slice9.md §12:
// "read queues in compartment" (Queue Service) + the existing slice 1
// Logging read policy already in the IAM template covers the
// per-queue detection call. All read-only; Squadron never executes a
// PutMessages / CreateQueue / DeleteQueue mutation.
//
// Partial-scan semantics: a per-compartment list failure or a
// per-queue Logging call failure skips that compartment / queue but
// continues walking the rest. The slice 9 three-way dispatcher in
// ScanEventSources applies its own partial-scan posture cleanly on
// top of this method's nil-error return.
func (s *Scanner) ScanQueues(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	if s.TenancyOCID == "" {
		return nil, errors.New("oci: TenancyOCID is required")
	}
	if s.Region == "" {
		return nil, errors.New("oci: Region is required")
	}

	signingKey, parseErr := s.signingKey()
	if parseErr != nil {
		return nil, fmt.Errorf("oci: %s: signing failed: %w", ServiceIDQueue, parseErr)
	}

	compartments, err := s.compartmentsForEventSource(ctx, signingKey, scope)
	if err != nil {
		return nil, fmt.Errorf("oci: %s: compartment listing failed: %w", ServiceIDQueue, err)
	}

	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.TenancyOCID
	}

	var snapshots []scanner.EventSourceInstanceSnapshot
	for _, comp := range compartments {
		queues, queuesErr := s.listQueuesAll(ctx, signingKey, comp.ID)
		if queuesErr != nil {
			continue
		}
		for _, queue := range queues {
			snap := s.projectOCIQueue(ctx, signingKey, queue, comp.ID, accountID)
			snapshots = append(snapshots, snap)
		}
	}

	// Poison-rate substrate slice 4 chunk 4 (v0.89.181, #823 Stream
	// 220) — enrich the honest-framing poison-rate Detail keys with
	// real OCI Monitoring readings (dead-letter gauge max-min delta
	// over a trailing 1h window). FINAL cloud — CLOSES the substrate
	// arc. Nil-tolerant on monitoringClient: deployments without the
	// Monitoring wiring see this as a no-op and keep the slice-3 §3.3
	// absent sentinels (cold-start parity).
	// See docs/proposals/poison-rate-substrate-slice4.md §3-§5.
	s.enrichOCIQueuePoisonRate(ctx, snapshots)

	return snapshots, nil
}

// listQueuesAll walks every page of /queues for a single compartment.
// OCI signals additional pages via the opc-next-page response header;
// the loop passes that token back as the page=<token> query parameter
// on the next call. Mirrors listStreamsAll and listNotificationTopicsAll.
func (s *Scanner) listQueuesAll(ctx context.Context, sk *SigningKey, compartmentID string) ([]ociQueue, error) {
	var all []ociQueue
	nextPage := ""
	for {
		page, nextToken, callErr := s.listQueuesPage(ctx, sk, compartmentID, nextPage)
		if callErr != nil {
			return nil, callErr
		}
		all = append(all, page...)
		if nextToken == "" {
			break
		}
		nextPage = nextToken
	}
	return all, nil
}

// listQueuesPage walks one page of /queues. The returned nextPage
// string is the opc-next-page header value (empty when there are no
// more pages).
func (s *Scanner) listQueuesPage(ctx context.Context, sk *SigningKey, compartmentID, page string) ([]ociQueue, string, error) {
	endpoint := s.queuesEndpoint()
	u := fmt.Sprintf(
		"%s/%s/queues?compartmentId=%s",
		strings.TrimRight(endpoint, "/"),
		queuesListAPIVersion,
		url.QueryEscape(compartmentID),
	)
	if page != "" {
		u = u + "&page=" + url.QueryEscape(page)
	}
	body, nextPage, callErr := s.doSignedGETWithPage(ctx, sk, u)
	if callErr != nil {
		return nil, "", callErr
	}
	var out ociQueueList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, "", &ociCallError{Wrapped: fmt.Errorf("queues response parse: %w", jerr)}
	}
	return out, nextPage, nil
}

// listLogsForQueue walks /logs with searchTerm=<queue.id> and returns
// true when the response carries at least one log resource whose
// Configuration.Source.Resource equals the supplied queue OCID.
// Mirrors listLogsForStream and listLogsForTopic exactly — same OCI
// Logging endpoint, same searchTerm convention, same defensive
// Source.Resource side-check fallback.
//
// A failure on the Logging call (network error, rate limit, 5xx)
// returns (false, error) so the caller can decide whether to dim the
// axis or accept the per-queue miss. The slice 9 chunk 1 projectOCIQueue
// wrapper treats a Logging failure as "axis defaults to false" and
// keeps walking — a single failing /logs call must not abort the whole
// scan.
func (s *Scanner) listLogsForQueue(ctx context.Context, sk *SigningKey, compartmentID, queueID string) (bool, error) {
	// Slice 11 chunk 1 (v0.89.161, #803 Stream 200) consolidated the
	// three per-surface Logging detection helpers into a single
	// shared listLogsForOCIResource (lives in scanner_streaming.go).
	// This wrapper is retained for call-site stability — the slice
	// 9 chunk 1 projectOCIQueue + every test that exercises the
	// Queue Service Logging detection axis continue to find the
	// helper under its original name.
	return s.listLogsForOCIResource(ctx, sk, compartmentID, queueID)
}

// queuesEndpoint returns the OCI Queue Service control-plane API base
// URL. When ociEndpoint is set (tests), it's used directly. In
// production the per-region Queue Service endpoint pattern is
// https://messaging.<region>.oci.oraclecloud.com.
func (s *Scanner) queuesEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://messaging.%s.oci.oraclecloud.com", s.Region)
}

// projectOCIQueue maps an ociQueue into the provider-agnostic
// EventSourceInstanceSnapshot per the slice 9 chunk 1 contract:
//
//   - Provider: "oci".
//   - Surface: "queues".
//   - AccountID: the per-scan tenancy OCID.
//   - Region: the scanner's configured Region.
//   - ResourceName: the queue's DisplayName.
//   - ResourceARN: the queue's OCID (used for Logging detection).
//   - SourceType: "queue" (per-surface discriminator).
//   - HasLogAxis / HasTraceAxis: both flip when the Logging axis is
//     satisfied (mirrors slice 1 + slice 7 pattern).
//   - HasPropagationConfig: NOT applicable to Queue Service.
//   - Detail: lifecycle_state, compartment_id, has_log_group,
//     visibility_in_seconds, retention_in_seconds,
//     dead_letter_queue_delivery_count, kms_key_id_set.
func (s *Scanner) projectOCIQueue(ctx context.Context, sk *SigningKey, queue ociQueue, compartmentID, accountID string) scanner.EventSourceInstanceSnapshot {
	snap := scanner.EventSourceInstanceSnapshot{
		Provider:     providerOCIEventSource,
		Surface:      queuesEventSourceSurface,
		AccountID:    accountID,
		Region:       s.Region,
		ResourceName: queue.DisplayName,
		ResourceARN:  queue.ID,
		SourceType:   queuesSourceTypeQueue,
	}
	hasLog, logErr := s.listLogsForQueue(ctx, sk, compartmentID, queue.ID)
	if logErr == nil && hasLog {
		snap.HasLogAxis = true
		snap.HasTraceAxis = true
	}
	snap.Detail = map[string]any{
		"lifecycle_state":                  queue.LifecycleState,
		"compartment_id":                   compartmentID,
		"has_log_group":                    snap.HasLogAxis,
		"visibility_in_seconds":            queue.VisibilityInSeconds,
		"retention_in_seconds":             queue.RetentionInSeconds,
		"dead_letter_queue_delivery_count": queue.DeadLetterQueueDeliveryCount,
		"kms_key_id_set":                   queue.CustomEncryptionKeyID != "",
	}

	// DLQ configuration analysis slice 1 chunk 4 (v0.89.166, #808
	// Stream 205) — adds the three OCI Queue Service DLQ axis
	// Detail keys (has_dlq, dlq_retry_count,
	// dlq_retry_count_in_band) per
	// docs/proposals/dlq-configuration-analysis-slice1.md §3 +
	// §11.14-16. ADDITIVE only — none of the slice-9 keys above are
	// modified here, so callers that have not yet adopted the DLQ
	// axis keys see byte-identical output to v0.89.165.
	applyOCIQueueDLQDetail(&snap, queue)

	// Consumer lag detection slice 2 chunk 4 (v0.89.171, #813
	// Stream 210) — adds the four OCI Queue Service lag axis
	// Detail keys (lag_backlog_depth, lag_backlog_depth_high,
	// lag_consumer_silence_seconds, lag_consumer_silence_high) per
	// docs/proposals/consumer-lag-detection-slice2.md §3 + §11.7-8.
	// ADDITIVE only — none of the slice-9 + slice-1-DLQ keys above
	// are modified here, so callers that have not yet adopted the
	// lag axis keys see byte-identical output to v0.89.170.
	applyOCIQueueLagDetail(&snap, queue)

	// Poison-message rate analysis slice 3 chunk 4 (v0.89.176,
	// #818 Stream 215) — adds the two OCI Queue Service poison-rate
	// axis Detail keys (poison_rate_per_hour, poison_rate_high_band)
	// per docs/proposals/poison-message-rate-slice3.md §3.3. §3.3
	// honest framing: both keys hard-coded to absent state until a
	// future slice integrates the OCI Monitoring substrate. ADDITIVE
	// only — none of the slice-9 + slice-1-DLQ + slice-2-lag keys
	// above are modified here, so callers that have not yet adopted
	// the poison-rate axis keys see byte-identical output to
	// v0.89.175.
	applyOCIQueuePoisonRateDetail(&snap, queue)

	// Poison-DEPTH signal (#159, v0.89.305) — the honest,
	// always-available "poison present" signal that closes what the
	// rate axis above could not: the oci_queue Monitoring namespace
	// has no dead-letter metric, but the Queue Service DATA-PLANE
	// GetStats call exposes the DLQ's visibleMessages directly. This
	// reads it best-effort (nil-tolerant: no DLQ configured, unknown
	// messagesEndpoint, or a failed call all yield the -1 absent
	// sentinel) and writes poison_dlq_depth + poison_dlq_nonempty,
	// mirroring AWS SQS DLQ-depth (#156). ADDITIVE only — no prior
	// Detail key is modified.
	applyOCIQueuePoisonDepthDetail(&snap, s.queueDLQDepth(ctx, sk, queue))

	return snap
}

// classifyOCIQueueError maps an OCI Queue Service / Logging call
// failure into the operator-visible PartialReason string under the
// queues service identifier. Parallels classifyOCIStreamingError and
// classifyOCIONSError so the audit consumer sees identical structure
// across all three OCI event source surfaces.
func classifyOCIQueueError(err error, atRoot bool) string {
	if err == nil {
		return ""
	}
	var oce *ociCallError
	if errors.As(err, &oce) {
		if oce.IsNetwork {
			wrapped := ""
			if oce.Wrapped != nil {
				wrapped = oce.Wrapped.Error()
			}
			return fmt.Sprintf("%s: network error: %s", ServiceIDQueue, truncate(wrapped, 200))
		}
		if oce.StatusCode == http.StatusTooManyRequests || oce.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDQueue)
		}
		switch oce.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check fingerprint, user_ocid, and private key)", ServiceIDQueue)
		case http.StatusForbidden:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: permission denied (verify the policy grants 'read queues in compartment'): %s", ServiceIDQueue, truncate(msg, 200))
		case http.StatusNotFound:
			if atRoot {
				return fmt.Sprintf("%s: tenancy root has no queues or compartment not found", ServiceIDQueue)
			}
			return ""
		}
		msg := oce.Message
		if msg == "" {
			msg = oce.BodyHint
		}
		return fmt.Sprintf("%s: http %d: %s", ServiceIDQueue, oce.StatusCode, truncate(msg, 200))
	}
	return fmt.Sprintf("%s: %s", ServiceIDQueue, truncate(err.Error(), 200))
}
