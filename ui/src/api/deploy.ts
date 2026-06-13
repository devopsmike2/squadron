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
}

export interface InventoryPreview {
  path: string;
  hosts: string[] | null;
  fetch_error?: string;
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
