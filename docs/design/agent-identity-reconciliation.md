# Design: agent-identity reconciliation (one host = one agent)

Status: **accepted — Option A shipped; promoted to brain ADR 0005**
Date: 2026-07-02

## Implementation (shipped)

Option A landed in two behavior-preserving slices:

- **Slice 1 — `refactor(agentid)` (835a9c8):** extracted `getAgentID` out of
  `internal/otlp/parser` into a new shared `internal/agentid` package (`Derive`,
  frozen namespace `7c0f5d2e-6b1a-4f3c-9e2d-1a2b3c4d5e6f`) so the OTLP and OpAMP
  paths compute the fleet id identically. Pure move; a frozen-namespace guard test
  pins the namespace.
- **Slice 2 — `feat(opamp)` (063d7cb):** OpAMP registration derives the fleet id
  from the `AgentDescription` via `agentid.Derive` and persists the store row under
  it. `Agent` gained `FleetId`/`FleetIdStr` (default = `instance_uid`) and a
  `storeID()` guard (never write a zero UUID). `Agents` gained a second index
  `agentsByFleetId`; `FindAgent` resolves fleet-id-first with an `instance_uid`
  fallback, so config push / restart / cert rotation and the rollout engine (which
  already pass the store `agent.ID` = fleet id) resolve unchanged; the telemetry hot
  path and tracer are untouched (tracer stays keyed by `instance_uid`).
  No-regression fallback: an agent reporting no usable `service.instance.id` keeps
  fleet id = `instance_uid`.

Tests: `deriveFleetId` precedence/fallback, the `Agents` dual-map (fleet-first
resolve, instance fallback, rebind drops stale, disconnect clears both), and a
convergence proof that a distinct `instance_uid` vs `service.instance.id` persists
the store row under the fleet id — race-clean.

---


## Problem

A single physical host that is both **OpAMP-managed** and **shipping OTLP telemetry**
registers as **two agents** in Squadron's fleet:

- the **OpAMP** entry — id = the collector's `instance_uid` — holds config, version,
  effective-config, health;
- the **OTLP** entry — id = `getAgentID(service.instance.id)` — holds the metric/log
  data and the display name.

An operator sees config on one card and telemetry on another. Our on-prem onboarding
works around this by forcing one shared UUID (commit 26f4e81), but any **third-party
agent** whose OpAMP `instance_uid` differs from its OTLP `service.instance.id` (the
common default — the opampextension mints its own ULID) still double-registers.

## Current state (how identity flows)

- **OTLP path** (`internal/otlp/parser/parser.go:391` `getAgentID`): id = the UUID
  `service.instance.id` verbatim, else `UUIDv5("service.instance.id:"+v)`, else
  `host.name`, else `service.name`, else `"default"`. Namespace UUID is frozen.
- **OpAMP path** (`internal/opamp/server.go:193`): id = `uuid.UUID(msg.InstanceUid)`
  verbatim. This id is **load-bearing across the whole OpAMP subsystem**:
  - in-memory `Agents.agentsById` map (`agents.go`),
  - config push `ConfigSender.FindAgent(agentId)` (`config_sender.go:48`),
  - rollout dispatch → `commander.SendConfigToAgent(agent.ID, …)` → `FindAgent`
    (`rollouts/engine.go:925`),
  - restart (`server.go` `RestartAgent`) and cert rotation
    (`agents.go` `OfferAgentConnectionSettings`),
  - disconnect → `UpdateAgentStatus` (`server.go:167`),
  - connection tracer spans.
- **Store**: one `agents` table (`sqlite/sqlite.go:74`) written by BOTH paths →
  two rows when the ids differ.
- **Telemetry data** (DuckDB `metrics_*`/`logs`/`traces`) is keyed by
  `agent_id = getAgentID(...)` and enriched by `enricher.go` — the OTLP-derived id.

So today it only "just works" because for a well-behaved collector
`instance_uid == service.instance.id == a UUID`, and all three (store, OpAMP wire,
telemetry) collapse to that one value. The moment they diverge, you get two rows.

## Options

### Option A — Fleet id = `getAgentID(reported service.instance.id)`, wire keeps `instance_uid` via a mapping layer  ⟵ recommended

On OpAMP registration, compute the Squadron **fleet id** the same way the OTLP path
does — `getAgentID(AgentDescription.service.instance.id)` (fallback: `instance_uid`
when the agent reports no usable identity → no regression). Use that as the
`AgentService` record id, so it **converges with the OTLP discovery row and the
telemetry `agent_id`** — one row, telemetry + config together.

The OpAMP **wire** still needs the raw `instance_uid` to send on the socket, so the
in-memory `Agents` struct gains a second index:

- `agentsById map[uuid.UUID]*Agent` (wire instance_uid — unchanged)
- `agentsByFleetId map[uuid.UUID]*Agent` (new)

and the handful of wire ops that currently `FindAgent(id)` resolve by fleet id first
(fallback instance_uid): `config_sender.go:48`, `RestartAgent`, cert rotation. The
rollout engine is **unchanged** — it already passes the store `agent.ID`, which now
equals the fleet id, and resolves through the new map.

- **Blast radius:** contained to `internal/opamp` (registration + `Agents` dual-map +
  3 `FindAgent` call sites) plus extracting `getAgentID` into a shared package
  (`internal/agentid`) so OTLP and OpAMP compute it identically. **Telemetry hot path
  untouched.**
- **Pros:** fleet id becomes the deterministic, OTLP-consistent identity; telemetry
  path unchanged; rollouts unchanged; one row per host for *any* agent.
- **Cons:** touches load-bearing OpAMP wire lookups (needs careful tests); on upgrade,
  existing OpAMP agents re-register under their new fleet id (old rows age out — a
  one-time cosmetic churn on single-instance OSS).

### Option B — Keep `instance_uid` canonical, redirect OTLP telemetry to the OpAMP agent

Keep the OpAMP id as the fleet id; when OTLP telemetry arrives with a
`service.instance.id` that matches a known OpAMP agent, rewrite its `agent_id` to that
agent's `instance_uid` before the DuckDB write (via a `service.instance.id →
instance_uid` alias the OpAMP registration maintains).

- **Blast radius:** the **OTLP hot path** (enricher/discovery must resolve + rewrite
  the id per batch), plus an alias registry and pre-existing-row merge + ordering
  (OTLP-first vs OpAMP-first) handling.
- **Pros:** OpAMP wire ops untouched.
- **Cons:** adds a lookup/rewrite to the hot ingest path; telemetry `agent_id` no
  longer equals `getAgentID` (surprising); messy merge of rows already written under
  the OTLP id. Higher risk where it hurts most (ingest).

## Recommendation

**Option A.** It puts the deterministic OTLP-derived id at the center, leaves the
high-throughput telemetry path alone, and localizes the only real complexity (wire id
≠ fleet id) to the in-memory `Agents` struct where a two-key map cleanly solves it.

## Risk / migration / back-compat

- **No-regression fallback:** if an OpAMP agent reports no usable `service.instance.id`,
  fleet id = `instance_uid` (today's behavior).
- **Re-registration on upgrade:** existing OpAMP agents get a new fleet id once; old
  rows go stale and age out via the existing retention GC. Acceptable for
  single-instance OSS; note in the release notes.
- **Correlation limits:** if an agent reports a *different* `service.instance.id` over
  OpAMP than in its OTLP resource, they can't be auto-correlated — that's a
  misconfiguration, not something to paper over. `host.name` is a possible secondary
  correlator (future).
- **Tests:** unit-test the shared `getAgentID`; unit-test the `Agents` dual-map
  (fleet-id + instance_uid resolution, disconnect removes both); an integration test
  that an OpAMP registration + OTLP telemetry for the same `service.instance.id` yield
  one agent. **Live-test** (per the definition-of-done): a third-party-style collector
  (opampextension default ULID + a distinct OTLP `service.instance.id`) shows one card.

## Open decision for Michael

Approve **Option A** (recommended), prefer **Option B**, or **split it** — ship the
low-risk shared-`getAgentID` extraction + fallback first, defer the wire mapping-layer
change behind a flag.
