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
   (audit fan-out dispatcher), and `extension/detectors` (commercial-tier
   detector activation).
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
  detectors (AWS `Errors`/`Invocations`). This one is genuinely OSS: it
  stays a runtime switch and is **not** behind an edition tag.

See [docs/oss-vs-enterprise.md](oss-vs-enterprise.md) for the full boundary
and [docs/architecture/oss-enterprise-separation.md](architecture/oss-enterprise-separation.md)
for the contract every future paid feature follows.
