// Use Cases — the guided-demo landing page.
//
// A grid of flagship use-cases; picking one launches an in-app coach-mark tour
// (TourHost) that walks the real pages, backed by real seeded data. New tours
// are added declaratively in components/tour/tours.ts.

import { PlayCircle, Radar, type LucideIcon } from "lucide-react";

import { startTour, TOURS } from "@/components/tour/tours";
import { Button } from "@/components/ui/button";

// Icons referenced by tours, resolved by name (keeps tours.ts free of JSX/imports).
const ICONS: Record<string, LucideIcon> = {
  Radar,
  PlayCircle,
};

export default function UseCasesPage() {
  return (
    <div className="mx-auto max-w-5xl p-6">
      <div className="mb-6">
        <h1 className="text-2xl font-semibold">Use cases</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Pick a use case and Squadron walks you through — step by step, on the
          real pages — how it handles that job. Each tour runs on built-in sample
          data, so you can explore the full flow without connecting anything.
        </p>
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        {TOURS.map((tour) => {
          const Icon = ICONS[tour.icon] ?? PlayCircle;
          return (
            <div
              key={tour.id}
              className="flex flex-col rounded-lg border border-border bg-card p-5"
            >
              <div className="mb-3 flex items-center gap-3">
                <span className="flex h-10 w-10 items-center justify-center rounded-md bg-primary/10 text-primary">
                  <Icon className="h-5 w-5" />
                </span>
                <div>
                  <h2 className="font-semibold leading-tight">{tour.title}</h2>
                  <span className="text-xs text-muted-foreground">
                    {tour.duration}
                  </span>
                </div>
              </div>
              <p className="flex-1 text-sm text-muted-foreground">
                {tour.blurb}
              </p>
              <div className="mt-4">
                <Button
                  size="sm"
                  onClick={() => startTour(tour.id)}
                  data-tour-start={tour.id}
                >
                  <PlayCircle className="mr-1.5 h-4 w-4" />
                  Start tour
                </Button>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
