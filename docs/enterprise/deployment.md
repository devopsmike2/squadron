# Enterprise deployment

An operator runbook for building, booting, and proving a Squadron Enterprise
binary (RBAC + multi-tenancy + SSO/SCIM + the Compliance Pack). This page
mirrors the enterprise `DEPLOYMENT.md` runbook; every command, endpoint, config
key, and env var is drawn from it.

## 1. Build

The enterprise binary is **composed at build time** from three trees — the OSS
open core plus the private `squadron-enterprise` and `squadron-compliance` packs.
Which code is compiled in *is* the entitlement boundary; it is not a runtime
flag.

```bash
cd squadron-enterprise
make build-enterprise      # -> bin/squadron-enterprise
```

`make build-enterprise` runs `scripts/build-enterprise.sh`, which performs a
build-time **overlay** onto the OSS open core and fully **reverts it on exit**
(success *or* failure) via an `EXIT` trap, leaving the OSS tree pristine:

1. **Drops the edition wire files** into the OSS `cmd/all-in-one/` — the
   enterprise wire files (identity, tenant resolver, scoped store, strict,
   detectors, RBAC store/audit/handler, tenant handler, enterprise-server) plus
   the Compliance Pack's `wire_compliance.go`.
2. **Injects `require` + local `replace`** for both private modules into the OSS
   `go.mod` via `go mod edit` (the OSS `go.mod` deliberately carries no private
   references).
3. **Builds** `go build -mod=mod -tags "enterprise compliance" -o bin/squadron-enterprise ./cmd/all-in-one`.
4. **Restores** the wire files, `go.mod`, and `go.sum`.

!!! warning "The sibling `squadron-compliance` checkout is required"
    The build script exits with an error
    (`SQUADRON_COMPLIANCE does not exist: <path>`) if it is missing. Paths
    default to `../squadron` and `../squadron-compliance` and are overridable:

    ```bash
    make build-enterprise \
      SQUADRON_OSS=/path/to/squadron \
      SQUADRON_COMPLIANCE=/path/to/squadron-compliance
    ```

The `enterprise` umbrella tag composes **both** packs
(`-tags "enterprise compliance"`): Enterprise supplies identity/RBAC/tenancy plus
commercial detectors; Compliance supplies group-approval policy, change windows,
SIEM export, and access-audit middleware.

## 2. Boot

Boot with a config file via `--config` (default `./squadron.yaml`):

```bash
bin/squadron-enterprise --config squadron.yaml
```

### Config shape

```yaml
storage:
  app:
    type: sqlite            # application store (RBAC/tenant tables live here too)
    path: ./data/app.db
  telemetry:
    type: duckdb            # telemetry store
    path: ./data/telemetry.db

server:
  http_port: 8080
  opamp_port: 8081

otlp:
  grpc_endpoint: 0.0.0.0:4317
  http_endpoint: 0.0.0.0:4318

auth:
  enabled: true             # RBAC + tenant isolation only bite when auth is on

ingest:
  otlp:
    tenant_id: default      # REQUIRED under enterprise strict (see below)
```

!!! note "auth must be on for governance to bite"
    `auth.enabled=false` unmounts the bearer middleware; with no actor on the
    context, `RequireScope` short-circuits and the authorizer is never consulted.
    RBAC deny-by-default and tenant resolution only apply when auth is **on**.

### The secrets key

Sealing the OIDC client secret at rest (AES-256-GCM) requires a secrets key.

!!! warning "`SQUADRON_SECRETS_KEY` must be a base64-encoded 32-byte key"
    Set `SQUADRON_SECRETS_KEY` to a base64-encoded **32-byte** value. Without
    it, the OIDC client secret cannot be unsealed and SSO login fails. Generate
    one with `openssl rand -base64 32`.

### Strict tenant scoping is auto-on

Strict scoping is **auto-on in the enterprise wire — there is no config knob**.
The enterprise build calls `sqlite.SetStrictTenantScoping(true)`,
`opamp.SetRejectUntenantedConnections(true)`, and installs the OTLP fatal-check
on `ingest.otlp.tenant_id`. Consequently:

- Under strict, if `ingest.otlp.tenant_id` is **unset** while the OTLP receiver
  is enabled → **startup is fatal**: `enterprise edition requires
  ingest.otlp.tenant_id when the OTLP receiver is enabled; untenanted telemetry
  is rejected (ADR 0012)`.
- An OpAMP connection presenting **no** `x-squadron-tenant` header → **rejected
  at connect (HTTP 401)**.

Confirm strict is active from the startup log: `enterprise: strict tenant
scoping ENABLED (store rejects unstamped contexts; OpAMP rejects untenanted
connections)`.

## 3. Bootstrap token (first admin)

On first start with `auth.enabled=true` and **zero** tokens in the store, the
binary issues one bootstrap token automatically:

- label **`bootstrap`**, **wildcard** scope, **no expiry**;
- issued only when the token list is empty (idempotent across restarts);
- logged **loudly at Warn** with the plaintext: `API auth is enabled and no
  tokens exist yet — issued a bootstrap token.`

Capture that plaintext from the logs — it is the only way to authenticate to a
freshly-enabled Squadron. The enterprise RBAC engine grants **implicit admin**
to tokens whose label is in the bootstrap set (defaults to `bootstrap`, extended
additively via `SQUADRON_RBAC_BOOTSTRAP_LABELS="bootstrap,break-glass"`), so it
can provision roles and tenants before any role exists — no lockout.

```bash
export TOKEN="sqd_<bootstrap-plaintext-from-logs>"   # the sqd_ prefix is required
```

!!! tip "Revoke it when you're done"
    Once real scoped tokens and roles are in place, revoke the bootstrap token.
    Keep at least one break-glass label around as the strict-identity lockout
    safety net.

## 4. Prove the edition

Confirm you are running the enterprise edition — not OSS — three ways:

=== "Startup log"

    ```
    squadron build edition {edition=squadron-enterprise}
    ```

=== "/metrics gauge"

    ```bash
    curl -s localhost:8080/metrics | grep squadron_build_info
    # -> squadron_build_info{edition="squadron-enterprise"} 1
    ```

=== "Capability probe"

    Enterprise-only routes exist on the enterprise binary and 404 on OSS. With a
    role-less token you get a `403` (route present, access denied); OSS returns
    `404` (route absent):

    ```bash
    curl -s -o /dev/null -w '%{http_code}\n' \
      localhost:8080/api/v1/rbac/roles -H "Authorization: Bearer $TOKEN"
    # enterprise: 200 (admin) or 403 (role-less) ; OSS: 404
    ```

## Post-deploy checklist

- [ ] `make build-enterprise` succeeded; `bin/squadron-enterprise` exists; the
      OSS tree is left clean (`git status` unchanged).
- [ ] Startup log shows `squadron build edition {edition=squadron-enterprise}`.
- [ ] `/metrics` reports `squadron_build_info{edition="squadron-enterprise"} 1`.
- [ ] `SQUADRON_SECRETS_KEY` is set to a base64 32-byte key.
- [ ] `ingest.otlp.tenant_id` is set (else the binary refuses to start under
      strict).
- [ ] Startup log shows `enterprise: strict tenant scoping ENABLED`.
- [ ] Bootstrap token captured from the Warn log (`sqd_` prefix present); revoked
      once real roles/tokens exist.

Next: provision [tenants and RBAC](rbac.md), then wire [SSO/SCIM](sso-scim.md).
