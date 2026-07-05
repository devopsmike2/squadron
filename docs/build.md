# Build model & editions

Squadron ships as **editions** selected at build time via Go build tags.
The open-core (OSS) edition is the default; the **enterprise** edition is
the umbrella that adds the closed-source packs. The entitlement boundary
is *which code is compiled in* — not a runtime flag — so an OSS binary
cannot be turned into an enterprise binary by editing config.

This is the same open/closed seam used throughout the codebase: the open
core defines extension-point interfaces and wires **no-op providers**;
the private repo supplies the real providers, which are dropped into the
build tree and picked up under the edition build tag.

## Editions at a glance

| Edition | Build tags | Make target | Build identity |
|---|---|---|---|
| **OSS** (default) | *(none)* | `make build` / `make build-backend` | `squadron-oss` |
| **Enterprise** | `enterprise compliance` | `make build-enterprise` | (enterprise wire returns its own id) |

`enterprise` is an **umbrella** edition. Today it composes two packs:

- **Compliance Pack** (`compliance` tag): enforced group approval policy,
  change windows, SIEM export (Splunk HEC + HMAC-signed webhooks), and
  per-request access-audit middleware.
- **Commercial-tier detectors** (`enterprise` tag): the add-on-dependent
  serverless regression detectors — AWS Lambda cold-start / error-rate via
  Lambda Insights (#152) and Azure Functions cold-start / error-rate via
  Application Insights (#153).
- **Identity** (`enterprise` tag): SSO (SAML/OIDC) + SCIM, role-based access
  control, and multi-tenant isolation. OSS keeps bearer tokens + flat scopes +
  a single implicit tenant; the enterprise edition supplies a role-based
  authorizer and a per-tenant scoped store against the same seam. See ADR 0006.

The `compliance` seam predates the umbrella and keeps its own tag, so the
enterprise build sets both (`-tags "enterprise compliance"`). New paid
packs should go behind the `enterprise` tag unless there's a reason to
ship them independently.

## How the seam works

Every extension point has three pieces:

1. **Interface in the open core** — under `extension/` (not `internal/`) so
   the private enterprise repo can import it across module boundaries.
   Current interfaces: `extension/policy` (group approval),
   `extension/changewindow` (rollout blackout windows), `extension/siem`
   (audit fan-out dispatcher), `extension/detectors` (commercial-tier
   detector activation), and `extension/identity` (authentication,
   authorization, and tenant resolution — ADR 0006).
2. **A no-op / limited default provider** wired by the OSS build. This is
   the working OSS behaviour: the feature is inert (groups can carry
   `require_approval` metadata but the engine doesn't enforce it; SIEM
   destinations are stored but never delivered to; commercial detectors
   never activate regardless of `commercial_detectors.enabled`).
3. **The real provider in the private repo**, wired by the edition build.

The wiring lives in tag-guarded files in `cmd/all-in-one/`, each exposing
the **same function symbol** so `main.go` has a single call site and does
not care which edition is active:

| Seam | OSS wire (`//go:build !<tag>`) | Edition stub (`//go:build <tag>`) |
|---|---|---|
| Compliance | `wire_oss.go` → no-op providers, returns `squadron-oss` | `wire_compliance.go` → panics with guidance |
| Commercial detectors | `wire_detectors_oss.go` → `detectors.NoOpProvider` | `wire_detectors_enterprise.go` → panics with guidance |
| Identity (authn/authz/tenant) | `wire_identity_oss.go` → `identity.OSSProviders` (bearer + flat-scope + single tenant) | `wire_identity_enterprise.go` → panics with guidance |
| Tenant-scoped store | `wire_scopedstore_oss.go` → identity pass-through | `wire_scopedstore_enterprise.go` → panics with guidance |

The edition-tagged files that ship in **this** (open-core) repo are
**stubs**: they compile so `go build -tags <edition>` type-checks the seam,
but they `panic("...see docs/build.md")` at startup. That is deliberate —
a build assembled with the edition tag but **without** the private wire
files fails loudly instead of silently falling back to OSS behaviour.

## Building each edition

```bash
# OSS (default)
make build            # UI + backend -> bin/squadron
make build-backend    # backend only  -> bin/squadron

# Enterprise (from THIS repo -> stub binary that panics at startup)
make build-enterprise # -> bin/squadron-enterprise, tags "enterprise compliance"
```

To produce a **real** enterprise binary, run `make build-enterprise` from a
tree where the private `squadron-enterprise` (and `squadron-compliance`)
wire files have been dropped into `cmd/all-in-one/`, replacing the
open-core stubs. The private repos own that drop-in step in their own
release tooling; the open core only guarantees the seam and the stubs.

## Confirming which edition is running

The build identity is surfaced two ways so operators never have to guess:

- **Startup log**: `squadron build edition {edition=squadron-oss}`.
- **/metrics**: the `squadron_build_info{edition="squadron-oss"} 1` gauge.

The OSS build also logs, when `commercial_detectors.enabled` is set on an
OSS binary, that the flag is inert (the detectors stay dormant because the
entitlement is the enterprise edition, not the flag).

## Runtime flags are not entitlement

Some config switches gate **cost/safety**, not access:

- `commercial_detectors.enabled` — in the enterprise edition, opts into the
  per-scan Lambda Insights / Application Insights API cost. In OSS it is
  inert.
- `serverless_metric_detection.enabled` — the **native-metric** serverless
  detectors (AWS `Errors`/`Invocations`, plus the GCP and OCI native Cloud
  Monitoring cold-start + error-rate detectors). This one is genuinely OSS:
  it stays a runtime switch and is **not** behind an edition tag.

See [docs/oss-vs-enterprise.md](oss-vs-enterprise.md) for the full boundary
and [docs/architecture/oss-enterprise-separation.md](architecture/oss-enterprise-separation.md)
for the contract every future paid feature follows.

## Editions contract: OSS-inert guarantees

The RBAC + multi-tenancy work (ADRs 0006/0010/0011/0012) added several
enterprise **capability seams** to this open-core repo. Every one is **inert in
the OSS build** — it changes no OSS runtime behavior — and only becomes
load-bearing when the enterprise wire files are compiled in under the
`enterprise` tag. The OSS test suite *proves* this inertness; each seam below is
paired with the editions-contract test that locks it, so a regression fails a
test rather than silently changing edition behavior.

| Seam (in this repo) | OSS-inert behavior | Test that locks it |
|---|---|---|
| `identity.Authenticator` / `Authorizer` / `TenantResolver` (`extension/identity`) | OSS wires `BearerAuthenticator` + `ScopeAuthorizer` (flat scope, empty = legacy full access) + `SingleTenantResolver` (always `DefaultTenant`) | `cmd/all-in-one/editions_contract_test.go::TestOSSEdition_IdentityProviders`; `extension/identity/identity_test.go::TestOSSProviders_WiresDefaults`, `TestSingleTenantResolver_AlwaysDefault` |
| `ScopeAuthorizer` resource-awareness | The authorizer **ignores** the `Resource` passed by `RequireScope` — a decision with `Resource{}` and with `Resource{Type,ID}` is identical | `extension/identity/identity_test.go::TestScopeAuthorizer_IgnoresResource` (+ `TestScopeAuthorizer_MirrorsHasScope` pins empty-scope → allow) |
| Scoped store (`scopedApplicationStore`) | Identity **pass-through** — returns exactly the store it was given, so the ~9 optional store interfaces `main.go` type-asserts stay intact | `cmd/all-in-one/editions_contract_test.go::TestOSSEdition_ScopedStoreIsPassthrough` |
| SQLite `tenant_id` predicate (`tenant_scope.go`) | Single `default` tenant → `WHERE tenant_id='default'` returns everything; reads/writes round-trip byte-identically to pre-tenancy behavior | `internal/storage/applicationstore/sqlite/tenant_scope_contract_test.go::TestTenantScope_OSSByteIdentical` (+ `_Isolation`, `_SystemSeesAll` prove the enterprise-active behavior) |
| `sqlite.SetStrictTenantScoping` | Never called in OSS → `strictTenantScoping` stays **false**; an unstamped context falls back to `DefaultTenant` (no error) | `internal/storage/applicationstore/sqlite/tenant_scope_contract_test.go::TestTenantScope_StrictFlag` asserts the default is off and only errors once explicitly flipped on |
| `SetEnterpriseRBACHandler` + `/api/v1/rbac/*` | Handler left nil → the late-bound route returns **404** (`"RBAC management is an enterprise feature"`) | `internal/api/enterprise_rbac_seam_test.go::TestEnterpriseRBACSeam_OSS404` (+ `_ServesWhenWired` proves the injected path) |
| `SetEnterpriseTenantHandler` + `/api/v1/tenants/*` | Handler left nil → the late-bound route returns **404** (`"tenant management is an enterprise feature"`) | `internal/api/enterprise_tenant_seam_test.go::TestEnterpriseTenantSeam_OSS404` (+ `_ServesWhenWired`) |
| `opamp.SetRejectUntenantedConnections` | Never called in OSS → `rejectUntenantedConnections` stays **false**; a connection with no `x-squadron-tenant` header is accepted onto `DefaultTenant` | `internal/opamp/server_tenant_test.go::TestRejectUntenantedConnectionsSeam` asserts the OSS default is reject-off; `TestResolveConnTenant` pins empty-header → `DefaultTenant` |
| `ingest.otlp.tenant_id` (`internal/config`) | Defaults to `default`; OTLP ingest is stamped `default` and inert — no fail-fast in OSS | covered by the config default + the OTLP stamping path (the enterprise fatal-check lives only in the enterprise wire) |
| `SetEnterpriseSCIMHandler` + `/api/v1/scim/v2/*` (ADR 0014 Arc C) | Handler left nil → the late-bound route returns **404** (`"SCIM provisioning is an enterprise feature"`). Mounted UNDER the bearer group (SCIM is authed by a reserved-label, `scim:*`-scoped, tenant-bound service token). OSS 404s ALL SCIM routes including `/scim/v2/ServiceProviderConfig` — OSS has no SCIM, so RFC 7643 §5 discovery is deliberately NOT special-cased; the public-vs-authed ServiceProviderConfig question is an enterprise (slice 4c) decision | `internal/api/enterprise_oidc_scim_seam_test.go::TestEnterpriseSCIMSeam_OSS404` (+ `_ServesWhenWired`) |
| `SetEnterpriseOIDCHandler` + `/auth/oidc/*` (ADR 0014 Arc C) | Handler left nil → the late-bound route returns **404** (`"OIDC single sign-on is an enterprise feature"`). Mounted OUTSIDE the bearer group (pre-authentication login + callback, on the root router alongside `/health`/`/metrics`) | `internal/api/enterprise_oidc_scim_seam_test.go::TestEnterpriseOIDCSeam_OSS404` (+ `_ServesWhenWired`) |
| `scim:read` / `scim:write` scopes (`internal/services/auth_service.go`) | Present in the scope inventory (`AllScopes` / `IsValidScope`) so a token can carry them, but **inert**: OSS mounts no SCIM route, so no route requires them (slice 4c wires `RequireScope("scim:write")`) | `internal/services/identity_source_contract_test.go::TestSCIMScopes_InInventory` |
| `services.SetStrictIdentitySource` (ADR 0014 Arc C, slice 4d) | Never called in OSS → `strictIdentitySource` stays **false**. The slice-4d enforcement is now **implemented** in `RequireBearer` (reject a bearer whose label is not a validated identity source, via `services.IdentitySourceValidated`) but is **inert** in OSS: the check is gated on `StrictIdentitySource()` first, so with the flag false the block is skipped and a raw operator token authenticates byte-identically. When the enterprise wire flips the toggle (alongside the populated reserved allow-set), a non-`oidc:`/`scim:`/`bootstrap` token is rejected with the generic bad-token 401 (no provenance leak) | `internal/services/identity_source_contract_test.go::TestStrictIdentitySource_OSSDefaultInert`; `internal/services/strict_identity_source_test.go::TestIdentitySourceValidated_*`; `internal/api/middleware/auth_test.go::TestRequireBearer_Strict{Off_RawTokenAuthenticates,On_RawToken_401,On_ValidatedIdentity_200}` |

The through-line: the OSS build has a single implicit `default` tenant, strict
scoping off, flat-scope authorization that ignores resources, nil enterprise
handlers (→ 404), and a pass-through scoped store. The enterprise edition
supplies real providers against these same seams — the boundary is *which code
is compiled in*, and the tests above prove the OSS side never drifts.
