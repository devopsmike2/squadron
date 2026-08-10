# High availability (multi-instance)

Squadron can run **active-active**: multiple interchangeable instances behind a
load balancer, sharing one Postgres/Aurora application store, with exactly one
instance running singleton orchestration and every instance delivering config to
the agents it owns. This is the org-scale HA architecture from **ADR 0035**.

This guide picks up from [deployment.md](./deployment.md#postgres-for-ha) and is
a sibling of [sqlite-to-postgres-migration.md](./sqlite-to-postgres-migration.md):
move your store to Postgres first, then turn on multi-instance HA here.

> **Multi-instance HA requires the Enterprise build + a Postgres/Aurora store.**
> The Postgres-backed leader election that makes active-active safe is an
> Enterprise capability. **OSS single-instance behavior is unchanged** — a lone
> OSS instance is always its own leader, runs every loop, and needs none of
> this. See [WHEN HA applies](#when-ha-applies) below before you scale out.

- [When HA applies](#when-ha-applies)
- [Load balancer & routing](#load-balancer-routing)
- [Leader election](#leader-election)
- [Connection registry](#connection-registry)
- [Upgrades & rollback](#upgrades-rollback)
- [Data tier](#data-tier)
- [Validate your HA](#validate-your-ha)

## When HA applies

Multi-instance HA is **opt-in and Enterprise-gated**. Two things must both be
true before running more than one instance:

1. **The Enterprise build.** Leader election across instances is done with a
   Postgres **advisory-lock elector** that ships in the enterprise composition
   (ADR 0035 S2). The OSS build wires an `AlwaysLeader` elector with no
   coordination — if you ran two OSS instances, *both* would be leaders and every
   singleton loop would run twice (double rollouts, duplicate alerts, N-fold
   usage reports). **Do not run more than one OSS instance.**
2. **A Postgres/Aurora application store** (`storage.app.type: postgres`, ADR
   0033). SQLite does not replicate and is single-writer; it is single-instance
   only. Migrate first — see
   [sqlite-to-postgres-migration.md](./sqlite-to-postgres-migration.md).

The elector is selected purely on **backend topology**: a Postgres DSN
(`storage.app.type: postgres` + a DSN) selects the advisory-lock elector; sqlite
or no store falls back to `AlwaysLeader`. There is **no license flag or elector
config** to set — an Enterprise instance pointed at Postgres is HA-ready as-is.
(This is deliberately not license-gated: silently downgrading a Postgres fleet to
`AlwaysLeader` would double-run coordinated singletons — an infra-correctness
issue, not a downgradable feature.)

Single-instance stays fully supported on both editions and is unchanged. HA is
strictly additive.

## Load balancer & routing

Put all instances behind one load balancer. The app tier is **stateless** — all
durable state is in the shared store — so:

- **Any instance serves any UI, REST/API, and OTLP ingest request.** Route these
  round-robin; no session affinity is needed.
- **OpAMP connections land on any instance, with NO stickiness required.** An
  agent opens its long-lived OpAMP WebSocket to whichever instance the LB picks
  and stays pinned there for the life of that connection (a normal long-lived
  WS). You do **not** need to configure sticky sessions for OpAMP: config
  delivery does not depend on *which* instance an agent lands on. Whichever
  instance owns an agent's socket reconciles that agent toward desired state via
  its per-instance reconcile loop (ADR 0035 S3a). A config change computed on the
  leader is written to durable desired state, and the owning instance delivers it
  — even if that is a different instance from the one that made the decision.
- **Health-check on `/readyz`.** Point the LB/readiness probe at `GET /readyz`
  (a cheap store `Ping` — an instance that can't reach Postgres is pulled from
  rotation). Use `GET /livez` for the liveness probe: it is dependency-free and
  returns 200 while the process is up, so a slow store never crash-loops a live
  instance.

!!! note "Enterprise strict tenant scoping"
    The Enterprise build enforces strict tenant scoping (ADR 0012): OpAMP
    connections must carry an `x-squadron-tenant` header and OTLP ingest requires
    a tenant id. Make sure your collectors send the header (the OpAMP extension's
    `headers:` block) and that `ingest.otlp.tenant_id` is set. This is a tenanting
    requirement, not an HA one, but it bites the first time you point real
    collectors at an Enterprise cluster.

### Convergence latency

Config changes converge within the **reconcile interval** (default **30s**,
tunable via `ha.reconcile_interval`). The reconcile loop reading durable desired
state — not any notification — is the delivery guarantee: a missed signal or an
instance restart still converges on the next tick. A LISTEN/NOTIFY latency
optimization to converge faster than a full tick (ADR 0035 S3c) is **planned for
Enterprise and not yet shipped** — do not design around sub-tick delivery today.

## Leader election

Leader election is **automatic** and needs **no config beyond the shared Postgres
DSN**. Each singleton campaigns for a per-name session-scoped `pg_advisory_lock`;
whichever instance holds the lock runs that loop, and competitors block until the
lock is released.

Exactly one instance across the cluster runs each of these singleton background
jobs (the rest stand by):

- rollout sequencing + abort evaluation
- cost-spike detection
- alert evaluation
- silent-agent watcher
- discovery scan scheduling
- AI proposer bridge
- deploy poller (open-run sync)
- usage reporting

The division of labor is **leader decides, instances deliver**: the leader
sequences a rollout by writing desired state for the stage's agents, and each
instance reconciles the agents *it* owns into that stage. The leader never needs
a WebSocket to an agent it doesn't own.

**Failover is automatic and fast.** If the leader dies, its Postgres session ends
and the advisory locks release; a standby instance's blocked campaign is granted
and it picks up every singleton **within seconds** (proven at ~10ms in the S5
proof-out). Advisory-lock mutual exclusion means there is no window of dual
ownership, so singleton work is never double-run across the handover. Delivery and
reconciliation continue on every instance throughout — only the *decide/sequence*
half pauses briefly until re-election.

## Connection registry

Because an agent is connected to exactly one instance, the cluster keeps a
**connection registry** in the store so the leader (and operators) can see who
owns what:

- Keyed **`agent_id → instance_id + heartbeat`** (`connected_at`,
  `last_heartbeat_at`). One owner per agent; reconnect is last-writer-wins.
- The owning instance **refreshes the heartbeat every reconcile tick** for each
  agent connected to it.
- **Stale grace = 2× the reconcile interval** (default 60s). A live owner
  refreshes well inside that window; a dead owner's rows cross the grace and
  become reclaimable, and the agent re-registers under its new owner on
  reconnect. (Heartbeat-based liveness is deliberate: the OpAMP server has no
  ping/pong or read deadline, so a silently-dead peer is only otherwise detected
  by TCP keepalive — effectively unbounded.)

The registry is for **ownership/coverage visibility and coordination** — it is
not the delivery mechanism. Delivery is guaranteed by each instance reconciling
its owned agents against durable desired state; the registry tells you which
instances are live and how much of the fleet each one covers.

## Upgrades & rollback

Roll the cluster **one instance at a time** — no `Recreate`, no full-stop
maintenance window:

1. Drain and replace a **non-leader** instance first; its agents reconnect to
   another instance through the LB and re-register in the connection registry.
   Reconcile converges them; no config is lost.
2. When you drain the **leader**, its advisory locks release and another instance
   is elected within seconds (see [Leader election](#leader-election)). Singleton
   work resumes on the new leader; in-flight rollouts continue under it because
   rollout state is durable in the shared store.
3. Repeat until every instance is on the new version.

**Config-schema forward-compat.** A rolling upgrade means old and new instances
run against the **same shared store at the same time**, so schema changes must be
forward-compatible for at least one version: a new version's schema migration
(applied idempotently on start via `CREATE TABLE / IF NOT EXISTS`) must not break
the still-running old version, and the old version must tolerate additive columns
it doesn't read. Squadron's store migrations are additive for exactly this
reason. Rollback is symmetric — take instances back to the prior version one at a
time; because migrations are additive, the older binary keeps working against the
newer schema.

## Data tier

An active-active app tier in front of a **single** database just relocates the
single point of failure to the database. **True HA requires an HA store:**

- **Aurora PostgreSQL Multi-AZ** (recommended), or
- **Postgres with a hot standby + automated failover** (e.g. Patroni).

The app tier survives a DB failover automatically: when the primary fails over,
the leader's advisory lock is lost, and a new leader is elected against the new
primary — so singleton orchestration resumes on its own once the database is back.

Squadron does **not** provide or manage the HA database; it is an operator
requirement. See [sqlite-to-postgres-migration.md](./sqlite-to-postgres-migration.md)
for RDS/Aurora sizing.

## Validate your HA

Before you trust an HA cluster in production, reproduce the guarantees on a
scratch environment with the **HA proof harness** in the repo:

```
deploy/ha-proof/
```

It stands up one Postgres and two Enterprise-composed instances on one host and
verifies, end-to-end: (1) leader election — exactly one runner per singleton;
(2) failover — the survivor takes over with no dual ownership; (3) cross-instance
config delivery — a rollout driven on the leader reaches an agent connected to
the *non-leader* and converges. See `deploy/ha-proof/README.md` for the exact
build, run, and evidence-gathering steps. This harness is the reference for what
"working HA" looks like — run it against a topology that mirrors yours.
