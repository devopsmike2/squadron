/**
 * Timeline — v0.40 postmortem view.
 *
 * One horizontal time axis with a row per source (audit, deploys,
 * cost spikes). Each event renders as a circular marker at its
 * timestamp's x-position. Markers are clickable and link to the
 * source page for full details.
 *
 * Design choices:
 *
 *   - SVG-free, position-based layout. We compute each marker's
 *     left-percent from (time - since) / (until - since). No
 *     virtualization — the backend caps at 500-2000 events which
 *     fits comfortably in DOM nodes.
 *
 *   - Window selector (1h / 6h / 24h / 7d). The natural span an
 *     on-call would scrub through; longer ranges hit the audit
 *     spam ceiling and become noise.
 *
 *   - Source toggles. If you only care about Deploys + Cost Spikes
 *     during a postmortem, hide audit to focus.
 *
 *   - The marker is a button — clicking jumps to the originating
 *     page (/audit, /deploy, /savings). The page becomes a launching
 *     pad, not a terminal destination.
 *
 * Added in v0.40.0 (postmortem timeline view).
 */

import { FilterIcon } from "lucide-react";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import useSWR from "swr";

import {
  fetchTimeline,
  severityColor,
  sourceLabel,
  type TimelineEvent,
  type TimelineSource,
} from "@/api/timeline";
import { Card, CardContent } from "@/components/ui/card";
import { InfoTooltip } from "@/components/ui/info-tooltip";

// Window options. The 1h is intentionally short — postmortem zoom
// after you've narrowed the incident; 7d is the upper bound where
// audit spam still renders without visual collapse.
const WINDOW_OPTIONS: { value: string; label: string; ms: number }[] = [
  { value: "1h", label: "1 hour", ms: 60 * 60 * 1000 },
  { value: "6h", label: "6 hours", ms: 6 * 60 * 60 * 1000 },
  { value: "24h", label: "24 hours", ms: 24 * 60 * 60 * 1000 },
  { value: "7d", label: "7 days", ms: 7 * 24 * 60 * 60 * 1000 },
];

const ALL_SOURCES: TimelineSource[] = ["audit", "deploy", "cost_spike"];

// Refresh interval. The timeline is for postmortem analysis, not
// live wall-of-status; 30s polling keeps it fresh without
// hammering the merger handler.
const REFRESH_MS = 30_000;

export default function TimelinePage() {
  const [windowKey, setWindowKey] = useState("24h");
  const [activeSources, setActiveSources] = useState<TimelineSource[]>([
    ...ALL_SOURCES,
  ]);

  const win = WINDOW_OPTIONS.find((w) => w.value === windowKey)!;

  // Anchor the window to "now" at hook-call time so until doesn't
  // drift while the user is interacting. Refetches every REFRESH_MS
  // pick up the new now anchor.
  const { since, until } = useMemo(() => {
    const now = Date.now();
    return {
      since: new Date(now - win.ms).toISOString(),
      until: new Date(now).toISOString(),
    };
  }, [win, /* eslint-disable-next-line react-hooks/exhaustive-deps */ Math.floor(Date.now() / REFRESH_MS)]);

  const { data, error, isLoading } = useSWR(
    ["timeline", since, until, activeSources.join(",")],
    () =>
      fetchTimeline({
        since,
        until,
        sources: activeSources.length === ALL_SOURCES.length ? undefined : activeSources,
      }),
    { refreshInterval: REFRESH_MS },
  );

  // Group events into swimlanes by source. The map preserves the
  // canonical lane order regardless of event-arrival order.
  const grouped = useMemo(() => {
    const buckets = new Map<TimelineSource, TimelineEvent[]>();
    for (const s of ALL_SOURCES) buckets.set(s, []);
    for (const ev of data?.items ?? []) {
      buckets.get(ev.source)?.push(ev);
    }
    return buckets;
  }, [data]);

  const toggleSource = (s: TimelineSource) => {
    setActiveSources((prev) =>
      prev.includes(s) ? prev.filter((x) => x !== s) : [...prev, s],
    );
  };

  // Pre-compute the tick labels (5 evenly-spaced markers across the
  // axis) so the SVG-free layout has visual anchors. Times are
  // shown in local time — operators reading a postmortem care about
  // wall clock, not UTC.
  const ticks = useMemo(() => {
    const startMs = new Date(since).getTime();
    const endMs = new Date(until).getTime();
    const span = endMs - startMs;
    return [0, 0.25, 0.5, 0.75, 1].map((f) => ({
      pct: f * 100,
      label: new Date(startMs + f * span).toLocaleString(undefined, {
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      }),
    }));
  }, [since, until]);

  return (
    <div className="flex flex-col gap-4">
      <header className="flex items-end justify-between gap-4">
        <div>
          <div className="text-[10px] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
            Squadron
          </div>
          <h1 className="text-2xl font-semibold tracking-tight text-foreground">
            <span className="inline-flex items-center gap-1.5">
              Timeline
              <InfoTooltip label="About timeline" maxWidth={360}>
                Postmortem view. Audit events, deploys, and cost
                spikes are merged onto a single time axis so you
                can answer "what happened?" without hopping between
                pages. Click a marker to jump to its full record on
                the source page.
              </InfoTooltip>
            </span>
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            {isLoading
              ? "Loading…"
              : data
                ? `${data.count} events across ${activeSources.length} sources`
                : "No data"}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <div className="flex gap-1 rounded-md border border-border bg-card/40 p-0.5">
            {WINDOW_OPTIONS.map((o) => (
              <button
                key={o.value}
                type="button"
                onClick={() => setWindowKey(o.value)}
                className={`rounded px-2.5 py-1 text-xs transition-colors ${
                  windowKey === o.value
                    ? "bg-primary/20 text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                {o.value}
              </button>
            ))}
          </div>
        </div>
      </header>

      {/* Source toggle strip. The active state has a tinted ring so
          the operator can see at a glance which sources are
          contributing to the rendered markers. */}
      <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-card/60 px-3 py-2 backdrop-blur">
        <FilterIcon className="h-3.5 w-3.5 text-muted-foreground/70" />
        <span className="text-[10px] uppercase tracking-wider text-muted-foreground/70">
          Sources
        </span>
        {ALL_SOURCES.map((s) => {
          const active = activeSources.includes(s);
          return (
            <button
              key={s}
              type="button"
              onClick={() => toggleSource(s)}
              className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs transition-colors ${
                active
                  ? "border-primary/40 bg-primary/10 text-foreground"
                  : "border-border bg-transparent text-muted-foreground hover:bg-accent/40"
              }`}
            >
              {sourceLabel(s)}
            </button>
          );
        })}
      </div>

      {error ? (
        <Card>
          <CardContent className="p-6 text-sm text-destructive">
            Couldn't load timeline: {(error as Error).message}
          </CardContent>
        </Card>
      ) : (
        <Card>
          <CardContent className="p-4">
            {/* Tick row at top of the axis */}
            <div className="relative mb-2 h-5">
              {ticks.map((t, i) => (
                <span
                  key={i}
                  className="absolute -translate-x-1/2 font-tabular text-[10px] text-muted-foreground"
                  style={{ left: `${t.pct}%` }}
                >
                  {t.label}
                </span>
              ))}
            </div>

            {/* One swimlane per source */}
            {ALL_SOURCES.map((src) => {
              const events = grouped.get(src) ?? [];
              const visible = activeSources.includes(src);
              if (!visible) return null;
              const startMs = new Date(since).getTime();
              const endMs = new Date(until).getTime();
              const span = Math.max(endMs - startMs, 1);
              return (
                <div key={src} className="mb-4">
                  <div className="mb-1 flex items-center justify-between text-xs">
                    <span className="font-medium uppercase tracking-wider text-muted-foreground">
                      {sourceLabel(src)}
                    </span>
                    <span className="text-muted-foreground">
                      {events.length} event{events.length === 1 ? "" : "s"}
                    </span>
                  </div>
                  <div className="relative h-10 rounded border border-border/40 bg-background/40">
                    {/* Axis line through the middle of the lane */}
                    <div
                      aria-hidden
                      className="absolute left-0 right-0 top-1/2 h-px"
                      style={{
                        background:
                          "color-mix(in oklch, var(--muted-foreground) 25%, transparent)",
                      }}
                    />
                    {events.map((ev) => {
                      const ms = new Date(ev.time).getTime();
                      const pct = ((ms - startMs) / span) * 100;
                      if (pct < 0 || pct > 100) return null;
                      const marker = (
                        <span
                          className="absolute top-1/2 inline-flex h-3 w-3 -translate-x-1/2 -translate-y-1/2 items-center justify-center rounded-full border-2 transition-transform hover:scale-150"
                          style={{
                            left: `${pct}%`,
                            background: severityColor(ev.severity),
                            borderColor:
                              "color-mix(in oklch, var(--background) 70%, transparent)",
                          }}
                          title={`${ev.title}\n${ev.subtitle ?? ""}\n${new Date(ev.time).toLocaleString()}`}
                        />
                      );
                      // If the event has an href, wrap it in a Link
                      // so click navigates to the source page.
                      // Otherwise we render the marker as a plain
                      // span (no interaction surface).
                      return ev.href ? (
                        <Link
                          key={ev.id}
                          to={ev.href}
                          className="absolute"
                          style={{ left: 0, top: 0, right: 0, bottom: 0 }}
                        >
                          {marker}
                        </Link>
                      ) : (
                        <span key={ev.id}>{marker}</span>
                      );
                    })}
                  </div>
                </div>
              );
            })}

            {data && data.count === 0 && !isLoading && (
              <div className="py-12 text-center text-sm text-muted-foreground">
                No events in the selected window. Try widening the
                range or enabling more sources.
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {/* Most-recent-first event list under the swimlanes. Useful
          when the timeline is too dense to read individual markers
          — the list gives a scrollable detail view of the same
          data set. */}
      {data && data.items.length > 0 && (
        <Card>
          <CardContent className="p-0">
            <div className="border-b border-border px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted-foreground">
              Recent events
            </div>
            <div className="max-h-[400px] overflow-y-auto divide-y divide-border/40">
              {data.items.slice(0, 50).map((ev) => (
                <Link
                  key={ev.id}
                  to={ev.href ?? "#"}
                  className="flex items-center gap-3 px-4 py-2 text-sm transition-colors hover:bg-accent/40"
                >
                  <span
                    className="inline-block h-2 w-2 rounded-full"
                    style={{ background: severityColor(ev.severity) }}
                    aria-hidden
                  />
                  <span className="w-44 font-tabular text-xs text-muted-foreground">
                    {new Date(ev.time).toLocaleString()}
                  </span>
                  <span className="w-24 text-[10px] uppercase tracking-wider text-muted-foreground/70">
                    {sourceLabel(ev.source)}
                  </span>
                  <span className="flex-1 truncate text-foreground">
                    {ev.title}
                  </span>
                  {ev.subtitle && (
                    <span className="truncate text-xs text-muted-foreground">
                      {ev.subtitle}
                    </span>
                  )}
                </Link>
              ))}
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
