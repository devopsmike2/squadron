/**
 * InfoTooltip — small "ⓘ" icon that reveals an explanation on hover/focus.
 *
 * Use this everywhere a fleet operator might look at a number or status
 * badge and ask "what does this mean?" Examples: the "Unknown" pipeline
 * health verdict (means we haven't seen otelcol self-metrics yet, not
 * that anything is broken), the "Missing" inventory count (means a
 * declared host hasn't checked in, not necessarily that it's down).
 *
 * The wrapper is intentionally tiny and tightly opinionated so we don't
 * end up with ten different inline help styles across the app. If you
 * need a longer or more formatted explanation, drop a <span> with whatever
 * children you want — the trigger and positioning stay consistent.
 *
 * Accessibility: Radix's Tooltip wraps the trigger in an accessible
 * button by default, but we also accept `label` so screen readers
 * announce "info: <text>" rather than reading the visual icon name.
 *
 * Added in v0.38.0 (UX polish pass).
 */

import { InfoIcon } from "lucide-react";
import * as React from "react";

import { Tooltip, TooltipContent, TooltipTrigger } from "./tooltip";

import { cn } from "@/lib/utils";

interface InfoTooltipProps {
  /** Text or rich content shown when the icon is hovered/focused. */
  children: React.ReactNode;
  /** Visually-hidden label for screen readers. Defaults to "Info". */
  label?: string;
  /**
   * Extra classes for the trigger button — usually just to nudge
   * spacing relative to whatever sits before it. The icon size is
   * fixed to keep the rhythm consistent across the app.
   */
  className?: string;
  /** Maximum width of the bubble. Default 280px reads comfortably. */
  maxWidth?: number;
  /** Side to prefer for the bubble. Radix flips if there's no room. */
  side?: "top" | "right" | "bottom" | "left";
}

export function InfoTooltip({
  children,
  label = "Info",
  className,
  maxWidth = 280,
  side = "top",
}: InfoTooltipProps) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          aria-label={label}
          className={cn(
            // No native button chrome — this should feel like a small
            // helper icon, not an interactive control with its own
            // visual weight.
            "inline-flex h-3.5 w-3.5 items-center justify-center rounded-full text-muted-foreground/60 transition-colors hover:text-foreground focus:text-foreground focus:outline-none",
            className,
          )}
        >
          <InfoIcon className="h-3 w-3" aria-hidden="true" />
        </button>
      </TooltipTrigger>
      <TooltipContent
        side={side}
        sideOffset={6}
        style={{ maxWidth: `${maxWidth}px` }}
        className="text-left leading-snug normal-case tracking-normal"
      >
        {children}
      </TooltipContent>
    </Tooltip>
  );
}
