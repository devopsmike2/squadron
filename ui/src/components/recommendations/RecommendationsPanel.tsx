/**
 * Recommendations panel — fleet or per-agent view of the v0.25
 * cost-recommendation engine output. Mounted on /cost-insights as
 * the top-of-fold callout above the existing volume panels, and as
 * a compact card inside the agent detail drawer.
 *
 * Each card supports:
 *  - "Copy snippet" → clipboard write of the suggested YAML
 *  - "Open in editor" → deeplink to /configs/new with the snippet
 *    prefilled (handled by ConfigsPage via location.state)
 *  - "Dismiss" → POST /recommendations/:id/dismiss, then SWR
 *    revalidates and the card disappears
 *
 * Severity drives a small left-border accent and a pill color, but
 * the body is otherwise calm — operators are scanning, not
 * hunting.
 */

import {
  AlertTriangleIcon,
  CheckIcon,
  CoinsIcon,
  CopyIcon,
  ExternalLinkIcon,
  EyeOffIcon,
  InfoIcon,
  Loader2Icon,
  SparklesIcon,
} from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import useSWR from "swr";

import { formatBytes } from "../insights/VolumePanel";

import { explainSnippet, useAICapabilities } from "@/api/ai";
import type { InsightsWindow } from "@/api/insights";
import {
  dismissRecommendation,
  getRecommendations,
  getRecommendationsForAgent,
  type Recommendation,
  type RecommendationSeverity,
} from "@/api/recommendations";
import { Card, CardContent } from "@/components/ui/card";

interface RecommendationsPanelProps {
  /** "fleet" mounts on Cost Insights; "agent" mounts in the drawer. */
  mode: "fleet" | "agent";
  /** Required when mode="agent". */
  agentId?: string;
  window?: InsightsWindow;
  /** Cap the rendered list. Default 6 — operators dismiss anything
   * stale, so the active list shouldn't grow unbounded. */
  limit?: number;
  /** Drawer variant tightens padding + drops the section header. */
  compact?: boolean;
}

export function RecommendationsPanel({
  mode,
  agentId,
  window = mode === "agent" ? "24h" : "1h",
  limit = 6,
  compact = false,
}: RecommendationsPanelProps) {
  const swrKey =
    mode === "fleet"
      ? `recs:fleet:${window}:${limit}`
      : agentId
        ? `recs:agent:${agentId}:${window}`
        : null;
  const { data, isLoading, error, mutate } = useSWR(
    swrKey,
    () =>
      mode === "fleet"
        ? getRecommendations(window, limit)
        : getRecommendationsForAgent(agentId!, window),
    { refreshInterval: 30000, dedupingInterval: 5000 },
  );

  const items = data?.items ?? [];

  if (error) {
    return (
      <Card>
        <CardContent
          className={`flex items-center gap-2 text-sm text-muted-foreground ${compact ? "p-3" : "p-4"}`}
        >
          <AlertTriangleIcon className="h-4 w-4" />
          Couldn't load recommendations.
        </CardContent>
      </Card>
    );
  }

  if (isLoading && items.length === 0) {
    return (
      <Card>
        <CardContent
          className={`flex items-center gap-2 text-sm text-muted-foreground ${compact ? "p-3" : "p-6"}`}
        >
          <Loader2Icon className="h-4 w-4 animate-spin" />
          Analyzing telemetry…
        </CardContent>
      </Card>
    );
  }

  if (items.length === 0) {
    return (
      <Card>
        <CardContent
          className={`flex items-center gap-2 text-sm text-muted-foreground ${compact ? "p-3" : "p-6"}`}
        >
          <CheckIcon className="h-4 w-4 text-[var(--success)]" />
          {mode === "fleet"
            ? "No active recommendations. Your fleet looks well-tuned."
            : "No recommendations for this agent right now."}
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardContent className={compact ? "p-3" : "p-4"}>
        {!compact && (
          <div className="mb-3 flex items-baseline justify-between">
            <div>
              <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
                Recommendations
              </div>
              <div className="text-sm text-muted-foreground">
                Heuristic advice from the v0.25 engine — estimates are sampled.
              </div>
            </div>
            <div className="font-tabular text-[11px] text-muted-foreground">
              {items.length} active
            </div>
          </div>
        )}
        <div className="space-y-2">
          {items.map((rec) => (
            <RecommendationRow
              key={rec.id}
              rec={rec}
              onChanged={() => mutate()}
            />
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

// ----------------------------------------------------------------
// Individual row
// ----------------------------------------------------------------

function RecommendationRow({
  rec,
  onChanged,
}: {
  rec: Recommendation;
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState<"copy" | "dismiss" | "explain" | null>(null);
  const [copied, setCopied] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [explanation, setExplanation] = useState<string | null>(null);
  const [explainError, setExplainError] = useState<string | null>(null);
  const { capabilities } = useAICapabilities();
  const aiEnabled = capabilities?.enabled === true;

  const handleExplain = async () => {
    if (busy !== null || !rec.snippet) return;
    setBusy("explain");
    setExplainError(null);
    setExpanded(true); // open the body so the explanation has somewhere to land
    try {
      const out = await explainSnippet({
        snippet: rec.snippet,
        signal: rec.signal || undefined,
        goal: rec.title,
      });
      setExplanation(out.explanation);
    } catch (err) {
      setExplainError(
        err instanceof Error ? err.message : "AI explain failed.",
      );
    } finally {
      setBusy(null);
    }
  };

  const handleCopy = async () => {
    if (!rec.snippet) return;
    setBusy("copy");
    try {
      await navigator.clipboard.writeText(rec.snippet);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      // Clipboard may be blocked in iframes / non-https. Fall back
      // silently — operators can still expand the card and copy by
      // hand from the visible <pre>.
    } finally {
      setBusy(null);
    }
  };

  const handleDismiss = async () => {
    setBusy("dismiss");
    try {
      await dismissRecommendation(rec.id);
      onChanged();
    } catch {
      // Toast would be nicer; for v0.25 a silent retry-on-next-poll
      // is acceptable. The SWR revalidate above will pull the new
      // state in.
      setBusy(null);
    }
  };

  return (
    <div
      className="relative rounded-md border border-border bg-background/40 p-3 transition-colors hover:bg-background/70"
      style={{
        // Severity stripe — left edge accent, independent of bg
        // changes on hover.
        boxShadow: `inset 3px 0 0 0 ${severityColor(rec.severity)}`,
      }}
    >
      <div className="flex items-start gap-3">
        <div className="mt-0.5 shrink-0">{severityIcon(rec.severity)}</div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-baseline gap-x-2">
            <div className="text-sm font-medium leading-snug">{rec.title}</div>
            <SeverityPill severity={rec.severity} />
            {rec.est_savings_bytes > 0 && (
              <span className="font-tabular inline-flex items-center gap-1 text-[11px] text-muted-foreground">
                <CoinsIcon className="h-3 w-3" />~{formatBytes(rec.est_savings_bytes)} / window
              </span>
            )}
            {rec.agent_id && rec.agent_name && (
              <span className="font-tabular text-[11px] text-muted-foreground">
                {rec.agent_name}
              </span>
            )}
          </div>
          {expanded && (
            <div className="mt-2 text-sm text-muted-foreground">
              {rec.detail}
              {/* v0.26: AI explanation slot. Renders above the YAML
                  so the operator sees the plain-English summary
                  before the raw snippet. Loading + error states
                  stay inline rather than toast-y. */}
              {(explanation || explainError || busy === "explain") && (
                <div
                  className="mt-3 rounded-md border border-border bg-background/40 p-2 text-sm"
                  style={{
                    borderColor:
                      "color-mix(in oklch, var(--info) 40%, transparent)",
                    background:
                      "color-mix(in oklch, var(--info) 8%, transparent)",
                  }}
                >
                  <div className="mb-1 flex items-center gap-1 text-[10px] uppercase tracking-wider text-muted-foreground">
                    <SparklesIcon className="h-3 w-3" />
                    AI explanation
                  </div>
                  {busy === "explain" && !explanation ? (
                    <div className="flex items-center gap-2 text-muted-foreground">
                      <Loader2Icon className="h-3 w-3 animate-spin" />
                      Thinking…
                    </div>
                  ) : explainError ? (
                    <div className="text-[var(--destructive)]">{explainError}</div>
                  ) : (
                    <div>{explanation}</div>
                  )}
                </div>
              )}
              {rec.snippet && (
                <pre className="mt-3 max-h-64 overflow-auto rounded-sm bg-muted/60 p-2 font-mono text-[11px] leading-snug">
                  {rec.snippet}
                </pre>
              )}
            </div>
          )}
        </div>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-1.5 text-[11px]">
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          className="rounded-md border border-border px-2 py-1 text-muted-foreground hover:bg-accent hover:text-accent-foreground"
        >
          {expanded ? "Hide details" : "Show details"}
        </button>
        {rec.snippet && (
          <>
            {/* AI: Explain — only renders when /api/v1/ai/status
                reports enabled=true. Hidden entirely when AI is
                off (rather than disabled + tooltipped) so the UI
                doesn't dangle dead controls. */}
            {aiEnabled && (
              <button
                type="button"
                onClick={handleExplain}
                disabled={busy !== null}
                className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-muted-foreground hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
                title="Ask Claude what this snippet does"
              >
                {busy === "explain" ? (
                  <Loader2Icon className="h-3 w-3 animate-spin" />
                ) : (
                  <SparklesIcon className="h-3 w-3" />
                )}
                Explain
              </button>
            )}
            <button
              type="button"
              onClick={handleCopy}
              disabled={busy !== null}
              className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-muted-foreground hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
            >
              {copied ? (
                <CheckIcon className="h-3 w-3" />
              ) : (
                <CopyIcon className="h-3 w-3" />
              )}
              {copied ? "Copied" : "Copy snippet"}
            </button>
            <Link
              to="/configs/new"
              state={{
                prefillName: prefillConfigName(rec),
                prefillSnippet: rec.snippet,
                source: "recommendation",
                recommendationId: rec.id,
              }}
              className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-muted-foreground hover:bg-accent hover:text-accent-foreground"
            >
              <ExternalLinkIcon className="h-3 w-3" />
              Open in editor
            </Link>
          </>
        )}
        <button
          type="button"
          onClick={handleDismiss}
          disabled={busy !== null}
          className="ml-auto inline-flex items-center gap-1 rounded-md border border-transparent px-2 py-1 text-muted-foreground hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
        >
          <EyeOffIcon className="h-3 w-3" />
          Dismiss
        </button>
      </div>
    </div>
  );
}

// ----------------------------------------------------------------
// Severity helpers — colors/icons/pills all routed through one
// place so a theme tweak doesn't need a multi-file change.
// ----------------------------------------------------------------

function severityColor(s: RecommendationSeverity): string {
  switch (s) {
    case "critical":
      return "var(--destructive)";
    case "warn":
      return "var(--warning)";
    case "info":
      return "var(--info)";
  }
}

function severityIcon(s: RecommendationSeverity) {
  switch (s) {
    case "critical":
      return <AlertTriangleIcon className="h-4 w-4" style={{ color: severityColor(s) }} />;
    case "warn":
      return <AlertTriangleIcon className="h-4 w-4" style={{ color: severityColor(s) }} />;
    case "info":
      return <InfoIcon className="h-4 w-4" style={{ color: severityColor(s) }} />;
  }
}

function SeverityPill({ severity }: { severity: RecommendationSeverity }) {
  const color = severityColor(severity);
  return (
    <span
      className="font-tabular rounded-md border px-1.5 py-0.5 text-[10px] uppercase tracking-wider"
      style={{
        color,
        borderColor: `color-mix(in oklch, ${color} 40%, transparent)`,
        background: `color-mix(in oklch, ${color} 12%, transparent)`,
      }}
    >
      {severity}
    </span>
  );
}

// prefillConfigName composes a useful default name from the
// recommendation context. The operator usually wants to rename
// this anyway, but a meaningful seed beats "New Config".
function prefillConfigName(rec: Recommendation): string {
  switch (rec.category) {
    case "noisy_attribute":
      return `Drop ${stripQuotes(rec.title.replace(/^Drop attribute /, "").replace(/ from .*/, ""))}`;
    case "drop_hotspot":
      return `Larger batches for ${rec.signal ?? "pipeline"}`;
    case "outlier_agent":
      return `Investigate ${rec.agent_name || rec.agent_id?.slice(0, 8)}`;
    case "empty_signal":
      return `Prune ${rec.signal} from ${rec.agent_name || "agent"}`;
    default:
      return "Recommended config";
  }
}

function stripQuotes(s: string): string {
  return s.replace(/^"|"$/g, "");
}
