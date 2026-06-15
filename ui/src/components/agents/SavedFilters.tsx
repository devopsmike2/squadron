/**
 * SavedFilters — chip strip + "Save current" affordance for the
 * Agents page filter bar.
 *
 * Rendered ABOVE the existing filter bar so it doesn't disrupt the
 * layout of the existing controls. Each saved filter becomes a chip;
 * clicking it applies the filter, hovering it shows a remove (X)
 * affordance. The "Save current" button on the right captures
 * whatever the current filter state is and prompts for a name.
 *
 * Designed to be page-agnostic via the `scope` prop and the
 * `currentParams` / `onApply` callbacks. The Agents page wires its
 * own params; another page (Cost Insights, Audit) could reuse this
 * with a different param shape.
 *
 * Added in v0.38.0 (UX polish pass).
 */

import { BookmarkIcon, BookmarkPlusIcon, XIcon } from "lucide-react";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useSavedFilters, paramsMatch } from "@/lib/savedFilters";

interface SavedFiltersProps {
  /** Per-page namespace key. e.g. "agents" or "cost-insights". */
  scope: string;
  /** Current filter state to be saved when the user clicks Save. */
  currentParams: Record<string, string>;
  /** Called when the user clicks a saved chip. Receives the params blob. */
  onApply: (params: Record<string, string>) => void;
  /** Whether the current filter is non-default (controls Save button enabled state). */
  hasAnyFilter: boolean;
}

export function SavedFilters({
  scope,
  currentParams,
  onApply,
  hasAnyFilter,
}: SavedFiltersProps) {
  const { filters, add, remove } = useSavedFilters(scope);
  const [naming, setNaming] = useState(false);
  const [draft, setDraft] = useState("");

  const handleSave = () => {
    if (!draft.trim()) {
      setNaming(false);
      return;
    }
    add(draft, currentParams);
    setDraft("");
    setNaming(false);
  };

  // Nothing to show and nothing to save — render nothing rather than
  // an empty strip. Reduces visual noise on first-run pages.
  if (filters.length === 0 && !hasAnyFilter) {
    return null;
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      {filters.length > 0 && (
        <div className="flex items-center gap-1 text-[10px] uppercase tracking-wider text-muted-foreground/70">
          <BookmarkIcon className="h-3 w-3" />
          <span>Saved</span>
        </div>
      )}
      {filters.map((f) => {
        const isActive = paramsMatch(f.params, currentParams);
        return (
          <span
            key={f.id}
            className={`group inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs transition-colors ${
              isActive
                ? "border-primary/50 bg-primary/10 text-foreground"
                : "border-border bg-card/40 text-muted-foreground hover:bg-accent/40 hover:text-foreground"
            }`}
          >
            <button
              type="button"
              onClick={() => onApply(f.params)}
              className="truncate"
              title={
                Object.entries(f.params)
                  .filter(([, v]) => v)
                  .map(([k, v]) => `${k}=${v}`)
                  .join(", ") || "no filters"
              }
            >
              {f.name}
            </button>
            <button
              type="button"
              onClick={() => remove(f.id)}
              aria-label={`Remove ${f.name}`}
              className="opacity-0 transition-opacity group-hover:opacity-100 focus:opacity-100"
            >
              <XIcon className="h-3 w-3" />
            </button>
          </span>
        );
      })}

      {hasAnyFilter && (
        <div className="ml-auto flex items-center gap-2">
          {naming ? (
            <>
              <Input
                autoFocus
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") handleSave();
                  if (e.key === "Escape") {
                    setDraft("");
                    setNaming(false);
                  }
                }}
                placeholder="Filter name…"
                className="h-7 w-44 text-xs"
              />
              <Button
                size="sm"
                variant="default"
                onClick={handleSave}
                className="h-7 px-2 text-xs"
              >
                Save
              </Button>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => {
                  setDraft("");
                  setNaming(false);
                }}
                className="h-7 px-2 text-xs"
              >
                Cancel
              </Button>
            </>
          ) : (
            <Button
              size="sm"
              variant="outline"
              onClick={() => setNaming(true)}
              className="h-7 px-2 text-xs"
            >
              <BookmarkPlusIcon className="mr-1.5 h-3 w-3" />
              Save filter
            </Button>
          )}
        </div>
      )}
    </div>
  );
}
