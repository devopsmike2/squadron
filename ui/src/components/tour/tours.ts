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
];

export function findTour(id: string): Tour | undefined {
  return TOURS.find((t) => t.id === id);
}
