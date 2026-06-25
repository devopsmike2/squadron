// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Event source tier slice 10 chunk 1 (v0.89.159, #801 Stream 198) —
// GCP Pub/Sub Lite scanner. CLOSES the cross-cloud event source
// widening pass at 3-3-3-3 / 12 surfaces across 4 clouds.
//
// See docs/proposals/event-source-tier-slice10.md.
//
// Pub/Sub Lite is GCP's partitioned-log primitive, the structural
// analog of AWS Kinesis Data Streams and Azure Event Hubs. Distinct
// from full Pub/Sub in that Lite trades managed routing + global
// delivery for cost efficiency at high volume — operators self-manage
// partition capacity via reservations.
//
// Two detection axes per design doc §3:
//   - Cloud Logging configured (HasLogAxis): the project has at least
//     one Cloud Logging sink whose filter references
//     resource.type=pubsublite_topic AND the topic's ID. Detected via
//     a single project-wide sink list call cached across the walk.
//   - Reservation attached (Detail[has_reservation]): the topic's
//     reservationConfig.throughputReservation resolves to an existing
//     reservation in the topic's zone. Detected via per-zone
//     reservation list cached across the walk.

const (
	// PubSubLiteAPIVersion pins the Admin API path for the slice 10
	// chunk 1 list calls. The v1 surface is the stable GA path.
	PubSubLiteAPIVersion = "v1"

	// pubsubliteAPIHost is the Pub/Sub Lite Admin API hostname.
	pubsubliteAPIHost = "https://pubsublite.googleapis.com"

	// loggingAPIHost is the Cloud Logging API hostname used by the
	// slice 10 chunk 1 sink discovery for the HasLogAxis detection.
	loggingAPIHost = "https://logging.googleapis.com"

	// ServiceIDPubSubLite is the per-service identifier the scanner
	// reports against Result.FailedServices when the Lite walk
	// produces a non-fatal error. Mirrors ServiceIDPubSub +
	// ServiceIDCloudTasks.
	ServiceIDPubSubLite = "pubsublite"

	// PubSubLiteSurface drives the proposer's recommendation-kind
	// prefix routing for Pub/Sub Lite event sources: pubsublite-* →
	// GCP. The snapshot Surface field carries this value verbatim.
	PubSubLiteSurface = "pubsublite"

	// pubsubLiteSourceTypeTopic is the SourceType discriminator string
	// for Pub/Sub Lite topic rows.
	pubsubLiteSourceTypeTopic = "topic"
)

// pubsubliteTopic is the bare JSON shape of a single
// projects.locations.topics list entry. Slice 10 chunk 1 reads Name +
// PartitionConfig + RetentionConfig + ReservationConfig; other
// top-level fields are intentionally untyped.
type pubsubliteTopic struct {
	Name              string                            `json:"name"`
	PartitionConfig   *pubsubliteTopicPartitionConfig   `json:"partitionConfig,omitempty"`
	RetentionConfig   *pubsubliteTopicRetentionConfig   `json:"retentionConfig,omitempty"`
	ReservationConfig *pubsubliteTopicReservationConfig `json:"reservationConfig,omitempty"`
}

type pubsubliteTopicPartitionConfig struct {
	Count    int                               `json:"count,omitempty"`
	Capacity *pubsubliteTopicPartitionCapacity `json:"capacity,omitempty"`
}

type pubsubliteTopicPartitionCapacity struct {
	PublishMibPerSec   int `json:"publishMibPerSec,omitempty"`
	SubscribeMibPerSec int `json:"subscribeMibPerSec,omitempty"`
}

type pubsubliteTopicRetentionConfig struct {
	PerPartitionBytes string `json:"perPartitionBytes,omitempty"`
	Period            string `json:"period,omitempty"`
}

type pubsubliteTopicReservationConfig struct {
	ThroughputReservation string `json:"throughputReservation,omitempty"`
}

// pubsubliteListTopicsResponse is the paginated list envelope.
type pubsubliteListTopicsResponse struct {
	Topics        []*pubsubliteTopic `json:"topics,omitempty"`
	NextPageToken string             `json:"nextPageToken,omitempty"`
}

// pubsubliteReservation is the bare JSON shape of a single
// projects.locations.reservations list entry. The slice 10 chunk 1
// projection only reads Name + ThroughputCapacity for diagnostic
// purposes; deeper reservation analysis is slice 11+ candidate.
type pubsubliteReservation struct {
	Name               string `json:"name"`
	ThroughputCapacity int    `json:"throughputCapacity,omitempty"`
}

type pubsubliteListReservationsResponse struct {
	Reservations  []*pubsubliteReservation `json:"reservations,omitempty"`
	NextPageToken string                   `json:"nextPageToken,omitempty"`
}

// loggingSink is the bare JSON shape of a Cloud Logging sink as
// returned by logging.projects.sinks.list. Slice 10 chunk 1 reads
// Name + Filter for the HasLogAxis detection.
type loggingSink struct {
	Name   string `json:"name,omitempty"`
	Filter string `json:"filter,omitempty"`
}

type loggingListSinksResponse struct {
	Sinks         []*loggingSink `json:"sinks,omitempty"`
	NextPageToken string         `json:"nextPageToken,omitempty"`
}

// ScanPubSubLiteTopics walks the configured zones' Pub/Sub Lite
// topics and returns the mapped event source snapshots. Slice 10
// chunk 1 of the event-source-tier arc.
//
// Per-zone failures are non-fatal: the walk continues with the next
// zone after recording the failure into the partial-failure surface.
// A single-zone IAM gap doesn't drop the topics in zones the SA CAN
// reach.
//
// Detection per docs/proposals/event-source-tier-slice10.md §3:
//   - HasLogAxis ← project has a Cloud Logging sink filtering on
//     resource.type=pubsublite_topic AND the topic's ID. Detected
//     via a single project-wide sink list call cached across the
//     walk.
//   - Detail[has_reservation] ← reservationConfig.throughputReservation
//     resolves to an existing reservation in the topic's zone.
//
// IAM contract per design doc §12:
//   - pubsublite.topics.list
//   - pubsublite.topics.get
//   - pubsublite.reservations.list
//   - logging.logSinks.list (already in slice 1 Logging read scope)
//
// All read-only. Squadron never executes a PublishMessages /
// CreateTopic / CreateReservation mutation.
func (s *Scanner) ScanPubSubLiteTopics(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	if s.ProjectID == "" {
		return nil, errors.New("gcp: ProjectID is required")
	}
	if len(s.SAJSON) == 0 && s.httpClient == nil {
		return nil, errors.New("gcp: SAJSON is required")
	}
	client, err := s.buildPubSubLiteHTTPClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: build pubsublite http client: %w", err)
	}
	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.ProjectID
	}
	zones := s.pubsubLiteZones(scope)
	if len(zones) == 0 {
		return nil, nil
	}

	// Slice 10 chunk 1 HasLogAxis detection: project-wide Cloud
	// Logging sink list, cached for the walk. A failure here is
	// non-fatal: the topic walk proceeds with an empty sink list
	// (HasLogAxis defaults to false on every topic).
	sinks := s.listLoggingSinksForPubSubLite(ctx, client)

	var all []scanner.EventSourceInstanceSnapshot
	var lastErr error
	successes := 0
	for _, zone := range zones {
		topics, err := s.walkPubSubLiteZone(ctx, client, zone)
		if err != nil {
			lastErr = err
			continue
		}
		successes++

		// Slice 10 chunk 1 reservation axis: per-zone reservation list
		// (cached per zone). A failure here leaves Detail[has_reservation]
		// false on every topic in the zone — the topic rows still
		// surface, the operator sees the inventory.
		reservations := s.listPubSubLiteReservations(ctx, client, zone)

		for _, t := range topics {
			if t == nil || t.Name == "" {
				continue
			}
			hasLog := pubsubliteSinkMatchesTopic(sinks, t.Name)
			hasReservation := pubsubliteReservationExists(reservations, t.ReservationConfig)
			all = append(all, projectPubSubLiteTopic(t, zone, accountID, hasLog, hasReservation))
		}
	}
	if successes == 0 && lastErr != nil {
		return all, lastErr
	}
	return all, nil
}

// pubsubLiteZones resolves the zones to walk. Mirrors cloudTasksRegions.
// Pub/Sub Lite is zone-pinned, so scope.Regions carries zone strings
// (us-central1-a, etc.) for the slice 10 walk.
func (s *Scanner) pubsubLiteZones(scope scanner.ScanScope) []string {
	if len(scope.Regions) > 0 {
		return scope.Regions
	}
	return nil
}

// walkPubSubLiteZone issues the paginated GET against
// /v1/admin/projects/{project}/locations/{zone}/topics, decodes each
// response, and returns the list. Per-page failures abort the walk
// (mirrors walkCloudTasksRegion).
func (s *Scanner) walkPubSubLiteZone(ctx context.Context, client *http.Client, zone string) ([]*pubsubliteTopic, error) {
	var all []*pubsubliteTopic
	pageToken := ""
	for {
		listURL := s.pubsubliteListTopicsURL(zone, pageToken)
		resp, err := s.pubsubliteGetTopics(ctx, client, listURL)
		if err != nil {
			return all, err
		}
		all = append(all, resp.Topics...)
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return all, nil
}

// pubsubliteListTopicsURL constructs the full Admin API URL for a
// zone's topic list page.
func (s *Scanner) pubsubliteListTopicsURL(zone, pageToken string) string {
	host := pubsubliteAPIHost
	if s.endpoint != "" {
		host = s.endpoint
	}
	u := fmt.Sprintf(
		"%s/%s/admin/projects/%s/locations/%s/topics",
		strings.TrimRight(host, "/"),
		PubSubLiteAPIVersion,
		s.ProjectID,
		zone,
	)
	if pageToken != "" {
		u += "?pageToken=" + pageToken
	}
	return u
}

// pubsubliteListReservationsURL constructs the per-zone reservation
// list URL.
func (s *Scanner) pubsubliteListReservationsURL(zone, pageToken string) string {
	host := pubsubliteAPIHost
	if s.endpoint != "" {
		host = s.endpoint
	}
	u := fmt.Sprintf(
		"%s/%s/admin/projects/%s/locations/%s/reservations",
		strings.TrimRight(host, "/"),
		PubSubLiteAPIVersion,
		s.ProjectID,
		zone,
	)
	if pageToken != "" {
		u += "?pageToken=" + pageToken
	}
	return u
}

// pubsubliteGetTopics issues a single GET and decodes the response.
func (s *Scanner) pubsubliteGetTopics(ctx context.Context, client *http.Client, listURL string) (*pubsubliteListTopicsResponse, error) {
	body, err := pubsubliteGetRaw(ctx, client, listURL)
	if err != nil {
		return nil, err
	}
	var out pubsubliteListTopicsResponse
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, fmt.Errorf("pubsublite topics list parse: %w", jerr)
	}
	return &out, nil
}

// listPubSubLiteReservations walks the per-zone reservation list.
// Failures are silenced — the caller treats an empty reservation list
// as "no reservations resolvable" and flags every topic's reservation
// axis as false.
func (s *Scanner) listPubSubLiteReservations(ctx context.Context, client *http.Client, zone string) []*pubsubliteReservation {
	var all []*pubsubliteReservation
	pageToken := ""
	for {
		listURL := s.pubsubliteListReservationsURL(zone, pageToken)
		body, err := pubsubliteGetRaw(ctx, client, listURL)
		if err != nil {
			return all
		}
		var resp pubsubliteListReservationsResponse
		if jerr := json.Unmarshal(body, &resp); jerr != nil {
			return all
		}
		all = append(all, resp.Reservations...)
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return all
}

// listLoggingSinksForPubSubLite walks every Cloud Logging sink at
// project scope ONCE per scan. Slice 10 chunk 1 reuses the
// project-wide sink list to detect per-topic Logging configuration:
// any sink with a filter that mentions resource.type=pubsublite_topic
// AND the topic's ID matches the per-topic HasLogAxis detection rule.
//
// Failures are silenced — an empty sink list flags every topic's
// HasLogAxis as false. The topic rows still surface so the operator
// sees the inventory.
func (s *Scanner) listLoggingSinksForPubSubLite(ctx context.Context, client *http.Client) []*loggingSink {
	var all []*loggingSink
	pageToken := ""
	for {
		listURL := s.loggingListSinksURL(pageToken)
		body, err := pubsubliteGetRaw(ctx, client, listURL)
		if err != nil {
			return all
		}
		var resp loggingListSinksResponse
		if jerr := json.Unmarshal(body, &resp); jerr != nil {
			return all
		}
		all = append(all, resp.Sinks...)
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return all
}

func (s *Scanner) loggingListSinksURL(pageToken string) string {
	host := loggingAPIHost
	if s.endpoint != "" {
		host = s.endpoint
	}
	u := fmt.Sprintf(
		"%s/v2/projects/%s/sinks",
		strings.TrimRight(host, "/"),
		s.ProjectID,
	)
	if pageToken != "" {
		u += "?pageToken=" + pageToken
	}
	return u
}

// pubsubliteGetRaw issues a single GET and returns the raw body or
// an HTTP-statused error. Centralizes the Admin API + Logging API
// transport so all four list calls share auth + pagination.
func pubsubliteGetRaw(ctx context.Context, client *http.Client, listURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("pubsublite: http %d: %s", resp.StatusCode, truncatePSL(string(body), 200))
	}
	return body, nil
}

func truncatePSL(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// pubsubliteSinkMatchesTopic returns true when any Cloud Logging sink
// has a filter containing both `resource.type="pubsublite_topic"` AND
// the topic's short ID. The match is substring-based and tolerant of
// quote style and whitespace variations — the design doc §3 detection
// rule is "any sink filter references the topic by name".
func pubsubliteSinkMatchesTopic(sinks []*loggingSink, topicName string) bool {
	if topicName == "" {
		return false
	}
	short := pubsubliteShortTopicID(topicName)
	for _, sink := range sinks {
		if sink == nil {
			continue
		}
		f := sink.Filter
		if !strings.Contains(f, "pubsublite_topic") {
			continue
		}
		if short != "" && strings.Contains(f, short) {
			return true
		}
	}
	return false
}

// pubsubliteShortTopicID extracts the last path segment from a fully
// qualified topic resource name
// (projects/{p}/locations/{z}/topics/{id}).
func pubsubliteShortTopicID(name string) string {
	idx := strings.LastIndex(name, "/")
	if idx < 0 {
		return name
	}
	return name[idx+1:]
}

// pubsubliteReservationExists returns true when the topic's
// reservation reference resolves to an existing reservation in the
// supplied list. An empty reservationConfig or an unresolvable
// reference returns false — this is the
// pubsublite-reservation-attach recommendation's firing condition.
func pubsubliteReservationExists(reservations []*pubsubliteReservation, cfg *pubsubliteTopicReservationConfig) bool {
	if cfg == nil || cfg.ThroughputReservation == "" {
		return false
	}
	target := cfg.ThroughputReservation
	short := pubsubliteShortTopicID(target)
	for _, r := range reservations {
		if r == nil {
			continue
		}
		if r.Name == target {
			return true
		}
		if short != "" && pubsubliteShortTopicID(r.Name) == short {
			return true
		}
	}
	return false
}

// projectPubSubLiteTopic maps a topic into the provider-agnostic
// EventSourceInstanceSnapshot per the slice 10 chunk 1 contract:
// Provider="gcp", Surface="pubsublite", SourceType="topic",
// ResourceARN=topic.Name (full resource path).
func projectPubSubLiteTopic(t *pubsubliteTopic, zone, accountID string, hasLog, hasReservation bool) scanner.EventSourceInstanceSnapshot {
	detail := map[string]any{
		"source_type":     pubsubLiteSourceTypeTopic,
		"has_log":         hasLog,
		"has_reservation": hasReservation,
	}
	if t.PartitionConfig != nil {
		if t.PartitionConfig.Count > 0 {
			detail["partition_count"] = t.PartitionConfig.Count
		}
		if t.PartitionConfig.Capacity != nil {
			if t.PartitionConfig.Capacity.PublishMibPerSec > 0 {
				detail["publish_mib_per_sec"] = t.PartitionConfig.Capacity.PublishMibPerSec
			}
			if t.PartitionConfig.Capacity.SubscribeMibPerSec > 0 {
				detail["subscribe_mib_per_sec"] = t.PartitionConfig.Capacity.SubscribeMibPerSec
			}
		}
	}
	if t.RetentionConfig != nil {
		if t.RetentionConfig.PerPartitionBytes != "" {
			detail["per_partition_bytes"] = t.RetentionConfig.PerPartitionBytes
		}
		if t.RetentionConfig.Period != "" {
			detail["retention_period"] = t.RetentionConfig.Period
		}
	}
	if t.ReservationConfig != nil && t.ReservationConfig.ThroughputReservation != "" {
		detail["reservation_reference"] = t.ReservationConfig.ThroughputReservation
	}

	return scanner.EventSourceInstanceSnapshot{
		Provider:     string(credstore.ProviderGCP),
		Surface:      PubSubLiteSurface,
		AccountID:    accountID,
		Region:       zone,
		ResourceName: pubsubliteShortTopicID(t.Name),
		ResourceARN:  t.Name,
		SourceType:   pubsubLiteSourceTypeTopic,
		HasLogAxis:   hasLog,
		// Pub/Sub Lite trace axis is client-side (publisher-side
		// traceparent in message attributes), not detectable from the
		// Admin API. Slice 11+ candidate via substrate MetricQuerier.
		HasTraceAxis:         false,
		HasPropagationConfig: false,
		Detail:               detail,
	}
}

// buildPubSubLiteHTTPClient reuses the shared OAuth client builder
// from buildOAuthHTTPClient. Test path: s.httpClient verbatim.
func (s *Scanner) buildPubSubLiteHTTPClient(ctx context.Context) (*http.Client, error) {
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
