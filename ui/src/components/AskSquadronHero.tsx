// AskSquadronHero.tsx — v0.81. Dashboard hero card surfacing the
// Ask Squadron entry with three example questions. Same posture as
// the v0.64 command palette entry and the v0.81 sidebar entry: only
// renders when AI is configured server side. Hidden gracefully when
// AI is off so a misconfigured deployment doesn't see a teaser for
// a feature that's not wired.
//
// Discoverability is the whole point. A new operator landing on the
// Dashboard for the first time sees this card and learns (a) that
// Squadron has a conversational deputy, (b) what kinds of questions
// it answers, and (c) how to open it. The example questions
// pre-fill the dialog rather than auto-submitting so the operator
// reads first, then sends.

import { ArrowRight, Sparkles } from "lucide-react";

import { useAICapabilities } from "@/api/ai";
import { dispatchAskOpen } from "@/components/AskSquadronDialog";
import { Card, CardContent } from "@/components/ui/card";

// Example questions chosen to span the four bag sources the v0.66
// + v0.68 backend wired: rollouts, audit events, cost spikes, and
// agents. Each one is a real question an operator might ask while
// triaging.
const EXAMPLES = [
  "Why is the canary rollout paused?",
  "What changed in the last hour?",
  "Anything wrong in the fleet?",
];

export function AskSquadronHero() {
  const { capabilities, loading } = useAICapabilities();
  if (loading) return null;
  if (!capabilities?.enabled) return null;

  return (
    <Card
      data-tour="ask-squadron-hero"
      className="border-violet-500/30 bg-gradient-to-br from-violet-500/5 to-transparent"
    >
      <CardContent className="p-4">
        <div className="flex items-start gap-3">
          <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-md bg-violet-500/15 text-violet-600 dark:text-violet-400">
            <Sparkles className="h-5 w-5" />
          </div>
          <div className="min-w-0 flex-1">
            <div className="flex items-center justify-between gap-2">
              <div className="text-sm font-medium">Ask Squadron</div>
              <button
                type="button"
                onClick={() => dispatchAskOpen()}
                className="inline-flex items-center gap-1 rounded border px-2 py-1 text-xs font-medium text-muted-foreground hover:bg-muted/40 hover:text-foreground"
              >
                Open
                <ArrowRight className="h-3 w-3" />
              </button>
            </div>
            <p className="mt-1 text-xs text-muted-foreground">
              Ask in plain English. The deputy answers from your rollouts, audit
              events, cost spikes, and agents — with citation chips that link
              straight to the source row.
            </p>
            <div className="mt-3 flex flex-wrap gap-1.5">
              {EXAMPLES.map((q) => (
                <button
                  key={q}
                  type="button"
                  onClick={() => dispatchAskOpen(q)}
                  className="inline-flex items-center gap-1 rounded-full border bg-muted/40 px-2.5 py-1 text-[11px] font-medium text-muted-foreground hover:bg-muted hover:text-foreground"
                >
                  {q}
                </button>
              ))}
            </div>
          </div>
        </div>
      </CardContent>
    </Card>
  );
}
