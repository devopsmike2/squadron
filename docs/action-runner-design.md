# Squadron Action Runner: Design Doc

Status: Draft, v0.1 (post-pivot)
Owner: devopsmike2
Targets: Move 2 (post-Move 1 demo), MVP roughly 6 to 8 weeks
focused engineering after the protocol freeze

## Problem

Squadron's existing capability (push OTel collector config via
OpAMP, gate every push through configlint, change windows,
two-person approval, staged rollout, audit) solves about half the
ops cases an engineering team hits. The other half cannot be
addressed through collector configuration alone: restart a stuck
systemd unit, drain a load balancer pool member, toggle a feature
flag, run a pre-approved kubectl command, rotate an expired secret
on a managed instance. For those cases an engineer logs in, runs
the action manually, then writes the ticket.

The Action Runner is the opt-in component that lets Squadron run
those actions on the node, gated by the same policy machinery the
config-push path already uses. The AI proposes, the human approves,
the runner executes, the result lands in audit, and (with Move 3)
the incident ticket drafts itself from the captured context.

## Goals

- The runner can execute scoped node actions on behalf of an
  approved Squadron proposal and report the result back as audit
  evidence.
- A runner refuses to execute anything outside its declared
  capability set. The capability set is owned by the operator who
  installed the runner, not by the proposer.
- Every action request is signed by Squadron and verified by the
  runner before execution. A compromised Squadron control plane
  cannot push a forged action; a compromised runner cannot execute
  one without the signed request.
- Squadron never holds privileged credentials for the node. The
  runner runs locally with whatever scoped privileges the operator
  granted it (typically a dedicated service account); Squadron only
  ever signs requests.
- Dry-run is mandatory for every action type. The UI shows the
  approver what the runner predicts the action will do before they
  approve.
- Every successful action emits a rollback proposal (or marks
  itself as inherently idempotent / non-reversible) so the engineer
  has a one-click revert path.

## Non-Goals

- Arbitrary shell command execution. The runner does not expose a
  "run any command" surface. Every action is a named, schema'd
  operation with explicit parameters.
- Replacing operators' existing CI/CD. Heavyweight deploy actions
  (running an Ansible playbook against a fleet) stay in the deploy
  provider system; the runner handles the surgical, per-node
  actions that don't justify a CI run.
- A plugin marketplace in v1. Operators define actions by editing a
  declarative config on the runner. Plugin distribution and signing
  is a v2 problem.

## Threat Model

Three threats drive the design.

**T1: A compromised Squadron control plane attempts to push a
malicious action.** Mitigation: every action request is signed
with a key Squadron holds and the runner verifies before
execution. The signing key is rotatable per-runner and the runner
also pins the issuer identity at install time, so a swapped
Squadron instance cannot quietly take over an existing runner.

**T2: A compromised runner attempts to escalate beyond its
declared capabilities.** Mitigation: the runner process runs under
a dedicated service account whose OS-level permissions are scoped
to exactly what its declared capabilities need. The signature
check happens before any privileged work; out-of-policy actions
are rejected by the runner itself, not by the OS. Defense in depth.

**T3: A malicious operator (or compromised AI) attempts to abuse
the system to run something outside policy.** Mitigation: every
action requires approval by a human other than the proposer, the
change-window policy still applies, and the audit trail captures
the actor at every step. The two-person rule and the change-window
gate already in Squadron extend to actions automatically because
actions flow through the same proposal machinery.

## Architecture

Two new components, one extension to the existing control plane.

### squadron-action-runner

A standalone Go binary installed on nodes where the operator wants
the capability. Lifecycle:

1. At install time, the operator runs `squadron-action-runner init`
   which generates a runner identity (Ed25519 keypair), prompts
   for the Squadron control plane URL and a one-time enrollment
   token, and writes a config file at
   `/etc/squadron/action-runner.yaml`. The config declares the
   runner's capability set (which action types it is allowed to
   perform and any constraints, e.g. "restart-systemd-service may
   only target units matching `squadron-*` or `nginx*`").
2. On first start, the runner connects to Squadron, presents the
   enrollment token, and registers its identity and capability
   set. Squadron persists this registration and audits it. The
   one-time token is consumed.
3. On each subsequent start the runner authenticates using its
   private key; mutual TLS is the transport.
4. The runner polls (or maintains a long-lived gRPC stream) for
   action requests addressed to it.
5. For each incoming request: verify the signature, check the
   action type is in the declared capability set, run the action's
   dry-run, return the dry-run output to Squadron, wait for the
   "approved, proceed" follow-up, execute, return the result.

The runner does not initiate actions. It only ever responds to
signed requests.

### Squadron control plane extensions

Three new bits in the open core.

**internal/actions package.** Holds the action type registry,
the action protocol structs, and the signer. The registry maps
action type names to a struct that describes (a) parameter schema,
(b) dry-run behavior, (c) execute behavior, (d) rollback behavior
or "irreversible". The signer wraps an Ed25519 signing key and
produces signed action requests.

**Proposal model extension.** A proposal can now carry either a
rollout spec (the v0.52 model), an action spec (new), or both. The
storage schema gains a `proposal_actions` table with action_id,
proposal_id, runner_id, action_type, parameters_json,
dry_run_output, execution_output, status (proposed, approved,
executed, failed, rolled_back). The proposer service produces
either kind of proposal; the existing gate (require_approval,
change windows, two-person rule) covers both kinds the same way.

**UI surfaces for action proposals.** The approval drawer renders
the action with its dry-run output prominently. The approver
clicks "Approve and execute" to proceed; Squadron sends the
"approved" follow-up to the runner; the result lands in the
audit timeline as new event types (action.proposed, action.approved,
action.executed, action.failed, action.rolled_back).

## Action Type Catalog (MVP)

Ship one in v1; the next three slot in as separate sub-tickets.

**restart-systemd-service (MVP).** Parameters: unit_name (string,
required), restart_strategy ("restart" | "try-restart" | "reload",
default "restart"). Constraint at runner config: unit_name must
match a glob declared in the runner's capability set. Dry-run:
`systemctl status <unit>` plus the planned command. Execute: the
chosen restart command. Result: exit code, stdout, stderr, final
unit state. Rollback: not auto-emitted (restart is rarely
something you want to undo; the proposer can include a manual
rollback note).

**kubectl-apply (planned, second).** Parameters: resource_kind,
namespace, name, manifest_yaml. Constraint at runner config: an
allowlist of (kind, namespace) pairs the runner may modify.
Dry-run: `kubectl apply --dry-run=server`. Execute: `kubectl
apply`. Rollback: capture the previous resource state in dry-run,
emit a rollback proposal that re-applies it.

**feature-flag-toggle (planned, third).** Parameters: provider
(launchdarkly, flagsmith, openfeature-generic), flag_key, value,
environment. Constraint at runner config: provider creds plus
allowlist of flag prefixes the runner may touch. Dry-run: fetch
current value, show diff. Execute: update flag. Rollback:
auto-emit a proposal that reverts to the captured previous value.

**lb-pool-member-toggle (planned, fourth).** Parameters: provider
(aws-elb, gcp-lb, haproxy-stats-api, envoy-admin), pool_id,
member_id, state (enabled | disabled | draining). Constraint:
provider creds plus allowlist of pool IDs. Dry-run: fetch current
member state. Execute: toggle. Rollback: auto-emit reversion.

## Protocol

### Capability declaration (runner → Squadron, at registration)

```yaml
runner_id: ed25519:abc123...
hostname: web-prod-1.example
capabilities:
  - type: restart-systemd-service
    constraints:
      unit_name_glob:
        - "squadron-*"
        - "nginx*"
        - "myapp-*"
  - type: kubectl-apply
    constraints:
      allowed:
        - kind: Deployment
          namespace: production
          name_glob: "myapp-*"
```

The runner publishes this once at registration and again on every
change (operator edits the config and reloads the runner). Squadron
stores it and refuses to send actions outside it.

### Signed action request (Squadron → runner)

```json
{
  "request_id": "uuid",
  "proposal_id": "uuid",
  "runner_id": "ed25519:abc123...",
  "action": {
    "type": "restart-systemd-service",
    "parameters": {
      "unit_name": "myapp-api",
      "restart_strategy": "restart"
    }
  },
  "issued_at": "2026-06-17T19:30:00Z",
  "expires_at": "2026-06-17T19:35:00Z",
  "phase": "dry_run" | "execute",
  "signature": "ed25519:..."
}
```

The runner verifies the signature against Squadron's pinned public
key, checks the action type is in its capability set, checks the
parameters satisfy the declared constraints, then performs the
requested phase. The 5-minute expiry stops replayed requests.

### Action result (runner → Squadron)

```json
{
  "request_id": "uuid",
  "phase": "dry_run" | "execute",
  "status": "success" | "failure" | "denied",
  "started_at": "...",
  "completed_at": "...",
  "stdout": "...",
  "stderr": "...",
  "exit_code": 0,
  "result_data": { /* action-specific */ }
}
```

## Rollout Plan

Three phases.

**Phase 1: protocol and one action type (weeks 1-4).** Finalize
the protocol in this doc. Build internal/actions registry +
signer. Build the runner binary with restart-systemd-service.
Wire the proposal model to carry action specs. Get an end-to-end
test working: Squadron signs request, runner verifies, dry-runs,
returns; Squadron asks for approval; approve; runner executes.

**Phase 2: UI and audit (weeks 5-6).** Approval drawer surfaces
the action. Audit timeline shows the new event types. Runner
install docs cover the enrollment flow. First demo video:
"Squadron AI just restarted my stuck systemd unit at 2 AM" in 90
seconds.

**Phase 3: three more action types + security review (weeks 7-8).**
kubectl-apply, feature-flag-toggle, lb-pool-member-toggle. Paid
external security review of the signing scheme (budget five to ten
thousand dollars). Address whatever the review surfaces before any
non-pilot customer install.

## Open Questions

- Should the runner support gRPC streams or stick to HTTP long-poll
  for the initial release? gRPC is cleaner but adds complexity for
  customers running behind strict egress proxies. Recommendation:
  start with HTTPS long-poll, add gRPC in v2.
- How do we handle a runner that loses connectivity mid-execution?
  Recommendation: actions are idempotent or carry an idempotency
  key; the runner re-reports the result on reconnect; Squadron
  treats the second report as a confirmation rather than a new
  action.
- How do we handle multi-step actions (drain pool member, wait,
  restart service, re-add to pool)? Recommendation: in v1, these
  are multiple proposals with explicit ordering. v2 can introduce
  a Workflow type that chains actions with conditions.
- Do we need a kill switch for in-flight actions? Recommendation:
  yes, in v1. The runner accepts a signed "abort" request for any
  request_id and attempts to halt cleanly. For restart-systemd-service
  this is mostly cosmetic (restarts are fast), but for kubectl-apply
  and longer-running actions it matters.

## What This Doc Is For

- Engineering: the protocol and threat model freeze before code
  starts in earnest.
- Sales conversations: a prospect who asks "could you do node
  actions too?" gets pointed at this doc as the answer to "yes,
  here is the design, we add the runner when you sign the SOW."
- Security review: the doc the external reviewer reads before
  asking us hard questions.

## Revision History

- v0.1 (this draft): initial protocol + threat model + MVP scope
  written immediately after the engineer-copilot pivot. Not yet
  reviewed.
