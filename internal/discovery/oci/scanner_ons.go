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

// Event source tier slice 7 chunk 1 — OCI Notification Service scanner.
//
// Slice 7 (#791 design doc) closes the cross-cloud widening pass at
// 3-2-2-2 surfaces by adding OCI Notification Service (ONS) as the
// second OCI event source surface alongside Streaming. ONS serves the
// pub/sub fan-out pattern — the analog of AWS SNS + GCP Pub/Sub on
// the alert distribution side.
//
// The Logging detection axis mirrors the slice 1 chunk 4 Streaming
// pattern: the per-topic Logging /logs call uses the same searchTerm
// optimization, same OCI Logging endpoint, and same defensive
// Source.Resource side-check fallback.
//
// See docs/proposals/event-source-tier-slice7.md.

// notificationsListAPIVersion pins the OCI Notification Service
// /topics list API path version. OCI versions live in the path
// (e.g. "/20181201/") not a query parameter; the constant lives here
// so the scanner path construction is single-sourced. ONS uses a
// different version date than Streaming / Identity / Compute /
// Database / OKE / Functions — the constant keeps the per-surface
// version pin explicit.
const notificationsListAPIVersion = "20181201"

// notificationsEventSourceSurface is the Surface discriminator string
// for OCI Notification Service snapshots. The proposer's
// recommendation-kind prefix routing switches on
// "ons" -> OCI (slice 7 chunk 2). Mirrors
// streamingEventSourceSurface from slice 1 chunk 4.
const notificationsEventSourceSurface = "notifications"

// notificationsSourceTypeTopic is the SourceType discriminator string
// for ONS topic snapshots. Per-resource discriminator analogous to
// streamingSourceTypeStream / sourceTypeQueue / sourceTypeRule across
// the other event-source surfaces. Single-sourced here so the UI
// renderer's per-source-type column does not drift from the scanner.
const notificationsSourceTypeTopic = "ons_topic"

// ServiceIDONS is the slice 7 (event source tier — OCI Notification
// Service) service identifier the scanner reports against
// Result.FailedServices when the ONS walk produces a non-fatal error.
// Mirrors the streaming / database / OKE service identifiers; the
// per-provider connection model carries the provider discriminator
// separately, so the identifier is unprefixed.
//
// See docs/proposals/event-source-tier-slice7.md §11 acceptance tests
// 10 + the slice 6 partial-scan posture pattern.
const ServiceIDONS = "notifications"

// ociNotificationTopic is the bare JSON shape of an ONS topic returned
// by the /topics list call. Slice 7 chunk 1 reads:
//
//   - TopicID: OCID for the per-topic Logging /logs lookup AND for
//     ResourceARN on the snapshot. Note that the ONS list response
//     names the field "topicId" rather than the "id" name used by
//     Streaming (which is the broader OCI API convention) — the
//     scanner sticks with the wire shape.
//   - Name: human-readable name, used for ResourceName.
//   - CompartmentID: for per-topic Logging searchTerm construction.
//   - LifecycleState: surfaced in Detail.lifecycle_state per design
//     doc §3 (informational only — Squadron does not gate the snapshot
//     on lifecycle state since a CREATING / DELETING topic is still
//     operationally meaningful).
//   - ShortTopicID + KmsKeyID: surfaced in Detail (informational only
//     per design doc §3 — kms_key_id_set / short_topic_id_set
//     booleans; the raw OCID values do not surface to the UI). The
//     scanner records that they exist without leaking the value.
//
// Subscription count is NOT recorded on the topic snapshot — slice 7
// chunk 1 records only what the /topics list response directly carries
// to keep the walk single-pass per topic. The per-topic
// /subscriptions?topicId= walk is deferred to slice 8+ candidate
// (design doc §13: "ONS Subscription confirmation lag detection").
//
// Pagination follows the opc-next-page response header (see
// listNotificationTopicsAll below), same convention as Streaming.
type ociNotificationTopic struct {
	TopicID        string `json:"topicId"`
	Name           string `json:"name"`
	CompartmentID  string `json:"compartmentId"`
	LifecycleState string `json:"lifecycleState"`
	ShortTopicID   string `json:"shortTopicId,omitempty"`
	KmsKeyID       string `json:"kmsKeyId,omitempty"`
}

// ociNotificationTopicList is the JSON envelope returned by the ONS
// /topics list call. OCI returns the list directly as a JSON array,
// matching the Streaming /streams convention.
type ociNotificationTopicList = []ociNotificationTopic

// ScanNotificationTopics walks the OCI Notification Service surface
// for the configured scope. Two-pass walk: per compartment list topics
// (paginated via opc-next-page), then per topic call the OCI Logging
// /logs endpoint with searchTerm=<topic.id> to detect whether a log
// group attaches to the topic.
//
// Detection per docs/proposals/event-source-tier-slice7.md §3:
//
//   - HasLogAxis  <- the OCI Logging /logs call returns at least one
//     resource whose Configuration.Source.Resource equals the topic's
//     OCID. A topic with no Logging entry leaves the axis false.
//   - HasTraceAxis <- same as HasLogAxis. ONS does not have a
//     first-class OCI APM integration; like Streaming in slice 1,
//     OCI Logging is the closest observability proxy and carries
//     both axes. Slice 8+ may separate the two when OCI exposes a
//     more granular detection.
//
// Scope semantics: scope.AccountID overrides the per-snapshot
// AccountID stamped on every row; empty falls back to the scanner's
// configured TenancyOCID. scope.CompartmentIDs narrows the walk to a
// subset of compartments; empty defaults to "tenancy root + first-
// level children" via the existing compartmentsForEventSource helper
// (shared with ScanStreams per slice 7 design doc §5).
//
// IAM contract per docs/proposals/event-source-tier-slice7.md §12:
// "read ons-topics in compartment" (ONS) + the existing Logging read
// policy already in the IAM template from slice 1 (covers the
// per-topic detection call). All read-only; Squadron never executes
// a PublishMessage / CreateTopic / DeleteTopic mutation.
//
// Partial-scan semantics: a per-compartment list failure or a
// per-topic Logging call failure skips that compartment / topic but
// continues walking the rest. The chunk 5 trampoline (existing) wraps
// this entry point and records partial failures via
// recordPartialFailure; the ScanNotificationTopics method itself
// swallows per-compartment failures silently (returning the partial
// snapshot slice) so the slice 7 two-way dispatcher in
// ScanEventSources can apply its own partial-scan posture cleanly.
func (s *Scanner) ScanNotificationTopics(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	// Substrate validation. The Scan entry point does this on the
	// way in; ScanNotificationTopics guards defensively at its own
	// entry point so the chunk 5 trampoline (and the slice 7 two-way
	// dispatcher) can call this method directly without
	// re-validating.
	if s.TenancyOCID == "" {
		return nil, errors.New("oci: TenancyOCID is required")
	}
	if s.Region == "" {
		return nil, errors.New("oci: Region is required")
	}

	signingKey, parseErr := s.signingKey()
	if parseErr != nil {
		return nil, fmt.Errorf("oci: %s: signing failed: %w", ServiceIDONS, parseErr)
	}

	// Determine the compartment set. Shared with ScanStreams per
	// design doc §5 — same default behavior keeps the two-way
	// dispatcher's per-surface walks aligned on the same compartment
	// set when scope.CompartmentIDs is empty.
	compartments, err := s.compartmentsForEventSource(ctx, signingKey, scope)
	if err != nil {
		return nil, fmt.Errorf("oci: %s: compartment listing failed: %w", ServiceIDONS, err)
	}

	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.TenancyOCID
	}

	var snapshots []scanner.EventSourceInstanceSnapshot
	for _, comp := range compartments {
		topics, topicsErr := s.listNotificationTopicsAll(ctx, signingKey, comp.ID)
		if topicsErr != nil {
			// Partial failure on this compartment's topics walk —
			// skip this compartment but continue walking the rest.
			// The chunk 5 integration will surface this via
			// recordPartialFailure when called through Scan.
			continue
		}
		for _, topic := range topics {
			snap := s.projectOCITopic(ctx, signingKey, topic, comp.ID, accountID)
			snapshots = append(snapshots, snap)
		}
	}
	return snapshots, nil
}

// listNotificationTopicsAll walks every page of /topics for a single
// compartment. OCI signals additional pages via the opc-next-page
// response header; the loop passes that token back as the page=
// <token> query parameter on the next call. An empty or missing
// header terminates the loop. Mirrors listStreamsAll in
// scanner_streaming.go — same pagination convention.
func (s *Scanner) listNotificationTopicsAll(ctx context.Context, sk *SigningKey, compartmentID string) ([]ociNotificationTopic, error) {
	var all []ociNotificationTopic
	nextPage := ""
	for {
		page, nextToken, callErr := s.listNotificationTopicsPage(ctx, sk, compartmentID, nextPage)
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

// listNotificationTopicsPage walks one page of /topics. The returned
// nextPage string is the opc-next-page header value (empty when there
// are no more pages).
func (s *Scanner) listNotificationTopicsPage(ctx context.Context, sk *SigningKey, compartmentID, page string) ([]ociNotificationTopic, string, error) {
	endpoint := s.notificationsEndpoint()
	u := fmt.Sprintf(
		"%s/%s/topics?compartmentId=%s",
		strings.TrimRight(endpoint, "/"),
		notificationsListAPIVersion,
		url.QueryEscape(compartmentID),
	)
	if page != "" {
		u = u + "&page=" + url.QueryEscape(page)
	}
	body, nextPage, callErr := s.doSignedGETWithPage(ctx, sk, u)
	if callErr != nil {
		return nil, "", callErr
	}
	var out ociNotificationTopicList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, "", &ociCallError{Wrapped: fmt.Errorf("topics response parse: %w", jerr)}
	}
	return out, nextPage, nil
}

// listLogsForTopic walks /logs with searchTerm=<topic.id> and returns
// true when the response carries at least one log resource whose
// Configuration.Source.Resource equals the supplied topic OCID. The
// detection is defensive: even if the searchTerm optimization is
// unsupported on a particular tenancy and the /logs call returns the
// entire compartment-wide log list, the per-resource Source.Resource
// side-check still flips HasLogAxis correctly when (and only when)
// one of the entries references the topic's OCID.
//
// Mirrors listLogsForStream from slice 1 chunk 4 exactly — same OCI
// Logging endpoint, same searchTerm convention, same defensive
// Source.Resource side-check. The slice 7 design doc §5 calls out
// this parallel: both surfaces use OCI Logging as the closest
// observability integration, so the per-resource detection helper
// has identical wire shape across both surfaces.
//
// A failure on the Logging call (network error, rate limit, 5xx)
// returns (false, error) so the caller can decide whether to dim
// the axis or accept the per-topic miss. The chunk 1 projectOCITopic
// wrapper treats a Logging failure as "axis defaults to false" and
// keeps walking — a single failing /logs call must not abort the
// whole scan.
func (s *Scanner) listLogsForTopic(ctx context.Context, sk *SigningKey, compartmentID, topicID string) (bool, error) {
	// Slice 11 chunk 1 (v0.89.161, #803 Stream 200) consolidated the
	// three per-surface Logging detection helpers into a single
	// shared listLogsForOCIResource (lives in scanner_streaming.go).
	// This wrapper is retained for call-site stability — the slice
	// 7 chunk 1 projectOCITopic + every test that exercises the ONS
	// Logging detection axis continue to find the helper under its
	// original name.
	return s.listLogsForOCIResource(ctx, sk, compartmentID, topicID)
}

// notificationsEndpoint returns the OCI Notification Service
// control-plane API base URL. When ociEndpoint is set (tests), it's
// used directly — the test mock dispatches /topics + /logs on the
// same httptest server that already routes the compartments path. In
// production the per-region ONS endpoint pattern is
// https://notification.<region>.oraclecloud.com (note: distinct from
// the Streaming endpoint family which uses .oci.oraclecloud.com —
// OCI's per-service hostname conventions are not uniform).
func (s *Scanner) notificationsEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://notification.%s.oraclecloud.com", s.Region)
}

// projectOCITopic maps an ociNotificationTopic into the
// provider-agnostic EventSourceInstanceSnapshot. The slice 7 chunk 1
// mapping is:
//
//   - Provider: "oci".
//   - Surface: "notifications".
//   - AccountID: the per-scan tenancy OCID (the scope-provided
//     value or the scanner's TenancyOCID fallback).
//   - Region: the scanner's configured Region — ONS's response does
//     not echo region per-row.
//   - ResourceName: the topic's human-readable Name.
//   - ResourceARN: the topic's OCID (used for Logging detection).
//   - SourceType: "ons_topic" (per-surface discriminator).
//   - HasLogAxis: true when listLogsForTopic returns true (any log
//     resource for the compartment carries a
//     Configuration.Source.Resource matching the topic's OCID).
//   - HasTraceAxis: same as HasLogAxis per design doc §3 (OCI Logging
//     is the closest observability proxy for ONS; slice 8+ may
//     separate the two when OCI exposes a more granular detection).
//   - HasPropagationConfig: NOT applicable to ONS (slice 2 per-
//     message propagation was Streaming-specific via the retention
//     threshold; ONS topics do not expose an equivalent per-message
//     header-preservation knob). Left at the zero value (false) and
//     PropagationNotes left nil — the UI's propagation column on the
//     event-sources tab renders "—" for ONS rows accordingly.
//   - Detail: lifecycle_state, compartment_id, has_log_group,
//     short_topic_id_set (informational booleans without leaking the
//     raw OCID values — see slice 7 design doc §3).
func (s *Scanner) projectOCITopic(ctx context.Context, sk *SigningKey, topic ociNotificationTopic, compartmentID, accountID string) scanner.EventSourceInstanceSnapshot {
	snap := scanner.EventSourceInstanceSnapshot{
		Provider:     providerOCIEventSource,
		Surface:      notificationsEventSourceSurface,
		AccountID:    accountID,
		Region:       s.Region,
		ResourceName: topic.Name,
		ResourceARN:  topic.TopicID,
		SourceType:   notificationsSourceTypeTopic,
	}
	hasLog, logErr := s.listLogsForTopic(ctx, sk, compartmentID, topic.TopicID)
	if logErr == nil && hasLog {
		snap.HasLogAxis = true
		// Slice 7 chunk 1 OCI Logging proxy: a topic with a log group
		// is the proxy for trace observability readiness since OCI
		// Logging is the closest existing observability integration
		// for ONS (the surface has no first-class OCI APM
		// integration). Slice 8+ may separate this from HasLogAxis
		// if OCI exposes a granular per-event trace detection.
		snap.HasTraceAxis = true
	}
	snap.Detail = map[string]any{
		"lifecycle_state":     topic.LifecycleState,
		"compartment_id":      compartmentID,
		"has_log_group":       snap.HasLogAxis,
		"short_topic_id_set":  topic.ShortTopicID != "",
		"kms_key_id_set":      topic.KmsKeyID != "",
	}
	return snap
}

// classifyOCIONSError maps an OCI Notification Service / Logging call
// failure into the operator-visible PartialReason string under the
// notifications service identifier. Parallels
// classifyOCIStreamingError so the audit consumer sees identical
// structure across both OCI event source surfaces.
//
// Slice 7 chunk 1 ships this helper for the chunk 5 trampoline to
// surface partial failures via recordPartialFailure; the chunk 1
// ScanNotificationTopics entry point itself swallows per-compartment
// failures silently (returning the partial snapshot slice). The
// helper is unused inside ScanNotificationTopics but exported here so
// the chunk 5 integration finds it under the same name family.
//
// Error mappings (per the slice 7 design doc §12 threat model):
//
//   - HTTP 401 -> credentials_invalid (signature rejected).
//   - HTTP 403 -> permission_denied with hint pointing at the
//     "read ons-topics in compartment" policy statement.
//   - HTTP 404 mid-walk -> empty string (silent skip — many
//     compartments have no ONS topics; surfacing 404s as partial
//     failures would be noise).
//   - HTTP 429 -> rate_limit.
//   - Transport / network errors -> network-error with truncated
//     underlying error.
//   - Any other 4xx/5xx -> truncated message under the notifications
//     identifier.
func classifyOCIONSError(err error, atRoot bool) string {
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
			return fmt.Sprintf("%s: network error: %s", ServiceIDONS, truncate(wrapped, 200))
		}
		if oce.StatusCode == http.StatusTooManyRequests || oce.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDONS)
		}
		switch oce.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check fingerprint, user_ocid, and private key)", ServiceIDONS)
		case http.StatusForbidden:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: permission denied (verify the policy grants 'read ons-topics in compartment'): %s", ServiceIDONS, truncate(msg, 200))
		case http.StatusNotFound:
			if atRoot {
				return fmt.Sprintf("%s: tenancy root has no ONS topics or compartment not found", ServiceIDONS)
			}
			return ""
		}
		msg := oce.Message
		if msg == "" {
			msg = oce.BodyHint
		}
		return fmt.Sprintf("%s: http %d: %s", ServiceIDONS, oce.StatusCode, truncate(msg, 200))
	}
	return fmt.Sprintf("%s: %s", ServiceIDONS, truncate(err.Error(), 200))
}
