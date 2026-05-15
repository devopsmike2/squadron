# Concepts

The four nouns you'll run into everywhere in Squadron are **agents**,
**groups**, **configs**, and **drift**. Read this once and the UI and API
shapes will make sense.

- [Agents](#agents)
- [Groups](#groups)
- [Configs](#configs)
- [Drift](#drift)
- [How it fits together](#how-it-fits-together)

## Agents

An **agent** is one OpenTelemetry collector that's connected to Squadron
over OpAMP. Squadron tracks:

- A stable UUID assigned at first registration.
- The resource attributes from the collector's config (host, service.name,
  deployment.environment, etc.). These become the agent's **labels**.
- Connection status (online / offline / error) — driven by the OpAMP
  heartbeat.
- The currently-effective config (what Squadron last pushed) and the
  reported config hash (what the agent says it's running).
- Drift status (see below).

Agents are managed via `/api/v1/agents`. The UI's Agents page lists them
with filters for status, group, and drift.

### Labels

Agent labels are the same key/value pairs the collector reports on its
OpAMP heartbeat. Squadron doesn't invent labels — whatever your collector
config declares (via `resource_attributes`, the OS attributes processor, or
explicit `labels` in the OpAMP extension config) is what shows up.

Labels matter for rollouts: you can use a label selector to pick a specific
agent or sub-environment as the canary for a staged rollout. See
[Rollouts → label-mode selection](./rollouts.md#label-mode-selection).

## Groups

A **group** is a named collection of agents you want to manage together —
typically by environment, region, or workload type.

- Agents are assigned to at most one group.
- A group can have a default config; new agents that join the group
  inherit it.
- Rollouts target a group, not an individual agent.

Groups are managed via `/api/v1/groups` or the Groups page in the UI.

A common organization: one group per (environment, role) pair. For example,
`prod-app-collectors`, `prod-gateway-collectors`, `staging-app-collectors`.
Granular enough to canary safely without proliferating into single-agent
groups.

## Configs

A **config** is one immutable, versioned OpenTelemetry collector YAML
document. Configs are stored in Squadron with a hash; each save creates a
new version. The hash is what the collector reports back so Squadron can
compute drift.

Configs come in three flavors based on what they're bound to:

| Bound to    | Field        | Behavior                                                     |
|-------------|--------------|--------------------------------------------------------------|
| One agent   | `agent_id`   | Pushed only to that agent. Overrides any group-level config. |
| A group     | `group_id`   | Pushed to every agent in the group that doesn't have an agent-specific override. |
| Neither     | (both empty) | A draft. Useful for staging via a rollout.                    |

Configs are listed at `/api/v1/configs` (with optional `agent_id` or
`group_id` filters). The UI's config editor includes:

- Schema-aware YAML completion for the OTel collector.
- The **lint engine** that flags anti-patterns (missing batch, wrong
  `memory_limiter` position, undefined component references, localhost
  exporters). Findings show up live as you type.
- A **template library** for common pipeline shapes.

The lint engine is also exposed at `/api/v1/configs/lint` for CI.

## Drift

**Drift** is the gap between what Squadron thinks an agent should be
running and what the agent says it's actually running. The drift state
machine has three positions:

- **synced** — the agent's reported config hash matches the latest config
  Squadron has for it. Healthy.
- **drifted** — hashes don't match. The agent is either still applying a
  pending push, has rejected the new config, or is running something
  Squadron didn't send it.
- **unknown** — the agent hasn't reported a hash yet (just-registered or
  recently-reconnected).

Drift transitions are recorded as `agent.drift.drifted` /
`agent.drift.synced` audit events with `from` and `to` hash payloads.
That's what feeds the rollout engine's auto-abort criteria — a stage's
canary going drifted past the threshold flips the rollout to aborted
and rolls back to the previous config.

Drift is a powerful signal precisely because it's an observation, not an
intent. If an agent's local config file is hand-edited, Squadron will see
the hash mismatch and report drift even though no push happened.

## How it fits together

A typical flow:

1. You register a new group (`staging-collectors`).
2. You write a config and save it as the group's default. Squadron pushes
   it to every agent in the group; their drift state goes
   **unknown → drifted → synced**.
3. You hack on the config in the editor, save a new version. To roll it
   out safely you create a [rollout](./rollouts.md) instead of just
   pushing it. Squadron progressively widens the canary set, watching
   drift state at each stage.
4. If a stage's canary drift exceeds the abort threshold, Squadron
   rolls back automatically. The audit log records each transition; the
   alert webhook fires; the rollout state goes
   **in_progress → aborted → rolled_back**.
5. Drift counts and other fleet signals can drive alerts. See
   [Alerts](./alerts.md).
