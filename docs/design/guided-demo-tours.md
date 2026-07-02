# Design: guided use-case tours (in-app demo)

Status: **accepted — building** (Michael, this session)
Date: 2026-07-02

## Problem

The demo today only covers per-cloud discovery (canned inventory + AI recs) plus a
separate CLI-seeded cost-spike loop (`cmd/squadron-demo-seed`). A first-time user —
e.g. an SRE evaluating the OSS — never sees Squadron's flagship capabilities:
safe fleet config rollouts, cost-spike → AI-fix → incident, on-prem agent
onboarding, or env → Terraform IaC. There's no guided path that says "here's how
Squadron handles *your* use-case, step by step."

## Goal

A **Use Cases** entry point where the user picks a flagship use-case; Squadron then
runs an **in-app coach-mark tour over the real pages**, backed by **real seeded
data**, narrating each step as it drives the actual UI.

Five tours:
1. **Instrument my cloud** — discovery → gaps → AI recs → merge-ready Terraform PR.
2. **Safe config rollout** — fleet → push an OTel config change as a staged canary.
3. **Cost spike → AI fix** — cost spike → AI proposes → approve → action → incident.
4. **On-prem agent onboarding** — one-command install → agents in Fleet w/ config+metrics+logs.
5. **Cloud env → Terraform IaC** — scanned inventory → generate import blocks → PR.

## Approach

### Tour engine (frontend, dependency-free)

A small custom coach-mark engine — no new npm dependency (keeps the bundle + the
partial-install CI surface clean, and we need cross-route control a generic lib
fights us on).

- `components/tour/TourHost.tsx` — mounted at app root inside `<Router>` next to
  `CommandPalette` (so it can `useNavigate` and portal an overlay across routes).
  Holds the active tour + step index. Listens for a `TOUR_START_EVENT`
  (`CustomEvent`, mirrors the existing `ASK_OPEN_EVENT` pattern) so any component
  can start a tour by id.
- Overlay: a fixed dim layer with a **spotlight hole** over the current step's
  target element (`getBoundingClientRect` + padding) and a **tooltip card**
  (title, body, `Step N of M`, Back / Next / Exit) anchored near it. No target →
  centered intro/outro card.
- Step lifecycle: on step change, if `location.pathname !== step.route`, navigate;
  then **poll for the target selector** (rAF loop, ~3s timeout) before painting the
  spotlight — pages load async (SWR), so we wait. Timeout → show the card centered
  with a "couldn't find this element" soft note (never a dead tour).
- Robustness: highlight + narrate + user clicks **Next**. We do NOT auto-click real
  buttons (fragile); some steps auto-navigate routes, and a step may call a small
  `onEnter` hook (e.g. ensure the demo connection is seeded) — idempotent + guarded.

### Tour registry (declarative)

`components/tour/tours.ts`:

```ts
type TourStep = {
  route?: string;            // navigate here first (if not already)
  target?: string;           // CSS selector to spotlight (data-tour="…")
  title: string;
  body: string;
  placement?: "top" | "bottom" | "left" | "right" | "center";
  onEnter?: () => Promise<void>; // idempotent side-effect (e.g. seed)
};
type Tour = { id: string; title: string; blurb: string; icon: string; steps: TourStep[] };
```

Pages expose stable `data-tour="<name>"` anchors on the elements a tour points at —
decouples tours from styling/class churn. Adding anchors is additive and harmless.

### Use Cases page + nav

`pages/UseCases.tsx` at `/use-cases`, added to `App.tsx` routes and the sidebar. A
grid of cards (one per tour) with title/blurb/icon and a "Start tour" button that
dispatches `TOUR_START_EVENT`. Also add a "Guided tours" affordance from the
Dashboard/empty-states so new users discover it.

### Real seeded data (backend)

Tours must run on real data end-to-end:
- **Discovery** (tours 1, 5): the existing demo connection + canned scan/recs
  (`internal/discovery/demo`) already covers this. The tour's first step ensures the
  demo connection is enabled (idempotent `onEnter`).
- **Fleet + config + cost-spike + incident** (tours 2, 3, 4): today only
  `cmd/squadron-demo-seed` seeds these, and only via a separate binary. Slice 1
  extracts that seeding into a reusable package and exposes an **idempotent,
  demo-scoped seed endpoint** the tours can call on start (with a matching
  disable/cleanup), so the fleet/cost tours have genuine rows to drive.

## Slice plan

- **Slice 0** (this): tour engine + Use Cases page + route/nav + reference tour
  **"Instrument my cloud"** (discovery demo already seeded). tsc/build gate; ship.
- **Slice 1**: extract seed logic → package; in-app demo-seed endpoint (fleet
  group+config+agents, cost-spike+incident) + disable; tests; ship.
- **Slice 2+**: author the remaining four tours against real seeded data + the
  `data-tour` anchors they need; live-verify each on a running stack; update
  `docs/demo.md`; ship per tour.

## Non-goals / risks

- Not a replacement for the hero video (`docs/demo-script.md`); complementary.
- No auto-clicking of destructive/real controls in a tour (open-PR, apply) — the
  tour highlights and narrates; the user chooses to act.
- Seed data is demo-scoped (reserved ids) and removable, matching today's model.
- Live-verification (per the definition-of-done) needs a running stack; unit/tsc
  gates run in CI, browser walkthrough is the final check per tour.
