# Migrating from OSS to Enterprise

This guide covers what changes when you move an existing Squadron OSS
deployment to Squadron Enterprise. Read [Deployment](./deployment.md) first
for the install shapes and [Editions & build model](./build.md) for the seam
that separates the two.

## The two facts that make this easy

**1. It's a build overlay, not a different product.** Enterprise is the same
Squadron codebase compiled with the private enterprise wire files overlaid and
the `enterprise` (and `compliance`) build tags set. The enterprise pack's
`make build-enterprise` clones OSS, drops the `//go:build enterprise` wire
files into `cmd/all-in-one/`, injects the `require`/`replace` into `go.mod`,
builds, and reverts the OSS tree afterward. Nothing in the OSS source tree
changes permanently — entitlement is simply *which code got compiled in*. In
OSS those seams are inert: the enterprise HTTP mounts return 404 and the
provider hooks return nil.

**2. There is no schema migration.** `tenant_id` already ships in the OSS base
schema — every per-tenant table carries `tenant_id TEXT NOT NULL DEFAULT
'default'`, and `api_tokens` gains it through an idempotent `ALTER TABLE`.
OSS runs a single implicit `default` tenant; enterprise activates the *same
rows* for real multi-tenancy. You point the enterprise binary at your existing
data directory / PVC and it boots against the same database. No dump, no
reload, no ALTER you have to run yourself.

Because of these two facts, "migrating" is really: swap the binary/image for
an enterprise build, then add the configuration below to turn the enterprise
controls on.

## Before you switch

- **Back up the data directory** (`/app/data`, or the Helm PVC). This is
  reversible in principle — the schema is shared — but always snapshot first.
- **Have your enterprise build or image ready.** The enterprise binary is
  produced by the private enterprise pack (`make build-enterprise`); the OSS
  image is not the enterprise image.
- **Set `SQUADRON_SECRETS_KEY`.** Enterprise seals stored secrets (OIDC client
  secrets) with it. In OSS a `secrets.key` is auto-generated to the data
  volume; for enterprise, provide a stable, backed-up key so sealed values
  survive a restart or a new replica.

## Configuration deltas

Everything below is additive to a working OSS config. The strict-isolation
flags are enforced by the enterprise wire itself (no config knob) — they turn
on automatically when you run the enterprise build.

| Area | What you add | Notes |
|---|---|---|
| **Auth** | `auth.enabled: true` | RBAC and tenant isolation only bite once auth is on. |
| **OTLP tenancy** | `ingest.otlp.tenant_id: default` (or a real tenant) | **Required** under enterprise strict when the OTLP receiver is enabled — unset is a startup fatal. |
| **Tenants** | `POST /api/v1/tenants/` per tenant; bind tokens via `POST /api/v1/tenants/<id>/tokens` | Trailing slash matters — bare `/tenants` 307-redirects. |
| **RBAC** | roles via `POST /api/v1/rbac/roles`, bindings via `/rbac/bindings` | Deny-by-default once any role exists. `SQUADRON_RBAC_BOOTSTRAP_LABELS` (default `bootstrap`) grants break-glass admin. |
| **SSO / OIDC** | connection rows in the enterprise `oidc_connections` table (`issuer`, `client_id`, sealed `client_secret`, per-connection `redirect_uri`, `tenant_id`, `default_role`) | Stored in the DB, not YAML; one IdP maps to one tenant. Needs `SQUADRON_SECRETS_KEY`. |
| **SCIM** | provision a service token with the reserved `scim:` label and `scim:write` scope, tenant-bound | For directory sync. |
| **Per-tenant trace budgets** | `extension/tracebudget` provider (enterprise) | OSS has a single uniform global cap (`SQUADRON_TRACEINDEX_MAX_ROWS` / `trace_index.per_tenant_max_rows`); differentiated per-tenant budgets are the enterprise wedge. |
| **Usage / chargeback** | `SQUADRON_USAGE_ENABLED`, `SQUADRON_USAGE_ENDPOINT`; `/api/v1/usage/*` | Enterprise-only endpoints (404 in OSS). |

### Strict flags you get automatically

The enterprise wire turns these on with no config knob:

- **Strict tenant scoping** — every per-tenant query is tenant-filtered.
- **Reject untenanted connections** — a header-less OpAMP connection is
  rejected (401) instead of falling back to `default`.
- **Strict identity source** — raw operator tokens are rejected; only
  `oidc:`, `scim:`, and `bootstrap`-labelled identities pass.

Plan for these: any collector or client that worked against OSS by relying on
the implicit `default` tenant or an unlabelled token needs a tenant header /
labelled token before it will connect to the enterprise build.

## Verify the switch

- Startup log shows `edition=squadron-enterprise`.
- `/metrics` exposes `squadron_build_info{edition="squadron-enterprise"} 1`.
- A request with no tenant context is rejected rather than served the
  `default` tenant.
- Your existing agents and configs are still present — same database, same
  rows.

## Rolling back

Because the schema is shared, rolling back is symmetric: stop the enterprise
binary, start the OSS binary against the same data directory. The enterprise
controls (multi-tenant rows, RBAC bindings, OIDC connections) simply go
dormant — OSS reads the data as the single `default` tenant and ignores the
enterprise-only tables. Keep your `SQUADRON_SECRETS_KEY` if you intend to
switch back to enterprise later, so the sealed OIDC secrets remain readable.

## See also

- [Deployment](./deployment.md) — install shapes, Helm, OpenShift, prod checklist
- [Editions & build model](./build.md) — the OSS/enterprise seam and its locking tests
- [OSS vs Enterprise](./oss-vs-enterprise.md) — the feature boundary
