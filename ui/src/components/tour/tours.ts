// Guided use-case tours — declarative registry + start API.
//
// A tour is an ordered list of coach-mark steps the TourHost drives over the
// REAL pages, backed by REAL seeded data. Pages expose stable `data-tour="…"`
// anchors on the elements a step points at, so tours are decoupled from styling
// churn. See docs/design/guided-demo-tours.md.

import { enableDemoData } from "@/api/demoData";

export type TourPlacement = "top" | "bottom" | "left" | "right" | "center";

export interface TourStep {
  /** Navigate here first if not already on this path. */
  route?: string;
  /** CSS selector to spotlight (prefer a `data-tour="…"` anchor). Omit for a
   *  centered narration card (intro/outro). */
  target?: string;
  title: string;
  body: string;
  placement?: TourPlacement;
  /** Idempotent side-effect run before the step paints (e.g. ensure the demo
   *  connection is seeded). Must be safe to re-run. */
  onEnter?: () => Promise<void>;
}

export interface Tour {
  id: string;
  title: string;
  blurb: string;
  /** lucide-react icon name resolved by the Use Cases page. */
  icon: string;
  /** Rough time to complete, shown on the card. */
  duration: string;
  steps: TourStep[];
}

// --- Event API (mirrors ASK_OPEN_EVENT) ------------------------------------

export const TOUR_START_EVENT = "squadron:tour-start";

/** Start a tour by id from anywhere. TourHost (mounted at app root) handles it. */
export function startTour(tourId: string): void {
  document.dispatchEvent(
    new CustomEvent<{ tourId: string }>(TOUR_START_EVENT, {
      detail: { tourId },
    }),
  );
}

// --- Registry ---------------------------------------------------------------

// Guard so the demo-seed side-effect only fires once per tour run even though
// onEnter is invoked on every (re)entry to step 0.
let demoEnsured = false;
async function ensureDemoData(): Promise<void> {
  if (demoEnsured) return;
  try {
    // Seed the FULL demo scenario (fleet + config + cost spike + discovery), so
    // every feature the tours showcase is already populated with sample data —
    // the user never has to connect a cloud or deploy an agent. Idempotent.
    await enableDemoData();
    demoEnsured = true;
  } catch {
    // Non-fatal: if demo data already exists (idempotent) or the request fails,
    // the tour still narrates over whatever is present.
    demoEnsured = true;
  }
}

export const TOURS: Tour[] = [
  {
    id: "instrument-cloud",
    title: "Instrument my cloud",
    blurb:
      "Watch Squadron scan a cloud account, find OpenTelemetry instrumentation gaps, and turn them into merge-ready Terraform — no real credentials needed.",
    icon: "Radar",
    duration: "~2 min",
    steps: [
      {
        route: "/discovery/aws?account=demo-000000000000",
        placement: "center",
        onEnter: ensureDemoData,
        title: "Instrument my cloud",
        body: "Squadron finds what in your cloud isn't sending telemetry yet, and writes the Terraform to fix it. We've loaded a sample AWS account with real findings so you can see it working — nothing to connect or configure. Click Next.",
      },
      {
        route: "/discovery/aws?account=demo-000000000000",
        target: '[data-tour="aws-tab-inventory"]',
        placement: "bottom",
        title: "The discovered inventory",
        body: "Squadron has scanned the sample account and inventoried what's running — EC2, Lambda, RDS — flagging which resources are already instrumented vs. which have observability gaps. Open this tab to see it.",
      },
      {
        route: "/discovery/aws?account=demo-000000000000",
        target: '[data-tour="aws-tab-recommendations"]',
        placement: "bottom",
        title: "AI recommendations, as Terraform",
        body: "For each gap, Squadron proposes a concrete fix — the right OTel/ADOT setup for that resource — as merge-ready Terraform. From here you'd open a pull request straight to your IaC repo. That's the loop: scan → gaps → fix, as code.",
      },
      {
        placement: "center",
        title: "That's the instrument-my-cloud loop",
        body: "Point Squadron at a real account and it does exactly this against your infrastructure. Explore the sample inventory and recommendations on your own, or pick another use case.",
      },
    ],
  },

  {
    id: "config-rollout",
    title: "Roll out a config change",
    blurb:
      "Edit a collector config once and stage it across the fleet — a canary-first rollout that watches health and rolls back on its own if a stage goes bad.",
    icon: "Rocket",
    duration: "~2 min",
    steps: [
      {
        route: "/configs",
        placement: "center",
        onEnter: ensureDemoData,
        title: "Roll out a config change",
        body: "Squadron manages every collector's config centrally. Change it once here and stage the change across the fleet — no SSH-ing into boxes. We've loaded sample configs and a fleet so you can see it. Click Next.",
      },
      {
        route: "/configs",
        placement: "center",
        title: "Your collector configs",
        body: "This is every OpenTelemetry Collector config Squadron tracks — versioned, so editing one creates a new revision you can roll out. Open any config to see the diff-aware editor with live validation.",
      },
      {
        route: "/rollouts",
        target: '[data-tour="rollouts-new"]',
        placement: "left",
        title: "Stage it as a rollout",
        body: "A rollout ships a config revision in stages — canary first, then wider — advancing on dwell time and rolling back automatically if a stage's abort criteria fire. You get a safe, observable deploy instead of a fleet-wide flip.",
      },
      {
        placement: "center",
        title: "That's the rollout loop",
        body: "Edit once, stage safely, and let Squadron watch each stage's health. Explore the sample configs and rollouts on your own, or pick another use case.",
      },
    ],
  },

  {
    id: "cost-spike-fix",
    title: "Catch a cost spike, fix it",
    blurb:
      "See Squadron flag a telemetry cost spike, attribute it to the pipeline that caused it, and draft the config change that brings spend back down.",
    icon: "Coins",
    duration: "~2 min",
    steps: [
      {
        route: "/",
        placement: "center",
        onEnter: ensureDemoData,
        title: "Catch a cost spike, fix it",
        body: "Telemetry bills spike when a pipeline starts emitting far more than usual. Squadron watches for that and turns it into an actionable finding. We've seeded a sample spike so you can see the whole loop. Click Next.",
      },
      {
        route: "/",
        target: '[data-tour="cost-spike-banner"]',
        placement: "bottom",
        title: "The spike, front and center",
        body: "This banner fires only when there's an open, unacknowledged cost spike — projection jumped well above the rolling baseline. It's the heads-up an on-call engineer needs before the invoice arrives.",
      },
      {
        route: "/",
        target: '[data-tour="ask-squadron-hero"]',
        placement: "bottom",
        title: "Ask the deputy why",
        body: "Ask Squadron answers in plain English from your own data — it'll cite the spike, name the pipeline driving it, and can draft the config change (e.g. tighten sampling or drop a noisy metric) to bring spend back down.",
      },
      {
        route: "/savings",
        placement: "center",
        title: "Attribution and savings",
        body: "The Savings view attributes the spend and tracks what each applied fix saved — so the loop closes: detect → attribute → fix → verify. Explore it on your own, or pick another use case.",
      },
    ],
  },

  {
    id: "onprem-onboarding",
    title: "Onboard an on-prem agent",
    blurb:
      "Point a collector running on your own hardware at Squadron over OpAMP and watch it register into the fleet — no cloud account required.",
    icon: "Server",
    duration: "~2 min",
    steps: [
      {
        route: "/quickstart",
        placement: "center",
        onEnter: ensureDemoData,
        title: "Onboard an on-prem agent",
        body: "Squadron manages OpenTelemetry Collectors wherever they run — including your own data-center hardware — via the OpAMP protocol. Let's walk the path from install to a live agent. Click Next.",
      },
      {
        route: "/quickstart",
        placement: "center",
        title: "Pick your starting point",
        body: "Whether you're starting fresh or already have collectors running, Quickstart hands you the exact config — the OpAMP extension pointed at this Squadron — to drop onto each host. No agent rewrite; just point and reload.",
      },
      {
        route: "/agents",
        placement: "center",
        title: "It shows up in the fleet",
        body: "Once a collector's OpAMP extension reaches Squadron, it registers here as an agent — identified by host, grouped, and ready for config management. The sample fleet shows what a populated roster looks like.",
      },
      {
        placement: "center",
        title: "That's on-prem onboarding",
        body: "One config snippet turns any collector — cloud or bare metal — into a managed agent. Explore the sample fleet on your own, or pick another use case.",
      },
    ],
  },

  {
    id: "env-to-terraform",
    title: "Turn your environment into Terraform",
    blurb:
      "Squadron inventories what's actually running and generates Terraform import blocks — bring drifted, click-built resources under version control as a merge-ready PR.",
    icon: "FileCode",
    duration: "~2 min",
    steps: [
      {
        route: "/discovery/aws?account=demo-000000000000",
        placement: "center",
        onEnter: ensureDemoData,
        title: "Turn your environment into Terraform",
        body: "Plenty of infrastructure gets click-built and never makes it into IaC. Squadron scans what's really running and writes the Terraform to adopt it — imports and all. We've loaded a sample AWS account. Click Next.",
      },
      {
        route: "/discovery/aws?account=demo-000000000000",
        target: '[data-tour="aws-tab-inventory"]',
        placement: "bottom",
        title: "The real inventory",
        body: "Squadron's scan inventoried what exists in the account — the same resources you'd otherwise hand-write import blocks for. Open this tab to see them.",
      },
      {
        route: "/discovery/aws?account=demo-000000000000",
        target: '[data-tour="aws-generate-tf"]',
        placement: "top",
        title: "Generate import blocks → PR",
        body: "From the scan, Squadron emits Terraform import blocks with the canonical resource IDs already resolved, dedup'd against what's already in your repo — then opens a merge-ready pull request. Your click-built environment becomes reviewable code.",
      },
      {
        placement: "center",
        title: "That's env → Terraform",
        body: "Scan an account and Squadron drafts the imports to bring it under version control. Explore the sample inventory on your own, or pick another use case.",
      },
    ],
  },
];

export function findTour(id: string): Tour | undefined {
  return TOURS.find((t) => t.id === id);
}
