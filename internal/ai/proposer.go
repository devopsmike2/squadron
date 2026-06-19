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

// ProposeFromCostSpike is the v0.53 AI proposer. Given a cost spike
// the detector fired plus contextual data about the affected fleet
// (top contributing agents, attributes, recommendations already on
// file), the proposer asks the merge-grade model to draft a
// staged rollout that would address the spike. The result is a
// ProposalResult containing a RolloutInputCandidate payload the
// caller can pipe through services.RolloutService.Create, plus a
// natural-language reasoning string the UI surfaces in the
// approval drawer and the SIEM fan-out carries to external
// systems.
//
// This method NEVER applies the proposal directly. It produces a
// proposal record that goes through Squadron's existing
// require_approval gate. The compliance posture from prior versions
// is preserved: AI never bypasses the human + the change-window +
// the staged rollout pipeline. The AI just drafts; Squadron decides
// whether to apply.
//
// Squadron Move 1 (the demo loop). The bridge daemon (SQ-1.4) is
// the usual caller; tests can call this directly with a stubbed
// client.

// CostSpikeContext is the input to ProposeFromCostSpike. It's a
// flat struct rather than the raw applicationstore CostSpikeEvent
// because the proposer also wants ambient fleet context (top
// contributors, recent lint findings, recommendations already on
// file) that lives in different packages. The bridge daemon
// assembles this from the cost-spike record plus a few short
// reads.
type CostSpikeContext struct {
	// Identifiers + framing.
	SpikeID  string
	Signal   string // "metrics", "logs", "traces"; optional
	Severity string // "warn" or "critical"

	// Cost framing.
	BaselineMonthlyUSD   float64
	PeakMonthlyUSD       float64
	PeakPctAboveBaseline float64
	StartedAt            time.Time

	// Attribution — top contributors from the detector's payload.
	TopAgents     []string // agent IDs or hostnames, in descending order of contribution
	TopAttributes []string // attribute names driving the spike (e.g. "container.id", "k8s.pod.uid")

	// Group targeting. The proposer needs a group to address; the
	// bridge daemon picks the smallest blast-radius group whose
	// agents overlap with TopAgents.
	GroupID   string
	GroupName string

	// Ambient context the proposer summarizes into the prompt.
	RecentLintFindings    []string // rule IDs that recently fired on configs in this group
	RecentRecommendations []string // titles of open recommendations for this group
}

// EvidenceRefCandidate mirrors services.EvidenceRef but lives in
// the ai package so the proposer doesn't import services (and we
// avoid a circular dependency between internal/ai and
// internal/services). The bridge daemon converts these to
// services.EvidenceRef when it posts the proposal.
type EvidenceRefCandidate struct {
	Kind        string `json:"kind"`
	ID          string `json:"id,omitempty"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
}

// RolloutInputCandidate is the proposer's draft rollout. Shape
// mirrors services.RolloutInput intentionally so the bridge daemon
// can convert with a trivial mapping. Kept here so the ai package
// has no import dependency on services.
//
// Fields the proposer never sets (RequestedBy, ProposedBy,
// ProposalReasoning, EvidenceRefs) are filled in by the bridge
// daemon before it calls services.RolloutService.Create.
type RolloutInputCandidate struct {
	Name            string                  `json:"name"`
	GroupID         string                  `json:"group_id"`
	TargetConfigID  string                  `json:"target_config_id"`
	Stages          []RolloutStageCandidate `json:"stages"`
	AbortCriteria   AbortCriteriaCandidate  `json:"abort_criteria"`
	NotificationURL string                  `json:"notification_url,omitempty"`
	RequireApproval bool                    `json:"require_approval"`
}

// RolloutStageCandidate is the per-stage shape the model fills in.
// Mode is "percentage" (canary by % of fleet) or "label" (target a
// labeled subset). Mirrors services.RolloutStage.
type RolloutStageCandidate struct {
	Mode          string            `json:"mode"`
	Percentage    int               `json:"percentage,omitempty"`
	LabelSelector map[string]string `json:"label_selector,omitempty"`
	DwellSeconds  int               `json:"dwell_seconds"`
}

// AbortCriteriaCandidate mirrors services.RolloutAbortCriteria.
type AbortCriteriaCandidate struct {
	MaxDriftedAgents           int `json:"max_drifted_agents,omitempty"`
	MaxErrorLogsPerMinute      int `json:"max_error_logs_per_minute,omitempty"`
	MinDwellSecondsBeforeAbort int `json:"min_dwell_seconds_before_abort,omitempty"`
}

// ProposalKind is the discriminator for the v0.79 structured output
// union. Old responses without a kind field decode as
// ProposalKindRollout for backwards compatibility — the field is
// optional and defaults to rollout.
type ProposalKind string

const (
	ProposalKindRollout ProposalKind = "rollout"
	ProposalKindPlan    ProposalKind = "plan"
)

// PlanCandidate is the proposer's draft multi step plan. Shape
// mirrors handlers.CreatePlanRequest — Steps is the ordered list
// the bridge daemon hands to services.RolloutService.CreatePlan.
// Each step is a PlanStepCandidate carrying an inline YAML config
// snippet that the v0.78 plan create path materializes server side.
//
// Plans are emitted for cost spikes where a single config change
// won't fix the spike, OR where staged progressive changes with
// observation between steps reduce regression risk. See
// proposer_prompt.go for the decision framework the model sees.
type PlanCandidate struct {
	Steps []PlanStepCandidate `json:"steps"`
}

// PlanStepCandidate is one rollout intent inside a plan. Each step
// supplies an InlineConfigSnippet (YAML the v0.78 server materializes
// into a Config row) and the same stages/abort_criteria shape as a
// standalone rollout. PlanID + PlanStepIndex are assigned by the
// server at CreatePlan time — the model doesn't set them.
type PlanStepCandidate struct {
	Name                string                  `json:"name"`
	GroupID             string                  `json:"group_id"`
	InlineConfigSnippet string                  `json:"inline_config_snippet"`
	Stages              []RolloutStageCandidate `json:"stages"`
	AbortCriteria       AbortCriteriaCandidate  `json:"abort_criteria"`
	// RequireApproval is honored on step 0 only — steps 1..N are
	// forced to false server side per the v0.69 design (plans
	// approve as a unit at step 0). The model may set it on step 0
	// when the change is risky enough to gate behind operator
	// approval.
	RequireApproval bool `json:"require_approval,omitempty"`
}

// ProposalResult is what ProposeFromCostSpike returns. The proposer
// either returns a Proposal (which the bridge daemon converts +
// posts) or Declined=true with a Reason (no good action to propose;
// the bridge daemon logs and moves on).
//
// v0.79 — Kind discriminates between rollout and plan responses.
// Empty / missing Kind decodes as ProposalKindRollout for backwards
// compatibility with v0.58-78 prompt outputs. When Kind is plan, the
// Plan field carries the candidate; when rollout (or empty), the
// Proposal field carries the candidate. The bridge dispatches on
// Kind at decode time.
type ProposalResult struct {
	// Declined is set when the model decided no productive
	// proposal exists for the given spike. Common reasons: the
	// spike is too small to act on, the attribution is ambiguous,
	// the recommended action would require operator judgment
	// the proposer can't make. When Declined is true, the rest
	// of the fields are unset and the bridge daemon should not
	// post anything.
	Declined bool   `json:"declined"`
	Reason   string `json:"reason,omitempty"`

	// v0.79 — discriminator. Set to one of ProposalKindRollout or
	// ProposalKindPlan. Empty defaults to rollout for backwards
	// compatibility with model outputs that don't emit the field.
	Kind ProposalKind `json:"kind,omitempty"`

	// Proposal is the staged rollout draft the bridge daemon
	// converts into a services.RolloutInput. Set when Kind is
	// rollout (or empty for back-compat).
	Proposal RolloutInputCandidate `json:"proposal,omitempty"`

	// Plan is the multi step plan the bridge daemon converts into
	// services.RolloutService.CreatePlan. Set when Kind is plan.
	// v0.79.
	Plan PlanCandidate `json:"plan,omitempty"`

	// Reasoning is the natural-language explanation that flows
	// onto the rollout record as proposal_reasoning. The UI
	// surfaces this in the approval drawer; SIEM fan-out carries
	// it to external systems.
	Reasoning string `json:"reasoning,omitempty"`

	// Evidence is the list of refs the model cited. The bridge
	// daemon converts these to services.EvidenceRef and persists
	// alongside the proposal.
	Evidence []EvidenceRefCandidate `json:"evidence,omitempty"`

	// Metering. The handler logs these so we can track AI cost
	// per proposal.
	Model     string `json:"model,omitempty"`
	TokensIn  int    `json:"tokens_in,omitempty"`
	TokensOut int    `json:"tokens_out,omitempty"`
}

// ProposeFromCostSpike asks the Merge model (Sonnet by default) to
// draft a staged rollout that would address the supplied cost
// spike. See package doc for the constraints and prompt approach.
//
// Errors are returned for service-level problems (disabled, HTTP
// failure, malformed response that can't be salvaged). The
// proposer declining to propose is NOT an error; it's a normal
// ProposalResult with Declined=true.
func (s *Service) ProposeFromCostSpike(ctx context.Context, in CostSpikeContext) (*ProposalResult, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if in.GroupID == "" {
		return nil, errors.New("group_id is required")
	}
	if in.SpikeID == "" {
		return nil, errors.New("spike_id is required")
	}

	resp, err := s.callMessages(ctx, callOpts{
		Model:    s.cfg.MergeModel,
		System:   proposeFromCostSpikeSystem,
		UserText: buildProposeUserMessage(in),
	})
	if err != nil {
		return nil, fmt.Errorf("propose from cost spike: %w", err)
	}

	// Parse the JSON block out of the response. The system prompt
	// asks for a strict JSON object; the helper extracts it even
	// when the model preambles with a sentence.
	//
	// v0.79 — the parsed shape carries both Proposal (rollout
	// candidate) and Plan (plan candidate). Kind discriminates;
	// empty Kind defaults to rollout for backwards compatibility
	// with v0.58-78 model outputs.
	type parsed struct {
		Declined  bool                   `json:"declined"`
		Reason    string                 `json:"reason"`
		Kind      ProposalKind           `json:"kind"`
		Proposal  RolloutInputCandidate  `json:"proposal"`
		Plan      PlanCandidate          `json:"plan"`
		Reasoning string                 `json:"reasoning"`
		Evidence  []EvidenceRefCandidate `json:"evidence"`
	}
	raw := extractJSONBlock(resp.Text)
	var p parsed
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("propose from cost spike: model response was not valid JSON: %w (raw=%s)", err, truncateString(resp.Text, 400))
	}

	// Default Kind to rollout when the model didn't emit the field.
	// Old (pre v0.79) prompt outputs that only set Proposal land
	// here cleanly.
	kind := p.Kind
	if kind == "" {
		kind = ProposalKindRollout
	}

	result := &ProposalResult{
		Declined:  p.Declined,
		Reason:    strings.TrimSpace(p.Reason),
		Kind:      kind,
		Reasoning: strings.TrimSpace(p.Reasoning),
		Evidence:  p.Evidence,
		Model:     resp.Model,
		TokensIn:  resp.TokensIn,
		TokensOut: resp.TokensOut,
	}
	if !p.Declined {
		switch kind {
		case ProposalKindRollout:
			result.Proposal = p.Proposal
			if err := validateProposal(p.Proposal, in.GroupID); err != nil {
				return nil, fmt.Errorf("propose from cost spike: model returned an invalid proposal: %w", err)
			}
		case ProposalKindPlan:
			result.Plan = p.Plan
			if err := validatePlan(p.Plan, in.GroupID); err != nil {
				return nil, fmt.Errorf("propose from cost spike: model returned an invalid plan: %w", err)
			}
		default:
			return nil, fmt.Errorf("propose from cost spike: unknown kind %q (expected rollout or plan)", kind)
		}
	}
	return result, nil
}

// validatePlan catches obvious problems on a plan candidate before
// the bridge daemon hands it to services.RolloutService.CreatePlan.
// v0.79. Mirrors validateProposal's "smoke test" posture — the
// full validation happens at CreatePlan time.
func validatePlan(p PlanCandidate, expectedGroupID string) error {
	if len(p.Steps) == 0 {
		return errors.New("plan has no steps")
	}
	if len(p.Steps) > 10 {
		// 10 step ceiling. A plan with more than this many steps
		// is almost certainly a model that's lost the plot —
		// CreatePlan accepts up to 1000 but a sane proposer
		// should rarely emit more than 3-4. Cap loudly here so
		// pathological outputs don't sneak through.
		return fmt.Errorf("plan has %d steps (max 10)", len(p.Steps))
	}
	for i, step := range p.Steps {
		if step.GroupID == "" {
			return fmt.Errorf("plan step %d missing group_id", i)
		}
		if step.GroupID != expectedGroupID {
			return fmt.Errorf("plan step %d group_id %q does not match context group_id %q",
				i, step.GroupID, expectedGroupID)
		}
		if strings.TrimSpace(step.InlineConfigSnippet) == "" {
			return fmt.Errorf("plan step %d missing inline_config_snippet", i)
		}
		if len(step.Stages) == 0 {
			return fmt.Errorf("plan step %d has no stages", i)
		}
	}
	return nil
}

// truncateString is a small helper that complements truncate
// (which deals in []byte) so error messages can include a short
// excerpt of a misshapen model response without dragging in the
// full body.
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// validateProposal catches obvious problems before we hand the
// candidate to the rollout service. We intentionally don't
// re-implement the full validator here; this is the proposer's
// smoke test.
func validateProposal(p RolloutInputCandidate, expectedGroupID string) error {
	if p.GroupID == "" {
		return errors.New("proposal missing group_id")
	}
	if p.GroupID != expectedGroupID {
		return fmt.Errorf("proposal group_id %q does not match context group_id %q", p.GroupID, expectedGroupID)
	}
	if p.TargetConfigID == "" {
		return errors.New("proposal missing target_config_id")
	}
	if len(p.Stages) == 0 {
		return errors.New("proposal has no stages")
	}
	for i, st := range p.Stages {
		switch st.Mode {
		case "percentage", "label":
			// allowed
		default:
			return fmt.Errorf("proposal stage %d has unknown mode %q", i, st.Mode)
		}
		if st.Mode == "percentage" && (st.Percentage <= 0 || st.Percentage > 100) {
			return fmt.Errorf("proposal stage %d has invalid percentage %d", i, st.Percentage)
		}
		if st.DwellSeconds < 0 {
			return fmt.Errorf("proposal stage %d has negative dwell_seconds", i)
		}
	}
	return nil
}
