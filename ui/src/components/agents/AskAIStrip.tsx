/**
 * AskAIStrip — natural-language fleet query bar.
 *
 * Sits above the SavedFilters strip on /agents. The operator types
 * "show me prod agents that haven't checked in for an hour", the
 * model translates that to filter params, and the strip both
 * applies them and surfaces a one-line explanation so the operator
 * sees what the AI thought they meant.
 *
 * Hides entirely when AI is disabled (no Anthropic key) — same
 * pattern as the v0.26 AI Assist dropdowns. No buttons that 503
 * on click.
 *
 * Added in v0.44.0 (AI features).
 */

import { SparklesIcon } from "lucide-react";
import { useState } from "react";

import {
  translateFleetQuery,
  useAICapabilities,
  type FleetQueryResponse,
} from "@/api/ai";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

interface AskAIStripProps {
  /** Available label keys to pass as grounding hint. */
  labelKeys?: string[];
  /** Group names for the schema hint. */
  groupNames?: string[];
  /** Apply the resolved filter params to the page state. */
  onApply: (params: FleetQueryResponse) => void;
}

export function AskAIStrip({
  labelKeys,
  groupNames,
  onApply,
}: AskAIStripProps) {
  const { capabilities, loading } = useAICapabilities();
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState(false);
  const [explanation, setExplanation] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Hide entirely when AI is disabled or while we're still probing
  // — better than flickering a button that vanishes a beat later.
  if (loading || !capabilities?.enabled) return null;

  const submit = async () => {
    if (!query.trim() || busy) return;
    setBusy(true);
    setError(null);
    try {
      const resp = await translateFleetQuery({
        query,
        schema: { label_keys: labelKeys, groups: groupNames },
      });
      setExplanation(resp.explanation);
      onApply(resp);
    } catch (e) {
      setError(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-1.5">
      <div className="flex items-center gap-2 rounded-lg border border-border bg-card/60 px-3 py-2 backdrop-blur">
        <SparklesIcon className="h-3.5 w-3.5 text-primary/80" />
        <span className="text-[10px] uppercase tracking-wider text-muted-foreground/70">
          Ask AI
        </span>
        <Input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") submit();
          }}
          placeholder="Show me prod agents that haven't checked in for an hour…"
          className="h-7 flex-1 text-sm"
        />
        <Button
          size="sm"
          variant="default"
          onClick={submit}
          disabled={busy || !query.trim()}
          className="h-7 px-3 text-xs"
        >
          {busy ? "Thinking…" : "Ask"}
        </Button>
      </div>
      {explanation && (
        <div className="px-3 text-[11px] text-muted-foreground">
          <span className="font-medium text-foreground">Understood as:</span>{" "}
          {explanation}
        </div>
      )}
      {error && (
        <div className="px-3 text-[11px] text-destructive">{error}</div>
      )}
    </div>
  );
}
