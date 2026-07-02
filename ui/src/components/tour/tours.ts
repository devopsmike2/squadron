// Guided use-case tours — declarative registry + start API.
//
// A tour is an ordered list of coach-mark steps the TourHost drives over the
// REAL pages, backed by REAL seeded data. Pages expose stable `data-tour="…"`
// anchors on the elements a step points at, so tours are decoupled from styling
// churn. See docs/design/guided-demo-tours.md.

import { enableDemoConnection } from "@/api/discovery";

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

// Guard so the demo-enable side-effect only fires once per tour run even though
// onEnter is invoked on every (re)entry to step 0.
let demoEnsured = false;
async function ensureAWSDemo(): Promise<void> {
  if (demoEnsured) return;
  try {
    await enableDemoConnection();
    demoEnsured = true;
  } catch {
    // Non-fatal: if the demo connection already exists (idempotent upsert) or
    // the user is offline, the tour still narrates over whatever is present.
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
        route: "/discovery/aws",
        placement: "center",
        onEnter: ensureAWSDemo,
        title: "Instrument my cloud",
        body: "Squadron finds what in your cloud isn't sending telemetry yet, and writes the Terraform to fix it. We've loaded a sample AWS account so you can see the whole flow. Click Next to begin.",
      },
      {
        route: "/discovery/aws",
        target: '[data-tour="aws-tab-account"]',
        placement: "bottom",
        title: "1. Connect an account",
        body: "Normally you'd connect a real AWS account here with a read-only role. For the demo we've wired a sample account already — so we can skip straight to the results.",
      },
      {
        route: "/discovery/aws",
        target: '[data-tour="aws-tab-inventory"]',
        placement: "bottom",
        title: "2. Discover the inventory",
        body: "Squadron scans the account and inventories what's running — EC2, Lambda, RDS — and flags which resources are already instrumented vs. which have observability gaps. Open this tab to see the sample inventory.",
      },
      {
        route: "/discovery/aws",
        target: '[data-tour="aws-tab-recommendations"]',
        placement: "bottom",
        title: "3. Get AI recommendations",
        body: "For each gap, Squadron proposes a concrete fix — the right OTel/ADOT setup for that resource — as merge-ready Terraform. From here you'd open a pull request straight to your IaC repo. That's the loop: scan → gaps → fix, as code.",
      },
      {
        placement: "center",
        title: "That's the instrument-my-cloud loop",
        body: "Point Squadron at a real account and it does exactly this against your infrastructure. Explore the sample inventory and recommendations on your own, or pick another use case from the Use Cases page.",
      },
    ],
  },
];

export function findTour(id: string): Tour | undefined {
  return TOURS.find((t) => t.id === id);
}
