// Wire types for Squadron rollouts. Mirror of services.Rollout.

export type RolloutState =
  | "pending"
  | "in_progress"
  | "paused"
  | "succeeded"
  | "aborted"
  | "rolled_back";

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
  created_at: string;
  updated_at: string;
  completed_at?: string;
}

export interface RolloutInput {
  name: string;
  group_id: string;
  target_config_id: string;
  stages: RolloutStage[];
  abort_criteria: RolloutAbortCriteria;
  notification_url: string;
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
