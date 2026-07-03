// Use Cases — the guided-demo landing page.
//
// A one-click "Enable demo data" control populates every feature area with
// clearly-labeled sample data, then a grid of flagship use-cases each launches
// an in-app coach-mark tour (TourHost) that showcases that feature working on
// the sample data — no real cloud, agent, or config required. New tours are
// added declaratively in components/tour/tours.ts.

import {
  ArrowRight,
  Coins,
  Database,
  FileCode,
  PlayCircle,
  Radar,
  Rocket,
  Server,
  Trash2,
  type LucideIcon,
} from "lucide-react";
import { useState } from "react";
import { useNavigate } from "react-router-dom";

import { enableDemoData, removeDemoData } from "@/api/demoData";
import { startTour, TOURS } from "@/components/tour/tours";
import { Button } from "@/components/ui/button";

// Icons referenced by tours, resolved by name (keeps tours.ts free of JSX/imports).
const ICONS: Record<string, LucideIcon> = {
  Radar,
  PlayCircle,
  Rocket,
  Coins,
  Server,
  FileCode,
};

export default function UseCasesPage() {
  const navigate = useNavigate();
  const [busy, setBusy] = useState<"enable" | "remove" | null>(null);
  const [enabled, setEnabled] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // One-click orchestration: seed the full demo scenario and land the user
  // straight on a populated page, collapsing the old two-step (enable here,
  // then go hunt for a page to look at). If the data's already loaded we skip
  // the seed and just navigate. Fleet Status is the landing target because
  // it's the app's home and the demo seeds a live fleet + cost spike there.
  const onEnableAndExplore = async () => {
    if (enabled) {
      navigate("/");
      return;
    }
    setBusy("enable");
    setError(null);
    try {
      await enableDemoData();
      setEnabled(true);
      navigate("/");
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not enable demo data.");
    } finally {
      setBusy(null);
    }
  };

  const onRemove = async () => {
    setBusy("remove");
    setError(null);
    try {
      await removeDemoData();
      setEnabled(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not remove demo data.");
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="mx-auto max-w-5xl p-6">
      <div className="mb-6">
        <h1 className="text-2xl font-semibold">Use cases</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          See how Squadron handles each job — walked through step by step on the
          real pages, running on built-in sample data. No cloud account, agent,
          or config required.
        </p>
      </div>

      {/* One-click demo data. Populates fleet, configs, a cost spike, and a
          sample cloud inventory so every feature is explorable immediately. */}
      <div className="mb-6 flex flex-col gap-3 rounded-lg border border-border bg-muted/40 p-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-start gap-3">
          <span className="mt-0.5 flex h-9 w-9 items-center justify-center rounded-md bg-primary/10 text-primary">
            <Database className="h-5 w-5" />
          </span>
          <div>
            <h2 className="text-sm font-semibold">Demo data</h2>
            <p className="text-sm text-muted-foreground">
              {enabled
                ? "Sample data is loaded across Fleet, Configs, Cost, and Discovery. Explore any page, or start a tour below."
                : "Load clearly-labeled sample data across every feature so you can explore Squadron working. Remove it any time."}
            </p>
            {error && <p className="mt-1 text-xs text-destructive">{error}</p>}
          </div>
        </div>
        <div className="flex shrink-0 gap-2">
          <Button
            onClick={onEnableAndExplore}
            disabled={busy !== null}
            size="sm"
          >
            {busy === "enable" ? (
              "Loading…"
            ) : enabled ? (
              <>
                Explore Fleet
                <ArrowRight className="ml-1.5 h-4 w-4" />
              </>
            ) : (
              <>
                <Database className="mr-1.5 h-4 w-4" />
                Enable &amp; explore
              </>
            )}
          </Button>
          <Button
            onClick={onRemove}
            disabled={busy !== null}
            size="sm"
            variant="outline"
          >
            <Trash2 className="mr-1.5 h-4 w-4" />
            {busy === "remove" ? "Removing…" : "Remove"}
          </Button>
        </div>
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
