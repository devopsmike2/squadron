/**
 * ChangeWindowsModal — v0.49 change-window editor.
 *
 * Opens from the Groups page Policy column and lets the operator
 * add / edit / remove recurring blackout windows on an existing
 * group. Each window has:
 *   - Name (human label for the badge: "summer peak", "Q4 freeze")
 *   - Days of the week (checkbox row, empty = every day)
 *   - Local start + end times in HH:MM
 *   - IANA timezone (default America/Chicago — Southern Company's grid)
 *   - Optional effective-from / -to range for one-off blackouts
 *
 * On save: PUT /api/v1/groups/:id with the full windows list.
 * Backend revalidates each window and rejects the whole payload
 * on first error (UX-wise: operator fixes the row, retries).
 *
 * The rollout engine reads the persisted windows on every tick and
 * refuses to advance rollouts while any window is active; the rollout
 * card surfaces this via the LastBlackoutReason field.
 */

import { Plus, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";

import { updateGroup, type ChangeWindow, type Group } from "@/api/groups";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

interface ChangeWindowsModalProps {
  group: Group | null;
  open: boolean;
  onClose: () => void;
  onSaved: () => void;
}

// Default for new windows. America/Chicago because Southern Company's
// grid is on Central time — easy to change per row but a sensible
// starting point reduces clicks for the common case.
const NEW_WINDOW: ChangeWindow = {
  name: "",
  days_of_week: [1, 2, 3, 4, 5],
  start_local: "16:00",
  end_local: "21:00",
  timezone: "America/Chicago",
};

const DOW_LABELS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

export function ChangeWindowsModal({
  group,
  open,
  onClose,
  onSaved,
}: ChangeWindowsModalProps) {
  const [windows, setWindows] = useState<ChangeWindow[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Reset to the current group's windows whenever the modal opens
  // with a different group. Using useEffect (not useMemo) so saving
  // doesn't fight with the snapshot.
  useEffect(() => {
    if (open && group) {
      setWindows(group.change_windows ?? []);
      setError(null);
    }
  }, [open, group]);

  const toggleDow = (windowIdx: number, dow: number) => {
    setWindows((prev) => {
      const next = [...prev];
      const w = { ...next[windowIdx] };
      const days = new Set(w.days_of_week ?? []);
      if (days.has(dow)) days.delete(dow);
      else days.add(dow);
      w.days_of_week = Array.from(days).sort((a, b) => a - b);
      next[windowIdx] = w;
      return next;
    });
  };

  const updateField = <K extends keyof ChangeWindow>(
    idx: number,
    field: K,
    value: ChangeWindow[K],
  ) => {
    setWindows((prev) => {
      const next = [...prev];
      next[idx] = { ...next[idx], [field]: value };
      return next;
    });
  };

  const addWindow = () => {
    setWindows((prev) => [...prev, { ...NEW_WINDOW }]);
  };

  const removeWindow = (idx: number) => {
    setWindows((prev) => prev.filter((_, i) => i !== idx));
  };

  const save = async () => {
    if (!group) return;
    setSaving(true);
    setError(null);
    try {
      await updateGroup(group.id, { change_windows: windows });
      onSaved();
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : "save failed");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>
            Change windows — {group?.name ?? "(loading…)"}
          </DialogTitle>
          <DialogDescription>
            Rollouts to this group won't advance while a window is active. Use
            these for peak-demand hours, storm-response windows, or quarterly
            freezes. Add an effective date range to restrict a window to a
            one-off period.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3 max-h-[60vh] overflow-y-auto py-2">
          {windows.length === 0 && (
            <div className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
              No blackout windows. Click below to add one.
            </div>
          )}
          {windows.map((w, i) => (
            <div
              key={i}
              className="rounded-md border border-orange-500/20 bg-orange-500/5 p-3 space-y-2"
            >
              <div className="flex items-start justify-between gap-2">
                <div className="flex-1">
                  <Label className="text-[11px] uppercase tracking-wider text-muted-foreground">
                    Window {i + 1}
                  </Label>
                  <Input
                    value={w.name}
                    onChange={(e) => updateField(i, "name", e.target.value)}
                    placeholder="e.g. summer peak demand"
                    className="mt-0.5"
                  />
                </div>
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={() => removeWindow(i)}
                  title="Remove window"
                >
                  <Trash2 className="h-4 w-4" />
                </Button>
              </div>

              <div>
                <Label className="text-xs">Days of week</Label>
                <div className="mt-1 flex gap-1">
                  {DOW_LABELS.map((label, dow) => {
                    const active = (w.days_of_week ?? []).includes(dow);
                    return (
                      <button
                        key={dow}
                        type="button"
                        onClick={() => toggleDow(i, dow)}
                        className={`flex-1 rounded border px-1 py-0.5 text-[11px] transition-colors ${
                          active
                            ? "border-orange-500/40 bg-orange-500/15 text-orange-700"
                            : "border-border bg-background text-muted-foreground hover:bg-muted"
                        }`}
                      >
                        {label}
                      </button>
                    );
                  })}
                </div>
                <p className="mt-0.5 text-[10px] text-muted-foreground">
                  No days selected = window applies every day.
                </p>
              </div>

              <div className="grid grid-cols-3 gap-2">
                <div>
                  <Label className="text-xs">Start (local)</Label>
                  <Input
                    type="time"
                    value={w.start_local}
                    onChange={(e) =>
                      updateField(i, "start_local", e.target.value)
                    }
                    className="mt-0.5"
                  />
                </div>
                <div>
                  <Label className="text-xs">End (local)</Label>
                  <Input
                    type="time"
                    value={w.end_local}
                    onChange={(e) =>
                      updateField(i, "end_local", e.target.value)
                    }
                    className="mt-0.5"
                  />
                </div>
                <div>
                  <Label className="text-xs">Timezone</Label>
                  <Input
                    value={w.timezone}
                    onChange={(e) => updateField(i, "timezone", e.target.value)}
                    placeholder="America/Chicago"
                    className="mt-0.5 font-mono text-xs"
                  />
                </div>
              </div>
              <p className="text-[10px] text-muted-foreground">
                If end ≤ start, the window wraps past midnight (e.g. 22:00 to
                06:00).
              </p>

              <div className="grid grid-cols-2 gap-2">
                <div>
                  <Label className="text-xs">
                    Effective from{" "}
                    <span className="text-muted-foreground">(optional)</span>
                  </Label>
                  <Input
                    value={w.effective_from ?? ""}
                    onChange={(e) =>
                      updateField(
                        i,
                        "effective_from",
                        e.target.value || undefined,
                      )
                    }
                    placeholder="2026-11-15T00:00:00Z"
                    className="mt-0.5 font-mono text-[11px]"
                  />
                </div>
                <div>
                  <Label className="text-xs">
                    Effective to{" "}
                    <span className="text-muted-foreground">(optional)</span>
                  </Label>
                  <Input
                    value={w.effective_to ?? ""}
                    onChange={(e) =>
                      updateField(
                        i,
                        "effective_to",
                        e.target.value || undefined,
                      )
                    }
                    placeholder="2027-01-05T23:59:59Z"
                    className="mt-0.5 font-mono text-[11px]"
                  />
                </div>
              </div>
            </div>
          ))}

          <Button
            variant="outline"
            size="sm"
            onClick={addWindow}
            className="gap-1"
          >
            <Plus className="h-3.5 w-3.5" />
            Add window
          </Button>
        </div>

        {error && <div className="text-xs text-destructive">{error}</div>}

        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button onClick={save} disabled={saving}>
            {saving ? "Saving…" : "Save windows"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
