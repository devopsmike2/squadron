# Security gates

Squadron's CI runs security scanners as **gates** (`.github/workflows/security.yml`).
Each gate is **baselined**: `main` is green today, and any *new* high/critical
finding fails the build. Everything currently suppressed is listed below with a
justification and a TODO — nothing is silenced without a reason.

> The gates run on every PR and every push to `main`/`develop`.

## Gates

| Gate | Tool | What fails the build | Baseline / allowlist |
|------|------|----------------------|----------------------|
| Reachable Go vulns | `govulncheck` | Any **call-graph-reachable** vuln not in the allowlist | `.govulncheck-allow.txt` |
| Go SAST | `gosec -severity high -confidence high` | Any High-severity **and** High-confidence finding | inline `#nosec` (one) |
| Secrets | `gitleaks dir` (default rules) | Any secret outside the allowlisted fixtures | `.gitleaks.toml` |
| Go dependency vulns | `trivy fs --severity HIGH,CRITICAL` (Go graph) | Any HIGH/CRITICAL Go CVE not ignored | `.trivyignore` |
| UI prod dependency vulns | `npm audit --omit=dev --audit-level=high` | Any high/critical advisory in the **production** tree | none (tree is clean) |
| SBOM | `syft -o spdx-json` | never (informational artifact) | n/a |

## What is baselined, and why

All items trace back to the 2026-08-24 deep security scan triage.

### govulncheck — `.govulncheck-allow.txt`
- **GO-2026-5158** (`go.opentelemetry.io/otel` baggage DoS) — reachable via the
  OTLP telemetry path. Deferred pending a **coordinated** bump of the whole otel
  module set (core + otlp exporters move in lockstep). TODO: bump otel, then
  delete the line. This is the **only** reachable vuln; everything else
  (grpc, x/text) was already fixed by earlier dependency bumps.

### gosec — inline `#nosec`
- **G701** in `internal/storage/telemetrystore/duckdb/duckdb.go` — DuckDB's `SET
  memory_limit=...` has no bind form, so the value is string-interpolated. The
  value is operator config only (a flag / `SQUADRON_DUCKDB_MEMORY_LIMIT`), never
  request/agent input, and is now regex-validated before use (defense-in-depth).
  Annotated `#nosec G701` with that rationale. No other high/high findings exist;
  the medium/low FPs from the deep scan (G204 exec, G404 rng, G101 label consts,
  G201/G202 fmt) fall below the high/high threshold and never reach this gate.

### gitleaks — `.gitleaks.toml`
Full git history is clean (trufflehog `--only-verified` = 0). The allowlist
covers only **verified non-secrets**: synthetic test fixtures that deliberately
embed fake tokens (`internal/ai/redact_test.go`, `audit_explain_test.go`,
`stress_seeds_test.go`, `iacconnstore/github_pat_test.go`,
`scannerfactory/factory_test.go`), an AAD label constant
(`credstore/oci_signing_key.go`), UI example strings
(`DiscoveryOCI.tsx`/`.test.tsx`, `CommandPaletteHint.tsx`), and generated build
output (`ui/dist`, `ui/coverage`, `ui/.pnpm-store`, `node_modules`). `.env` is
**not** allowlisted — it is the real-secret tripwire.

### trivy — `.trivyignore`
- **CVE-2026-56864 / CVE-2026-56865** (`golang.org/x/mod`, fixed in v0.40.0) —
  imported transitively but **not reachable** (govulncheck confirms). Accepted;
  TODO: `go get golang.org/x/mod@v0.40.0` then remove. The Trivy gate skips
  `ui/` (UI prod deps are governed by `npm audit`); frontend dev/build-tooling
  advisories are surfaced non-gating in the Trivy SARIF upload (Security tab).

### npm audit
The UI **production** dependency tree (`ui/package-lock.json`, CI's `npm ci`
source) currently reports 0 high/critical advisories, so no baseline is needed.
Dev/build-tooling advisories are intentionally out of scope for this gate.

## Adding a new suppression
Prefer fixing the finding (bump the dep, fix the code). Only if a finding is a
verified false-positive or a consciously-deferred risk, add it to the relevant
file above **with a justification and a TODO**, and update this doc.
