// API client for v0.34 deploy integration.
// Mirrors internal/deploy + handlers/deploy.go.

import { apiDelete, apiGet, apiPost, apiPut } from "./base";

export interface DeployTarget {
  id: string;
  name: string;
  provider: "github";
  github_owner: string;
  github_repo: string;
  github_workflow: string;
  github_branch: string;
  has_credential: boolean;
  default_inputs?: Record<string, string>;
  config_id?: string;
  inventory_path?: string;
  created_at: string;
  updated_at: string;
  /** v0.35: latest run for this target — undefined if never deployed. */
  last_run?: DeployRun;
}

/** v0.35: live OpAMP status for each parsed inventory host. */
export interface HostLiveStatus {
  hostname: string;
  status: "healthy" | "silent" | "never_seen";
  agent_id?: string;
  last_seen?: string;
  silence_for?: string;
}

export interface InventoryPreview {
  path: string;
  hosts: HostLiveStatus[] | null;
  fetch_error?: string;
}

/** v0.35: per-check validation result. */
export interface CheckStatus {
  status: "ok" | "warn" | "fail" | "skip";
  message: string;
}

export interface ValidationResult {
  github_auth: CheckStatus;
  workflow_exists: CheckStatus;
  inventory: CheckStatus;
  lint_check: CheckStatus;
  overall_ok: boolean;
}

export function validateDeployTarget(id: string): Promise<ValidationResult> {
  return apiPost<ValidationResult>(
    `/deploy/targets/${encodeURIComponent(id)}/validate`,
    {},
  );
}

export function redeployRun(id: string): Promise<DeployRun> {
  return apiPost<DeployRun>(
    `/deploy/runs/${encodeURIComponent(id)}/redeploy`,
    {},
  );
}

/** Color for host-status badge. */
export function hostStatusColor(s: HostLiveStatus["status"]): string {
  switch (s) {
    case "healthy":
      return "var(--status-healthy, #22c55e)";
    case "silent":
      return "var(--status-warn, #eab308)";
    case "never_seen":
      return "var(--status-critical, #ef4444)";
  }
}

export function fetchDeployInventory(
  id: string,
): Promise<InventoryPreview> {
  return apiGet<InventoryPreview>(
    `/deploy/targets/${encodeURIComponent(id)}/inventory`,
  );
}

export interface DeployRun {
  id: string;
  target_id: string;
  target_name?: string;
  requested_by: string;
  requested_at: string;
  inputs?: Record<string, string>;
  github_run_id?: number;
  github_run_url?: string;
  status: "queued" | "in_progress" | "completed";
  conclusion?: "success" | "failure" | "cancelled" | "timed_out" | "skipped" | "";
  completed_at?: string;
  expected_hosts?: string[];
  verification_state?: "" | "pending" | "verified" | "missing_agents";
  verified_at?: string;
  notes?: string;
}

export interface LintFinding {
  severity: "error" | "warning" | "info";
  rule: string;
  message: string;
  line?: number;
  path?: string;
}

export interface DeployTargetsResponse {
  items: DeployTarget[];
  count: number;
  enabled: boolean;
}

export function listDeployTargets(): Promise<DeployTargetsResponse> {
  return apiGet<DeployTargetsResponse>("/deploy/targets");
}

export function getDeployTarget(id: string): Promise<DeployTarget> {
  return apiGet<DeployTarget>(`/deploy/targets/${encodeURIComponent(id)}`);
}

export function createDeployTarget(body: {
  name: string;
  github_owner: string;
  github_repo: string;
  github_workflow: string;
  github_branch?: string;
  default_inputs?: Record<string, string>;
  config_id?: string;
  inventory_path?: string;
  pat: string;
}): Promise<DeployTarget> {
  return apiPost<DeployTarget>("/deploy/targets", body);
}

export function updateDeployTarget(
  id: string,
  body: Partial<DeployTarget> & { pat?: string },
): Promise<DeployTarget> {
  return apiPut<DeployTarget>(`/deploy/targets/${encodeURIComponent(id)}`, body);
}

export function deleteDeployTarget(id: string): Promise<{ ok: boolean }> {
  return apiDelete<{ ok: boolean }>(
    `/deploy/targets/${encodeURIComponent(id)}`,
  );
}

export function lintDeployTarget(
  id: string,
): Promise<{ findings: LintFinding[]; config_id: string }> {
  return apiPost<{ findings: LintFinding[]; config_id: string }>(
    `/deploy/targets/${encodeURIComponent(id)}/lint`,
    {},
  );
}

export function listDeployRuns(
  targetID?: string,
): Promise<{ items: DeployRun[]; count: number }> {
  const q = targetID ? `?target_id=${encodeURIComponent(targetID)}` : "";
  return apiGet<{ items: DeployRun[]; count: number }>(`/deploy/runs${q}`);
}

export function getDeployRun(id: string): Promise<DeployRun> {
  return apiGet<DeployRun>(`/deploy/runs/${encodeURIComponent(id)}`);
}

export function triggerDeployRun(body: {
  target_id: string;
  inputs?: Record<string, string>;
  expected_hosts?: string[];
  notes?: string;
}): Promise<DeployRun> {
  return apiPost<DeployRun>("/deploy/runs", body);
}

/** Color for a run's status badge. */
export function runColor(r: DeployRun): string {
  if (r.status !== "completed") return "var(--info, #3b82f6)";
  if (r.conclusion === "success") return "var(--status-healthy, #22c55e)";
  if (r.conclusion === "failure" || r.conclusion === "timed_out")
    return "var(--status-critical, #ef4444)";
  return "var(--status-warn, #eab308)";
}

export function runLabel(r: DeployRun): string {
  if (r.status === "queued") return "Queued";
  if (r.status === "in_progress") return "Running";
  if (r.conclusion === "success") return "Success";
  if (r.conclusion === "failure") return "Failed";
  if (r.conclusion === "cancelled") return "Cancelled";
  if (r.conclusion === "timed_out") return "Timed out";
  return r.status;
}

// ============================================================
// v0.39.0 — DORA-style deploy metrics
// ============================================================
//
// Deploy frequency / change failure rate / MTTR / lead time —
// computed in-process from deploy_runs by the backend. The UI
// presents these as a 4-tile KPI strip on the Deploy page.

export type DORAWindow = "7d" | "30d" | "90d";

export interface TargetDORA {
  target_id: string;
  target_name?: string;
  total_runs: number;
  successful_runs: number;
  failed_runs: number;
  change_failure_rate: number; // 0..1
  mttr_minutes: number;
  lead_time_minutes: number;
}

export interface DORAMetrics {
  window: DORAWindow;
  total_runs: number;
  completed_runs: number;
  successful_runs: number;
  failed_runs: number;
  deploys_per_day: number;
  change_failure_rate: number; // 0..1
  mttr_minutes: number;
  lead_time_minutes: number;
  per_target: TargetDORA[];
}

export function fetchDeployMetrics(
  window: DORAWindow = "30d",
): Promise<DORAMetrics> {
  return apiGet<DORAMetrics>(`/deploy/metrics?window=${window}`);
}

/**
 * Format a minutes-as-float into a "1h 23m" style label. Numbers
 * larger than 24h roll up into days. Zero or NaN returns "—".
 */
export function formatMinutes(min: number | undefined): string {
  if (!min || !Number.isFinite(min) || min <= 0) return "—";
  if (min < 1) return "<1m";
  if (min < 60) return `${Math.round(min)}m`;
  const hours = min / 60;
  if (hours < 24) {
    const h = Math.floor(hours);
    const m = Math.round(min - h * 60);
    return m > 0 ? `${h}h ${m}m` : `${h}h`;
  }
  const days = hours / 24;
  return `${days.toFixed(1)}d`;
}
