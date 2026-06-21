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

import { apiBaseUrl } from "../config";

import { getAuthToken, onAuthChallenge } from "./auth-store";
import { apiDelete, apiGet, apiPatch, apiPost } from "./base";

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

// Update placement map endpoint ------------------------------------
//
// v0.89.4 (#610) — the deep-linked-wizard save target. The
// connections page route accepts
// `?connection_id=<uuid>&step=placement&kind=<resource_kind>` and
// auto-opens the wizard on the placement-map step, pre-filled with
// the connection's existing rows. Save in that flow calls this
// endpoint (not the create endpoint) because the connection already
// exists — we're editing the placement_map column only.
//
// Token is NEVER on the wire here — the substrate's stored
// cred_ciphertext is preserved untouched.

export interface IaCGitHubUpdatePlacementMapRequest {
  placement_map: IaCPlacementEntry[];
}

export interface IaCGitHubUpdatePlacementMapResponse {
  connection_id: string;
  repo_full_name: string;
  placement_map: IaCPlacementEntry[];
}

export function updateIaCGitHubPlacementMap(
  connectionID: string,
  req: IaCGitHubUpdatePlacementMapRequest,
): Promise<IaCGitHubUpdatePlacementMapResponse> {
  return apiPatch<IaCGitHubUpdatePlacementMapResponse>(
    `/iac/github/connections/${encodeURIComponent(connectionID)}/placement-map`,
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

// Open-PR endpoint --------------------------------------------------

// IaCGitHubOpenPRRequest is the wire shape POSTed to
// /iac/github/connections/:id/open-pr. The Recommendations tab
// assembles this from the per-step recommendation + the page-level
// account_id.
//
// snippet is the FULL Terraform body the proposer emitted, not a
// truncated preview — the backend appends it byte-for-byte to the
// placement-map file. Any truncation client-side would silently
// land a broken PR.
export interface IaCGitHubOpenPRRequest {
  scan_id: string;
  step_idx: number;
  resource_kind: string;
  snippet: string;
  proposer_reasoning: string;
  affected_resources: string[];
  account_id?: string;
  /** v0.89.12 #628 Stream 29 (slice 2) — structured HCL patch for
   * patch_existing kinds. When present, the backend's HCL-aware
   * merger applies the per-attribute edits in place and the PR
   * ships as a clean drop-in (no manual-merge label). When
   * absent or the merger refuses, the backend falls back to the
   * slice-1.5 append-only behavior. The UI forwards verbatim
   * whatever the discovery handler placed on
   * `Recommendation.hcl_patch`; no client-side schema parsing. */
  hcl_patch?: unknown;
}

export interface IaCGitHubOpenPRResponse {
  pr_number: number;
  pr_url: string;
  branch: string;
  commit_sha: string;
  file_path: string;
  repo_full_name: string;
  /** v0.89.11 #626 Stream 27 — slice 1.5 disposition: "new_file"
   * when the handler wrote a sibling file (clean drop-in),
   * "patch_existing" when the handler appended to the placement
   * file (manual merge required). Absent on pre-v0.89.11 server
   * responses. */
  disposition?: "new_file" | "patch_existing" | string;
  /** v0.89.11 #626 Stream 27 — true on patch_existing dispositions;
   * the UI's success card mirrors this with a "Needs manual merge"
   * marker so the operator's recall is anchored to the same
   * language the PR title carries.
   *
   * v0.89.12 (#628 Stream 29) — slice 2 — this is now driven by
   * `disposition_actual`: false on patch_existing_hcl_merged, true
   * on patch_existing_fell_back_to_append. Existing
   * patch_existing-aware UI code that keys off this boolean keeps
   * working unchanged. */
  manual_merge_required?: boolean;
  /** v0.89.12 #628 Stream 29 (slice 2) — the actual disposition
   * path the handler took:
   *  - "new_file" — slice-1.5 sibling-file write.
   *  - "patch_existing_hcl_merged" — slice-2 HCL-aware merge
   *    completed cleanly. PR is merge-clean.
   *  - "patch_existing_fell_back_to_append" — slice-2 fell back to
   *    slice-1.5 append-only behavior; PR carries the
   *    manual-merge label. The UI's success card uses this to
   *    render either the green "HCL-merged" checkmark or the
   *    amber "Needs manual merge" banner. Absent on pre-v0.89.12
   *    server responses. */
  disposition_actual?:
    | "new_file"
    | "patch_existing_hcl_merged"
    | "patch_existing_fell_back_to_append"
    | string;
  /** v0.89.12 — true when the HCL merger detected
   * lifecycle.ignore_changes on the target resource referencing a
   * patched attribute. The UI's success card surfaces this as a
   * note. */
  lifecycle_ignored?: boolean;
  /** v0.89.12 — populated only on disposition_actual =
   * patch_existing_fell_back_to_append. One of:
   * "parse_error" | "resource_not_found" | "ambiguous_resource"
   * | "unknown_op" | "invalid_value_type" | "no_patch_emitted"
   * | "other". The UI's success card surfaces this so the
   * operator understands WHY the manual-merge banner is back. */
  hcl_patch_failure_reason?: string;
}

// IaCGitHubOpenPRError is a typed Error subclass that preserves the
// server's humanized-error envelope. The default base.ts fetch wrapper
// flattens 4xx bodies into a plain Error whose .message is the
// envelope's message but loses the code / suggested_step. The
// Recommendations tab needs the typed code to route NoPlacementMapping
// → wizard, RepoNotFound / AuthFailed → reconnect, etc., so this
// helper bypasses the base wrapper for the Open-PR path and surfaces
// the full envelope.
export class IaCGitHubOpenPRError extends Error {
  readonly code: string;
  readonly suggested_step: string;
  readonly doc_link?: string;
  readonly status: number;
  constructor(
    status: number,
    envelope: IaCHumanizedError | undefined,
    fallbackMessage: string,
  ) {
    super(envelope?.message ?? fallbackMessage);
    this.name = "IaCGitHubOpenPRError";
    this.code = envelope?.code ?? "Unknown";
    this.suggested_step = envelope?.suggested_step ?? "";
    this.doc_link = envelope?.doc_link;
    this.status = status;
  }
}

// openIaCGitHubPullRequest posts to the Open-PR endpoint, preserves
// the humanized-error code on failure (via IaCGitHubOpenPRError), and
// returns the typed success body on 200. Slice 1's success path is
// the close-the-loop demo — the recommendation card collapses into a
// PR-opened success state once this promise resolves.
//
// Why not reuse apiPost: apiPost flattens 4xx bodies into a plain
// Error whose .message is the envelope message but whose `code` field
// is gone. The Recommendations tab needs the code to choose the right
// recovery link (NoPlacementMapping → wizard, RepoNotFound →
// reconnect, AuthFailed → reconnect, DefaultBranchWriteRefused →
// critical error). Re-implement the small bit of fetch wrapping here
// rather than restructure base.ts.
export async function openIaCGitHubPullRequest(
  connectionID: string,
  req: IaCGitHubOpenPRRequest,
): Promise<IaCGitHubOpenPRResponse> {
  const url = `${apiBaseUrl}/iac/github/connections/${encodeURIComponent(
    connectionID,
  )}/open-pr`;
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
  };
  const token = getAuthToken();
  if (token) {
    headers["Authorization"] = `Bearer ${token}`;
  }

  const response = await fetch(url, {
    method: "POST",
    headers,
    body: JSON.stringify(req),
  });

  if (response.status === 401) {
    // Don't swallow into onAuthChallenge unconditionally — the
    // Open-PR 401 is "GitHub rejected the stored token" (an
    // operator-recoverable IaC-connection state), not "Squadron auth
    // expired" (which onAuthChallenge handles). The handler returns
    // 401 with code=AuthFailed in the former case; for the latter
    // shape (envelope missing code) fall through to the global path.
    let envelope: IaCHumanizedError | undefined;
    try {
      const body = await response.json();
      if (body?.error && typeof body.error === "object") {
        envelope = body.error as IaCHumanizedError;
      }
    } catch {
      // ignore
    }
    if (!envelope) {
      onAuthChallenge();
    }
    throw new IaCGitHubOpenPRError(
      401,
      envelope,
      "Authentication failed. Re-run the IaC connect wizard.",
    );
  }

  if (!response.ok) {
    let envelope: IaCHumanizedError | undefined;
    try {
      const body = await response.json();
      if (body?.error && typeof body.error === "object") {
        envelope = body.error as IaCHumanizedError;
      }
    } catch {
      // ignore — fall back to generic message
    }
    throw new IaCGitHubOpenPRError(
      response.status,
      envelope,
      `Open-PR request failed: ${response.status} ${response.statusText}`,
    );
  }

  return response.json();
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
