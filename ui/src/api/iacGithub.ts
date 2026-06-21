// API client for the v0.89.3 #603 Stream 19 Connect-IaC-repo surface.
//
// Slice 1 (per docs/proposals/603-connect-iac-repo.md §10): GitHub only,
// PAT-only auth, one repo per connection, the seven slice-1 resource
// kinds in §6. The wizard collects the wire shape this client emits;
// the Phase 2 handlers in internal/api/handlers/iac_github.go validate
// and persist it.
//
// Token discipline. The PAT only exists in the React shell's local
// component state and as the `token` field on the two write requests
// below. It is NEVER written to localStorage / sessionStorage / URL
// params / SWR cache. The server seals it via MarshalGitHubPATCreds
// before the substrate sees plaintext.
//
// Note on field shapes: the Go handler is the canonical definition.
// Keep this file in sync when handler fields change — drift surfaces
// at integration time only, which the design doc calls out as a known
// trade-off (same posture as ./discovery.ts).

import { apiDelete, apiGet, apiPost } from "./base";

// HumanizedError mirrors scanner.HumanizedError. The wizard renders
// `message` verbatim and uses `suggested_step` to deep-link back to the
// wizard step the operator needs to revisit. `code` is the wrapper-
// level error class (AuthFailed / RepoNotFound / MalformedRepoFullName
// / NoPlacementMapping / ...).
export interface IaCHumanizedError {
  code: string;
  message: string;
  suggested_step: string;
  doc_link?: string;
}

// Placement-map row, wire-shape. Matches the substrate's
// PlacementMapEntry (snake_case, no IDs).
export interface IaCPlacementEntry {
  provider: string;
  resource_kind: string;
  file_path: string;
}

// Validate endpoint --------------------------------------------------

export interface IaCGitHubValidateRequest {
  token: string;
  repo_full_name: string;
  // default_branch is optional — when empty, the server reads it from
  // GitHub and returns it in the response so the wizard can fill the
  // field on the operator's behalf.
  default_branch?: string;
  placement_map: IaCPlacementEntry[];
}

export interface IaCGitHubPreflightRow {
  provider: string;
  resource_kind: string;
  file_path: string;
  exists: boolean;
  sha_short?: string;
  err?: IaCHumanizedError;
}

export interface IaCGitHubValidateResponse {
  repo_full_name: string;
  default_branch: string;
  repo_err?: IaCHumanizedError;
  preflight_results: IaCGitHubPreflightRow[];
  errors?: IaCHumanizedError[];
}

export function validateIaCGitHub(
  req: IaCGitHubValidateRequest,
): Promise<IaCGitHubValidateResponse> {
  return apiPost<IaCGitHubValidateResponse>("/iac/github/validate", req);
}

// Save (create connection) endpoint ---------------------------------

export interface IaCGitHubSaveConnectionRequest {
  token: string;
  repo_full_name: string;
  default_branch: string;
  // repo_layout: "mono" | "multi". The human partner's explicit ask
  // for slice 1 — captured at connect time so the PR builder can tune
  // path examples without re-asking the operator.
  repo_layout: "mono" | "multi";
  // Optional advanced fields. Empty strings are fine — the handler
  // substitutes the substrate defaults (DefaultBranchPrefix = "squadron/rec",
  // no reviewer request) at save time.
  branch_prefix?: string;
  reviewer_team_handle?: string;
  placement_map: IaCPlacementEntry[];
}

export interface IaCGitHubSaveConnectionResponse {
  connection_id: string;
  repo_full_name: string;
  status: string;
}

export function saveIaCGitHubConnection(
  req: IaCGitHubSaveConnectionRequest,
): Promise<IaCGitHubSaveConnectionResponse> {
  return apiPost<IaCGitHubSaveConnectionResponse>(
    "/iac/github/connections",
    req,
  );
}

// List endpoint ------------------------------------------------------

// IaCGitHubConnection mirrors iacGitHubConnectionRow — the server's
// redacted view of an IaCConnection. The token / cred_ciphertext are
// NEVER on the wire.
export interface IaCGitHubConnection {
  connection_id: string;
  provider: string;
  auth_kind: string;
  repo_full_name: string;
  default_branch: string;
  repo_layout: string;
  branch_prefix?: string;
  reviewer_team_handle?: string;
  placement_map: IaCPlacementEntry[];
  created_at: string; // ISO-8601
}

export interface IaCGitHubListConnectionsResponse {
  connections: IaCGitHubConnection[];
}

export function listIaCGitHubConnections(): Promise<IaCGitHubListConnectionsResponse> {
  return apiGet<IaCGitHubListConnectionsResponse>("/iac/github/connections");
}

// Delete endpoint ----------------------------------------------------

// Idempotent on the server side — a non-existent connection_id
// returns 204 just like a successful delete. The UI mutates the SWR
// cache regardless of success/failure to give the operator immediate
// feedback; a network-level failure surfaces via the apiDelete throw.
export function deleteIaCGitHubConnection(connectionID: string): Promise<void> {
  return apiDelete<void>(
    `/iac/github/connections/${encodeURIComponent(connectionID)}`,
  );
}
