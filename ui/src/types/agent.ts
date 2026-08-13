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

/** Computed-on-read verdict (backlog #5) that a telemetry-only agent is a
 * likely duplicate of an OpAMP-managed agent sharing its host.name. Advisory
 * only — Squadron never auto-merges or auto-deletes. Absent for agents that are
 * not suspected duplicates. */
export interface SuspectedDuplicate {
  of_agent_id: string;
  of_agent_name: string;
  reason: string;
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
  /** Backlog #5: set when this telemetry-only agent looks like a duplicate of
   * an OpAMP-managed agent on the same host. Drives the warning badge/banner
   * and the Dismiss / Decommission actions. */
  suspected_duplicate_of?: SuspectedDuplicate;
}

/** Agent label keys Squadron manages internally and hides from the
 * operator-facing label chips (e.g. the duplicate-dismissed marker). */
export const RESERVED_LABEL_PREFIX = "squadron.io/";

/** Filter Squadron-internal labels out of a label map for display. */
export function visibleLabels(
  labels: Record<string, string> | undefined,
): [string, string][] {
  return Object.entries(labels ?? {}).filter(
    ([k]) => !k.startsWith(RESERVED_LABEL_PREFIX),
  );
}

export interface AgentStats {
  totalAgents: number;
  onlineAgents: number;
  offlineAgents: number;
  errorAgents: number;
  groupsCount: number;
}
