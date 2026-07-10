# SSO and SCIM

The enterprise edition adds IdP **single sign-on (OIDC)** and directory
**provisioning (SCIM 2.0)** — without changing the OSS `Authenticator` contract.
The design choice that makes this work: OIDC login **mints a normal Squadron
bearer**; every subsequent request flows through the unchanged
`RequireBearer` → `Validate` path. There is no per-request ID-token validation
and no new user schema — the subject is encoded in the token *label*
(`oidc:<sub>`).

## OIDC login flow

`/auth/oidc/login` and `/auth/oidc/callback` mount **pre-bearer** (on the root
router alongside `/healthz` and `/metrics`) — the login flow is how a browser
obtains a bearer in the first place. In OSS these routes 404.

Connections live in the enterprise SQLite `oidc_connections` table, **not in
YAML** (plaintext secrets, no CRUD, and boot-only loading all disqualify YAML).
Each connection carries an `issuer`, `client_id`, a `client_secret` **sealed at
rest** via `SQUADRON_SECRETS_KEY` (AES-256-GCM), a **per-connection**
`redirect_uri`, a bound `tenant_id` (one IdP connection → one tenant), and a
`default_role`.

```mermaid
sequenceDiagram
    autonumber
    participant B as Browser
    participant S as Squadron (enterprise)
    participant I as IdP

    B->>S: GET /auth/oidc/login?conn=<id>
    S->>B: Set sealed state+nonce cookie; 302 to IdP
    B->>I: Authenticate at IdP
    I->>B: Redirect to /auth/oidc/callback?code=...
    B->>S: GET /auth/oidc/callback
    S->>I: Exchange code, fetch + verify ID token (JWKS)
    S->>S: Check state + nonce
    S->>S: Issue bearer label="oidc:<sub>", assign tenant, bind roles
    S->>B: 200 {token, token_id, expires_at}
    B->>S: Subsequent requests: Authorization: Bearer <token>
```

- **Session TTL** = the ID token's `exp`, with a **12h fallback** if the IdP
  omits it. **Logout = revoke** the minted token (idempotent, audited).
- **JIT provisioning** — a first-time subject is auto-created:
  `Issue(label="oidc:<sub>")` → assign to the connection's tenant → bind roles.
  A label longer than 64 chars becomes `oidc:sha256:<hex>` (hashed, not
  truncated, to avoid identity collision).

!!! warning "Requirements before OIDC login works"
    `SQUADRON_SECRETS_KEY` must be set to a base64 32-byte key so the client
    secret can be unsealed, and the connection's `tenant_id` must reference an
    existing tenant — [provision the tenant](multi-tenancy.md#provisioning-tenants)
    first. Under strict, a connection with **no** tenant binding fails fast.

## SCIM 2.0 directory

SCIM provisioning is mounted **under the bearer group** at `/api/v1/scim/v2/*`
and authenticated by a SCIM **service token**. In OSS these routes 404.

**Mint the SCIM service token internally, not via the public API.** The token
must carry a reserved **`scim:`-prefixed** label, the **`scim:write`** scope, and
be **bound to the target tenant**. Because `scim:` is a reserved label prefix,
the public `POST /api/v1/auth/tokens` **rejects** a `scim:` label — so mint it
internally (the path bootstrap uses), assign it to the tenant, and hand the
plaintext to the IdP. A SCIM connection can therefore provision **only** its own
tenant.

Point the IdP at the SCIM base URL `https://<host>/api/v1/scim/v2` with
`Content-Type: application/scim+json`. The surface: `ServiceProviderConfig` /
`Schemas` / `ResourceTypes`, Users + Groups CRUD, `eq` filter on
`userName`/`externalId`, and PATCH `active` (Users) / `members` (Groups).

### Two hard mapping requirements

- **`externalId` == the OIDC `sub`.** SCIM keys users on `externalId`, and OIDC
  login materializes a user's SCIM roles by matching `externalId` against the
  login `sub`. A different `externalId` means the SCIM-assigned roles are never
  applied at login.
- **Group `displayName` == the RBAC role name.** Group→role mapping is a
  case-insensitive first-match of the SCIM Group `displayName` against existing
  [RBAC role names](rbac.md). Role names must be unique per tenant, and the IdP
  group must be named exactly for the role you intend.

!!! note "SCIM is the directory, not the binder"
    SCIM records users, groups, and memberships — it does **not** write per-user
    RBAC bindings. At OIDC login the callback reads the user's active
    group→role set and **materializes** those bindings under the real
    `oidc:<sub>` label. A user with no SCIM record falls back to the connection's
    `default_role`. **Deprovision** (`DELETE /Users/{id}` or PATCH
    `active:false`) marks the user inactive and drops memberships in one
    transaction → their roles stop materializing at the next login, plus a
    best-effort revoke of any live `oidc:`/`scim:`-labeled token.

## Strict identity-source mode

Like strict tenant scoping, strict identity-source enforcement is **auto-on in
the enterprise wire — no config knob**. The wire calls
`services.SetStrictIdentitySource(true)` after the reserved allow-set
(`bootstrap` exact + `oidc:`/`scim:` prefixes) is populated.

!!! warning "Under strict, raw operator tokens are rejected"
    `RequireBearer` rejects any bearer whose label is **not** a validated
    identity source — i.e. not `oidc:`/`scim:`-prefixed and not the `bootstrap`
    break-glass label — with the **generic bad-token 401** (no provenance leak).
    A raw pasted operator token no longer authenticates. Only IdP-minted
    (`oidc:<sub>`), SCIM-service (`scim:*`), and break-glass (`bootstrap`) tokens
    pass. Confirm from the startup log: `enterprise: … strict identity-source
    ENABLED`.

### Checklist before relying on strict identity

- [ ] At least one OIDC connection provisioned with a bound `tenant_id`.
- [ ] A SCIM service token minted internally (reserved `scim:` label,
      `scim:write` scope, tenant-bound).
- [ ] A raw operator token (a plain label like `ci-bot`) is **rejected (401)**
      under strict.
- [ ] An `oidc:`/`scim:`-labeled token authenticates.
- [ ] The `bootstrap` break-glass token still passes (lockout safety net).
