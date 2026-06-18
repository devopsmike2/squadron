// RolloutPreviewPane renders the diff + lint preview for a candidate
// rollout. Used inline in the create form: once the operator has
// picked a group and entered a target config id, this fetches the
// preview and shows a Monaco diff editor so they can see exactly
// what the rollout will change before clicking Start.
//
// The pane is collapsible — large diffs add a lot of vertical space
// and operators creating multiple rollouts in a row don't always
// need to re-read the same diff. Default state is expanded since
// the whole point is "look at the diff before you ship".

import { DiffEditor } from "@monaco-editor/react";
import {
  AlertCircle,
  AlertTriangle,
  ChevronDown,
  ChevronRight,
  Info,
} from "lucide-react";
import { useEffect, useState } from "react";
import useSWR from "swr";

import { getRolloutPreview } from "@/api/rollouts";
import { Badge } from "@/components/ui/badge";
import type { LintFinding, RolloutPreview } from "@/types/rollout";

interface RolloutPreviewPaneProps {
  /** The group the rollout will target. */
  groupId: string;
  /** The target config id the operator is considering. */
  targetConfigId: string;
}

// useDebouncedValue returns `value` but only after it has been stable
// for `delayMs`. Used to keep us from hammering the preview endpoint
// on every keystroke of the target-config-id input.
function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const h = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(h);
  }, [value, delayMs]);
  return debounced;
}

export function RolloutPreviewPane({
  groupId,
  targetConfigId,
}: RolloutPreviewPaneProps) {
  // Debounce 250ms so the operator can paste a UUID without three
  // wasted requests.
  const debouncedGroup = useDebouncedValue(groupId, 250);
  const debouncedTarget = useDebouncedValue(targetConfigId, 250);
  const ready = debouncedGroup.length > 0 && debouncedTarget.length > 0;

  const key = ready ? `preview:${debouncedGroup}:${debouncedTarget}` : null;
  const { data, error, isLoading } = useSWR<RolloutPreview>(key, () =>
    getRolloutPreview(debouncedGroup, debouncedTarget),
  );

  const [expanded, setExpanded] = useState(true);

  if (!ready) {
    return (
      <div className="rounded-md border border-dashed p-3 text-xs text-muted-foreground">
        Preview will appear here once both a group and target config id are set.
      </div>
    );
  }

  if (isLoading && !data) {
    return (
      <div className="rounded-md border p-3 text-xs text-muted-foreground">
        Loading preview…
      </div>
    );
  }

  if (error) {
    return (
      <div className="rounded-md border p-3 text-xs text-red-600">
        Couldn't build the preview:{" "}
        {error instanceof Error ? error.message : String(error)}
      </div>
    );
  }

  if (!data) return null;

  const { diff, lint_findings: lintFindings, current, target } = data;
  const lintCounts = countSeverity(lintFindings);

  return (
    <div className="rounded-md border">
      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        className="w-full flex items-center justify-between gap-2 px-3 py-2 text-left hover:bg-muted/40"
      >
        <div className="flex items-center gap-2 text-sm">
          {expanded ? (
            <ChevronDown className="h-3.5 w-3.5 text-muted-foreground" />
          ) : (
            <ChevronRight className="h-3.5 w-3.5 text-muted-foreground" />
          )}
          <span className="font-medium">Preview</span>
          {/* Summary chips so a collapsed pane still tells the
              operator the headline numbers. */}
          {diff.identical ? (
            <Badge variant="outline" className="text-[10px] uppercase">
              identical
            </Badge>
          ) : (
            <span className="text-xs text-muted-foreground">
              <span className="text-emerald-700">+{diff.added}</span>
              {" / "}
              <span className="text-red-600">-{diff.removed}</span>
            </span>
          )}
          {lintCounts.error > 0 && (
            <Badge
              variant="outline"
              className="text-[10px] uppercase bg-red-500/10 text-red-700 border-red-500/20"
            >
              {lintCounts.error} lint error
              {lintCounts.error === 1 ? "" : "s"}
            </Badge>
          )}
          {lintCounts.warning > 0 && lintCounts.error === 0 && (
            <Badge
              variant="outline"
              className="text-[10px] uppercase bg-amber-500/10 text-amber-700 border-amber-500/20"
            >
              {lintCounts.warning} lint warning
              {lintCounts.warning === 1 ? "" : "s"}
            </Badge>
          )}
        </div>
      </button>

      {expanded && (
        <div className="border-t">
          {diff.identical ? (
            <div className="px-3 py-3 text-xs text-amber-700">
              Target config is identical to the group's current effective
              config. A rollout would not change anything. (You can still start
              it — for example, to re-push after manual edits — but double-check
              this is what you meant.)
            </div>
          ) : (
            <div className="h-64">
              {/* Monaco diff editor. Read-only because this is preview
                  only — the operator edits the source config elsewhere.
                  Originalmodel = current, modified = target so the
                  add/remove conventions match the diff math. */}
              <DiffEditor
                height="100%"
                language="yaml"
                theme="vs-dark"
                original={current?.content ?? ""}
                modified={target.content}
                options={{
                  readOnly: true,
                  renderSideBySide: false,
                  minimap: { enabled: false },
                  fontSize: 12,
                  scrollBeyondLastLine: false,
                }}
              />
            </div>
          )}
          {lintFindings.length > 0 && (
            <div className="border-t px-3 py-2 space-y-1">
              <div className="text-[11px] uppercase tracking-wider text-muted-foreground">
                Lint findings on target config
              </div>
              <ul className="space-y-0.5">
                {lintFindings.map((f, i) => (
                  <LintRow key={i} finding={f} />
                ))}
              </ul>
            </div>
          )}
          {!current && (
            <div className="border-t px-3 py-2 text-[11px] text-muted-foreground">
              This group has no current effective config — Squadron will treat
              the rollout as the first push and the rollback target will be
              empty.
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function LintRow({ finding }: { finding: LintFinding }) {
  const icon =
    finding.severity === "error" ? (
      <AlertCircle className="h-3.5 w-3.5 text-red-600 shrink-0 mt-0.5" />
    ) : finding.severity === "warning" ? (
      <AlertTriangle className="h-3.5 w-3.5 text-amber-600 shrink-0 mt-0.5" />
    ) : (
      <Info className="h-3.5 w-3.5 text-blue-600 shrink-0 mt-0.5" />
    );
  return (
    <li className="flex items-start gap-2 text-xs">
      {icon}
      <span className="min-w-0">
        <span className="text-foreground">{finding.message}</span>
        <span className="block text-[11px] text-muted-foreground">
          <span className="font-mono">{finding.rule}</span>
          {finding.line ? ` · line ${finding.line}` : ""}
          {finding.path ? ` · ${finding.path}` : ""}
        </span>
      </span>
    </li>
  );
}

function countSeverity(findings: LintFinding[]): {
  error: number;
  warning: number;
  info: number;
} {
  const out = { error: 0, warning: 0, info: 0 };
  for (const f of findings) {
    if (f.severity === "error") out.error++;
    else if (f.severity === "warning") out.warning++;
    else out.info++;
  }
  return out;
}
