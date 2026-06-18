// ConfigLintPanel renders the live result of the server-side configlint
// engine. Sits beneath the YAML editor and updates on debounce as the
// operator types.
//
// Findings are grouped by severity (error first, then warning, then info)
// and each row is clickable to scroll the editor to the source line — see
// onJumpToLine.

import {
  AlertCircle,
  AlertTriangle,
  CheckCircle2,
  Info,
  Loader2,
  SparklesIcon,
} from "lucide-react";
import { useState } from "react";

import { remediateLint, useAICapabilities } from "@/api/ai";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useConfigLint } from "@/hooks/useConfigLint";
import type { LintFinding, LintSeverity } from "@/types/config-tools";

interface ConfigLintPanelProps {
  value: string;
  /** Called when the user clicks a finding row; jumps the editor to that line. */
  onJumpToLine?: (line: number) => void;
  /**
   * v0.44 — called with a Claude-remediated YAML when the operator
   * clicks "Auto-fix with AI". The parent applies the fixed YAML to
   * the editor (typically by setValue) and the lint refreshes on the
   * next debounce. Hidden when AI is disabled or there are no
   * actionable findings.
   */
  onRemediate?: (fixedYAML: string, summary: string) => void;
}

const severityOrder: Record<LintSeverity, number> = {
  error: 0,
  warning: 1,
  info: 2,
};

const severityIcon = (sev: LintSeverity) => {
  switch (sev) {
    case "error":
      return <AlertCircle className="h-4 w-4 text-red-600" />;
    case "warning":
      return <AlertTriangle className="h-4 w-4 text-amber-600" />;
    case "info":
      return <Info className="h-4 w-4 text-blue-600" />;
  }
};

const severityBadge = (sev: LintSeverity) => {
  const cls =
    sev === "error"
      ? "bg-red-500/10 text-red-700 border-red-500/20"
      : sev === "warning"
        ? "bg-amber-500/10 text-amber-700 border-amber-500/20"
        : "bg-blue-500/10 text-blue-700 border-blue-500/20";
  return (
    <Badge variant="outline" className={`${cls} text-[10px] uppercase`}>
      {sev}
    </Badge>
  );
};

export function ConfigLintPanel({
  value,
  onJumpToLine,
  onRemediate,
}: ConfigLintPanelProps) {
  const { findings, isLinting, error } = useConfigLint(value);
  // v0.44 — only fire the Remediate affordance when AI is enabled
  // and there's something to fix. Same pattern as the existing
  // AI-Assist dropdowns: no buttons that 503 on click.
  const { capabilities } = useAICapabilities();
  const aiEnabled = capabilities?.enabled ?? false;
  const [remediating, setRemediating] = useState(false);
  const [remediateErr, setRemediateErr] = useState<string | null>(null);

  const sorted = [...findings].sort(
    (a, b) => severityOrder[a.severity] - severityOrder[b.severity],
  );

  const errorCount = findings.filter((f) => f.severity === "error").length;
  const warningCount = findings.filter((f) => f.severity === "warning").length;
  const actionable = findings.filter(
    (f) => f.severity === "warning" || f.severity === "error",
  );

  const handleRemediate = async () => {
    if (!onRemediate || remediating || actionable.length === 0) return;
    setRemediating(true);
    setRemediateErr(null);
    try {
      const resp = await remediateLint({
        yaml: value,
        findings: actionable.map((f) => ({
          severity: f.severity as "warning" | "error",
          code: f.rule,
          message: f.message,
          path: f.path,
        })),
      });
      onRemediate(resp.fixed_yaml, resp.summary);
    } catch (e) {
      setRemediateErr(String((e as Error).message ?? e));
    } finally {
      setRemediating(false);
    }
  };

  return (
    <div className="flex flex-col border-t bg-muted/20">
      <div className="flex items-center justify-between px-4 py-2 border-b">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">Squadron Lint</span>
          {isLinting && (
            <Loader2 className="h-3 w-3 animate-spin text-muted-foreground" />
          )}
        </div>
        <div className="flex items-center gap-2">
          {findings.length === 0 && !isLinting && !error && (
            <Badge
              variant="outline"
              className="text-xs bg-emerald-500/10 text-emerald-700 border-emerald-500/20 gap-1"
            >
              <CheckCircle2 className="h-3 w-3" />
              Clean
            </Badge>
          )}
          {errorCount > 0 && (
            <Badge
              variant="outline"
              className="text-xs bg-red-500/10 text-red-700 border-red-500/20"
            >
              {errorCount} error{errorCount !== 1 ? "s" : ""}
            </Badge>
          )}
          {warningCount > 0 && (
            <Badge
              variant="outline"
              className="text-xs bg-amber-500/10 text-amber-700 border-amber-500/20"
            >
              {warningCount} warning{warningCount !== 1 ? "s" : ""}
            </Badge>
          )}
          {/* v0.44 — Auto-fix with AI. Only shows when AI is
              configured, there's something to fix, and the parent
              exposed a remediate callback to apply the result. */}
          {aiEnabled && actionable.length > 0 && onRemediate && (
            <Button
              size="sm"
              variant="outline"
              onClick={handleRemediate}
              disabled={remediating}
              className="h-7 gap-1.5 px-2 text-xs"
            >
              <SparklesIcon className="h-3 w-3 text-primary/80" />
              {remediating ? "Fixing…" : "Auto-fix with AI"}
            </Button>
          )}
        </div>
      </div>
      {remediateErr && (
        <div className="px-4 py-2 text-xs text-red-600">
          Auto-fix failed: {remediateErr}
        </div>
      )}

      {error && (
        <div className="px-4 py-2 text-xs text-red-600">
          Failed to reach lint service: {error}
        </div>
      )}

      <ul className="max-h-48 overflow-auto divide-y divide-border/60">
        {sorted.map((f, i) => (
          <li key={`${f.rule}-${i}`}>
            <button
              type="button"
              onClick={() => f.line && onJumpToLine?.(f.line)}
              className="w-full text-left px-4 py-2 hover:bg-muted/40 flex items-start gap-3"
            >
              <span className="mt-0.5 shrink-0">
                {severityIcon(f.severity)}
              </span>
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2 mb-0.5">
                  {severityBadge(f.severity)}
                  <span className="text-xs font-mono text-muted-foreground">
                    {f.rule}
                  </span>
                  {f.line ? (
                    <span className="text-xs text-muted-foreground">
                      line {f.line}
                    </span>
                  ) : null}
                </div>
                <div className="text-sm">{f.message}</div>
                {f.path && (
                  <div className="text-xs font-mono text-muted-foreground mt-0.5 truncate">
                    {f.path}
                  </div>
                )}
              </div>
            </button>
          </li>
        ))}
      </ul>

      {sorted.length === 0 && !isLinting && !error && (
        <div className="px-4 py-3 text-xs text-muted-foreground">
          No issues found. The Squadron lint engine checks for undefined
          component references, missing batch processors, memory_limiter
          ordering, and localhost exporters in containerized deployments.
        </div>
      )}
    </div>
  );
}

// Exported for use in headers/badges if a parent wants to surface counts
// outside the panel itself.
export function summarizeFindings(findings: LintFinding[]): {
  errors: number;
  warnings: number;
  infos: number;
} {
  return {
    errors: findings.filter((f) => f.severity === "error").length,
    warnings: findings.filter((f) => f.severity === "warning").length,
    infos: findings.filter((f) => f.severity === "info").length,
  };
}
