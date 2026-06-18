// Wire types for Squadron rollouts. Mirror of services.Rollout.

export type RolloutState =
  | "pending"
  | "in_progress"
  | "paused"
  | "succeeded"
  | "aborted"
  | "rolled_back"
  // v0.47 — approval workflow states.
  | "pending_approval"
  | "rejected";

// Stage mode mirrors services.RolloutStageMode. "percent" picks the first
// N% of agents in the group; "label" matches by key=value equality.
export type RolloutStageMode = "percent" | "label";

export interface RolloutStage {
  // Mode may be missing for rollouts persisted before v0.6; treat as "percent".
  mode?: RolloutStageMode;
  percentage?: number;
  label_selector?: Record<string, string>;
  dwell_seconds: number;
}

export interface RolloutAbortCriteria {
  max_drifted_agents: number;
  max_error_logs_per_minute?: number;
  min_dwell_seconds_before_abort?: number;
}

export interface Rollout {
  id: string;
  name: string;
  group_id: string;
  target_config_id: string;
  previous_config_id?: string;
  stages: RolloutStage[];
  abort_criteria: RolloutAbortCriteria;
  notification_url?: string;
  state: RolloutState;
  current_stage: number;
  stage_started_at?: string;
  abort_reason?: string;
  // v0.47 — approval workflow. When require_approval was set at
  // create time, the rollout enters pending_approval and the engine
  // refuses to advance until an approver transitions the state.
  require_approval?: boolean;
  requested_by?: string;
  approved_by?: string;
  approved_at?: string;
  rejected_by?: string;
  rejected_at?: string;
  approval_notes?: string;
  // v0.49 — change-window enforcement. Set by the engine when a
  // tick skips advancement because the target group has an active
  // blackout window. Cleared on the next successful advancement.
  // UI shows a 'In blackout' badge on the rollout card when set.
  last_blackout_reason?: string;
  last_blackout_at?: string;
  // v0.53 SQ-1.1 — AI proposer provenance. Set when the proposer
  // bridge drafted this rollout from a cost spike (or other signal).
  // proposed_by is "ai" | "operator" | "system"; the UI surfaces a
  // small AI badge on rows where proposed_by == "ai" and renders
  // proposal_reasoning + evidence_refs in the approval drawer.
  proposed_by?: string;
  proposal_reasoning?: string;
  evidence_refs?: RolloutEvidenceRef[];
  created_at: string;
  updated_at: string;
  completed_at?: string;
}

// RolloutEvidenceRef is one citation the AI proposer attached to a
// drafted rollout. Kinds are open-ended; the UI renders id +
// description with a clickable URL when present.
export interface RolloutEvidenceRef {
  kind: string;
  id?: string;
  url?: string;
  description?: string;
}

export interface RolloutInput {
  name: string;
  group_id: string;
  target_config_id: string;
  stages: RolloutStage[];
  abort_criteria: RolloutAbortCriteria;
  notification_url: string;
  // v0.47 — when true, the rollout enters pending_approval and waits
  // for an Approve call before the engine advances.
  require_approval?: boolean;
}

// AbortCriteriaRecipe mirrors services.AbortCriteriaRecipe. The
// cookbook is server-of-record; the UI fetches the list, displays it
// in a picker, and (when the operator picks one) prefills the
// abort_criteria fields. Operators can still tweak each value
// afterward.
export interface AbortCriteriaRecipe {
  id: string;
  name: string;
  description: string;
  when_to_use: string;
  criteria: RolloutAbortCriteria;
}

// RolloutTemplate mirrors services.RolloutTemplate. Bigger than a
// recipe: bundles stages + criteria + a default name. The picker
// prefills everything except group_id and target_config_id.
export interface RolloutTemplate {
  id: string;
  name: string;
  description: string;
  when_to_use: string;
  default_name: string;
  stages: RolloutStage[];
  abort_criteria: RolloutAbortCriteria;
}

// LintFinding mirrors configlint.Finding. Subset of the fields the
// preview pane uses; the full type lives in @/types/config but we
// inline what we need here to avoid a cross-module dep cycle.
export interface LintFinding {
  severity: "error" | "warning" | "info";
  rule: string;
  message: string;
  line?: number;
  path?: string;
}

// DiffResult mirrors configdiff.Result.
export interface DiffResult {
  unified: string;
  added: number;
  removed: number;
  identical: boolean;
}

// ConfigSummary is the projection of services.Config returned in the
// preview response. Same shape as the full Config type but kept inline
// here so the preview wire types are self-contained.
export interface ConfigSummary {
  id: string;
  name: string;
  agent_id?: string;
  group_id?: string;
  config_hash: string;
  content: string;
  version: number;
  created_at: string;
}

// RolloutPreview mirrors services.RolloutPreview. The UI displays this
// in the create form so operators see the diff before committing.
export interface RolloutPreview {
  group_id: string;
  current?: ConfigSummary;
  target: ConfigSummary;
  diff: DiffResult;
  lint_findings: LintFinding[];
}
