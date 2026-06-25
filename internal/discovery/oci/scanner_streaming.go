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

// OCIStreamingLogCategoryAllEvents is the OCI Logging service-defined
// category for all stream events. A stream with a log group configured
// for this category satisfies HasLogAxis per §3.4 of the event source
// tier slice 1 design doc. Slice 1 chunk 4 does NOT require the category
// value to match exactly — the proxy is "any log resource references
// the stream's OCID at all" — but the constant is kept here as the
// canonical sentinel so slice 2's per-category detection can pattern
// match against it without scanner_streaming.go drift.
const OCIStreamingLogCategoryAllEvents = "all"

// ServiceIDEventSource is the slice 1 (event source tier) service
// identifier the scanner reports against Result.FailedServices when
// the OCI Streaming walk produces a non-fatal error. Mirrors the
// compute / database / OKE / functions service identifiers
// ("ocicompute" / "ocidb" / "oke" / "ocifunc"); the per-provider
// connection model carries the provider discriminator separately, so
// the identifier is unprefixed.
//
// See docs/proposals/event-source-tier-slice1.md §10 (chunks list)
// + §11 acceptance tests 10-11. Chunk 1 chose "streaming" as the
// per-snapshot Surface discriminator; the service identifier follows
// the same family naming so the audit consumer's per-row
// FailedServices entries pattern-match cleanly across the scanner
// and proposer sides.
const ServiceIDEventSource = "streaming"

// streamingListAPIVersion pins the OCI Streaming /streams list API
// path version. OCI versions live in the path (e.g. "/20180418/") not
// a query parameter; the constant lives here so the scanner path
// construction is single-sourced. The Streaming surface uses a
// different version date than Identity / Compute / Database / OKE /
// Functions — the constant keeps the per-surface version pin
// explicit.
const streamingListAPIVersion = "20180418"

// loggingListAPIVersion pins the OCI Logging /logs list API path
// version. Single-sourced for the same reason as the other per-
// surface version constants.
const loggingListAPIVersion = "20200531"

// streamingEventSourceSurface is the Surface discriminator string for
// OCI Streaming snapshots. The proposer's recommendation-kind prefix
// routing switches on "eventbridge" -> AWS, "pubsub" -> GCP,
// "servicebus" -> Azure, "streaming" -> OCI. Slice 1 chunk 4 of the
// Event source tier arc.
const streamingEventSourceSurface = "streaming"

// OCIStreamingRetentionPropagationThresholdHours is the minimum
// retentionInHours value at which Squadron considers OCI Streaming's
// Kafka header preservation reliable. Streams with shorter retention
// may truncate headers in some OCI Streaming versions per the
// per-message metadata budget — the design doc §3.4 of
// event-source-tier-slice2.md describes the heuristic.
//
// Slice 2 chunk 4 (v0.89.106, #744 Stream 142) uses 24h as the
// threshold per §3.4 of the design doc. Operators with deliberately
// shorter retention for cost reasons can decline the recommendation;
// the verdict learning loop records the decline so the recommender
// surfaces the operator's preference back on next scan.
//
// See docs/proposals/event-source-tier-slice2.md §3.4 (detection
// surface) and §12 (threat model — 24h threshold tuning).
const OCIStreamingRetentionPropagationThresholdHours = 24

// streamingSourceTypeStream is the SourceType discriminator string for
// OCI Streaming streams. Mirrors the per-cloud "bus" / "topic" /
// "queue" / "namespace" / "stream" SourceType convention documented
// on scanner.EventSourceInstanceSnapshot.
const streamingSourceTypeStream = "stream"

// providerOCIEventSource is the Provider discriminator the scanner
// writes onto every event source snapshot row. Kept as a constant so
// future renames reuse the same string without scattering literal
// "oci" through the projection helper.
const providerOCIEventSource = "oci"

// ociStream is the bare JSON shape of an OCI Streaming Stream as
// returned by the /streams list call. Slice 1 chunk 4 reads ID
// (-> ResourceARN), Name (-> ResourceName), CompartmentID (carried
// through to the per-stream Logging detection call), and
// LifecycleState (surfaced raw via the Detail bag so the Inventory
// tab can dim non-ACTIVE rows the same way other tiers do).
//
// Slice 2 chunk 4 (v0.89.106, #744 Stream 142) adds RetentionInHours
// — the OCI Streaming /streams response includes this field on every
// StreamSummary, so chunk 4 reads it from the existing list call
// without an additional per-stream GetStream round trip. A missing or
// zero value (the JSON omitempty + Go zero-value combination is the
// only signal OCI provides; the API does not echo a sentinel) is
// treated as "below threshold" — see streamPreservesPropagation for
// the defensive default.
//
// OCI Streaming API path:
//
//	GET https://streaming.<region>.oci.oraclecloud.com/20180418/streams
//	  ?compartmentId=<compartment_ocid>
//
// Pagination follows the opc-next-page response header (see
// listStreamsAll below).
type ociStream struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	CompartmentID     string `json:"compartmentId"`
	LifecycleState    string `json:"lifecycleState"`
	RetentionInHours  int    `json:"retentionInHours"`
}

// ociStreamList is the JSON envelope returned by the Streaming
// /streams list call. OCI returns the list directly as a JSON array;
// the scanner unmarshals into a []ociStream slice.
type ociStreamList = []ociStream

// ociLogResource is the bare JSON shape of a single OCI Logging
// resource (a single log within a log group) as returned by the
// /logs list call. Slice 1 chunk 4 only reads ID (for diagnostic
// hints) and Configuration.Source.Resource — the latter being the
// OCID of the resource (e.g. a stream) emitting into this log. The
// per-stream Logging detection rule fires when ANY log resource for
// the compartment carries a Configuration.Source.Resource matching
// the stream's OCID.
//
// OCI Logging API path (per design doc §3.4):
//
//	GET https://logging.<region>.oci.oraclecloud.com/20200531/logs
//	  ?compartmentId=<compartment_ocid>
//	  &searchTerm=<stream_ocid>
//
// The searchTerm parameter narrows the per-stream call's response so
// the scanner does not have to walk every log resource in the
// compartment. The detection logic is defensive: if the searchTerm
// optimization is unsupported on a particular tenancy, the call
// still returns the full list and the scanner side-checks each
// entry's Source.Resource against the target stream OCID.
type ociLogResource struct {
	ID            string                   `json:"id"`
	DisplayName   string                   `json:"displayName"`
	Configuration ociLogConfiguration      `json:"configuration"`
}

// ociLogConfiguration is the nested config block on an OCI Logging
// resource. Slice 1 chunk 4 only reads the Source.Resource OCID
// (matched against the target stream OCID for the detection rule).
type ociLogConfiguration struct {
	Source ociLogSource `json:"source"`
}

// ociLogSource is the inner source block on an OCI Logging
// configuration. The Resource field carries the OCID of the resource
// (a stream, a function, an autonomous database, etc.) that emits
// records into the log. The Category field carries the log category
// (e.g. "all"); slice 1 chunk 4 does not filter on Category — the
// scanner-side proxy rule fires on Resource match alone, leaving
// per-category detection to slice 2.
type ociLogSource struct {
	Resource string `json:"resource"`
	Category string `json:"category"`
}

// ociLogResourceList is the JSON envelope returned by the Logging
// /logs list call. OCI returns the list directly as a JSON array.
type ociLogResourceList = []ociLogResource

// streamPreservesPropagation applies the slice 2 chunk 4 single-axis
// retention threshold detection rule. Returns (preserved, note).
// preserved is true when retentionInHours is at or above the
// OCIStreamingRetentionPropagationThresholdHours threshold; note is
// empty when preserved or a human-readable per-issue string when not.
//
// Detection rule (docs/proposals/event-source-tier-slice2.md §3.4):
//
//   - retentionInHours >= 24 → standard retention; Kafka headers
//     (including traceparent / x-amzn-trace-id) preserved for the
//     retention window. PROPAGATION PRESERVED.
//   - retentionInHours < 24 → short retention may truncate headers
//     in some OCI Streaming versions (the per-message metadata budget
//     shrinks with shorter retention). PROPAGATION POTENTIALLY BROKEN.
//   - retentionInHours == 0 → defensive default: a zero value is
//     either a deliberately-tiny retention or the API response omitted
//     the field on a legacy stream. Either way Squadron cannot prove
//     the threshold is met, so the rule treats zero as below-threshold
//     and surfaces an informational note. The exclusion table absorbs
//     false positives for streams that have a missing-field response
//     shape on a particular tenancy.
//
// This is a per-stream detection — the OCI Streaming surface has no
// per-rule / per-schema / per-policy sub-resources to AND across, so
// the stream-level HasPropagationConfig axis maps 1:1 to this
// helper's output. Slice 3 may add consumer-group-level detection.
func streamPreservesPropagation(retentionInHours int) (bool, string) {
	if retentionInHours >= OCIStreamingRetentionPropagationThresholdHours {
		return true, ""
	}
	return false, fmt.Sprintf(
		"stream retentionInHours=%d below threshold %d; Kafka headers may truncate",
		retentionInHours, OCIStreamingRetentionPropagationThresholdHours,
	)
}

// ScanEventSources is the OCI scanner's event-source-tier entry
// point. Slice 7 chunk 1 (v0.89.150) extends the dispatcher to
// two-way (Streaming + Notifications) with partial-scan posture
// mirroring the slice 6 Azure two-way dispatcher. Slice 8+ may add
// further OCI event source primitives (Queue service).
//
// Scope semantics: scope.AccountID overrides the per-snapshot
// AccountID stamped on every row; empty falls back to the scanner's
// configured TenancyOCID. scope.CompartmentIDs narrows the walk to a
// subset of compartments; empty defaults to "tenancy root + first-
// level children" via the existing listCompartments helper (same
// default as the Compute / Database / OKE / Functions walks).
//
// Partial-scan posture: Streaming failure with Notifications success
// still surfaces the ONS topics, and vice versa. The dispatcher only
// returns a hard error when BOTH surfaces fail — same posture as the
// slice 6 Azure dispatcher (Service Bus + Event Grid). The chunk 5
// trampoline above this is responsible for surfacing per-surface
// partial failures via recordPartialFailure; the dispatcher itself
// only returns the union of snapshots + the both-failed terminal
// error case.
//
// IAM contract per docs/proposals/event-source-tier-slice1.md §12
// (Streaming) + docs/proposals/event-source-tier-slice7.md §12 (ONS):
// "inspect streams in compartment" (Streaming) + "read ons-topics in
// compartment" (ONS) + the existing Logging read policy already in
// the IAM template (logs.read-log + logs.read-log-resource for the
// per-stream + per-topic detection calls). All read-only; Squadron
// never executes a Streaming / ONS / Logging mutation API.
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	var all []scanner.EventSourceInstanceSnapshot

	streams, strErr := s.ScanStreams(ctx, scope)
	if strErr == nil {
		all = append(all, streams...)
	}

	topics, onsErr := s.ScanNotificationTopics(ctx, scope)
	if onsErr == nil {
		all = append(all, topics...)
	}

	// Slice 9 chunk 1 (v0.89.156, #798 Stream 195) extends the OCI
	// dispatcher to three-way (Streaming + Notifications + Queues).
	// Queue Service is the transactional FIFO queue primitive
	// analogous to AWS SQS — distinct from ONS pub/sub fan-out.
	// See docs/proposals/event-source-tier-slice9.md §5.
	queues, qErr := s.ScanQueues(ctx, scope)
	if qErr == nil {
		all = append(all, queues...)
	}

	// Three-way partial-scan posture: only return an error when ALL
	// THREE surfaces failed. Any one- OR two-surface failure is
	// silenced at this layer so an IAM gap on one or two surfaces
	// doesn't drop the inventory the operator actually CAN see on
	// the remaining surface(s). Combinatorial single-failure paths
	// are pinned by slice 9 acceptance tests 8 + 9 + 10; the
	// two-of-three failure path is pinned by test 11; the
	// all-three-fail error-string contract by test 12.
	if strErr != nil && onsErr != nil && qErr != nil {
		return all, fmt.Errorf("oci: all event source surfaces failed: streaming=%v notifications=%v queues=%w", strErr, onsErr, qErr)
	}

	return all, nil
}

// ScanStreams walks the OCI Streaming surface for the configured
// scope. Two-pass walk: per compartment list streams (paginated via
// opc-next-page), then per stream call the OCI Logging /logs endpoint
// with searchTerm=<stream.id> to detect whether a log group attaches
// to the stream.
//
// Detection per docs/proposals/event-source-tier-slice1.md §3.4
// (slice 1 chunk 4 OCI Logging proxy):
//
//   - HasLogAxis  <- the OCI Logging /logs call returns at least one
//     resource whose Configuration.Source.Resource equals the
//     stream's OCID. A stream with no Logging entry leaves the axis
//     false.
//   - HasTraceAxis <- same as HasLogAxis. The chunk 4 OCI APM-for-
//     Streaming integration is not yet a first-class OCI surface
//     (design doc §3.4: "OCI Streaming does not have a direct OTel
//     integration; the closest signal is whether OCI Logging is
//     capturing stream events"), so the Logging proxy carries BOTH
//     axes. Slice 2 will separate the two axes when OCI exposes a
//     more granular detection.
//
// Per-compartment / per-stream / per-logging-call failures are
// swallowed inside the inner loops — a single failing /logs call
// must NOT abort the whole scan. The stream row still surfaces with
// its universal columns; the axes default to false when the inner
// Logging call fails. This matches the AWS EventBridge chunk's
// "single failing describe must not abort the whole scan" contract.
//
// scope.AccountID overrides the scanner's TenancyOCID for the
// snapshot's AccountID field — slice 1 wires the same value through
// both; the parameter is kept symmetric for future per-call scoping.
// scope.CompartmentIDs lets the caller scope the walk to a subset of
// compartments; an empty list defaults to "tenancy root + first-level
// children" via the existing listCompartments helper.
func (s *Scanner) ScanStreams(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	// Substrate validation. The Scan entry point does this on the
	// way in; ScanStreams guards defensively at its own entry point
	// so the chunk 5 trampoline can call this method directly
	// without re-validating.
	if s.TenancyOCID == "" {
		return nil, errors.New("oci: TenancyOCID is required")
	}
	if s.Region == "" {
		return nil, errors.New("oci: Region is required")
	}

	signingKey, parseErr := s.signingKey()
	if parseErr != nil {
		return nil, fmt.Errorf("oci: %s: signing failed: %w", ServiceIDEventSource, parseErr)
	}

	// Determine the compartment set. An explicit scope wins;
	// otherwise default to tenancy root + first-level children.
	compartments, err := s.compartmentsForEventSource(ctx, signingKey, scope)
	if err != nil {
		return nil, fmt.Errorf("oci: %s: compartment listing failed: %w", ServiceIDEventSource, err)
	}

	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.TenancyOCID
	}

	var snapshots []scanner.EventSourceInstanceSnapshot
	for _, comp := range compartments {
		streams, streamsErr := s.listStreamsAll(ctx, signingKey, comp.ID)
		if streamsErr != nil {
			// Partial failure on this compartment's streams walk —
			// skip this compartment but continue walking the rest.
			// The chunk 5 integration will surface this via
			// recordPartialFailure when called through Scan.
			continue
		}
		for _, stream := range streams {
			snap := s.projectOCIStream(ctx, signingKey, stream, comp.ID, accountID)
			snapshots = append(snapshots, snap)
		}
	}
	return snapshots, nil
}

// compartmentsForEventSource resolves the compartment set to walk.
// An explicit scope.CompartmentIDs wins (the chunk 5 trampoline or a
// per-compartment-scoped invocation supplies it). When the scope
// carries no compartment list, the scanner walks tenancy root +
// first-level children — same default as the compute / database /
// OKE / functions walks.
func (s *Scanner) compartmentsForEventSource(ctx context.Context, sk *SigningKey, scope scanner.ScanScope) ([]ociCompartment, error) {
	if len(scope.CompartmentIDs) > 0 {
		out := make([]ociCompartment, 0, len(scope.CompartmentIDs))
		for _, id := range scope.CompartmentIDs {
			out = append(out, ociCompartment{ID: id, LifecycleState: "ACTIVE"})
		}
		return out, nil
	}
	children, listErr := s.listCompartments(ctx, sk)
	if listErr != nil {
		return nil, listErr
	}
	all := append([]ociCompartment{
		{ID: s.TenancyOCID, Name: "root", LifecycleState: "ACTIVE"},
	}, children...)
	return all, nil
}

// listStreamsAll walks every page of /streams for a single
// compartment. OCI signals additional pages via the opc-next-page
// response header; the loop passes that token back as the page=
// <token> query parameter on the next call. An empty or missing
// header terminates the loop. Mirrors listApplicationsAll /
// listFunctionsAll in scanner_functions.go — same pagination
// convention across every OCI surface.
func (s *Scanner) listStreamsAll(ctx context.Context, sk *SigningKey, compartmentID string) ([]ociStream, error) {
	var all []ociStream
	nextPage := ""
	for {
		page, nextToken, callErr := s.listStreamsPage(ctx, sk, compartmentID, nextPage)
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

// listStreamsPage walks one page of /streams. The returned nextPage
// string is the opc-next-page header value (empty when there are no
// more pages).
func (s *Scanner) listStreamsPage(ctx context.Context, sk *SigningKey, compartmentID, page string) ([]ociStream, string, error) {
	endpoint := s.streamingEndpoint()
	u := fmt.Sprintf(
		"%s/%s/streams?compartmentId=%s",
		strings.TrimRight(endpoint, "/"),
		streamingListAPIVersion,
		url.QueryEscape(compartmentID),
	)
	if page != "" {
		u = u + "&page=" + url.QueryEscape(page)
	}
	body, nextPage, callErr := s.doSignedGETWithPage(ctx, sk, u)
	if callErr != nil {
		return nil, "", callErr
	}
	var out ociStreamList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, "", &ociCallError{Wrapped: fmt.Errorf("streams response parse: %w", jerr)}
	}
	return out, nextPage, nil
}

// listLogsForStream walks /logs with searchTerm=<stream.id> and
// returns true when the response carries at least one log resource
// whose Configuration.Source.Resource equals the supplied stream
// OCID. The detection is defensive: even if the searchTerm
// optimization is unsupported on a particular tenancy and the
// /logs call returns the entire compartment-wide log list, the
// per-resource Source.Resource side-check still flips HasLogAxis
// correctly when (and only when) one of the entries references the
// stream's OCID.
//
// A failure on the Logging call (network error, rate limit, 5xx)
// returns (false, error) so the caller can decide whether to dim
// the axis or accept the per-stream miss. The chunk 4 ScanStreams
// wrapper treats a Logging failure as "axis defaults to false" and
// keeps walking — a single failing /logs call must not abort the
// whole scan.
func (s *Scanner) listLogsForStream(ctx context.Context, sk *SigningKey, compartmentID, streamID string) (bool, error) {
	endpoint := s.loggingEndpoint()
	u := fmt.Sprintf(
		"%s/%s/logs?compartmentId=%s&searchTerm=%s",
		strings.TrimRight(endpoint, "/"),
		loggingListAPIVersion,
		url.QueryEscape(compartmentID),
		url.QueryEscape(streamID),
	)
	body, _, callErr := s.doSignedGETWithPage(ctx, sk, u)
	if callErr != nil {
		return false, callErr
	}
	var out ociLogResourceList
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return false, &ociCallError{Wrapped: fmt.Errorf("logs response parse: %w", jerr)}
	}
	for _, lg := range out {
		if lg.Configuration.Source.Resource == streamID {
			return true, nil
		}
	}
	return false, nil
}

// streamingEndpoint returns the OCI Streaming control-plane API base
// URL. When ociEndpoint is set (tests), it's used directly — the
// test mock dispatches /streams and /logs on the same httptest server
// that already routes the compartments path. In production the per-
// region Streaming endpoint pattern is
// https://streaming.<region>.oci.oraclecloud.com.
func (s *Scanner) streamingEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://streaming.%s.oci.oraclecloud.com", s.Region)
}

// loggingEndpoint returns the OCI Logging API base URL. When
// ociEndpoint is set (tests), it's used directly — the test mock
// dispatches /logs on the same httptest server that routes the
// streaming path. In production the per-region Logging endpoint
// pattern is https://logging.<region>.oci.oraclecloud.com.
func (s *Scanner) loggingEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://logging.%s.oci.oraclecloud.com", s.Region)
}

// projectOCIStream maps an ociStream into the provider-agnostic
// EventSourceInstanceSnapshot. The slice 1 chunk 4 mapping is:
//
//   - Provider: "oci".
//   - Surface: "streaming".
//   - AccountID: the per-scan tenancy OCID (the scope-provided
//     value or the scanner's TenancyOCID fallback).
//   - Region: the scanner's configured Region — OCI's Streaming
//     response does not echo region per-row (the surface is scoped
//     to the region the request was sent to).
//   - ResourceName: stream.Name (operator-readable stream name).
//   - ResourceARN: stream.ID (full OCI stream OCID — the canonical
//     handle the proposer's evidence list references).
//   - SourceType: "stream".
//   - HasLogAxis: true when listLogsForStream returns true (any
//     Logging resource references the stream's OCID).
//   - HasTraceAxis: same as HasLogAxis per the chunk 4 OCI Logging
//     proxy rule (design doc §3.4). Slice 2 separates the two axes
//     when OCI exposes APM integration for streams.
//   - Detail: {"lifecycle_state": stream.LifecycleState,
//     "compartment_id": compartmentID, "has_log_group": <bool>,
//     "retention_in_hours": stream.RetentionInHours}.
//     LifecycleState lets the Inventory tab dim non-ACTIVE rows;
//     compartment_id supports debugging multi-compartment walks;
//     has_log_group denormalizes the detection result so callers
//     reading the snapshot from JSON don't have to recompute the
//     axis from the boolean fields above; retention_in_hours
//     surfaces the slice 2 propagation input for the Inventory tab
//     side panel.
//
// Slice 2 chunk 4 (v0.89.106, #744 Stream 142): HasPropagationConfig
// + PropagationNotes are populated from streamPreservesPropagation
// applied to stream.RetentionInHours. A stream with retentionInHours
// at or above the OCIStreamingRetentionPropagationThresholdHours
// (24h) threshold has the axis flipped true with no note; a stream
// below the threshold (including the zero/missing-field defensive
// case) has the axis false with a human-readable note. The
// propagation detection runs unconditionally — it does not depend on
// the Logging proxy axis above, and a stream with no log group can
// still be propagation-preserved if its retention is sufficient.
//
// A failure on the per-stream Logging call leaves both observability
// axes false and stamps an empty Detail["has_log_group"] entry
// (false). The row still surfaces — the operator sees the stream in
// their inventory; only the observability detection axis is lost.
// The propagation axis is computed from the list-call response shape
// alone and is unaffected by Logging call failures.
func (s *Scanner) projectOCIStream(ctx context.Context, sk *SigningKey, stream ociStream, compartmentID, accountID string) scanner.EventSourceInstanceSnapshot {
	snap := scanner.EventSourceInstanceSnapshot{
		Provider:     providerOCIEventSource,
		Surface:      streamingEventSourceSurface,
		AccountID:    accountID,
		Region:       s.Region,
		ResourceName: stream.Name,
		ResourceARN:  stream.ID,
		SourceType:   streamingSourceTypeStream,
	}
	hasLog, logErr := s.listLogsForStream(ctx, sk, compartmentID, stream.ID)
	if logErr == nil && hasLog {
		snap.HasLogAxis = true
		// Slice 1 chunk 4 OCI Logging proxy: a stream with a log
		// group is the proxy for trace observability readiness
		// since OCI Logging is the closest existing observability
		// integration. The OCI APM-for-Streaming detection lands
		// in slice 2 and will separate this from HasLogAxis.
		snap.HasTraceAxis = true
	}
	// Slice 2 chunk 4: per-stream propagation detection driven by the
	// retentionInHours threshold. See streamPreservesPropagation godoc
	// for the per-rule logic + the zero/missing-field defensive case.
	preserved, note := streamPreservesPropagation(stream.RetentionInHours)
	snap.HasPropagationConfig = preserved
	if note != "" {
		snap.PropagationNotes = append(snap.PropagationNotes, note)
	}
	snap.Detail = map[string]any{
		"lifecycle_state":    stream.LifecycleState,
		"compartment_id":     compartmentID,
		"has_log_group":      snap.HasLogAxis,
		"retention_in_hours": stream.RetentionInHours,
	}
	return snap
}

// classifyOCIStreamingError maps an OCI Streaming / Logging call
// failure into the operator-visible PartialReason string under the
// streaming service identifier. Parallels classifyOCIError /
// classifyOCIDBError / classifyOCIOKEError / classifyOCIFunctionsError
// so the audit consumer sees identical structure across the five OCI
// service surfaces.
//
// Slice 1 chunk 4 ships this helper for the chunk 5 trampoline to
// surface partial failures via recordPartialFailure; the chunk 4
// ScanStreams entry point itself swallows per-compartment / per-
// stream failures silently (returning the partial snapshot slice).
// The helper is unused inside ScanStreams but exported here so the
// chunk 5 integration finds it under the same name family.
//
// Error mappings (per the slice 1 design doc §12 threat model):
//
//   - HTTP 401 -> credentials_invalid (signature rejected).
//   - HTTP 403 -> permission_denied with hint pointing at the
//     "inspect streams in compartment" policy statement.
//   - HTTP 404 mid-walk -> empty string (silent skip — many
//     compartments have no Streaming streams; surfacing 404s as
//     partial failures would be noise).
//   - HTTP 429 -> rate_limit.
//   - Transport / network errors -> network-error with truncated
//     underlying error.
//   - Any other 4xx/5xx -> truncated message under the streaming
//     identifier.
func classifyOCIStreamingError(err error, atRoot bool) string {
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
			return fmt.Sprintf("%s: network error: %s", ServiceIDEventSource, truncate(wrapped, 200))
		}
		if oce.StatusCode == http.StatusTooManyRequests || oce.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDEventSource)
		}
		switch oce.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check fingerprint, user_ocid, and private key)", ServiceIDEventSource)
		case http.StatusForbidden:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: permission denied (verify the policy grants 'inspect streams in compartment'): %s", ServiceIDEventSource, truncate(msg, 200))
		case http.StatusNotFound:
			if atRoot {
				return fmt.Sprintf("%s: Streaming surface not found (verify tenancy_ocid and the inspect-streams policy)", ServiceIDEventSource)
			}
			return ""
		default:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: OCI call failed (HTTP %d): %s", ServiceIDEventSource, oce.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", ServiceIDEventSource, truncate(err.Error(), 200))
}
