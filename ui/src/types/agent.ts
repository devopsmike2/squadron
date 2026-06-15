export type AgentStatus = "online" | "offline" | "error";
export type ConfigIntentSource = "agent" | "group";
export type ConfigDriftStatus =
  | "unknown"
  | "synced"
  | "drifted"
  | "no_intent"
  | "no_effective";

export interface ConfigIntent {
  source: ConfigIntentSource;
  source_name?: string;
  config_id: string;
  version: number;
  hash: string;
  updated_at: string;
  content?: string;
}

export interface ConfigDriftDetails {
  intent_hash?: string;
  effective_hash?: string;
  diff?: string;
  checked_at: string;
}

export interface Agent {
  id: string;
  name: string;
  status: AgentStatus;
  last_seen: string;
  version: string;
  group_id?: string;
  group_name?: string;
  labels: Record<string, string>;
  capabilities?: string[];
  effective_config?: string;
  config_intent?: ConfigIntent;
  drift_status?: ConfigDriftStatus;
  drift_details?: ConfigDriftDetails;
  /** v0.36: how Squadron first learned of this agent. "opamp"
   * means the supervisor opened a control connection; "otlp"
   * means we only see telemetry from it (no control channel,
   * no config push, no rollouts). */
  discovery_source?: "opamp" | "otlp";
}

export interface AgentStats {
  totalAgents: number;
  onlineAgents: number;
  offlineAgents: number;
  errorAgents: number;
  groupsCount: number;
}
