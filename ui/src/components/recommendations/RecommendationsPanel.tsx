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
  type RecommendationActionKind,
  type RecommendationSeverity,
  type RecommendationSourceKind,
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
  const [iacExpanded, setIacExpanded] = useState(false);
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

  // v0.85 — stubbed action handler. Slice 1 wires the typed
  // Action through to the UI but defers the actual mutation
  // dispatch (start rollout, create plan, open discovery action)
  // to a later slice. console.log + alert is the agreed slice-1
  // placeholder; the production version will branch on
  // rec.action.kind and call the appropriate mutation.
  const handleAction = () => {
    if (!rec.action) return;
    console.log("recommendation action invoked", {
      recommendation_id: rec.id,
      kind: rec.action.kind,
      payload: rec.action.payload,
    });
    window.alert(
      `Action "${actionButtonLabel(rec.action.kind)}" is wired in a later slice. ` +
        `Recommendation ${rec.id} (${rec.action.kind}).`,
    );
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
            {/* v0.85 — source kind badge. Renders next to the
                severity pill so an operator scanning the list
                can tell at a glance whether a recommendation
                came from a cost spike (JARVIS), a discovery
                scan, or an operator. Unknown / absent → no
                badge, no layout shift. */}
            {rec.source && <SourceBadge source={rec.source.kind} />}
            {rec.est_savings_bytes > 0 && (
              <span className="font-tabular inline-flex items-center gap-1 text-[11px] text-muted-foreground">
                <CoinsIcon className="h-3 w-3" />~
                {formatBytes(rec.est_savings_bytes)} / window
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
                    <div className="text-[var(--destructive)]">
                      {explainError}
                    </div>
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
              {/* v0.85 — Infrastructure-as-Code block. Present
                  for discovery-style recommendations whose
                  remediation is cloud-side (Terraform / CDK /
                  Pulumi). Reuses the same <pre> style as the
                  YAML snippet so the visual weight is
                  consistent; sits below the collector YAML
                  because the operator typically reads the
                  YAML first and reaches for IaC only when the
                  fix is cloud-side. Wrapped in its own
                  collapse because IaC snippets can be long
                  enough to overwhelm the details body. */}
              {rec.iac && (
                <div className="mt-3">
                  <button
                    type="button"
                    onClick={() => setIacExpanded((v) => !v)}
                    className="font-tabular text-[10px] uppercase tracking-wider text-muted-foreground hover:text-foreground"
                  >
                    {iacExpanded ? "Hide" : "Show"} Infrastructure-as-Code (
                    {rec.iac.format})
                  </button>
                  {iacExpanded && (
                    <pre className="mt-2 max-h-64 overflow-auto rounded-sm bg-muted/60 p-2 font-mono text-[11px] leading-snug">
                      {rec.iac.source}
                    </pre>
                  )}
                </div>
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
        {/* v0.85 — typed action button. Renders whichever label
            matches rec.action.kind. Slice 1 wires the click to a
            stub (console.log + alert) and defers the actual
            mutation dispatch to a later slice. */}
        {rec.action && (
          <button
            type="button"
            onClick={handleAction}
            disabled={busy !== null}
            className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-foreground hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
          >
            {actionButtonLabel(rec.action.kind)}
          </button>
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
      return (
        <AlertTriangleIcon
          className="h-4 w-4"
          style={{ color: severityColor(s) }}
        />
      );
    case "warn":
      return (
        <AlertTriangleIcon
          className="h-4 w-4"
          style={{ color: severityColor(s) }}
        />
      );
    case "info":
      return (
        <InfoIcon className="h-4 w-4" style={{ color: severityColor(s) }} />
      );
  }
}

// SourceBadge — v0.85 — sibling pill to SeverityPill that
// indicates where the recommendation came from. Colors are
// distinct from the severity palette so the two badges don't
// blur together: blue for cost spikes (the JARVIS arc the
// operator already knows), violet for discovery scans (the new
// universal-observation arc), neutral gray for manual. Unknown
// values fall through to the default (gray + raw kind text) so
// a future SourceKind addition doesn't require a UI deploy.
function SourceBadge({ source }: { source: RecommendationSourceKind }) {
  const label = sourceBadgeLabel(source);
  const color = sourceBadgeColor(source);
  return (
    <span
      className="font-tabular rounded-md border px-1.5 py-0.5 text-[10px] uppercase tracking-wider"
      style={{
        color,
        borderColor: `color-mix(in oklch, ${color} 40%, transparent)`,
        background: `color-mix(in oklch, ${color} 12%, transparent)`,
      }}
      title={`Source: ${source}`}
    >
      {label}
    </span>
  );
}

function sourceBadgeColor(source: RecommendationSourceKind): string {
  switch (source) {
    case "cost_spike":
      return "var(--info)"; // blue — JARVIS arc the operator already knows
    case "discovery_scan":
      // Violet for the universal-observation arc — distinct from
      // info/destructive/warning so the discovery surface reads
      // as its own neighborhood.
      return "oklch(0.62 0.18 295)";
    case "manual":
      return "var(--muted-foreground)";
  }
}

function sourceBadgeLabel(source: RecommendationSourceKind): string {
  switch (source) {
    case "cost_spike":
      return "Cost spike";
    case "discovery_scan":
      return "Discovery";
    case "manual":
      return "Manual";
  }
}

// actionButtonLabel — v0.85 — human-readable button text per
// ActionKind. Imperative voice matching the design doc's prose
// ("Start rollout", "Create plan", "View action").
function actionButtonLabel(kind: RecommendationActionKind): string {
  switch (kind) {
    case "rollout":
      return "Start rollout";
    case "plan":
      return "Create plan";
    case "discovery_action":
      return "View action";
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
