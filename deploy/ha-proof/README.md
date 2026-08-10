# HA proof harness — two Enterprise instances, one Postgres (HA S5, ADR 0035)

Reproducible proof that **two Squadron ENTERPRISE instances sharing one Postgres
backend behave correctly under HA**: leader election, failover, and
cross-instance config delivery.

This is a **verification harness**, not product code. It stands up:

- one `postgres:16-alpine` (the shared control-plane backend), and
- **two** enterprise-composed Squadron binaries against that same DSN
  (`storage.app.type: postgres`), on distinct ports (no load balancer — you
  talk to each instance directly).

## Why the Enterprise binary is mandatory

The OSS binary wires the `AlwaysLeader` elector (no coordination) — every
instance would run every singleton, so both instances would be leaders and this
would not be a valid HA test. You MUST use the **enterprise-composed** binary,
which overlays the Postgres advisory-lock elector
(`squadron-enterprise/leaderelection`). The elector is chosen purely on backend
topology: `storage.app.type == postgres` + a DSN ⇒ `PostgresElector`.

## Prerequisites

- Docker + Docker Compose, Go (1.25+), and the two private repos checked out as
  siblings: `../squadron` (this repo) and `../squadron-enterprise`.
- Ports free on the host: `18080/18081` (HTTP API), `14320/14321` (OpAMP),
  `14317/14318/14319/14322` (OTLP), `5432` (Postgres).

## Build the enterprise binary

From the **enterprise** repo (composes OSS + enterprise wires incl. the elector;
`COMPLIANCE=0` skips the third compliance repo — not needed for HA, keeps
rollouts un-gated by change-window/policy):

```bash
cd ../squadron-enterprise
SKIP_UI=1 COMPLIANCE=0 make build-enterprise
# -> ../squadron-enterprise/bin/squadron-enterprise
```

Confirm the elector is linked in (not the OSS panic-stub):

```bash
strings bin/squadron-enterprise | grep -E 'squadron-enterprise/leaderelection|squadron_leader:'
```

## Run

```bash
cd ../squadron/deploy/ha-proof
./run-proof.sh            # brings up PG + A + B, runs all checks, prints evidence
```

Or drive the phases by hand — see `run-proof.sh` for the exact commands. The
files:

| file | purpose |
|------|---------|
| `docker-compose.postgres.yml` | the single shared `postgres:16-alpine` |
| `instance-a.yaml` / `instance-b.yaml` | the two enterprise instances (same DSN, distinct ports) |
| `collector-nonleader.yaml` | a stock otelcol-contrib pointing at the NON-leader (registers + reports; see caveat) |
| `agentsim/` | a minimal OpAMP client that accepts remote config AND sends the tenant header (see caveat) |
| `drive-rollout.py` | creates a target config + rollout via the API |
| `run-proof.sh` | orchestrator |

### Config notes (enterprise strict mode)

- The enterprise build enables **strict tenant scoping** (ADR 0012), so every
  config carries `ingest.otlp.tenant_id: default` and every OpAMP client MUST
  send an `x-squadron-tenant` header. OSS does not require this.
- The stock `otelcol-contrib` opampextension only **reports** effective config;
  it cannot **accept/apply** a remote config without the OpAMP supervisor, so it
  cannot converge. It also sends no connection headers. `agentsim/` exists to
  cover both gaps (tenant header + accepts-and-echoes remote config), so the
  full `delivered → effective → Synced` convergence can be observed.

## What each check proves + where the evidence is

1. **Leader election — exactly one runner per singleton.**
   Start A, then B. A acquires all 8 singletons; B acquires 0.
   - Logs: `grep 'acquired leadership' logs/a.log | wc -l` ⇒ 8; same on B ⇒ 0.
   - DB (authoritative): `pg_locks` shows 8 advisory locks **granted** (A's 8
     pinned elector connections) and 8 **not-granted / waiting** (B's blocked
     campaigns). One holder per `squadron_leader:<name>` key.

2. **Failover — the other instance takes over, no double-ownership.**
   `kill -9` the leader. Postgres releases its advisory locks when the session
   dies; B's blocked campaigns are granted immediately.
   - Logs: B logs 8 `acquired leadership` lines whose timestamps are strictly
     **after** the leader's death (advisory-lock mutual exclusion ⇒ no window of
     dual ownership ⇒ no duplicate singleton emissions).
   - DB: `pg_locks` flips to 8 granted / 0 waiting, all held by B.

3. **Cross-instance config delivery.**
   Restart the dead instance (it becomes the NON-leader, campaigns block).
   Connect `agentsim` to the **non-leader**. Drive a rollout from the API.
   - The rollout **engine runs on the LEADER**, which logs
     `stage direct-push failed ... "agent not found"` (the agent's socket is on
     the other instance) and writes desired state.
   - The **non-leader's S3a reconcile loop** delivers the config to its
     connected agent; the agent reports it as effective; drift → `synced`; the
     **S3d convergence gate** advances the stage → rollout `succeeded`.

4. **Detectors fire once / discovery on the leader only.**
   `cost-spike-detector`, `alert-evaluator`, and `discovery-scan-scheduler` are
   three of the 8 elected singletons — each held by exactly one instance's
   advisory lock (check 1). So their loops (and any emissions) run on exactly one
   instance by construction.

## Cleanup

```bash
./run-proof.sh --down     # stops instances + agent, tears down the PG container
```
