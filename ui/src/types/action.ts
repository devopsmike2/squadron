// Action runner types — mirrors internal/storage/applicationstore/types
// for the action runner system (Move 2 of the engineer copilot
// roadmap).
//
// One ActionRequest is one signed request Squadron dispatched to a
// runner. The Phase field tells you whether this row is a dry_run
// preview or the actual execute. The runner posts back a result by
// updating Status, populating DryRunOutputJSON or ExecutionOutputJSON.

export type ActionPhase = "dry_run" | "execute";

export type ActionStatus = "pending" | "success" | "failure" | "denied";

export interface ActionRequest {
  id: string;
  proposal_id?: string;
  runner_id: string;
  action_type: string;
  parameters_json: string;
  signature: string;
  phase: ActionPhase;
  status: ActionStatus;
  denied_for?: string;
  dry_run_output_json?: string;
  execution_output_json?: string;
  issued_at: string;
  expires_at: string;
  started_at?: string;
  completed_at?: string;
}

export interface ActionRequestFilter {
  status?: ActionStatus;
  runner_id?: string;
  proposal_id?: string;
}

export interface ActionRunner {
  runner_id: string;
  hostname: string;
  public_key_pem: string;
  capabilities_json: string;
  registered_at: string;
  last_seen_at: string;
  revoked_at?: string;
}

// Capability shape mirrors internal/actions.Capability. Each runner
// declares the set of action types it is willing to run, plus a
// per-type policy that constrains parameters (e.g. which systemd
// units it will restart). We only need the surface fields here so
// the UI can show "this runner accepts: restart-systemd-service,
// restart-docker-container".
export interface ActionCapability {
  action_type: string;
  policy?: Record<string, unknown>;
}
