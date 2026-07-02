// TourHost — the in-app guided-tour engine.
//
// Mounted once at app root inside <Router> (next to CommandPalette) so it can
// useNavigate across routes and portal a coach-mark overlay over any page.
// Listens for TOUR_START_EVENT; drives the active tour's steps: runs each step's
// idempotent onEnter, navigates to its route, polls for its target element, then
// paints a spotlight + narration card. Dependency-free by design (see
// docs/design/guided-demo-tours.md).

import { ArrowLeft, ArrowRight, X } from "lucide-react";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type CSSProperties,
} from "react";
import { createPortal } from "react-dom";
import { useLocation, useNavigate } from "react-router-dom";

import { Button } from "@/components/ui/button";
import {
  findTour,
  TOUR_START_EVENT,
  type Tour,
  type TourStep,
} from "@/components/tour/tours";

const SPOTLIGHT_PAD = 8;
const CARD_WIDTH = 380;
const TARGET_POLL_MS = 3000;

export function TourHost() {
  const navigate = useNavigate();
  const location = useLocation();

  const [tour, setTour] = useState<Tour | null>(null);
  const [stepIndex, setStepIndex] = useState(0);
  const [rect, setRect] = useState<DOMRect | null>(null);
  // Bumped whenever we (re)enter a step, to cancel stale async target polls.
  const runToken = useRef(0);

  const step: TourStep | null = tour ? (tour.steps[stepIndex] ?? null) : null;

  const end = useCallback(() => {
    runToken.current += 1;
    setTour(null);
    setStepIndex(0);
    setRect(null);
  }, []);

  // Subscribe to start events.
  useEffect(() => {
    const handler = (e: Event) => {
      const id = (e as CustomEvent<{ tourId: string }>).detail?.tourId;
      const t = id ? findTour(id) : undefined;
      if (!t) return;
      setTour(t);
      setStepIndex(0);
    };
    document.addEventListener(TOUR_START_EVENT, handler);
    return () => document.removeEventListener(TOUR_START_EVENT, handler);
  }, []);

  // Drive the current step: onEnter → navigate → poll for target.
  useEffect(() => {
    if (!tour || !step) return;
    runToken.current += 1;
    const token = runToken.current;
    setRect(null);

    let raf = 0;
    const deadline = Date.now() + TARGET_POLL_MS;

    const locateTarget = () => {
      if (token !== runToken.current) return; // superseded by a newer step
      if (!step.target) return; // centered step, no spotlight
      const el = document.querySelector(step.target) as HTMLElement | null;
      if (el) {
        el.scrollIntoView({ block: "center", behavior: "smooth" });
        setRect(el.getBoundingClientRect());
        return;
      }
      if (Date.now() < deadline) {
        raf = requestAnimationFrame(locateTarget);
      }
      // Timed out → leave rect null → card renders centered (never a dead tour).
    };

    void (async () => {
      try {
        if (step.onEnter) await step.onEnter();
      } catch {
        // onEnter is best-effort; narration still proceeds.
      }
      if (token !== runToken.current) return;
      if (step.route && location.pathname !== step.route) {
        navigate(step.route);
      }
      // Give the destination a tick to mount before polling.
      raf = requestAnimationFrame(locateTarget);
    })();

    return () => cancelAnimationFrame(raf);
    // location.pathname intentionally omitted: we navigate FROM this effect and
    // don't want the resulting path change to re-run onEnter.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tour, stepIndex]);

  // Keep the spotlight glued to the target as the page scrolls/resizes.
  useEffect(() => {
    if (!step?.target) return;
    const recompute = () => {
      const el = document.querySelector(step.target!) as HTMLElement | null;
      if (el) setRect(el.getBoundingClientRect());
    };
    window.addEventListener("scroll", recompute, true);
    window.addEventListener("resize", recompute);
    return () => {
      window.removeEventListener("scroll", recompute, true);
      window.removeEventListener("resize", recompute);
    };
  }, [step]);

  if (!tour || !step) return null;

  const isLast = stepIndex === tour.steps.length - 1;
  const isFirst = stepIndex === 0;
  const next = () => (isLast ? end() : setStepIndex((i) => i + 1));
  const back = () => setStepIndex((i) => Math.max(0, i - 1));

  const card = (
    <div
      role="dialog"
      aria-label={`Guided tour: ${tour.title}`}
      className="pointer-events-auto w-[380px] max-w-[calc(100vw-24px)] rounded-lg border border-border bg-popover p-4 text-popover-foreground shadow-xl"
    >
      <div className="mb-1 flex items-center justify-between">
        <span className="text-xs font-medium text-muted-foreground">
          {tour.title} · Step {stepIndex + 1} of {tour.steps.length}
        </span>
        <button
          type="button"
          onClick={end}
          aria-label="Exit tour"
          className="rounded p-1 text-muted-foreground hover:bg-muted hover:text-foreground"
        >
          <X className="h-4 w-4" />
        </button>
      </div>
      <h3 className="text-sm font-semibold">{step.title}</h3>
      <p className="mt-1 text-sm text-muted-foreground">{step.body}</p>
      <div className="mt-4 flex items-center justify-between">
        <button
          type="button"
          onClick={end}
          className="text-xs text-muted-foreground underline-offset-2 hover:underline"
        >
          Skip tour
        </button>
        <div className="flex items-center gap-2">
          {!isFirst && (
            <Button variant="outline" size="sm" onClick={back}>
              <ArrowLeft className="mr-1 h-3.5 w-3.5" />
              Back
            </Button>
          )}
          <Button size="sm" onClick={next}>
            {isLast ? "Finish" : "Next"}
            {!isLast && <ArrowRight className="ml-1 h-3.5 w-3.5" />}
          </Button>
        </div>
      </div>
    </div>
  );

  // Positioning: with a target rect, spotlight it and anchor the card; otherwise
  // dim the screen and center the card.
  const overlay =
    rect && step.target ? (
      <div className="fixed inset-0 z-[9998]">
        {/* Spotlight: a transparent box over the target with a huge outset
            shadow that dims everything else. pointer-events:none so the real
            UI underneath stays interactive. */}
        <div
          className="pointer-events-none absolute rounded-md ring-2 ring-primary/70"
          style={{
            top: rect.top - SPOTLIGHT_PAD,
            left: rect.left - SPOTLIGHT_PAD,
            width: rect.width + SPOTLIGHT_PAD * 2,
            height: rect.height + SPOTLIGHT_PAD * 2,
            boxShadow: "0 0 0 9999px rgba(0,0,0,0.55)",
          }}
        />
        <div
          className="pointer-events-none absolute"
          style={cardAnchorStyle(rect, step.placement)}
        >
          {card}
        </div>
      </div>
    ) : (
      <div className="fixed inset-0 z-[9998] flex items-center justify-center bg-black/55 p-4">
        {card}
      </div>
    );

  return createPortal(overlay, document.body);
}

// cardAnchorStyle places the card near the spotlighted target, clamped to the
// viewport. Defaults to below the target.
function cardAnchorStyle(
  rect: DOMRect,
  placement: TourStep["placement"],
): CSSProperties {
  const gap = SPOTLIGHT_PAD + 10;
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  const clampLeft = (l: number) => Math.max(12, Math.min(l, vw - CARD_WIDTH - 12));

  switch (placement) {
    case "top":
      return { left: clampLeft(rect.left), bottom: vh - rect.top + gap };
    case "left":
      return {
        top: Math.max(12, rect.top),
        right: vw - rect.left + gap,
      };
    case "right":
      return { top: Math.max(12, rect.top), left: rect.right + gap };
    case "bottom":
    default:
      return { left: clampLeft(rect.left), top: rect.bottom + gap };
  }
}
