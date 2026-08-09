# SQLite → Postgres migration

Squadron's control-plane state (agents, groups, configs, rollouts, the audit
hash-chain, runners, tokens) defaults to an embedded **SQLite** file. For high
availability and org-scale fleets you can move it to an external **Postgres /
Aurora** database (ADR 0033). The Postgres backend is **OSS-selectable** — it is
chosen with `storage.app.type: postgres` + `storage.app.dsn`. (The HA/multi-
replica *operability* layer — failover routing, connection-pool tuning, leader
election — is the Enterprise angle; the backend itself is free.)

This guide covers a one-time migration of an existing SQLite deployment. It
picks up from [deployment.md](./deployment.md#postgres-for-ha) and is a sibling
of [oss-to-enterprise-migration.md](./oss-to-enterprise-migration.md).

> **Telemetry is out of scope.** Only the *application* store moves. The
> telemetry store stays DuckDB regardless (it is an analytical workload;
> Postgres is the wrong fit). Nothing in this guide touches `telemetry.db`.

- [Prerequisites](#prerequisites)
- [RDS sizing](#rds-sizing)
- [Migration path](#migration-path)
- [Compliance receipt (SOC 2)](#compliance-receipt-soc-2)
- [Rollback](#rollback)

## Prerequisites

- **Postgres 14 or newer** (Aurora PostgreSQL 14+ is fine). The schema uses
  `JSONB`, partial indexes (`... WHERE ended_at IS NULL`), `ON CONFLICT`
  upserts, and `TIMESTAMPTZ` — all core to Postgres 12+, but 14+ is the
  supported floor and what CI validates (the test image is `postgres:16-alpine`).
- **No extensions required.** Squadron's schema uses only core types. UUIDs are
  stored as `TEXT`, so `uuid-ossp`/`pgcrypto` are **not** needed. You do not need
  superuser — a plain owner role on the target database is enough. Squadron
  creates its own tables on first start (`CREATE TABLE IF NOT EXISTS`, idempotent).
- **A database + login role**, e.g.:
  ```sql
  CREATE ROLE squadron LOGIN PASSWORD '...';
  CREATE DATABASE squadron OWNER squadron;
  ```
- **TLS.** Use `sslmode=require` (or stricter) in the DSN for anything crossing a
  network:
  `postgres://squadron:...@host:5432/squadron?sslmode=require`.
- **`sqlite3` and `psql`** on the migration host, plus a copy of the live
  `app.db`. The migration reads the SQLite file; it never mutates it.

## RDS sizing

Squadron's own steady-state footprint on the pre-GA 24h soak is **~3.4 GB RSS**
(the Go process + embedded DuckDB rollups). That is the *application* pod's
memory, not the database's — the control-plane dataset Postgres actually holds
(agents, configs, rollouts, audit rows) is small: kilobytes to a few MB per
entity, low-hundreds of MB even on a large fleet with deep audit history.

Recommended starting instance:

| Fleet / posture | Instance | vCPU / RAM | Notes |
|---|---|---|---|
| Typical (≤ ~2k collectors), single writer | **db.t3.medium** | 2 / 4 GB | Burstable; fine for the control-plane write rate. |
| Larger fleet or HA pair (headroom) | **db.m6g.large** | 2 / 8 GB | Graviton price/perf; steady (non-burstable) baseline; the safer default when you also run a standby. |

Sizing rationale: the working set fits comfortably in 4–8 GB, so Postgres memory
is not the constraint — connection count and write concurrency are. Give
Postgres enough RAM to keep the working set + indexes cached (both options do)
and size the app pod separately around its ~3.4 GB footprint.

**Scaling guidance.** Scale up (or to `db.m6g.xlarge`+) when you push past
~2–5k collectors or run multiple Squadron replicas against one database. Enable
**Multi-AZ** for HA. Read replicas only help once the Enterprise HA layer routes
reads to them — a single OSS writer does not use them. Bound Squadron's pool
(`?pool_max_conns=...` on the DSN via pgx) so N replicas don't exhaust RDS's
`max_connections`.

## Migration path

Squadron is a **single-writer** application, so the clean, GA-recommended
mechanism is a **cold dump-and-restore cutover**: stop the writer, copy the
data, repoint the config, restart. A live/zero-downtime cutover is *not*
recommended for the first migration — see the note at the end of this section.

**Expected downtime: minutes.** The control-plane dataset is small; the copy is
bounded by row counts (typically thousands to low-millions of audit rows).
Budget < 10 minutes for a typical deployment plus provisioning time.

> ### CRITICAL: the audit tables must be copied VERBATIM
>
> `audit_events` and `audit_chain_checkpoints` are a **tamper-evident hash
> chain** (ADR 0027). Each row's `row_hash` is `SHA-256` over the row's
> immutable content **including the raw `payload` string, byte-for-byte**,
> chained through `prev_hash` in `seq` order. Prior checkpoints and any
> compliance attestations pin specific `(seq, row_hash)` tips.
>
> You **must** copy these tables row-for-row, preserving `seq`, `prev_hash`,
> `row_hash`, and `payload` **exactly** — same bytes. Do **NOT**:
> - re-insert audit rows through the API / `CreateAuditEvent` (it recomputes
>   `seq`/`prev_hash`/`row_hash` and **breaks chain continuity**);
> - land `payload` in a `JSONB` column or pass it through any tool that
>   re-serializes JSON (key reorder / whitespace normalization changes the
>   bytes → the recomputed `row_hash` no longer matches → the chain reads as
>   tampered). Squadron's Postgres schema deliberately stores `payload` as
>   `TEXT` for exactly this reason — keep it that way through the copy.

### Ordered steps

1. **Provision** Postgres and create the role + database (see Prerequisites).

2. **Create the schema on the target.** Point a *throwaway* Squadron start at the
   empty database so it runs its idempotent DDL, then stop it:
   ```bash
   # squadron.yaml (temporarily):  storage: { app: { type: postgres, dsn: "postgres://squadron:...@host:5432/squadron?sslmode=require" } }
   squadron --config squadron.yaml &   # creates all tables via CREATE TABLE IF NOT EXISTS
   sleep 5 && kill %1                    # stop once the schema is created
   ```
   (You can skip this and let the final start create the schema, but creating it
   up front lets you load data before the app writes anything.)

3. **Stop the live Squadron.** Downtime begins. Collectors keep running on their
   last pushed config; only the control plane is briefly unavailable.

4. **Export each table from SQLite to CSV** (headers on, so column order is not
   load-bearing). Do the **audit tables** with their exact column list and no
   transformation:
   ```bash
   sqlite3 -header -csv app.db \
     "SELECT id,timestamp,actor,event_type,target_type,target_id,payload,tenant_id,created_at,action,seq,prev_hash,row_hash,ai_explanation,ai_explanation_model,ai_explanation_generated_at FROM audit_events ORDER BY tenant_id, seq;" \
     > audit_events.csv
   sqlite3 -header -csv app.db \
     "SELECT tenant_id,checkpoint_seq,checkpoint_row_hash,rows_pruned,kind,created_at,sealed_sig FROM audit_chain_checkpoints;" \
     > audit_chain_checkpoints.csv
   # ...and one CSV per remaining table (groups, agents, configs, rollouts,
   # rollout_approvals, api_tokens, deploy_targets, ...), preserving tenant_id
   # and composite keys.
   ```

5. **Load the CSVs into Postgres** with `\copy` (client-side; needs no server
   file access). Load parents before children (groups/agents before configs;
   deploy_targets before deploy_runs). For the audit tables, load the **exact
   columns** so `seq`/`prev_hash`/`row_hash`/`payload` land verbatim:
   ```bash
   psql "$DSN" -c "\copy audit_events (id,timestamp,actor,event_type,target_type,target_id,payload,tenant_id,created_at,action,seq,prev_hash,row_hash,ai_explanation,ai_explanation_model,ai_explanation_generated_at) FROM 'audit_events.csv' WITH (FORMAT csv, HEADER true)"
   psql "$DSN" -c "\copy audit_chain_checkpoints (tenant_id,checkpoint_seq,checkpoint_row_hash,rows_pruned,kind,created_at,sealed_sig) FROM 'audit_chain_checkpoints.csv' WITH (FORMAT csv, HEADER true)"
   ```
   For **non-audit** tables you may instead use a converter such as
   [`pgloader`](https://pgloader.io/), which maps SQLite types automatically
   (notably SQLite's `0/1` → Postgres `BOOLEAN`, and SQLite JSON text → the
   `JSONB` columns). **Do not point pgloader at `audit_events` or
   `audit_chain_checkpoints`** — its JSON/type coercion would break the payload
   byte-identity the chain depends on. Copy those two tables by CSV only.

6. **Repoint the config** to Postgres and remove the SQLite path:
   ```yaml
   storage:
     app:
       type: postgres
       dsn: "postgres://squadron:...@host:5432/squadron?sslmode=require"
   ```

7. **Start Squadron.** It connects, `Ping`s, and re-applies the idempotent DDL
   (no-op on already-created tables). If the DSN is missing or unreachable,
   startup **fails with a specific Postgres error and does not fall back to
   SQLite** — fix the DSN rather than run on the wrong store.

8. **Verify** — run the compliance receipt below. Downtime ends once Squadron is
   up and the chain verifies.

> **`squadron migrate-store` (coming).** The CSV/`pgloader` mechanism above is
> the interim manual path. A built-in `squadron migrate-store` command — a raw,
> table-by-table row copy that reads the SQLite store and writes the Postgres
> store directly, **inserting audit rows with their stored
> `seq`/`prev_hash`/`row_hash`/`payload` verbatim** (a dedicated raw-insert path,
> never `CreateAuditEvent`) — is the deliverable for the later **live-cutover
> proof-out**. Until it lands, use the steps above and lean on the receipt to
> prove the chain survived.

## Compliance receipt (SOC 2)

The auditor receipt is: **the migrated Postgres chain recomputes to the same
head the pre-migration data attested.** If any audit byte changed in transit,
the recomputed `row_hash` diverges and the check fails — so a PASS is positive
proof the chain was preserved.

Use the offline verifier [`squadron-audit-verify`](../cmd/squadron-audit-verify)
(ADR 0027) — it re-hashes an exported chain with zero secrets and compares the
tip to a baseline.

```bash
# BEFORE cutover, on the SQLite instance — capture the baseline:
curl -sH "Authorization: Bearer $TOKEN" \
  "https://squadron.old/api/v1/audit/events?include_chain=1" > audit-export-pre.json
# (Enterprise only) also capture a key-sealed attestation of the head:
curl -sH "Authorization: Bearer $TOKEN" \
  "https://squadron.old/api/v1/audit-verify/tenants/default/attest" > attest-pre.json

# AFTER cutover, on the Postgres instance — export the migrated chain:
curl -sH "Authorization: Bearer $TOKEN" \
  "https://squadron.new/api/v1/audit/events?include_chain=1" > audit-export-post.json

# Recompute offline and prove the migrated chain matches the attested head:
squadron-audit-verify -export audit-export-post.json -attestation attest-pre.json -tenant default
```

Expected output (the lines that matter):

```
Squadron offline attestation verifier (ADR 0027)
  export rows:        <N>
  attestation tenant: default
  rows verified:      <N>
  chain:              OK (covers from seq 1)
  recomputed head:    seq=<H> hash=<...>
  attested head:      seq=<H> hash=<...>
  tenant cross-check: OK (default)
  head match:         PASS (recomputed tip matches the attestation)
```

`chain: OK` + `head match: PASS` is the receipt — the migration preserved the
hash chain byte-for-byte. A `head match: FAIL (tip mismatch ...)` means a
`payload`/`seq`/hash byte changed in the copy (usually re-serialized JSON or a
re-append) — go back and copy the audit tables verbatim.

**OSS (no Enterprise attest endpoint).** Two zero-Enterprise options:
- Quick self-check against the migrated store — the OSS self-verify route:
  ```bash
  curl -sH "Authorization: Bearer $TOKEN" "https://squadron.new/api/v1/audit-verify"
  # → {"ok":true,"rows_verified":<N>,"covers_from_seq":1,"first_break_seq":0}
  ```
- Or run the offline CLI comparing the pre/post exports directly (treat the
  pre-migration export's head `seq`+`row_hash` as the expected tip). Same PASS
  semantics, no key material.

## Rollback

The migration is **non-destructive to the source**: it only *reads* the SQLite
`app.db`, so the original file remains a valid rollback target.

To return to SQLite after a Postgres cutover:

1. **Stop Squadron.**
2. **Decide which store is authoritative.** After cutover, **Postgres holds any
   writes made since**. If you are rolling back inside a short window with no (or
   discardable) writes, the untouched original `app.db` is still authoritative
   and you lose nothing. If meaningful changes landed on Postgres, either accept
   losing them or copy the delta back out of Postgres first (same verbatim rule
   for the audit tables). Keeping the cutover window short makes rollback trivial.
3. **Revert the config** to the original SQLite settings:
   ```yaml
   storage:
     app:
       type: sqlite
       path: /var/lib/squadron/squadron.db
   ```
4. **Restart Squadron** on SQLite and confirm health with the self-verify route
   (`GET /api/v1/audit-verify` → `ok:true`) on the SQLite instance.

Because `storage.app.type` is the single switch and the source file is never
mutated, rollback is a config revert + restart — no reverse migration needed
when the cutover window was clean.

## See also

- [Deployment guide → Postgres for HA](./deployment.md#postgres-for-ha)
- [OSS → Enterprise migration](./oss-to-enterprise-migration.md)
- [Audit log](./audit-log.md) and ADR 0027 (tamper-evident audit chain)
