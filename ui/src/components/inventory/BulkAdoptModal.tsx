/**
 * BulkAdoptModal — v0.46 bulk adoption.
 *
 * Fires the configured adoption pipeline against a selected subset
 * of missing hosts. Squadron generates one per-host snippet and
 * packs them all into the workflow's adoption_payload input; the
 * customer's pipeline reads that JSON and applies each snippet to
 * the matching host.
 *
 * Unlike the per-host AdoptDrawer (which is a paste-into-the-SSH-
 * session affordance), this modal is for fleets of dozens or
 * hundreds where doing it manually doesn't scale.
 *
 * Added in v0.46.0.
 */

import { useMemo, useState } from "react";
import useSWR from "swr";

import {
  adoptDeployRun,
  listDeployTargets,
  type DeployTarget,
} from "@/api/deploy";
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

interface BulkAdoptModalProps {
  open: boolean;
  onClose: () => void;
  /** Set of hostnames the parent has pre-selected (typically missing rows). */
  candidates: string[];
  /** Optional onSuccess so the parent can refresh the inventory after fire. */
  onSuccess?: (runID: string) => void;
}

export function BulkAdoptModal({
  open,
  onClose,
  candidates,
  onSuccess,
}: BulkAdoptModalProps) {
  const targetsQ = useSWR(open ? "deploy-targets-for-adopt" : null, () =>
    listDeployTargets(),
  );
  // Track which candidates the user has selected. Default: all on,
  // since the parent already filtered down to actionable rows.
  const [selected, setSelected] = useState<Set<string>>(
    () => new Set(candidates),
  );
  const [targetID, setTargetID] = useState<string>("");
  const [opampURL, setOpampURL] = useState<string>("");
  const [notes, setNotes] = useState<string>("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Re-default selection when candidates change (e.g. modal reopened
  // with a different filter).
  useMemo(() => {
    setSelected(new Set(candidates));
  }, [candidates]);

  // Default the target to the first one with a non-empty PAT once
  // the list loads.
  useMemo(() => {
    if (targetID) return;
    const t = targetsQ.data?.items?.find((t: DeployTarget) => t.has_credential);
    if (t) setTargetID(t.id);
  }, [targetsQ.data, targetID]);

  const toggle = (h: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(h)) next.delete(h);
      else next.add(h);
      return next;
    });
  };

  const submit = async () => {
    if (!targetID || selected.size === 0) return;
    setBusy(true);
    setError(null);
    try {
      const run = await adoptDeployRun(targetID, {
        hostnames: Array.from(selected),
        notes: notes.trim() || undefined,
        opamp_server_url: opampURL.trim() || undefined,
      });
      onSuccess?.(run.id);
      onClose();
    } catch (e) {
      setError(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>Bulk adopt via deploy pipeline</DialogTitle>
          <DialogDescription>
            Squadron generates one OpAMP-extension snippet per host (each with
            that host's identity baked in) and fires the adoption pipeline with
            the batch payload. The pipeline applies each snippet to the matching
            host's <span className="font-medium">existing</span> collector
            config — receivers, processors, exporters, and pipelines are
            preserved.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 py-2">
          <div>
            <Label htmlFor="adopt-target">Adoption pipeline</Label>
            <select
              id="adopt-target"
              value={targetID}
              onChange={(e) => setTargetID(e.target.value)}
              className="mt-1 w-full rounded-md border border-border bg-background px-3 py-2 text-sm"
              disabled={!targetsQ.data}
            >
              <option value="">Select a deploy target…</option>
              {(targetsQ.data?.items ?? []).map((t: DeployTarget) => (
                <option key={t.id} value={t.id} disabled={!t.has_credential}>
                  {t.name}
                  {!t.has_credential ? " (no credential)" : ""}
                </option>
              ))}
            </select>
            <p className="mt-1 text-[11px] text-muted-foreground">
              The pipeline must accept an <code>adoption_payload</code> input
              and an <code>adoption_mode</code> flag. See the reference workflow
              in <code>deploy/test/adoption.yml</code>.
            </p>
          </div>

          <div>
            <Label htmlFor="opamp-url">
              OpAMP server URL{" "}
              <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="opamp-url"
              value={opampURL}
              onChange={(e) => setOpampURL(e.target.value)}
              placeholder="ws://squadron.your-co.com:4330/v1/opamp"
              className="mt-1 font-mono text-xs"
            />
            <p className="mt-1 text-[11px] text-muted-foreground">
              The URL baked into each snippet. Leave blank for the auto-detected
              dev URL; set explicitly when the CI runner can't reach localhost.
            </p>
          </div>

          <div>
            <div className="mb-1 flex items-center justify-between">
              <Label>
                Hosts ({selected.size} of {candidates.length} selected)
              </Label>
              <div className="flex gap-1">
                <button
                  type="button"
                  onClick={() => setSelected(new Set(candidates))}
                  className="text-[11px] text-primary hover:underline"
                >
                  All
                </button>
                <span className="text-[11px] text-muted-foreground">·</span>
                <button
                  type="button"
                  onClick={() => setSelected(new Set())}
                  className="text-[11px] text-primary hover:underline"
                >
                  None
                </button>
              </div>
            </div>
            <div className="max-h-48 overflow-auto rounded border border-border/60 bg-muted/20">
              {candidates.length === 0 ? (
                <div className="p-3 text-xs text-muted-foreground">
                  No missing hosts to adopt.
                </div>
              ) : (
                <ul className="divide-y divide-border/40">
                  {candidates.map((h) => (
                    <li key={h} className="flex items-center gap-2 px-3 py-1.5">
                      <input
                        type="checkbox"
                        id={`adopt-${h}`}
                        checked={selected.has(h)}
                        onChange={() => toggle(h)}
                        className="h-3.5 w-3.5"
                      />
                      <label
                        htmlFor={`adopt-${h}`}
                        className="cursor-pointer font-mono text-xs"
                      >
                        {h}
                      </label>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </div>

          <div>
            <Label htmlFor="adopt-notes">
              Notes <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="adopt-notes"
              value={notes}
              onChange={(e) => setNotes(e.target.value)}
              placeholder="Backfill OpAMP extension on prod east-1"
              className="mt-1 text-sm"
            />
          </div>

          {error && <div className="text-xs text-destructive">{error}</div>}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            onClick={submit}
            disabled={busy || !targetID || selected.size === 0}
          >
            {busy
              ? "Dispatching…"
              : `Adopt ${selected.size} host${selected.size === 1 ? "" : "s"}`}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
