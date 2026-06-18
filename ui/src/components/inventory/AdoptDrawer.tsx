/**
 * AdoptDrawer — per-host adoption snippet viewer.
 *
 * The v0.45 answer to "we discovered this host but pushing a uniform
 * config would clobber its custom log paths." Instead of generating
 * a full collector config, Squadron returns just the OpAMP extension
 * snippet (with host.name + labels baked in) that the operator can
 * merge into the agent's EXISTING config. Receivers, processors,
 * exporters, and pipelines stay exactly as the operator had them.
 *
 * The operator workflow:
 *   1. Click Adopt on a missing host row
 *   2. This drawer opens with the host-specific snippet
 *   3. Copy → SSH to the host → edit the agent's config → paste
 *   4. Restart the collector — Squadron sees it within seconds
 *
 * Added in v0.45.0.
 */

import { CheckIcon, CopyIcon } from "lucide-react";
import { useEffect, useState } from "react";
import useSWR from "swr";

import { getAdoptionSnippet } from "@/api/quickstart";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";

interface AdoptDrawerProps {
  open: boolean;
  onClose: () => void;
  /** Hostname of the missing host being adopted. */
  hostname: string;
  /**
   * Labels to bake into the snippet's agent_description block.
   * Typically pulled from the inventory row's metadata (env,
   * region, source pipeline, etc.).
   */
  labels?: Record<string, string>;
}

export function AdoptDrawer({
  open,
  onClose,
  hostname,
  labels,
}: AdoptDrawerProps) {
  const [copied, setCopied] = useState(false);

  // Fetch the per-host snippet. SWR key includes hostname + label
  // hash so the cache is correctly per-target.
  const labelKey = labels
    ? Object.entries(labels)
        .map(([k, v]) => `${k}=${v}`)
        .sort()
        .join(",")
    : "";
  const { data, error, isLoading } = useSWR(
    open ? ["adoption", hostname, labelKey] : null,
    () => getAdoptionSnippet({ hostname, labels }),
  );

  // Reset the "Copied!" affordance every time the drawer reopens —
  // otherwise a closed-then-reopened drawer might still show the
  // green checkmark from the last session.
  useEffect(() => {
    if (open) setCopied(false);
  }, [open]);

  const copy = async () => {
    if (!data?.yaml) return;
    try {
      await navigator.clipboard.writeText(data.yaml);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard write can fail in older Safari / WebViews — fall
      // back to a select-all hint via the textarea below.
    }
  };

  return (
    <Sheet open={open} onOpenChange={(o) => !o && onClose()}>
      <SheetContent className="w-[640px] sm:max-w-[640px]">
        <SheetHeader className="flex-shrink-0">
          <SheetTitle>Adopt {hostname}</SheetTitle>
          <SheetDescription>
            Paste this snippet into the agent's existing collector config to
            bring it under Squadron management. Your receivers, processors,
            exporters, and pipelines are{" "}
            <span className="font-medium text-foreground">not touched</span> —
            only the OpAMP extension is added.
          </SheetDescription>
        </SheetHeader>
        <div className="flex-1 space-y-3 overflow-y-auto p-4">
          {/* 3-step quick reference. Operators paste this snippet
              into 10s of hosts; the step list keeps the workflow
              tight when they're alt-tabbing between SSH and the UI. */}
          <ol className="ml-4 list-decimal space-y-1 text-xs text-muted-foreground">
            <li>Copy the YAML below.</li>
            <li>
              SSH to{" "}
              <code className="rounded bg-muted/40 px-1">{hostname}</code> and
              open the agent's collector config (commonly{" "}
              <code className="rounded bg-muted/40 px-1">
                /etc/otelcol-contrib/config.yaml
              </code>
              ).
            </li>
            <li>
              Merge in. If your config already has{" "}
              <code className="rounded bg-muted/40 px-1">extensions:</code> or{" "}
              <code className="rounded bg-muted/40 px-1">
                service.extensions:
              </code>
              , append rather than replace.
            </li>
            <li>
              Restart the collector. Squadron picks up the connection within
              seconds.
            </li>
          </ol>

          {isLoading && (
            <div className="text-xs text-muted-foreground">
              Generating snippet…
            </div>
          )}
          {error && (
            <div className="text-xs text-destructive">
              Couldn't fetch the snippet:{" "}
              {String((error as Error).message ?? error)}
            </div>
          )}
          {data && (
            <>
              <div className="flex items-center justify-between">
                <span className="text-[10px] uppercase tracking-wider text-muted-foreground">
                  Snippet
                </span>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={copy}
                  className="h-7 gap-1.5 px-2 text-xs"
                >
                  {copied ? (
                    <>
                      <CheckIcon className="h-3 w-3 text-emerald-600" />
                      Copied
                    </>
                  ) : (
                    <>
                      <CopyIcon className="h-3 w-3" />
                      Copy
                    </>
                  )}
                </Button>
              </div>
              <pre className="overflow-auto rounded border border-border/60 bg-muted/30 p-3 font-mono text-[11px] leading-relaxed text-foreground">
                {data.yaml}
              </pre>
              <div className="text-[11px] text-muted-foreground">
                OpAMP endpoint:{" "}
                <code className="rounded bg-muted/40 px-1">
                  {data.opamp_server_url}
                </code>
              </div>
            </>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}
