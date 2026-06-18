/**
 * Squadron logomark.
 *
 * Three nested chevrons — a fleet formation glyph. Reads as "command,
 * coordination, motion" without leaning on any particular sci-fi
 * franchise. Crisp at 16px (favicon), 24px (sidebar collapsed), and
 * scales cleanly to hero sizes (login/empty state).
 *
 * Two-tone: the outer chevron uses `--brand` (the dedicated brand
 * accent that's a touch brighter than --primary), the inner two
 * fades use opacity stops off the same color so it remains
 * monochromatic on a single-token theme switch.
 *
 * Pass a `className` to size it (e.g. "h-5 w-5"). The brand color
 * comes from the parent's `color: var(--brand)` because the SVG
 * uses currentColor; this keeps mode switching trivial.
 */

import * as React from "react";

type SquadronMarkProps = React.SVGProps<SVGSVGElement>;

export function SquadronMark({ className, ...rest }: SquadronMarkProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      className={className}
      aria-label="Squadron"
      role="img"
      // currentColor + opacity stops means a single CSS color sets
      // the whole glyph. Parents typically apply `text-brand`.
      style={{ color: "var(--brand)" }}
      {...rest}
    >
      {/* Outer chevron — leader of the formation */}
      <path
        d="M3.5 18.5L12 5L20.5 18.5L17 18.5L12 10.5L7 18.5L3.5 18.5Z"
        fill="currentColor"
      />
      {/* Mid chevron — squadron wingman, slightly transparent */}
      <path
        d="M7 21L12 13L17 21L14 21L12 17.5L10 21L7 21Z"
        fill="currentColor"
        opacity="0.65"
      />
      {/* Inner chevron — tail of the formation, most faded */}
      <path
        d="M10 22.5L12 19L14 22.5L12.7 22.5L12 21L11.3 22.5L10 22.5Z"
        fill="currentColor"
        opacity="0.35"
      />
    </svg>
  );
}

/**
 * Wordmark variant — the mark + "Squadron" with a tracked-out
 * uppercase styling. Used in the sidebar header, login hero, and
 * page <title>-style headers.
 */
export function SquadronWordmark({ className = "" }: { className?: string }) {
  return (
    <span
      className={`inline-flex items-center gap-2 font-semibold text-foreground ${className}`}
    >
      <SquadronMark className="h-5 w-5" />
      <span className="tracking-[0.18em] text-[0.9em] uppercase">Squadron</span>
    </span>
  );
}
