#!/usr/bin/env bash
#
# demo-seed.sh — stand up demo-worthy state on a running Squadron.
#
# One orchestrator that layers the pieces a live demo needs:
#   * OSS state  — groups, configs, a synthetic agent, a +312% cost spike,
#                  rollouts, actions, incidents, alerts, audit (via the
#                  squadron-demo-seed binary, direct-to-store).
#   * Enterprise — multi-tenant + RBAC (tenants, roles, bindings) via the
#                  authenticated HTTP API, when a bootstrap token is supplied.
#
# It is idempotent: re-running skips what already exists (the OSS seed is
# idempotent by design, and the enterprise phase tolerates 409-already-exists).
#
# Usage:
#   scripts/demo-seed.sh                 # OSS seed only (no $TOKEN set)
#   TOKEN=sqd_... scripts/demo-seed.sh   # OSS seed + enterprise tenants/RBAC
#   scripts/demo-seed.sh --migration-smoke   # ADR "OSS->enterprise" story
#   scripts/demo-seed.sh --help
#
# Configuration (Phase 0) is via environment variables — see below.
#
# NOTE: This script never boots Squadron and never provisions OIDC/SCIM (there
# is no public create endpoint for those — see Phase 3). It seeds state on an
# ALREADY-RUNNING instance (enterprise phase) and/or writes directly to the
# application store DB file (OSS phase).
set -euo pipefail

# ---------------------------------------------------------------------------
# Phase 0 — configuration
# ---------------------------------------------------------------------------
# SQUADRON_URL  Base URL of a running Squadron for the enterprise HTTP phase.
#               Default: http://localhost:8090
# SQUADRON_DB   Path to the application-store sqlite file the OSS seed writes
#               to (direct store; the server need not be running for Phase 1).
#               Default: ./data/squadron.db
# TOKEN         Bootstrap bearer token for the enterprise phase (Phase 2). It
#               is auto-issued and logged at Warn on the enterprise binary's
#               first boot with an empty token store (label "bootstrap",
#               wildcard scope). REQUIRED for Phase 2; if unset, Phase 2 is
#               skipped and the seed is OSS-only.
# SQUADRON_BIN  Path to the compiled squadron-demo-seed binary. Built on the
#               fly if missing. Default: ./bin/squadron-demo-seed
SQUADRON_URL="${SQUADRON_URL:-http://localhost:8090}"
SQUADRON_DB="${SQUADRON_DB:-./data/squadron.db}"
TOKEN="${TOKEN:-}"
SQUADRON_BIN="${SQUADRON_BIN:-./bin/squadron-demo-seed}"

MIGRATION_SMOKE=0

# ---------------------------------------------------------------------------
# Pretty printing helpers
# ---------------------------------------------------------------------------
hdr()  { printf '\n\033[36m== %s ==\033[0m\n' "$1"; }
ok()   { printf '\033[32m  ok  \033[0m %s\n' "$1"; }
info() { printf '  ->  %s\n' "$1"; }
warn() { printf '\033[33m warn \033[0m %s\n' "$1"; }
note() { printf '\033[35m note \033[0m %s\n' "$1"; }

usage() {
  # Print the leading comment block (everything up to the first blank line
  # after `set -euo pipefail`) is overkill; give a focused help instead.
  cat <<'HELP'
demo-seed.sh — orchestrate demo state on a running Squadron.

USAGE
  scripts/demo-seed.sh [--migration-smoke] [--help]

PHASES
  0  config       read env vars (see below).
  1  OSS state    run squadron-demo-seed against $SQUADRON_DB (direct store):
                  group, baseline config, agent, +312% cost spike, and the
                  rollout/action/incident/alert/audit trail that follows.
  2  enterprise   if $TOKEN is set, provision tenants + RBAC roles/bindings
                  over the HTTP API at $SQUADRON_URL (idempotent; 409-tolerant).
  3  OIDC/SCIM    honest NOTE only — these have no public create endpoint.
  4  summary      what was seeded + the URLs to open for the demo.

FLAGS
  --migration-smoke   Run/print the OSS->enterprise "same DB, no migration"
                      story: seed OSS state into a data dir, then print the
                      exact commands to boot the enterprise binary against the
                      SAME dir and the "data intact" assertions to check.
  --help, -h          Show this help and exit.

ENVIRONMENT
  SQUADRON_URL   enterprise HTTP base URL   (default http://localhost:8090)
  SQUADRON_DB    application-store sqlite   (default ./data/squadron.db)
  TOKEN          bootstrap bearer token     (required for Phase 2; else skipped)
  SQUADRON_BIN   squadron-demo-seed binary  (default ./bin/squadron-demo-seed;
                 built on the fly if missing)

EXAMPLES
  scripts/demo-seed.sh
  TOKEN=sqd_xxx SQUADRON_URL=http://localhost:8090 scripts/demo-seed.sh
  SQUADRON_DB=./data/app.db scripts/demo-seed.sh --migration-smoke
HELP
}

# ---------------------------------------------------------------------------
# curl helpers for the enterprise HTTP phase.
#
# api_post writes to a create endpoint and treats 200/201 as created and 409 as
# already-exists (idempotent). Any other status is surfaced as a warning but
# does NOT abort the run (set -e is neutralised by capturing the status). This
# keeps a partially-seeded instance re-runnable.
# ---------------------------------------------------------------------------
api_post() {
  local label="$1" path="$2" body="$3"
  local tmp code
  tmp="$(mktemp)"
  # -sS: quiet but show errors; -w: append the HTTP status for parsing.
  code="$(curl -sS -o "$tmp" -w '%{http_code}' \
    -X POST "${SQUADRON_URL}${path}" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H 'Content-Type: application/json' \
    -d "$body" 2>/dev/null || echo '000')"
  case "$code" in
    200|201) ok "$label — created ($code)" ;;
    409)     ok "$label — already exists (409, idempotent)" ;;
    000)     warn "$label — no response from ${SQUADRON_URL} (is it running?)" ;;
    401|403) warn "$label — auth failed ($code); is \$TOKEN a valid bootstrap token?" ;;
    *)       warn "$label — unexpected status $code: $(tr -d '\n' < "$tmp" | cut -c1-160)" ;;
  esac
  rm -f "$tmp"
}

# ---------------------------------------------------------------------------
# Phase 1 — OSS state (direct store, no running server required).
# Mirrors `make demo-seed`: build the binary if absent, then seed $SQUADRON_DB.
# ---------------------------------------------------------------------------
phase_oss() {
  hdr "Phase 1 — OSS state (direct store -> ${SQUADRON_DB})"
  if [ ! -x "$SQUADRON_BIN" ]; then
    info "binary ${SQUADRON_BIN} missing — building (mirrors 'make demo-seed')"
    mkdir -p "$(dirname "$SQUADRON_BIN")"
    go build -o "$SQUADRON_BIN" ./cmd/squadron-demo-seed
    ok "built ${SQUADRON_BIN}"
  fi
  mkdir -p "$(dirname "$SQUADRON_DB")"
  info "seeding demo scenario (group, baseline config, agent, +312% cost spike)"
  # The binary is idempotent; the cost spike drives an AI-drafted rollout that
  # surfaces under /rollouts within ~30s once the server + proposer are up.
  "$SQUADRON_BIN" --db "$SQUADRON_DB"
  ok "OSS seed complete — demo group / config / agent / cost spike in place"
  info "downstream (once the server + proposer run): rollout -> action ->"
  info "incident -> alerts -> audit trail all follow from the seeded spike"
}

# ---------------------------------------------------------------------------
# Phase 2 — enterprise multi-tenant + RBAC (HTTP API; needs $TOKEN).
# ---------------------------------------------------------------------------
phase_enterprise() {
  hdr "Phase 2 — enterprise tenants + RBAC (HTTP ${SQUADRON_URL})"
  if [ -z "$TOKEN" ]; then
    warn "\$TOKEN is not set — skipping the enterprise phase (OSS-only seed)."
    note "To provision tenants/RBAC, boot the enterprise binary with auth"
    note "enabled and an EMPTY token store; it auto-issues a bootstrap token"
    note "logged loudly at Warn:"
    note '  "API auth is enabled and no tokens exist yet — issued a bootstrap token."'
    note "Capture that sqd_... plaintext and re-run with TOKEN=sqd_..."
    return 0
  fi

  # --- Tenants (trailing slash matters; bare /tenants 307-redirects) ---------
  info "creating tenants (POST /api/v1/tenants/ — trailing slash)"
  api_post "tenant acme"      "/api/v1/tenants/" '{"name":"Acme"}'
  api_post "tenant beta-corp" "/api/v1/tenants/" '{"name":"Beta Corp"}'

  # --- RBAC roles ------------------------------------------------------------
  # A role is {name, permissions[]}; each permission is
  # {scope, resource_type, all_resources, resource_ids}.
  info "creating RBAC roles (POST /api/v1/rbac/roles)"
  api_post "role tenant-admin" "/api/v1/rbac/roles" '{
    "name": "tenant-admin",
    "permissions": [
      {"scope":"tenants:read","resource_type":"tenant","all_resources":true,"resource_ids":[]},
      {"scope":"tenants:write","resource_type":"tenant","all_resources":true,"resource_ids":[]},
      {"scope":"rbac:read","resource_type":"role","all_resources":true,"resource_ids":[]},
      {"scope":"rbac:write","resource_type":"role","all_resources":true,"resource_ids":[]},
      {"scope":"rollouts:read","resource_type":"rollout","all_resources":true,"resource_ids":[]},
      {"scope":"rollouts:write","resource_type":"rollout","all_resources":true,"resource_ids":[]}
    ]
  }'
  api_post "role auditor" "/api/v1/rbac/roles" '{
    "name": "auditor",
    "permissions": [
      {"scope":"audit:read","resource_type":"audit","all_resources":true,"resource_ids":[]},
      {"scope":"audit:verify","resource_type":"audit","all_resources":true,"resource_ids":[]}
    ]
  }'

  # --- Bindings --------------------------------------------------------------
  # A binding is {role_id, principal_kind, principal_ref}. There is no user
  # model in OSS/enterprise-core — bindings key on an API token id or its
  # label. We bind by token_label so the demo doesn't need a token id lookup;
  # mint tokens with these labels (or rename to match your tokens).
  info "creating RBAC bindings (POST /api/v1/rbac/bindings, by token_label)"
  api_post "binding tenant-admin->acme-admin" "/api/v1/rbac/bindings" \
    '{"role_id":"tenant-admin","principal_kind":"token_label","principal_ref":"acme-admin"}'
  api_post "binding auditor->compliance-bot" "/api/v1/rbac/bindings" \
    '{"role_id":"auditor","principal_kind":"token_label","principal_ref":"compliance-bot"}'
  note "bindings above reference role_id by NAME for readability; if your"
  note "build keys bindings on the role's generated id, read it back from"
  note "GET /api/v1/rbac/roles and substitute the id."
}

# ---------------------------------------------------------------------------
# Phase 3 — OIDC / SCIM honesty note (NO public create endpoint).
# ---------------------------------------------------------------------------
phase_identity_note() {
  hdr "Phase 3 — OIDC / SCIM (out-of-band; NOT seeded here)"
  cat <<'NOTE'
  +---------------------------------------------------------------------------+
  |  OIDC connections and SCIM users are deliberately NOT created by this      |
  |  script — there is no public create endpoint to call, and faking them      |
  |  would make the demo dishonest.                                            |
  |                                                                            |
  |   * OIDC connections live in the enterprise `oidc_connections` SQLite      |
  |     table (issuer / client_id / sealed client_secret / redirect_uri /      |
  |     tenant_id / default_role). They are added store-level, not via YAML    |
  |     or an API. Requires SQUADRON_SECRETS_KEY to seal the client secret.    |
  |   * SCIM users arrive via a directory sync: mint a SCIM service token      |
  |     INTERNALLY (reserved `scim:` label, `scim:write` scope, tenant-bound), |
  |     point the IdP at /api/v1/scim/v2, and let it push Users/Groups.        |
  |                                                                            |
  |  To show these surfaces in a demo, provision them out-of-band per          |
  |  squadron-enterprise/docs/DEPLOYMENT.md (sections 8 and 9).                |
  +---------------------------------------------------------------------------+
NOTE
}

# ---------------------------------------------------------------------------
# Phase 4 — summary + demo URLs.
# ---------------------------------------------------------------------------
phase_summary() {
  hdr "Phase 4 — summary"
  ok "OSS state seeded into ${SQUADRON_DB}"
  if [ -n "$TOKEN" ]; then
    ok "enterprise tenants + RBAC provisioned against ${SQUADRON_URL}"
  else
    warn "enterprise phase skipped (no \$TOKEN) — OSS-only seed"
  fi
  echo
  info "Open these for the demo (adjust host/port to your instance):"
  printf '    %s/                    Fleet status dashboard\n'      "$SQUADRON_URL"
  printf '    %s/rollouts            Staged rollouts + approval gate\n' "$SQUADRON_URL"
  printf '    %s/cost-insights       Cost Insights + recommendations\n' "$SQUADRON_URL"
  printf '    %s/savings             Savings hero + Quick Wins\n'   "$SQUADRON_URL"
  printf '    %s/audit               Audit timeline + integrity\n' "$SQUADRON_URL"
  printf '    %s/settings/identity   Tenants / roles / usage / budgets\n' "$SQUADRON_URL"
  echo
}

# ---------------------------------------------------------------------------
# --migration-smoke — the OSS -> enterprise "same DB, no migration" story.
#
# Documented (and partially executed) smoke: Phase 1 actually seeds OSS state
# into $SQUADRON_DB with the OSS binary; the enterprise boot against the SAME
# data dir is PRINTED (booting a server + a private enterprise build is out of
# scope for this scaffold). The point being demonstrated: it is a build
# overlay, not a schema migration — tenant_id already ships in the OSS base
# schema, so the enterprise binary boots against the same rows unchanged.
# See docs/oss-to-enterprise-migration.md.
# ---------------------------------------------------------------------------
migration_smoke() {
  hdr "Migration smoke — OSS -> Enterprise (same DB, no schema migration)"
  note "This flag seeds OSS state, then PRINTS the enterprise overlay steps."
  note "Booting the enterprise binary is out of scope for this scaffold — the"
  note "commands below are the documented smoke you run by hand."
  echo

  # Step 1 (executed): seed OSS state into the shared data dir.
  phase_oss

  # Step 2 (printed): boot the ENTERPRISE build against the SAME data dir.
  hdr "Next steps (run by hand)"
  local data_dir; data_dir="$(dirname "$SQUADRON_DB")"
  cat <<SMOKE
  # 1. Build the enterprise edition from the private pack (build overlay;
  #    reverts the OSS tree on exit). See squadron-enterprise/docs/DEPLOYMENT.md.
  (cd ../squadron-enterprise && make build-enterprise)   # -> bin/squadron-enterprise

  # 2. Boot it against the SAME data directory the OSS seed just wrote to.
  #    No dump, no reload, no ALTER — tenant_id already ships in the OSS schema
  #    (every per-tenant table: tenant_id TEXT NOT NULL DEFAULT 'default').
  #    Point storage.app.path at ${SQUADRON_DB} in your enterprise squadron.yaml,
  #    set auth.enabled: true and ingest.otlp.tenant_id: default, then:
  bin/squadron-enterprise --config squadron.yaml

  # 3. Capture the bootstrap token (logged at Warn on first boot, empty store):
  #    "API auth is enabled and no tokens exist yet — issued a bootstrap token."
  export TOKEN=sqd_<plaintext-from-logs>

  # 4. Confirm the OSS-seeded data is intact under the enterprise binary and
  #    that tenants can be added on top of it — no migration in between.
  scripts/demo-seed.sh    # re-run: Phase 1 is a no-op (idempotent), Phase 2 adds tenants

ASSERTIONS ("same DB, data intact"):
  * Startup log:  edition=squadron-enterprise
  * /metrics:     squadron_build_info{edition="squadron-enterprise"} 1
  * The demo group / config / agent seeded by Phase 1 are STILL present
    (same rows — they were written under the implicit 'default' tenant).
  * POST /api/v1/tenants/ succeeds (multi-tenancy now active on the same DB).
  * Data dir '${data_dir}' was never dumped/reloaded — only the binary changed.
SMOKE
  echo
  ok "migration smoke: OSS seed done; enterprise overlay steps printed above."
}

# ---------------------------------------------------------------------------
# Arg parsing
# ---------------------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --migration-smoke) MIGRATION_SMOKE=1 ;;
    -h|--help)         usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; echo "try --help" >&2; exit 2 ;;
  esac
  shift
done

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  hdr "Phase 0 — configuration"
  info "SQUADRON_URL = ${SQUADRON_URL}"
  info "SQUADRON_DB  = ${SQUADRON_DB}"
  info "SQUADRON_BIN = ${SQUADRON_BIN}"
  if [ -n "$TOKEN" ]; then info "TOKEN        = set (enterprise phase enabled)"
  else info "TOKEN        = unset (enterprise phase will be skipped)"; fi

  if [ "$MIGRATION_SMOKE" -eq 1 ]; then
    migration_smoke
    exit 0
  fi

  phase_oss
  phase_enterprise
  phase_identity_note
  phase_summary
}

main
