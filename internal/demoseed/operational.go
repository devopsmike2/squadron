package demoseed

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// This file seeds the demo's operational state — the rows that make the
// flagship "cost spike -> AI proposal -> staged rollout -> action -> incident"
// loop and the Rollouts / Actions / Runners / Incidents / Alerts / Timeline /
// Audit surfaces populated without a live LLM or a real fleet. Everything is
// demo-scoped by reserved id prefixes so RemoveOperational can tear it down.
//
// Design choice (honest, deterministic): rather than depend on the AI proposer
// bridge (which no-ops without ANTHROPIC_API_KEY), we seed a realistic
// AI-proposed rollout directly, marked proposed_by="ai" with reasoning +
// evidence pointing at the demo cost spike. It lands in pending_approval so the
// operator can Approve it and watch the engine drive it — the exact loop the
// proposer would produce, minus the model call.

// Reserved operational demo identities.
const (
	ConfigIDV2        = "cfg-demo-web-prod-baseline-v2"
	rolloutAIProposal = "rlo-demo-ai-proposal"
	rolloutInflight   = "rlo-demo-inflight"
	demoRunnerID      = "demo-runner-1"
	actionDemoID      = "act-demo-restart-1"
	incidentDraftID   = "inc-demo-1"
	incidentPubID     = "inc-demo-2"
	alertRuleIDPrefix = "alr-demo-"
	auditIDPrefix     = "aud-demo-"
	rolloutIDPrefix   = "rlo-demo-"
	demoOperatorActor = "operator:demo@squadron.dev"
)

// TunedYAML is the target config the AI proposal rolls out: the demo baseline
// with the high-cardinality hashing.rounds knob pinned from 12 to 6.
var TunedYAML = strings.Replace(BaselineYAML, "rounds: 12", "rounds: 6", 1)

// SeedOperational provisions the operational demo state. Idempotent: keyed off
// the v2 config's existence, so a second call is a no-op.
func SeedOperational(ctx context.Context, store types.ApplicationStore) error {
	if existing, err := store.GetConfig(ctx, ConfigIDV2); err == nil && existing != nil {
		return nil // already seeded
	}

	now := time.Now().UTC()

	if err := seedTunedConfig(ctx, store, now); err != nil {
		return fmt.Errorf("tuned config: %w", err)
	}
	if err := seedAIProposalRollout(ctx, store, now); err != nil {
		return fmt.Errorf("ai proposal rollout: %w", err)
	}
	if err := seedInflightRollout(ctx, store, now); err != nil {
		return fmt.Errorf("inflight rollout: %w", err)
	}
	if err := seedRunnerAndAction(ctx, store, now); err != nil {
		return fmt.Errorf("runner+action: %w", err)
	}
	if err := seedIncidentDrafts(ctx, store, now); err != nil {
		return fmt.Errorf("incidents: %w", err)
	}
	if err := seedAlertRules(ctx, store, now); err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	if err := seedAuditTrail(ctx, store, now); err != nil {
		return fmt.Errorf("audit trail: %w", err)
	}
	return nil
}

func seedTunedConfig(ctx context.Context, store types.ApplicationStore, now time.Time) error {
	gid := GroupID
	return store.CreateConfig(ctx, &types.Config{
		ID:         ConfigIDV2,
		Name:       "demo-baseline-tuned",
		GroupID:    &gid,
		ConfigHash: hashConfigContent(TunedYAML),
		Content:    TunedYAML,
		Version:    2,
		CreatedAt:  now,
	})
}

// seedAIProposalRollout is the flagship moment: an AI-drafted, staged rollout
// awaiting approval, its reasoning + evidence tied to the seeded cost spike.
func seedAIProposalRollout(ctx context.Context, store types.ApplicationStore, now time.Time) error {
	spikeID := latestDemoSpikeID(ctx, store)
	return store.CreateRollout(ctx, &types.Rollout{
		ID:               rolloutAIProposal,
		Name:             "Pin hashing.rounds 12→6 (cost mitigation)",
		GroupID:          GroupID,
		TargetConfigID:   ConfigIDV2,
		PreviousConfigID: ConfigID,
		Stages: []types.RolloutStage{
			{Mode: types.RolloutStageModePercent, Percentage: 10, DwellSeconds: 300},
			{Mode: types.RolloutStageModePercent, Percentage: 50, DwellSeconds: 600},
			{Mode: types.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0},
		},
		AbortCriteria:   types.RolloutAbortCriteria{MaxDriftedAgents: 2, MaxErrorLogsPerMinute: 100, MinDwellSecondsBeforeAbort: 60},
		State:           types.RolloutStatePendingApproval,
		RequireApproval: true,
		RequestedBy:     "ai-proposer",
		ProposedBy:      types.RolloutProposedByAI,
		ProposalReasoning: "Cost spike attribution shows hashing.rounds=12 driving high-cardinality " +
			"metric buckets (~+312% projected spend). Pinning rounds to 6 cuts bucket cardinality " +
			"~4096→256, reducing memory and export volume without dropping signal. Rolling out " +
			"10%→50%→100% with drift + error-rate guardrails.",
		EvidenceRefs: []types.RolloutEvidenceRef{
			{Kind: "cost_spike", ID: spikeID, Description: "+312% metrics spend spike, critical"},
			{Kind: "recommendation", Description: "Drop/limit high-cardinality attribute driving metric bytes"},
		},
		CreatedAt: now.Add(-2 * time.Minute),
		UpdatedAt: now.Add(-2 * time.Minute),
	})
}

// seedInflightRollout shows a rollout mid-canary so the engine advances it and
// the UI renders live stage progression. Guardrails set so it advances cleanly.
func seedInflightRollout(ctx context.Context, store types.ApplicationStore, now time.Time) error {
	started := now.Add(-40 * time.Second)
	return store.CreateRollout(ctx, &types.Rollout{
		ID:               rolloutInflight,
		Name:             "Roll out batch tuning to web tier",
		GroupID:          GroupID,
		TargetConfigID:   ConfigIDV2,
		PreviousConfigID: ConfigID,
		Stages: []types.RolloutStage{
			{Mode: types.RolloutStageModePercent, Percentage: 25, DwellSeconds: 60},
			{Mode: types.RolloutStageModePercent, Percentage: 60, DwellSeconds: 60},
			{Mode: types.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0},
		},
		// Effectively-never-abort guardrails so the demo rollout progresses to
		// succeeded rather than tripping on the disconnected demo agent.
		AbortCriteria:  types.RolloutAbortCriteria{MaxDriftedAgents: 100000, MaxErrorLogsPerMinute: 0, MinDwellSecondsBeforeAbort: 3600},
		State:          types.RolloutStateInProgress,
		CurrentStage:   1,
		StageStartedAt: &started,
		ProposedBy:     types.RolloutProposedByOperator,
		RequestedBy:    demoOperatorActor,
		CreatedAt:      now.Add(-8 * time.Minute),
		UpdatedAt:      started,
	})
}

func seedRunnerAndAction(ctx context.Context, store types.ApplicationStore, now time.Time) error {
	if err := store.CreateActionRunnerRegistration(ctx, &types.ActionRunnerRegistration{
		RunnerID:         demoRunnerID,
		Hostname:         "demo-web-canary-1.prod.internal",
		PublicKeyPEM:     "-----BEGIN PUBLIC KEY-----\nDEMOKEYPLACEHOLDERNOTUSEDFORVERIFICATION\n-----END PUBLIC KEY-----",
		CapabilitiesJSON: `[{"type":"restart-collector","description":"Restart the OTel collector service"}]`,
		RegisteredAt:     now.Add(-3 * time.Hour),
		LastSeenAt:       now.Add(-20 * time.Second),
	}); err != nil {
		return err
	}
	started := now.Add(-6 * time.Minute)
	completed := now.Add(-5*time.Minute - 40*time.Second)
	return store.CreateActionRequest(ctx, &types.ActionRequest{
		ID:                  actionDemoID,
		RunnerID:            demoRunnerID,
		ActionType:          "restart-collector",
		ParametersJSON:      `{"service":"otelcol","reason":"apply tuned batch config"}`,
		Signature:           "demo-seeded-signature",
		Phase:               "execute",
		Status:              "success",
		ExecutionOutputJSON: `{"exit_code":0,"stdout":"otelcol restarted; healthy in 1.8s","duration_ms":1834}`,
		IssuedAt:            now.Add(-6 * time.Minute),
		ExpiresAt:           now.Add(24 * time.Hour),
		StartedAt:           &started,
		CompletedAt:         &completed,
	})
}

func seedIncidentDrafts(ctx context.Context, store types.ApplicationStore, now time.Time) error {
	draftBody := "## Summary\nA +312% metrics cost spike on the web tier was traced to a high-cardinality " +
		"`hashing.rounds=12` setting. Squadron drafted a staged rollout pinning it to 6.\n\n" +
		"## Timeline\n- Cost spike detected (critical, +312%)\n- AI proposer drafted rollout (pin hashing.rounds 12→6)\n" +
		"- Operator approved; canary began\n- Collector restarted on canary; healthy\n\n" +
		"## Resolution\nhashing.rounds pinned 12→6 via staged rollout; canary healthy.\n\n" +
		"## Follow-ups\n- Confirm downstream dashboards unaffected\n- Decide whether rounds=6 becomes the fleet default"
	if err := store.CreateIncidentDraft(ctx, &types.IncidentDraft{
		ID:              incidentDraftID,
		ActionRequestID: actionDemoID,
		RolloutID:       rolloutAIProposal,
		Status:          "draft",
		Title:           "Cost spike mitigation: pin hashing.rounds on web tier",
		BodyMarkdown:    draftBody,
		DraftContentJSON: `{"summary":"cost spike +312% mitigated by pinning hashing.rounds 12->6",` +
			`"origin_spike":"` + latestDemoSpikeID(ctx, store) + `"}`,
		CreatedAt: now.Add(-4 * time.Minute),
		UpdatedAt: now.Add(-4 * time.Minute),
	}); err != nil {
		return err
	}
	return store.CreateIncidentDraft(ctx, &types.IncidentDraft{
		ID:           incidentPubID,
		Status:       "published",
		Title:        "Batch tuning rollout to reduce export volume",
		BodyMarkdown: "## Summary\nRolled out larger batch sizes to the web tier to cut export overhead.\n\n## Resolution\nCompleted cleanly across all stages.",
		Provider:     "clipboard",
		ExternalID:   "DEMO-1042",
		ExternalURL:  "https://example.com/incidents/DEMO-1042",
		CreatedAt:    now.Add(-3 * time.Hour),
		UpdatedAt:    now.Add(-2*time.Hour - 30*time.Minute),
	})
}

func seedAlertRules(ctx context.Context, store types.ApplicationStore, now time.Time) error {
	rules := []types.AlertRule{
		{
			ID: alertRuleIDPrefix + "high-drift", Name: "High drift rate",
			Description: "Fire when more than 5% of the fleet is config-drifted.",
			Query:       "fleet_drift_status_drifted", ThresholdOperator: types.ThresholdGreater,
			ThresholdValue: 5, IntervalSeconds: 60, Severity: types.AlertSeverityWarning, Enabled: false,
		},
		{
			ID: alertRuleIDPrefix + "agents-offline", Name: "Agents offline",
			Description: "Fire when any agent goes offline.",
			Query:       "fleet_agents_offline", ThresholdOperator: types.ThresholdGreater,
			ThresholdValue: 0, IntervalSeconds: 30, Severity: types.AlertSeverityCritical, Enabled: false,
		},
		{
			ID: alertRuleIDPrefix + "error-spike", Name: "Fleet error-log spike",
			Description: "Fire when fleet error logs exceed 100/min.",
			Query:       "fleet_error_logs_per_minute", ThresholdOperator: types.ThresholdGreater,
			ThresholdValue: 100, IntervalSeconds: 60, Severity: types.AlertSeverityCritical, Enabled: false,
		},
	}
	for i := range rules {
		rules[i].CreatedAt = now
		rules[i].UpdatedAt = now
		if err := store.CreateAlertRule(ctx, &rules[i]); err != nil {
			return err
		}
	}
	return nil
}

// seedAuditTrail writes the backdated operational sequence so the Audit log and
// the Timeline swimlanes render the full loop.
func seedAuditTrail(ctx context.Context, store types.ApplicationStore, now time.Time) error {
	spikeID := latestDemoSpikeID(ctx, store)
	type ev struct {
		off        time.Duration
		actor      string
		eventType  string
		targetType string
		targetID   string
		action     string
		payload    map[string]any
	}
	seq := []ev{
		{-45 * time.Minute, "system", "cost_spike.detected", "cost_spike", spikeID, "detected", map[string]any{"severity": "critical", "pct_above_baseline": 312, "signal": "metrics"}},
		{-44 * time.Minute, "ai-proposer", "proposal.created", "rollout", rolloutAIProposal, "created", map[string]any{"proposed_by": "ai", "reasoning": "pin hashing.rounds 12->6"}},
		{-42 * time.Minute, demoOperatorActor, "rollout.approved", "rollout", rolloutInflight, "approved", map[string]any{"approved_by": demoOperatorActor}},
		{-41 * time.Minute, "system", "rollout.stage_advanced", "rollout", rolloutInflight, "advanced", map[string]any{"stage_index": 0, "percentage": 25}},
		{-10 * time.Minute, "system", "action.dispatched", "action_request", actionDemoID, "dispatched", map[string]any{"action_type": "restart-collector", "runner_id": demoRunnerID}},
		{-9 * time.Minute, demoRunnerID, "action.executed", "action_request", actionDemoID, "executed", map[string]any{"status": "success", "duration_ms": 1834}},
		{-4 * time.Minute, "system", "incident.drafted", "incident_draft", incidentDraftID, "drafted", map[string]any{"title": "Cost spike mitigation: pin hashing.rounds on web tier"}},
		{-3 * time.Minute, demoOperatorActor, "incident.published", "incident_draft", incidentPubID, "published", map[string]any{"provider": "clipboard", "external_url": "https://example.com/incidents/DEMO-1042"}},
	}
	for i, e := range seq {
		ts := now.Add(e.off)
		if err := store.CreateAuditEvent(ctx, &types.AuditEvent{
			ID:         fmt.Sprintf("%s%03d", auditIDPrefix, i+1),
			Timestamp:  ts,
			Actor:      e.actor,
			EventType:  e.eventType,
			TargetType: e.targetType,
			TargetID:   e.targetID,
			Action:     e.action,
			Payload:    e.payload,
			CreatedAt:  ts,
		}); err != nil {
			return err
		}
	}
	return nil
}

// latestDemoSpikeID returns the most recent open demo cost-spike id, or a
// placeholder if none is present yet.
func latestDemoSpikeID(ctx context.Context, store types.ApplicationStore) string {
	spikes, err := store.ListCostSpikeEvents(ctx, types.CostSpikeFilter{Status: "open", Limit: 20})
	if err == nil {
		for _, s := range spikes {
			if strings.HasPrefix(s.ID, SpikeIDPrefix) {
				return s.ID
			}
		}
	}
	return SpikeIDPrefix + "unknown"
}

// RemoveOperational tears down the demo-scoped operational rows. Alert rules are
// deleted and the runner is revoked (both have removal APIs); rollouts, action
// requests, incident drafts, and audit events have no store-level delete, so
// they're left orphaned once the demo group is removed — harmless and invisible,
// mirroring how the demo config is handled.
func RemoveOperational(ctx context.Context, store types.ApplicationStore) error {
	rules, err := store.ListAlertRules(ctx)
	if err == nil {
		for _, r := range rules {
			if strings.HasPrefix(r.ID, alertRuleIDPrefix) {
				if derr := store.DeleteAlertRule(ctx, r.ID); derr != nil {
					return derr
				}
			}
		}
	}
	_ = store.RevokeActionRunnerRegistration(ctx, demoRunnerID, time.Now().UTC())
	return nil
}
