# CLAUDE.md — Squadron

> Read this first. It points to the **project brain** — persistent, cross-session memory for Squadron.

## Load the brain before working
At the start of any Squadron session, read:
- `~/Documents/Squadron-Brain/00-Identity/PROJECT.md` — what Squadron is, architecture, open-core seam, current state.
- `~/Documents/Squadron-Brain/00-Identity/WORKING-WITH-ME.md` — voice + engineering discipline to follow.
- `~/Documents/Squadron-Brain/MOC/index.md` — index of decisions, knowledge, and the external strategy/design docs.

Before any non-trivial change, check:
- `~/Documents/Squadron-Brain/decisions/` — settled architectural calls (don't re-litigate; e.g. the open-core boundary in `0001`).
- `~/Documents/Squadron-Brain/knowledge/` — known gotchas (esp. `ci-gotchas.md`).

## Keep the brain current
- Made an architectural decision? Add an ADR to `~/Documents/Squadron-Brain/decisions/` (next number, update its README index).
- Hit a non-obvious gotcha? Add it to `~/Documents/Squadron-Brain/knowledge/`.
- Changed a roadmap card's status? Update the `squadron-roadmap` board artifact.

## Non-negotiables (from the brain — summarized here)
- **Dogfood the real path end-to-end before claiming it works.** e2e finds bugs unit tests miss.
- **Every AI fix is a PR** (HCL-aware merge + `terraform validate` gate + verdict learning).
- **CI ≠ local.** `go vet` passing locally does not mean `make lint` (staticcheck) passes in CI. Run `make lint` before pushing. See `knowledge/ci-gotchas.md`.
- **Report the seam, not the pack.** Only claim enterprise behavior verifiable from this OSS tree; the private `squadron-enterprise` repo isn't visible here.
- **Boundary is load-bearing:** breadth + core loop stays OSS; monetize org-scale readiness. Check `decisions/0001-open-core-boundary.md` before moving a feature to Enterprise.

## Common commands
- `make test` / `make test-coverage` — Go tests.
- `make lint` — golangci-lint / staticcheck (the CI gate that catches what `go vet` doesn't).
- `make build` — build (includes UI).
- `make build-enterprise` — enterprise build (build tags; OSS wires no-op providers).

---
_The brain is curated synthesis, not a live repo/CI scrape — as current as its last update. A nightly compound job maintains it._
