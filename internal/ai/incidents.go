// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DraftIncidentFromAction is Move 3 of the engineer copilot roadmap.
// After an action runs (success or failure), the bridge calls this
// to draft a postmortem-style ticket the operator can review, edit,
// and publish through whatever ticketing system their team uses.
// See docs/incident-drafter-design.md for the data flow and threat
// model.
//
// The function does not write anything. It returns an
// IncidentDraftResult; the bridge persists the IncidentDraft and the
// API later lets the operator publish through a provider plug-in.

// IncidentDraftInput is the input shape the drafter operates on.
// The bridge assembles this from the action request, the result,
// the originating rollout, and the audit timeline. Fields are kept
// flat and primitive so the prompt template stays readable; the
// bridge is responsible for de-referencing IDs.
type IncidentDraftInput struct {
	// Identifiers + framing.
	ActionRequestID string
	RolloutID       string
	GroupName       string
	ActionType      string // e.g. "restart-systemd-service"
	Phase           string // "dry_run" or "execute"
	Status          string // "success" / "failure" / "denied"
	StartedAt       time.Time
	CompletedAt     time.Time

	// Action-specific summary. The bridge produces a one-line
	// description of what ran (e.g. "systemctl restart nginx.service")
	// without exposing internal hostnames or secrets.
	ActionSummary string

	// Trigger story. Carries forward from the proposer when the
	// action originated from an AI-drafted rollout. The drafter
	// uses this for the "what happened" framing of the ticket. May
	// be empty if the operator created the rollout manually and an
	// action ran as part of it.
	TriggerSummary   string
	ProposalReasoning string

	// Outcome bullets the bridge has extracted from the action
	// result's ResultData (planned_command, ran_command,
	// exit_code, unit_status). Kept as a list of short strings to
	// avoid leaking raw stdout into the ticket; the operator can
	// click through to the audit timeline for the unredacted view.
	OutcomeBullets []string

	// Audit references the bridge already has handy. The drafter
	// includes these in the ticket so the reader can cross-link.
	AuditReferences map[string]string

	// Whether the action actually changed state. False for
	// dry-run-only runs; true for successful execute-phase runs.
	StateChanged bool
}

// IncidentDraftResult is the drafter's output. The bridge maps
// these fields onto types.IncidentDraft and persists the result.
type IncidentDraftResult struct {
	// Declined is set when the model decided the action doesn't
	// merit a ticket (e.g. an internal dry-run noise event). When
	// Declined is true, the bridge does NOT persist a draft.
	Declined bool   `json:"declined"`
	Reason   string `json:"reason,omitempty"`

	// Title is the ticket title. Short, factual, no emojis.
	Title string `json:"title,omitempty"`

	// Summary is the "what happened, what we did, where to look"
	// paragraph the operator reads first.
	Summary string `json:"summary,omitempty"`

	// Timeline is the chronological narrative. Each entry has an
	// ISO 8601 timestamp and a short factual description.
	Timeline []IncidentTimelineEntry `json:"timeline,omitempty"`

	// ResolutionApplied describes the actual change, in the
	// minimum-precise form (config key changed from X to Y, service
	// restarted, capacity scaled). Empty when StateChanged was
	// false in the input.
	ResolutionApplied string `json:"resolution_applied,omitempty"`

	// FollowUps are the prompts for human work the drafter cannot
	// automate. The operator edits these before publishing.
	FollowUps []string `json:"follow_ups,omitempty"`

	// AuditReferences mirror the input map so the rendered ticket
	// surfaces clickable references at the bottom.
	AuditReferences map[string]string `json:"audit_references,omitempty"`

	// Metering. The bridge logs these for AI cost tracking.
	Model     string `json:"model,omitempty"`
	TokensIn  int    `json:"tokens_in,omitempty"`
	TokensOut int    `json:"tokens_out,omitempty"`
}

// IncidentTimelineEntry is one chronological entry in the draft.
type IncidentTimelineEntry struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// DraftIncidentFromAction asks the Merge model to write a postmortem
// ticket draft. See package doc and design doc for constraints. The
// model is asked to return strict JSON; we parse it the same way
// the proposer does.
func (s *Service) DraftIncidentFromAction(ctx context.Context, in IncidentDraftInput) (*IncidentDraftResult, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if in.ActionRequestID == "" {
		return nil, errors.New("action_request_id is required")
	}
	if in.ActionType == "" {
		return nil, errors.New("action_type is required")
	}

	resp, err := s.callMessages(ctx, callOpts{
		Model:    s.cfg.MergeModel,
		System:   draftIncidentSystem,
		UserText: buildDraftIncidentUserMessage(in),
	})
	if err != nil {
		return nil, fmt.Errorf("draft incident: %w", err)
	}

	type parsed struct {
		Declined          bool                    `json:"declined"`
		Reason            string                  `json:"reason"`
		Title             string                  `json:"title"`
		Summary           string                  `json:"summary"`
		Timeline          []IncidentTimelineEntry `json:"timeline"`
		ResolutionApplied string                  `json:"resolution_applied"`
		FollowUps         []string                `json:"follow_ups"`
	}
	raw := extractJSONBlock(resp.Text)
	var p parsed
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("draft incident: model response was not valid JSON: %w (raw=%s)", err, truncateString(resp.Text, 400))
	}

	result := &IncidentDraftResult{
		Declined:          p.Declined,
		Reason:            strings.TrimSpace(p.Reason),
		Title:             strings.TrimSpace(p.Title),
		Summary:           strings.TrimSpace(p.Summary),
		Timeline:          p.Timeline,
		ResolutionApplied: strings.TrimSpace(p.ResolutionApplied),
		FollowUps:         p.FollowUps,
		AuditReferences:   in.AuditReferences,
		Model:             resp.Model,
		TokensIn:          resp.TokensIn,
		TokensOut:         resp.TokensOut,
	}
	if !p.Declined {
		if result.Title == "" {
			return nil, errors.New("draft incident: model returned an empty title")
		}
		if result.Summary == "" {
			return nil, errors.New("draft incident: model returned an empty summary")
		}
	}
	return result, nil
}

// RenderIncidentMarkdown turns a draft result into the ticket body
// the UI shows and the provider publishes. Kept separate from the
// AI call so the operator can edit the draft fields in the UI and
// re-render without spending another model call.
func RenderIncidentMarkdown(r *IncidentDraftResult, input IncidentDraftInput) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("# " + r.Title + "\n\n")
	b.WriteString("## Summary\n\n")
	b.WriteString(r.Summary + "\n\n")

	if len(r.Timeline) > 0 {
		b.WriteString("## Timeline\n\n")
		for _, t := range r.Timeline {
			b.WriteString(fmt.Sprintf("  %s  %s\n", t.At.UTC().Format("15:04:05"), t.Text))
		}
		b.WriteString("\n")
	}

	if r.ResolutionApplied != "" {
		b.WriteString("## Resolution applied\n\n")
		b.WriteString(r.ResolutionApplied + "\n\n")
	}

	if len(r.FollowUps) > 0 {
		b.WriteString("## Follow up\n\n")
		for _, f := range r.FollowUps {
			b.WriteString("  " + f + "\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## Audit references\n\n")
	if input.ActionRequestID != "" {
		b.WriteString("  action_request: " + input.ActionRequestID + "\n")
	}
	if input.RolloutID != "" {
		b.WriteString("  rollout: " + input.RolloutID + "\n")
	}
	for k, v := range r.AuditReferences {
		b.WriteString("  " + k + ": " + v + "\n")
	}
	return b.String()
}
