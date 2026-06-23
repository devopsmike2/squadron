// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestAuditService_RecordAndList(t *testing.T) {
	svc := NewAuditService(memory.NewStore(), nil, zap.NewNop())
	ctx := context.Background()

	// Record three events for two different targets.
	require.NoError(t, svc.Record(ctx, AuditEntry{
		Actor:      AuditActorSystem,
		EventType:  AuditEventConfigStored,
		TargetType: AuditTargetConfig,
		TargetID:   "cfg-1",
		Action:     "stored",
		Payload:    map[string]any{"version": 1},
	}))
	require.NoError(t, svc.Record(ctx, AuditEntry{
		Actor:      AuditActorSystem,
		EventType:  AuditEventAgentDriftDrifted,
		TargetType: AuditTargetAgent,
		TargetID:   "agent-a",
		Action:     "drift",
	}))
	require.NoError(t, svc.Record(ctx, AuditEntry{
		Actor:      AuditActorOpAMP,
		EventType:  AuditEventAgentRegistered,
		TargetType: AuditTargetAgent,
		TargetID:   "agent-b",
		Action:     "created",
	}))

	// Unfiltered list returns all three, newest-first.
	all, err := svc.List(ctx, AuditEventFilter{})
	require.NoError(t, err)
	require.Len(t, all, 3)
	// The store is in-memory and append-only; the newest-recorded event
	// (agent-b registered) should be first.
	assert.Equal(t, "agent-b", all[0].TargetID)

	// Filter to a target type returns only matching events.
	agentEvents, err := svc.List(ctx, AuditEventFilter{TargetType: AuditTargetAgent})
	require.NoError(t, err)
	assert.Len(t, agentEvents, 2)

	// Filter to a specific target id narrows further.
	a, err := svc.List(ctx, AuditEventFilter{
		TargetType: AuditTargetAgent,
		TargetID:   "agent-a",
	})
	require.NoError(t, err)
	require.Len(t, a, 1)
	assert.Equal(t, AuditEventAgentDriftDrifted, a[0].EventType)
}

func TestAuditService_EventTypeFilter(t *testing.T) {
	// Regression guard for #580 (v0.87.2): the AuditEventFilter struct
	// previously had no EventType field; the SQL store had no
	// "AND event_type = ?" clause. This test seeds two distinct event
	// types and pins that filtering by EventType returns only matching
	// rows. Memory-store coverage stands in for the SQL clause too —
	// the in-memory walk mirrors the SQL filter shape.
	svc := NewAuditService(memory.NewStore(), nil, zap.NewNop())
	ctx := context.Background()

	const wanted = "discovery.aws.connection_created"
	const other = "discovery.aws.connection_read"

	for i := 0; i < 2; i++ {
		require.NoError(t, svc.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  wanted,
			TargetType: "aws_connection",
			TargetID:   "acct-w",
			Action:     "created",
		}))
	}
	for i := 0; i < 4; i++ {
		require.NoError(t, svc.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  other,
			TargetType: "aws_connection",
			TargetID:   "acct-r",
			Action:     "read",
		}))
	}

	got, err := svc.List(ctx, AuditEventFilter{EventType: wanted})
	require.NoError(t, err)
	require.Len(t, got, 2, "EventType filter must narrow to seeded count; if 6, the store ignored the filter")
	for i, ev := range got {
		assert.Equal(t, wanted, ev.EventType,
			"row[%d].EventType = %q, want %q", i, ev.EventType, wanted)
	}

	// EventType combines with TargetType (AND semantics, not OR).
	mixed, err := svc.List(ctx, AuditEventFilter{
		EventType:  wanted,
		TargetType: "aws_connection",
	})
	require.NoError(t, err)
	assert.Len(t, mixed, 2)
}

func TestAuditService_SinceFilter(t *testing.T) {
	svc := NewAuditService(memory.NewStore(), nil, zap.NewNop())
	ctx := context.Background()

	// Record event A, derive the cutoff from A's actual stored
	// timestamp, then record event B. The Since filter should return
	// only event B.
	//
	// Earlier the test captured the cutoff via time.Now() between the
	// two Record() calls, which is racy: on low-resolution clocks
	// (Windows is ~15ms) two consecutive time.Now() readings can
	// return identical values, and on fast machines the two Record()
	// timestamps and the cutoff capture can collide within a single
	// monotonic tick. The Since filter is `>=` (inclusive), so a
	// collision pulls A into the result and the Len==1 assertion
	// flakes. Fixed in #583 by deriving the cutoff from A's actual
	// stored timestamp + 1ns, which is strictly greater than A by
	// construction. Then a 50ms sleep guarantees B's recorded
	// timestamp is comfortably greater than the cutoff on any
	// platform's clock granularity.
	require.NoError(t, svc.Record(ctx, AuditEntry{
		Actor: AuditActorSystem, EventType: "test.event", TargetType: "x", Action: "a",
	}))

	prior, err := svc.List(ctx, AuditEventFilter{})
	require.NoError(t, err)
	require.Len(t, prior, 1)
	cutoff := prior[0].Timestamp.Add(1 * time.Nanosecond)

	time.Sleep(50 * time.Millisecond)
	require.NoError(t, svc.Record(ctx, AuditEntry{
		Actor: AuditActorSystem, EventType: "test.event", TargetType: "x", Action: "b",
	}))

	out, err := svc.List(ctx, AuditEventFilter{Since: cutoff})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "b", out[0].Action)
}

func TestAuditService_LimitCappedToOneThousand(t *testing.T) {
	svc := NewAuditService(memory.NewStore(), nil, zap.NewNop())
	ctx := context.Background()

	// 1500 events, ask for an absurd limit.
	for i := 0; i < 1500; i++ {
		require.NoError(t, svc.Record(ctx, AuditEntry{
			Actor: AuditActorSystem, EventType: "stress", TargetType: "x", Action: "a",
		}))
	}
	out, err := svc.List(ctx, AuditEventFilter{Limit: 999999})
	require.NoError(t, err)
	assert.Len(t, out, 1000, "limit must be capped to 1000")
}

func TestAuditService_Record_PublishesToBroker(t *testing.T) {
	// We don't import the events package here directly to keep this test
	// at the service-level API surface, but we can confirm the publish
	// side effect by passing a *real* broker and reading from a
	// subscription. The broker live in a separate package; we depend on
	// it indirectly via NewAuditService.
	t.Skip("broker round-trip is covered in internal/events tests; this skip documents the integration is wired in NewAuditService")
}

// TestAuditEventConstants_CheckRunPresent — v0.89.42 (#662 Stream 60,
// slice 1 chunk 1 of the GitHub Checks API back-signal arc).
// Defensive: pins the three check-run audit-event constants so a
// future refactor that accidentally deletes them lands here rather
// than in a SIEM dashboard that silently stops receiving events.
// The chunks-2/3/4 emit paths reference these constants by name;
// the constant strings are the contract SIEM consumers fan out on.
func TestAuditEventConstants_CheckRunPresent(t *testing.T) {
	checks := map[string]string{
		"AuditEventIaCCheckRunCreated": AuditEventIaCCheckRunCreated,
		"AuditEventIaCCheckRunUpdated": AuditEventIaCCheckRunUpdated,
		"AuditEventIaCCheckRunFailed":  AuditEventIaCCheckRunFailed,
	}
	for name, value := range checks {
		assert.NotEmpty(t, value, "%s must be a non-empty string", name)
	}
	// Pin the canonical dotted-name values so a future rename lands
	// here too. The strings are the SIEM-side contract.
	assert.Equal(t, "iac.check_run.created", AuditEventIaCCheckRunCreated)
	assert.Equal(t, "iac.check_run.updated", AuditEventIaCCheckRunUpdated)
	assert.Equal(t, "iac.check_run.failed", AuditEventIaCCheckRunFailed)
}

// TestAuditEventConstants_GCPDiscoveryPresent — v0.89.46 (#667
// Stream 65, GCP discovery slice 1 chunk 1). Defensive: pins the six
// GCP-discovery audit-event constants so a future refactor that
// accidentally deletes one lands here rather than in a SIEM
// dashboard that silently stops receiving GCP-side events. Chunks
// 2 / 3 / 5 (scanner, API handlers, proposer integration) reference
// these constants by name; the constant strings are the contract
// SIEM consumers fan out on. The six events mirror the AWS discovery
// arc's lifecycle one-for-one with project_id replacing account_id —
// see docs/proposals/gcp-discovery-slice1.md §10 contract item 6.
func TestAuditEventConstants_GCPDiscoveryPresent(t *testing.T) {
	checks := map[string]string{
		"AuditEventDiscoveryGCPConnectionCreated":        AuditEventDiscoveryGCPConnectionCreated,
		"AuditEventDiscoveryGCPConnectionDeleted":        AuditEventDiscoveryGCPConnectionDeleted,
		"AuditEventDiscoveryGCPScanStarted":              AuditEventDiscoveryGCPScanStarted,
		"AuditEventDiscoveryGCPScanCompleted":            AuditEventDiscoveryGCPScanCompleted,
		"AuditEventDiscoveryGCPScanFailed":               AuditEventDiscoveryGCPScanFailed,
		"AuditEventDiscoveryGCPRecommendationsGenerated": AuditEventDiscoveryGCPRecommendationsGenerated,
	}
	for name, value := range checks {
		assert.NotEmpty(t, value, "%s must be a non-empty string", name)
	}
	// Pin the canonical dotted-name values so a future rename lands
	// here too. The strings are the SIEM-side contract.
	assert.Equal(t, "discovery.gcp.connection_created", AuditEventDiscoveryGCPConnectionCreated)
	assert.Equal(t, "discovery.gcp.connection_deleted", AuditEventDiscoveryGCPConnectionDeleted)
	assert.Equal(t, "discovery.gcp.scan_started", AuditEventDiscoveryGCPScanStarted)
	assert.Equal(t, "discovery.gcp.scan_completed", AuditEventDiscoveryGCPScanCompleted)
	assert.Equal(t, "discovery.gcp.scan_failed", AuditEventDiscoveryGCPScanFailed)
	assert.Equal(t, "discovery.gcp.recommendations_generated", AuditEventDiscoveryGCPRecommendationsGenerated)
}

// TestAuditEventConstants_AzureDiscoveryPresent — v0.89.51 (#674
// Stream 72, Azure discovery slice 1 chunk 1). Defensive: pins the six
// Azure-discovery audit-event constants so a future refactor that
// accidentally deletes one lands here rather than in a SIEM dashboard
// that silently stops receiving Azure-side events. Chunks 2 / 3 / 5
// (scanner, API handlers, proposer integration) reference these
// constants by name; the constant strings are the contract SIEM
// consumers fan out on. The six events mirror the AWS and GCP
// discovery arc lifecycles one-for-one with subscription_id replacing
// account_id / project_id — see
// docs/proposals/azure-discovery-slice1.md §11, §13 contract item 6.
func TestAuditEventConstants_AzureDiscoveryPresent(t *testing.T) {
	checks := map[string]string{
		"AuditEventDiscoveryAzureConnectionCreated":        AuditEventDiscoveryAzureConnectionCreated,
		"AuditEventDiscoveryAzureConnectionDeleted":        AuditEventDiscoveryAzureConnectionDeleted,
		"AuditEventDiscoveryAzureScanStarted":              AuditEventDiscoveryAzureScanStarted,
		"AuditEventDiscoveryAzureScanCompleted":            AuditEventDiscoveryAzureScanCompleted,
		"AuditEventDiscoveryAzureScanFailed":               AuditEventDiscoveryAzureScanFailed,
		"AuditEventDiscoveryAzureRecommendationsGenerated": AuditEventDiscoveryAzureRecommendationsGenerated,
	}
	for name, value := range checks {
		assert.NotEmpty(t, value, "%s must be a non-empty string", name)
	}
	// Pin the canonical dotted-name values so a future rename lands
	// here too. The strings are the SIEM-side contract.
	assert.Equal(t, "discovery.azure.connection_created", AuditEventDiscoveryAzureConnectionCreated)
	assert.Equal(t, "discovery.azure.connection_deleted", AuditEventDiscoveryAzureConnectionDeleted)
	assert.Equal(t, "discovery.azure.scan_started", AuditEventDiscoveryAzureScanStarted)
	assert.Equal(t, "discovery.azure.scan_completed", AuditEventDiscoveryAzureScanCompleted)
	assert.Equal(t, "discovery.azure.scan_failed", AuditEventDiscoveryAzureScanFailed)
	assert.Equal(t, "discovery.azure.recommendations_generated", AuditEventDiscoveryAzureRecommendationsGenerated)
}
