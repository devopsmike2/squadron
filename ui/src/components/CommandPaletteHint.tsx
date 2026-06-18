/**
 * CommandPaletteHint — one-shot, dismissible toast pointing operators
 * at ⌘K.
 *
 * Squadron's command palette is one of its best features and the
 * single biggest productivity unlock once a user knows about it. But
 * the only visual hint today is the small "⌘K" letterforms in the
 * sidebar footer — invisible to anyone scanning the UI.
 *
 * This hint fires once per browser (localStorage flag) on a small
 * delay so it doesn't compete with the page's first paint, and
 * disappears automatically after 12s if the user ignores it. Clicking
 * the toast itself opens the palette directly — turning the hint
 * into a guided action rather than just a tip.
 *
 * Added in v0.38.0 (UX polish pass).
 */

import { CommandIcon, XIcon } from "lucide-react";
import { useEffect, useState } from "react";

// Bumped if we ever want to re-fire the hint after a UI overhaul.
// Format: <feature>.v<schema>. localStorage is a flat key/value so
// changing the key string is the cheapest "re-show" mechanism.
const SEEN_KEY = "squadron.hint.cmdk.v1";

// Pause before the hint appears, in ms. Long enough for first paint
// + a beat of orientation; short enough to feel responsive.
const DELAY_MS = 2_500;

// Auto-dismiss after this long if the user does nothing. Tuned so a
// distracted operator doesn't come back to a stale toast minutes
// later.
const TIMEOUT_MS = 12_000;

export function CommandPaletteHint() {
  const [open, setOpen] = useState(false);

  useEffect(() => {
    if (typeof window === "undefined") return;
    // Already seen — never fire again. Cheap localStorage read on
    // mount, no observer needed.
    if (localStorage.getItem(SEEN_KEY)) return;

    const showTimer = window.setTimeout(() => setOpen(true), DELAY_MS);
    return () => window.clearTimeout(showTimer);
  }, []);

  useEffect(() => {
    if (!open) return;
    const hideTimer = window.setTimeout(() => dismiss(), TIMEOUT_MS);
    return () => window.clearTimeout(hideTimer);
  }, [open]);

  const dismiss = () => {
    setOpen(false);
    try {
      localStorage.setItem(SEEN_KEY, new Date().toISOString());
    } catch {
      // Quota or disabled — fine. Worst case the hint reappears next
      // session, which is still better than no hint at all.
    }
  };

  const openPalette = () => {
    dismiss();
    // The CommandPalette listens for ⌘K — dispatch a synthetic
    // KeyboardEvent so we don't need to plumb a global open() handle.
    window.dispatchEvent(
      new KeyboardEvent("keydown", {
        key: "k",
        metaKey: true,
        ctrlKey: true,
        bubbles: true,
      }),
    );
  };

  if (!open) return null;

  return (
    <div
      className="fixed bottom-6 right-6 z-50 animate-in slide-in-from-bottom-3 fade-in-0"
      role="status"
      aria-live="polite"
    >
      <div className="flex items-center gap-3 rounded-lg border border-border bg-card/95 px-3 py-2.5 shadow-lg backdrop-blur">
        <div className="flex h-7 w-7 items-center justify-center rounded-md bg-primary/15 text-primary">
          <CommandIcon className="h-3.5 w-3.5" />
        </div>
        <div className="space-y-0.5">
          <p className="text-sm font-medium text-foreground">
            Press{" "}
            <kbd className="rounded border border-border bg-card px-1.5 py-0.5 text-[10px] font-tabular">
              ⌘K
            </kbd>{" "}
            to jump anywhere
          </p>
          <p className="text-[11px] text-muted-foreground">
            Navigate, search agents, run common actions — all from the keyboard.
          </p>
        </div>
        <button
          type="button"
          onClick={openPalette}
          className="ml-2 rounded-md bg-primary px-2.5 py-1.5 text-xs font-medium text-primary-foreground transition-colors hover:bg-primary/90"
        >
          Try it
        </button>
        <button
          type="button"
          onClick={dismiss}
          aria-label="Dismiss hint"
          className="rounded-md p-1 text-muted-foreground/70 transition-colors hover:bg-accent/50 hover:text-foreground"
        >
          <XIcon className="h-3.5 w-3.5" />
        </button>
      </div>
    </div>
  );
}
