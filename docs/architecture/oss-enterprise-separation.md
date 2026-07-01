# OSS / Enterprise separation — the contract

Squadron is **open-core**. The open-source core (Apache-2.0) is fully
functional on its own; the commercial value sits in closed packs that are
compiled in only in the **enterprise** edition. This document is the
contract every paid feature must follow so the boundary stays consistent
and enforceable.

The governing principle: **the entitlement boundary is which code is
compiled in, not a runtime flag.** A config switch may gate *cost* or
*safety*, but never *access*. An OSS binary must not be convertible into an
enterprise binary by editing configuration.

## The seam (four required parts)

Every paid feature is wired through the same open/closed seam:

1. **Interface in the open core, under `extension/`** (not `internal/`), so
   the private enterprise repo can import it across module boundaries. The
   interface is the *only* thing the open core knows about the feature.
2. **A no-op / limited default provider**, wired by the OSS build. This is
   real, shipping OSS behaviour — the feature is inert but the binary is
   complete (nothing crashes, nothing is half-wired).
3. **The real provider in the private repo**, wired by the edition build via
   a tag-guarded wire file that is dropped into the build tree at release
   time. The open core ships a **stub** wire file under the same tag that
   `panic`s-with-guidance, so an edition build assembled *without* the
   private code fails loudly instead of silently degrading to OSS.
4. **A single call site** in `main.go` that consults the wired provider and
   does not branch on edition. The **build identity** string the wire
   returns is logged at startup and exposed on `/metrics`
   (`squadron_build_info{edition=...}`).

See [docs/build.md](../build.md) for the build tags, Make targets, and the
drop-in mechanism.

## Current extension points

| Extension point | Open-core interface | OSS default (no-op) | Enterprise (private) | Tag |
|---|---|---|---|---|
| Group approval policy | `extension/policy` | groups carry `require_approval` metadata; engine does not enforce | store-backed enforcement (removes operator discretion) | `compliance` |
| Rollout change windows | `extension/changewindow` | windows stored as metadata; engine never blocks | store-backed blackout enforcement | `compliance` |
| SIEM audit dispatch | `extension/siem` | destinations stored, never delivered to | Splunk HEC + HMAC-signed webhook fan-out | `compliance` |
| Per-call access audit | (wired in `wire_compliance.go`) | middleware unmounted; no per-call evidence rows | `middleware.APIAccessAudit` mounted | `compliance` |
| Commercial-tier serverless detectors | `extension/detectors` | `NoOpProvider` — never activates; `commercial_detectors.enabled` inert | honours the switch; re-points cold-start / error-rate queries at Lambda Insights / Application Insights + wires observation stores | `enterprise` |

The wiring files that select the provider per edition:

- Compliance pack: `cmd/all-in-one/wire_oss.go` (`//go:build !compliance`) vs
  `cmd/all-in-one/wire_compliance.go` (`//go:build compliance`, stub).
- Commercial detectors: `cmd/all-in-one/wire_detectors_oss.go`
  (`//go:build !enterprise`) vs `cmd/all-in-one/wire_detectors_enterprise.go`
  (`//go:build enterprise`, stub).

## Edition naming

`enterprise` is the **umbrella** edition. It composes the packs above; the
enterprise build sets `-tags "enterprise compliance"`. The `compliance` tag
predates the umbrella and keeps its own name (the seam is proven and
untouched), so it remains a distinct tag *within* the enterprise edition
rather than being renamed. New paid packs go behind the `enterprise` tag
unless there is a specific reason to ship them independently. A single
umbrella (vs. per-pack tags) keeps the build matrix small for a small team;
opening a pack up to OSS later is a one-line provider swap.

## What is NOT behind the seam (deliberately OSS)

- **Native-metric serverless detection** (`serverless_metric_detection.enabled`):
  AWS `Errors`/`Invocations`, Cloud Monitoring, OCI Monitoring. This is a
  genuine OSS capability gated only by a runtime cost switch (per-scan metric
  reads), *not* an edition tag. Do not move it behind the seam.
- The full discovery → AI recommendation → Terraform-PR loop, the OTel fleet
  control plane, staged rollouts, config editor, cost/savings, alerts, audit
  log, and demo mode. Breadth + the core loop stay free.

## Adding a new paid feature — checklist

1. Define the narrowest possible interface under `extension/<name>` (adapt
   internal types at the wire layer; never import `internal/` from the
   private repo).
2. Ship an OSS no-op/limited default provider and a unit test proving it is
   inert (mirror `extension/detectors/detectors_test.go`).
3. Add the OSS wire (`//go:build !<tag>`) returning the no-op, and the
   open-core stub wire (`//go:build <tag>`) that panics-with-guidance.
4. Consult the provider at a single `main.go` call site; read the resolved
   value, never a raw config flag, at every downstream activation point.
5. If a runtime switch exists, document it as cost/safety only, and log that
   it is inert in OSS when set.
6. Update [docs/oss-vs-enterprise.md](../oss-vs-enterprise.md),
   [docs/build.md](../build.md), and this table.
7. Verify: default `go build` / `go vet` / `gofmt` clean and tests green;
   `go vet -tags "<edition>"` clean (stub compiles, panics at runtime).
