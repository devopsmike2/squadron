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
