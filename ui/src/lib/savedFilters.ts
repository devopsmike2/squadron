/**
 * Saved filters — localStorage-backed named filter combinations for
 * the Agents page (and any other surface that wants the same shape).
 *
 * The premise: operators ask the same questions of the fleet every
 * day ("show me drifted prod Windows agents in the East region") but
 * have to reconstruct the filter set from scratch each time. This
 * lets them name a filter combo once and recall it with one click.
 *
 * No backend involvement — saved filters are per-browser, per-user.
 * If we ever need cross-device sync we'll move them server-side, but
 * for v0.38 the localStorage approach is the right tradeoff: zero
 * latency, no API surface to maintain, no auth headaches.
 *
 * Storage layout (single key):
 *   localStorage["squadron.savedFilters.v1"] = JSON.stringify(SavedFilter[])
 *
 * The version suffix lets us migrate the shape later without losing
 * a user's saved filters — we can write a migrator that reads the
 * old key, transforms, and writes the new one.
 *
 * Added in v0.38.0 (UX polish pass).
 */

import { useEffect, useState, useCallback } from "react";

export interface SavedFilter {
  /** UUID-ish id. Stable across renames so React keys stay stable. */
  id: string;
  /** Human-friendly name shown in the chip. */
  name: string;
  /**
   * Free-form params. The Agents page uses {drift, status, groupId,
   * search}; other pages can pick their own keys without changes
   * here. Storing as a record keeps the helper page-agnostic.
   */
  params: Record<string, string>;
  /** ISO timestamp. Used to sort newest-first in the UI. */
  created: string;
  /**
   * Page namespace. Lets multiple pages share the storage key
   * without colliding — "agents" filters don't show up in the
   * Cost Insights page filter strip.
   */
  scope: string;
}

const STORAGE_KEY = "squadron.savedFilters.v1";

function readAll(): SavedFilter[] {
  if (typeof window === "undefined") return [];
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    // Defensive: drop entries missing required keys so a hand-edited
    // localStorage doesn't crash the page render.
    return parsed.filter(
      (f) => f && typeof f.id === "string" && typeof f.name === "string",
    );
  } catch {
    return [];
  }
}

function writeAll(filters: SavedFilter[]): void {
  if (typeof window === "undefined") return;
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(filters));
  } catch {
    // Quota or disabled storage — fail silently rather than crashing
    // the UI mid-save. Worst case the user's filter doesn't persist.
  }
}

/**
 * React hook that exposes the saved filters for one page scope and
 * gives you imperative add/remove operations. Re-reads on mount so
 * stale data from another tab is picked up after a page navigation.
 */
export function useSavedFilters(scope: string) {
  const [filters, setFilters] = useState<SavedFilter[]>(() =>
    readAll().filter((f) => f.scope === scope),
  );

  // Keep the in-memory copy in sync with cross-tab updates so a
  // saved filter from tab A appears in tab B without a refresh.
  useEffect(() => {
    const onStorage = (e: StorageEvent) => {
      if (e.key !== STORAGE_KEY) return;
      setFilters(readAll().filter((f) => f.scope === scope));
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, [scope]);

  const add = useCallback(
    (name: string, params: Record<string, string>) => {
      const all = readAll();
      const next: SavedFilter = {
        id: crypto.randomUUID(),
        name: name.trim() || "Untitled filter",
        params,
        created: new Date().toISOString(),
        scope,
      };
      const updated = [...all, next];
      writeAll(updated);
      setFilters(updated.filter((f) => f.scope === scope));
      return next;
    },
    [scope],
  );

  const remove = useCallback(
    (id: string) => {
      const all = readAll();
      const updated = all.filter((f) => f.id !== id);
      writeAll(updated);
      setFilters(updated.filter((f) => f.scope === scope));
    },
    [scope],
  );

  return { filters, add, remove };
}

/**
 * Deep-equality check tailored to the SavedFilter.params shape.
 * Used to highlight a filter chip as "currently active" when the
 * page filters match it exactly.
 */
export function paramsMatch(
  a: Record<string, string>,
  b: Record<string, string>,
): boolean {
  const ak = Object.keys(a).filter((k) => a[k]);
  const bk = Object.keys(b).filter((k) => b[k]);
  if (ak.length !== bk.length) return false;
  for (const k of ak) {
    if (a[k] !== b[k]) return false;
  }
  return true;
}
