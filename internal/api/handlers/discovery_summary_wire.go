// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/azureconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/gcpconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/ociconnstore"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// --- production store adapters ------------------------------------------
//
// The summary handler consumes minimal list-only adapters. nil
// per-provider store yields a nil adapter so the trampoline can pass
// it straight to the handler and have the handler treat the provider
// as not-wired.

type awsSummaryStoreAdapter struct{ cs credstore.Store }

// NewAWSSummaryStore wraps credstore.Store, filtered to Provider=AWS,
// for the unified summary handler. nil tolerated.
func NewAWSSummaryStore(cs credstore.Store) AWSSummaryStore {
	if cs == nil {
		return nil
	}
	return &awsSummaryStoreAdapter{cs: cs}
}

func (a *awsSummaryStoreAdapter) ListAWSAccountIDs(ctx context.Context) ([]string, error) {
	conns, err := a.cs.ListConnections(ctx, credstore.ListFilter{Provider: credstore.ProviderAWS})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(conns))
	for _, c := range conns {
		if c != nil {
			ids = append(ids, c.AccountID)
		}
	}
	return ids, nil
}

type gcpSummaryStoreAdapter struct{ s gcpconnstore.Store }

// NewGCPSummaryStore wraps gcpconnstore.Store. nil tolerated.
func NewGCPSummaryStore(s gcpconnstore.Store) GCPSummaryStore {
	if s == nil {
		return nil
	}
	return &gcpSummaryStoreAdapter{s: s}
}

func (a *gcpSummaryStoreAdapter) ListGCPConnectionIDs(ctx context.Context) ([]string, error) {
	conns, err := a.s.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(conns))
	for _, c := range conns {
		if c != nil {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

type azureSummaryStoreAdapter struct{ s azureconnstore.Store }

// NewAzureSummaryStore wraps azureconnstore.Store. nil tolerated.
func NewAzureSummaryStore(s azureconnstore.Store) AzureSummaryStore {
	if s == nil {
		return nil
	}
	return &azureSummaryStoreAdapter{s: s}
}

func (a *azureSummaryStoreAdapter) ListAzureConnectionIDs(ctx context.Context) ([]string, error) {
	conns, err := a.s.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(conns))
	for _, c := range conns {
		if c != nil {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

type ociSummaryStoreAdapter struct{ s ociconnstore.Store }

// NewOCISummaryStore wraps ociconnstore.Store. nil tolerated.
func NewOCISummaryStore(s ociconnstore.Store) OCISummaryStore {
	if s == nil {
		return nil
	}
	return &ociSummaryStoreAdapter{s: s}
}

func (a *ociSummaryStoreAdapter) ListOCIConnectionIDs(ctx context.Context) ([]string, error) {
	conns, err := a.s.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(conns))
	for _, c := range conns {
		if c != nil {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// --- production audit query adapter -------------------------------------
//
// Both queries route through the existing ApplicationStore.
// ListAuditEvents — no new ApplicationStore method needed.

type applicationStoreAuditQuery struct {
	store applicationstore.ApplicationStore
}

// NewApplicationStoreAuditQuery returns the production AuditQueryStore
// over an ApplicationStore. nil store yields nil so the trampoline can
// pass straight through.
func NewApplicationStoreAuditQuery(store applicationstore.ApplicationStore) AuditQueryStore {
	if store == nil {
		return nil
	}
	return &applicationStoreAuditQuery{store: store}
}

// auditScanLookupLimit caps the per-provider ListAuditEvents query.
// 500 rows is a conservative ceiling for deployments with hundreds of
// connections that scan daily.
const auditScanLookupLimit = 500

// auditProposalLookupLimit asks for more than the recent-
// recommendations cap (10) so the projection has headroom after
// de-duping; the handler caps the output at recentRecommendationsLimit
// before serializing.
const auditProposalLookupLimit = 50

func (a *applicationStoreAuditQuery) ListRecentScanCompletedByProvider(
	ctx context.Context, provider string,
) (map[string]ScanSummary, error) {
	eventType := scanCompletedEventType(provider)
	if eventType == "" {
		return map[string]ScanSummary{}, nil
	}
	events, err := a.store.ListAuditEvents(ctx, applicationstore.AuditEventFilter{
		EventType: eventType,
		Limit:     auditScanLookupLimit,
	})
	if err != nil {
		return nil, err
	}
	// ListAuditEvents returns newest-first; keep the first row per
	// scope so we get the most recent scan per connection.
	out := map[string]ScanSummary{}
	for _, e := range events {
		if e == nil {
			continue
		}
		scopeID := projectScopeID(provider, e)
		if scopeID == "" {
			continue
		}
		if _, seen := out[scopeID]; seen {
			continue
		}
		out[scopeID] = ScanSummary{
			ScopeID:             scopeID,
			CompletedAt:         e.Timestamp,
			InstanceCount:       totalInstancesFromPayload(provider, e.Payload),
			InstrumentedCount:   intFromPayload(e.Payload, "instrumented_count"),
			UninstrumentedCount: intFromPayload(e.Payload, "uninstrumented_count"),
			// Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream
			// 123) — project the optional serverless_count payload
			// field. Older scans pre-date the field; intFromPayload
			// returns 0 in that case so the cold-start posture stays
			// zero-safe.
			ServerlessCount: intFromPayload(e.Payload, "serverless_count"),
		}
	}
	return out, nil
}

func (a *applicationStoreAuditQuery) ListRecentDiscoveryProposals(
	ctx context.Context, limit int,
) ([]ProposalEvent, error) {
	if limit <= 0 {
		limit = recentRecommendationsLimit
	}
	queryLimit := auditProposalLookupLimit
	if queryLimit < limit {
		queryLimit = limit
	}
	events, err := a.store.ListAuditEvents(ctx, applicationstore.AuditEventFilter{
		EventType: services.AuditEventDiscoveryProposalCreated,
		Limit:     queryLimit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ProposalEvent, 0, len(events))
	for _, e := range events {
		if e == nil {
			continue
		}
		out = append(out, ProposalEvent{
			Provider:    proposalProviderFromPayload(e),
			Kind:        stringFromPayload(e.Payload, "recommendation_kind"),
			ResourceID:  stringFromPayload(e.Payload, "resource_id"),
			ScopeID:     proposalScopeID(e),
			Region:      stringFromPayload(e.Payload, "region"),
			GeneratedAt: e.Timestamp,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// --- audit projection helpers -------------------------------------------

// scanCompletedEventType maps a provider name to its scan_completed
// event type. AWS predates the constant family and uses the literal
// string.
func scanCompletedEventType(provider string) string {
	switch strings.ToLower(provider) {
	case "aws":
		return "discovery.aws.scan_completed"
	case "gcp":
		return services.AuditEventDiscoveryGCPScanCompleted
	case "azure":
		return services.AuditEventDiscoveryAzureScanCompleted
	case "oci":
		return services.AuditEventDiscoveryOCIScanCompleted
	default:
		return ""
	}
}

// projectScopeID extracts the per-provider scope ID the summary
// handler keys on. AWS uses TargetID (account_id); other providers
// carry the scope as a payload field and fall back to TargetID.
func projectScopeID(provider string, e *applicationstore.AuditEvent) string {
	switch strings.ToLower(provider) {
	case "aws":
		return e.TargetID
	case "gcp":
		if v := stringFromPayload(e.Payload, "project_id"); v != "" {
			return v
		}
		return e.TargetID
	case "azure":
		if v := stringFromPayload(e.Payload, "subscription_id"); v != "" {
			return v
		}
		return e.TargetID
	case "oci":
		if v := stringFromPayload(e.Payload, "tenancy_ocid"); v != "" {
			return v
		}
		return e.TargetID
	default:
		return e.TargetID
	}
}

// totalInstancesFromPayload reconciles the two scan_completed payload
// shapes. AWS payload carries instrumented + uninstrumented; non-AWS
// payloads carry total_resources directly.
func totalInstancesFromPayload(provider string, payload map[string]any) int {
	if payload == nil {
		return 0
	}
	if strings.ToLower(provider) == "aws" {
		return intFromPayload(payload, "instrumented_count") +
			intFromPayload(payload, "uninstrumented_count")
	}
	if v := intFromPayload(payload, "total_resources"); v > 0 {
		return v
	}
	return intFromPayload(payload, "instrumented_count") +
		intFromPayload(payload, "uninstrumented_count")
}

// proposalProviderFromPayload sniffs the payload to figure out which
// provider a discovery_proposal.created row came from. Falls back to
// "aws" when only account_id is present (slice 1 v0.89.28 shape).
func proposalProviderFromPayload(e *applicationstore.AuditEvent) string {
	if e == nil || e.Payload == nil {
		return ""
	}
	if v := stringFromPayload(e.Payload, "provider"); v != "" {
		return strings.ToLower(v)
	}
	if _, ok := e.Payload["tenancy_ocid"]; ok {
		return "oci"
	}
	if _, ok := e.Payload["subscription_id"]; ok {
		return "azure"
	}
	if _, ok := e.Payload["project_id"]; ok {
		return "gcp"
	}
	if _, ok := e.Payload["account_id"]; ok {
		return "aws"
	}
	return ""
}

// proposalScopeID extracts the per-provider scope, falling back to
// TargetID when no payload field matches.
func proposalScopeID(e *applicationstore.AuditEvent) string {
	if e == nil {
		return ""
	}
	for _, key := range []string{"account_id", "project_id", "subscription_id", "tenancy_ocid"} {
		if v := stringFromPayload(e.Payload, key); v != "" {
			return v
		}
	}
	return e.TargetID
}

// --- payload helpers ----------------------------------------------------

// intFromPayload extracts an integer field. SQLite-projected payloads
// arrive as float64; in-process payloads pass through as int.
func intFromPayload(payload map[string]any, key string) int {
	if payload == nil {
		return 0
	}
	v, ok := payload[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	}
	return 0
}

// stringFromPayload extracts a string field, returning "" when absent
// or wrong-typed.
func stringFromPayload(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	v, ok := payload[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
