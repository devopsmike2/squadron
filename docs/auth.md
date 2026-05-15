# Authentication

Squadron supports Bearer-token authentication on its REST API. Auth is
opt-in: by default Squadron runs with no auth so first-time evaluation
is friction-free, but you should turn it on before exposing Squadron
beyond a trusted network.

- [How it works](#how-it-works)
- [Turning it on](#turning-it-on)
- [Bootstrap: the first token](#bootstrap-the-first-token)
- [Managing tokens](#managing-tokens)
- [Scopes](#scopes)
- [Expiry](#expiry)
- [Using a token](#using-a-token)
- [Token lifecycle](#token-lifecycle)
- [Recovery: lost all tokens](#recovery-lost-all-tokens)
- [What's NOT included](#whats-not-included)

## How it works

When `auth.enabled` is true:

- Every `/api/v1/*` request must carry an `Authorization: Bearer <token>`
  header.
- `/metrics` and `/health` stay public so scrapers and load balancers
  don't need credentials.
- `OPTIONS` (CORS preflight) requests are allowed through unauthenticated;
  the real request that follows is checked normally.
- Tokens are sha256-hashed before storage — Squadron retains the digest
  and discards the plaintext. The plaintext is shown to the operator
  once at creation time and never again.
- Successful authentication stamps an actor onto the request context.
  Every audit event recorded during that request gets attributed to
  `operator:<token-label>` instead of the generic `system`.

When `auth.enabled` is false, no middleware is mounted and every endpoint
behaves as in pre-v0.8 Squadron. A loud warning is logged at startup to
discourage forgetting to turn it on for production.

## Turning it on

In `squadron.yaml`:

```yaml
auth:
  enabled: true
```

Or via environment variable:

```bash
AUTH_ENABLED=true ./squadron
```

Restart Squadron. On the next start with auth enabled and an empty
tokens table, Squadron emits a bootstrap token to stderr — see below.

## Bootstrap: the first token

The first time Squadron starts with `auth.enabled: true` and zero
tokens in the store, it issues a single token labeled `bootstrap` and
prints it to stderr at WARN level:

```
WARN  API auth is enabled and no tokens exist yet — issued a bootstrap token. Revoke it after creating your real tokens. {"bootstrap_token": "sqd_..."}
```

Copy that token from your container logs (or wherever stderr is going)
and use it to sign in to the UI. Then:

1. Open the UI; you'll be redirected to the login page.
2. Paste the bootstrap token.
3. Navigate to **API tokens** in the sidebar (or `/settings/tokens`).
4. Create properly-labeled tokens for each operator / automation that
   needs API access.
5. Revoke the bootstrap token. Its job is done.

The bootstrap flow only fires when the tokens table is empty. Restarts
after the first token exists are silent — Squadron doesn't keep issuing
new ones.

## Managing tokens

The **API tokens** page (`/settings/tokens` in the UI) is the canonical
place to manage tokens. You can also drive the API directly:

```bash
# Create a token. Plaintext is in the response body, ONCE.
curl -X POST http://localhost:8080/api/v1/auth/tokens \
  -H "Authorization: Bearer $SQUADRON_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"label": "ci-pipeline"}'
# → {"token": {"id": "...", "label": "ci-pipeline", ...}, "plaintext": "sqd_..."}

# List every issued token (active + revoked, newest first).
curl http://localhost:8080/api/v1/auth/tokens \
  -H "Authorization: Bearer $SQUADRON_TOKEN"

# Revoke. Idempotent — revoking a revoked token returns 204 the same
# way as revoking an active one.
curl -X POST http://localhost:8080/api/v1/auth/tokens/<id>/revoke \
  -H "Authorization: Bearer $SQUADRON_TOKEN"
```

Pick token labels that identify the bearer — they show up in the audit
log as `operator:<label>`. Suggested patterns:

- Per-person tokens: `alice@example.com`, `bob@example.com`.
- Per-automation tokens: `ci-pipeline`, `deploy-bot-staging`,
  `nightly-backup`.
- Per-host tokens: `dashboard-host-1`.

Resist the urge to share tokens between systems. The audit log can't
distinguish two consumers of the same token, and revoking the shared
token to rotate one of them breaks the others.

## Scopes

Each token carries a list of permission scopes. The middleware enforces
them per route: a token with `agents:read` can `GET /api/v1/agents` but
gets a **403 Forbidden** on `POST /api/v1/agents/:id/restart`. The
distinction matters — **401 Unauthorized** means "I don't know who you
are" (no token / bad token), **403 Forbidden** means "I know who you
are but you can't do this" (auth OK, scope missing). CLI clients
should branch on the status code, not the message.

### Vocabulary

| Scope                | Gates                                                      |
|----------------------|------------------------------------------------------------|
| `*` (wildcard)       | Every endpoint. Use for break-glass / bootstrap tokens.    |
| `agents:read`        | List + get agents; group → agents listing.                 |
| `agents:write`       | Push config to an agent, restart, move between groups; group restart. |
| `groups:read`        | List + get groups + group configs.                         |
| `groups:write`       | Create, edit, delete groups; assign group configs.         |
| `configs:read`       | List + get + lint + validate configs.                      |
| `configs:write`      | Create, update, delete configs.                            |
| `telemetry:read`     | Query metrics / logs / traces; saved-query CRUD.           |
| `alerts:read`        | List + get alert rules.                                    |
| `alerts:write`       | Create, update, delete alert rules.                        |
| `rollouts:read`      | List + get rollouts + recipes + templates + preview.       |
| `rollouts:write`     | Create, abort, pause, resume rollouts.                     |
| `audit:read`         | Read audit log; subscribe to the SSE event stream.         |
| `auth:read`          | List API tokens.                                           |
| `auth:write`         | Create + revoke API tokens.                                |

### Common bundles

```bash
# Read-only viewer: someone watching dashboards but not deploying.
squadronctl auth create-token --label viewer \
  --scope agents:read --scope groups:read --scope configs:read \
  --scope telemetry:read --scope alerts:read --scope rollouts:read \
  --scope audit:read

# CI deploy pipeline: pushes configs and ships rollouts, nothing else.
squadronctl auth create-token --label ci-deploy \
  --scope configs:write --scope rollouts:write --scope rollouts:read

# Alerts manager: tunes alert rules without touching configs.
squadronctl auth create-token --label alerts-manager \
  --scope alerts:read --scope alerts:write --scope audit:read

# Break-glass / bootstrap: full access. Revoke after use.
squadronctl auth create-token --label oncall-break-glass --full-access
```

### Backward compatibility

Tokens issued before v0.10 have no scopes recorded (the column didn't
exist). The middleware treats those tokens as having **full access**
so the v0.10 upgrade doesn't break every existing operator and
automation token. The token-list UI renders them with a `legacy: full
access` badge — operators should revoke and reissue with explicit
scopes when they get a chance.

New tokens are required to declare scopes; the API rejects an empty
scope list at create time. Pass `["*"]` for the explicit full-access
case so the choice is visible in the audit log.

## Expiry

Tokens can carry an optional expiry. When a token's `expires_at` is in
the past, Squadron rejects it at validate time — same 401 response as
revoked or unknown tokens, so a guesser can't learn from the status
which condition applies.

Expiry is optional and defaults to never. Long-lived automation tokens
without an expiry are fine but should be rotated by hand every few
months. Tokens issued before v0.11 have no expiry recorded and stay
valid until explicitly revoked.

### Setting an expiry

From the UI: pick a duration in the **Expires** radio group on the
token-create form. "Never", 7/30/90 days, or "Custom" for an RFC3339
timestamp.

From the CLI:

```bash
# Duration shorthand: d (days), h, m, s. Repeatable units NOT supported —
# pick the unit that lands closest to your intended date.
squadronctl auth create-token --label deploy-bot \
  --scope rollouts:write --scope configs:write \
  --expires-in 90d

# Or pass an explicit RFC3339 timestamp.
squadronctl auth create-token --label q4-release \
  --scope rollouts:write \
  --expires-at 2026-12-31T23:59:59Z
```

`--expires-in` and `--expires-at` are mutually exclusive. The server
rejects expiries already in the past with a 400, so accidental
copy-paste of last year's date fails loudly at create time rather than
silently producing a dead-on-arrival token.

### What happens at expiry

The expired token starts returning 401 from its next request onward.
The token row remains in the store with `expires_at` set; the
**API tokens** page renders it with an `expired` status badge and an
"expired N days ago" hint so operators can rotate it explicitly.

Best practice: pair expiry with a calendar reminder for the
operator/automation owner. Squadron does not (yet) email or webhook
the owner when a token nears expiry — that's on the roadmap.

### Recommended cadences

| Bearer type             | Expiry              |
|-------------------------|---------------------|
| Per-person operator     | 90 days             |
| Per-CI-pipeline         | 180-365 days        |
| Bootstrap / break-glass | None (manual revoke) |
| Production automation   | 365 days + calendar reminder |

These are starting points. Adjust based on how often the bearer's
credentials get audited / rotated in your org.

## Using a token

### From a script

```bash
export SQUADRON_TOKEN=sqd_xxxxxx
curl http://localhost:8080/api/v1/agents \
  -H "Authorization: Bearer $SQUADRON_TOKEN"
```

### From the UI

The UI stores your token in `localStorage` under `squadron.auth.token`.
It's attached automatically to every API call. Clear it by signing out
(or by deleting the key in your browser's devtools).

> **localStorage caveat.** A successful XSS on the Squadron UI could
> read the token. That's a real but bounded risk — the UI is admin-only,
> so anyone with XSS already has equivalent in-page access. If you need
> stricter handling, front Squadron with an OIDC-aware reverse proxy and
> leave `auth.enabled` off; the proxy enforces auth and Squadron sees
> only authenticated traffic.

## Token lifecycle

```
   (issue)         (validate)         (revoke)
        \              |                |
         \             ↓                ↓
   active ──────► last_used_at ──► revoked ──► (kept in store for audit history)
                  bumped on each
                  request
```

- `created_at` is stamped at issue.
- `last_used_at` is updated best-effort on every successful validation.
  Update failure does not fail the request.
- `revoked_at` is set on revoke. Subsequent validates return "no match"
  and the middleware returns 401.
- Revoked rows are NEVER deleted. Audit entries from before the revoke
  reference the token ID; keeping the row lets you resolve those IDs to
  labels long after revocation.

There is no "expires_at" or rotation timer yet — operators rotate by
revoking the old token and creating a new one. Automatic rotation is on
the roadmap.

## Recovery: lost all tokens

If the operator loses every token (the bootstrap token was discarded,
no other tokens were created), the only path back in is shell access to
the Squadron host:

```bash
# Mark every token as revoked so the bootstrap flow re-fires.
sqlite3 /data/app.db "UPDATE api_tokens SET revoked_at = CURRENT_TIMESTAMP"

# Then delete them so the bootstrap re-fires (the bootstrap checks for
# "any tokens exist" rather than "any active tokens").
sqlite3 /data/app.db "DELETE FROM api_tokens"

# Restart Squadron. A new bootstrap token will print to stderr.
```

Document this for your on-call before you ship — needing to do this is
typically a 3am scenario.

## What's NOT included

- **User accounts / passwords.** Squadron has tokens, not users.
- **SSO / OIDC.** Use a reverse proxy with auth in front if you need it.
  Squadron's bearer-token layer is orthogonal — you can run both.
- **RBAC.** Every token has full API access; there's no concept of
  "read-only" or "rollouts only" yet. Tracked on the roadmap.
- **Automatic rotation.** Operators rotate by hand: create new, swap,
  revoke old.
- **Per-token rate limits / quotas.** Not yet.
