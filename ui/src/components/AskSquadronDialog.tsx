// AskSquadronDialog — the v0.64 conversational surface.
//
// The operator opens this from the command palette (or any future
// caller) by dispatching the "squadron:ask-open" CustomEvent. The
// dialog manages its own open state; lifted state would require
// prop drilling through App.tsx for a feature that doesn't yet
// need shared parent awareness.
//
// Single turn. The operator types a question, the dialog POSTs
// /api/v1/ai/ask, renders the answer with ReactMarkdown, and
// renders one chip per citation that navigates to the cited
// resource. No history, no multi turn — that's a separate move
// per the JARVIS roadmap. Reopening the dialog resets state.

import * as DialogPrimitive from "@radix-ui/react-dialog";
import { ArrowRight, Loader2, Sparkles, X } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import { useNavigate } from "react-router-dom";

import {
  askSquadron,
  type AskCitation,
  type AskResponse,
  useAICapabilities,
} from "@/api/ai";

// askOpenEvent is the custom event the command palette dispatches
// to open this dialog without prop drilling. Exported so other
// callers (a Dashboard button, a sidebar item) can use the same
// hook.
//
// v0.81 — the event accepts an optional `prefill` detail payload
// carrying a question string. When set, the dialog opens with the
// input pre populated, letting Dashboard hero cards seed example
// questions. Empty detail (or no detail) opens the dialog with a
// fresh empty input, preserving the v0.64 behavior.
export const ASK_OPEN_EVENT = "squadron:ask-open";

// Helper for callers that want to pre-fill the input. Wraps the
// CustomEvent construction so dispatch sites read cleanly.
export function dispatchAskOpen(prefill?: string) {
  document.dispatchEvent(
    new CustomEvent(ASK_OPEN_EVENT, {
      detail: prefill ? { prefill } : undefined,
    }),
  );
}

interface InternalState {
  question: string;
  loading: boolean;
  answer: string;
  citations: AskCitation[];
  error: string | null;
}

const EMPTY: InternalState = {
  question: "",
  loading: false,
  answer: "",
  citations: [],
  error: null,
};

export function AskSquadronDialog() {
  const [open, setOpen] = useState(false);
  const [state, setState] = useState<InternalState>(EMPTY);
  const navigate = useNavigate();
  const inputRef = useRef<HTMLInputElement | null>(null);

  // Subscribe to the open event. Resetting state on open is
  // important: an operator who opened the dialog, asked a
  // question, closed it, and reopened expects a fresh slate
  // rather than a stale previous answer.
  useEffect(() => {
    const handler = (e: Event) => {
      // v0.81 — honor the optional prefill payload from
      // dispatchAskOpen so Dashboard hero cards can seed example
      // questions. Auto-submitting would be more aggressive than
      // operators want; pre-filling lets them read the question
      // first, then click Ask (or edit before sending).
      const detail = (e as CustomEvent<{ prefill?: string }>).detail;
      const prefill = detail?.prefill ?? "";
      setState({ ...EMPTY, question: prefill });
      setOpen(true);
    };
    document.addEventListener(ASK_OPEN_EVENT, handler);
    return () => document.removeEventListener(ASK_OPEN_EVENT, handler);
  }, []);

  // Autofocus the input when the dialog opens. Radix Dialog
  // handles the trap and Escape close itself, so the only piece
  // we own is the initial focus into the textbox.
  useEffect(() => {
    if (open) {
      const t = setTimeout(() => inputRef.current?.focus(), 50);
      return () => clearTimeout(t);
    }
  }, [open]);

  const submit = useCallback(async () => {
    const q = state.question.trim();
    if (!q || state.loading) return;
    setState((s) => ({ ...s, loading: true, error: null }));
    try {
      const resp: AskResponse = await askSquadron({ question: q });
      setState((s) => ({
        ...s,
        loading: false,
        answer: resp.answer,
        citations: resp.citations,
      }));
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setState((s) => ({ ...s, loading: false, error: msg }));
    }
  }, [state.question, state.loading]);

  const reset = () => setState(EMPTY);

  const onCitationClick = (c: AskCitation) => {
    setOpen(false);
    navigate(citationPath(c));
  };

  return (
    <DialogPrimitive.Root open={open} onOpenChange={setOpen}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-black/40 data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0" />
        <DialogPrimitive.Content
          aria-label="Ask Squadron"
          className="fixed left-1/2 top-[10vh] z-50 w-[92vw] max-w-2xl -translate-x-1/2 rounded-lg border bg-popover text-popover-foreground shadow-xl overflow-hidden focus:outline-none"
        >
          <div className="flex items-center justify-between gap-2 border-b px-4 py-3">
            <DialogPrimitive.Title className="flex items-center gap-2 text-sm font-medium">
              <Sparkles className="h-4 w-4 text-violet-500" />
              Ask Squadron
            </DialogPrimitive.Title>
            <DialogPrimitive.Close
              aria-label="Close"
              className="rounded p-1 text-muted-foreground hover:text-foreground hover:bg-muted/40"
            >
              <X className="h-4 w-4" />
            </DialogPrimitive.Close>
          </div>

          {/* Input row. The form swallows the enter key so the
              operator can submit by pressing Enter, which is the
              expected behavior for a chat shaped input. */}
          <form
            onSubmit={(e) => {
              e.preventDefault();
              void submit();
            }}
            className="flex items-center gap-2 border-b px-4 py-3"
          >
            <input
              ref={inputRef}
              type="text"
              value={state.question}
              onChange={(e) =>
                setState((s) => ({ ...s, question: e.target.value }))
              }
              placeholder="What's going on with the canary rollout? What changed in the last hour?"
              className="flex-1 bg-transparent text-sm outline-none placeholder:text-muted-foreground"
              maxLength={500}
              disabled={state.loading}
            />
            <button
              type="submit"
              disabled={!state.question.trim() || state.loading}
              className="inline-flex items-center gap-1 rounded border px-2 py-1 text-xs font-medium hover:bg-muted/40 disabled:opacity-50"
            >
              {state.loading ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : (
                <ArrowRight className="h-3.5 w-3.5" />
              )}
              Ask
            </button>
          </form>

          {/* Body. Three states: empty (just opened), error, or
              answer. The empty state is intentionally instructive
              — JARVIS feels mute when there's no priming. */}
          <div className="max-h-[60vh] overflow-y-auto px-4 py-3">
            {state.error && (
              <div className="rounded border border-red-500/30 bg-red-500/5 p-3 text-xs text-red-700 dark:text-red-300">
                <div className="font-medium mb-1">Ask Squadron failed</div>
                <div>{state.error}</div>
              </div>
            )}

            {!state.error && !state.answer && !state.loading && (
              <EmptyHint />
            )}

            {state.answer && (
              <>
                <div className="prose prose-sm max-w-none dark:prose-invert text-sm">
                  <ReactMarkdown>{state.answer}</ReactMarkdown>
                </div>
                {state.citations.length > 0 && (
                  <div className="mt-3 flex flex-wrap gap-1.5">
                    {state.citations.map((c) => (
                      <button
                        key={`${c.kind}:${c.id}`}
                        type="button"
                        onClick={() => onCitationClick(c)}
                        className="inline-flex items-center gap-1 rounded border bg-muted/40 px-2 py-0.5 text-[11px] font-medium text-muted-foreground hover:bg-muted hover:text-foreground"
                        title={`Open ${c.kind} ${c.id}`}
                      >
                        <span className={chipKindColor(c.kind)}>
                          {c.kind}
                        </span>
                        <span className="font-mono">{shortenID(c.id)}</span>
                      </button>
                    ))}
                  </div>
                )}
                <div className="mt-4 flex justify-end">
                  <button
                    type="button"
                    onClick={reset}
                    className="text-[11px] text-muted-foreground hover:text-foreground"
                  >
                    Ask another question
                  </button>
                </div>
              </>
            )}
          </div>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

// EmptyHint primes the operator with example questions. JARVIS
// feels useful only when the operator knows what to ask; a blank
// input is a UX dead end.
function EmptyHint() {
  const { capabilities, loading } = useAICapabilities();
  if (loading) return null;
  if (!capabilities?.enabled) {
    return (
      <div className="rounded border bg-muted/40 p-3 text-xs text-muted-foreground">
        AI assist is not configured on this Squadron deployment. Set
        <code className="mx-1 rounded bg-muted px-1 py-0.5 font-mono">
          ANTHROPIC_API_KEY
        </code>
        in the server config to enable Ask Squadron.
      </div>
    );
  }
  return (
    <div className="text-xs text-muted-foreground">
      <div className="mb-2 font-medium text-foreground">Try asking:</div>
      <ul className="space-y-1.5">
        <li>What's going on with the most recent rollout?</li>
        <li>Why is the canary paused?</li>
        <li>Who approved the last change to web prod?</li>
        <li>What was the last rollback and what did it undo?</li>
      </ul>
      <div className="mt-3 text-[11px] text-muted-foreground/80">
        Squadron answers from recent rollouts and audit events. It
        cites the rows it drew from; click a chip to open the
        source.
      </div>
    </div>
  );
}

// citationPath maps a citation to the Squadron route that surfaces
// the cited entity. v0.64 covers rollouts and audit events (the
// kinds the backend currently emits); other kinds fall through to
// a safe default so a future backend that emits "spike" or "rec"
// doesn't crash the chip.
function citationPath(c: AskCitation): string {
  switch (c.kind) {
    case "rollout":
      return `/rollouts?rollout=${encodeURIComponent(c.id)}`;
    case "audit":
      return `/audit?event=${encodeURIComponent(c.id)}`;
    case "agent":
      return `/agents?agent=${encodeURIComponent(c.id)}`;
    case "spike":
      return `/savings?spike=${encodeURIComponent(c.id)}`;
    case "rec":
      return `/cost-insights?rec=${encodeURIComponent(c.id)}`;
    default:
      return "/";
  }
}

// chipKindColor differentiates citation kinds visually so an
// operator scanning a wall of chips can spot the rollout chips
// apart from the audit chips at a glance.
function chipKindColor(kind: AskCitation["kind"]): string {
  switch (kind) {
    case "rollout":
      return "text-blue-600 dark:text-blue-400";
    case "audit":
      return "text-amber-600 dark:text-amber-400";
    case "agent":
      return "text-emerald-600 dark:text-emerald-400";
    case "spike":
      return "text-red-600 dark:text-red-400";
    case "rec":
      return "text-violet-600 dark:text-violet-400";
  }
}

// shortenID keeps long uuids from blowing out the chip width.
// Eight chars is enough to disambiguate within the small bag a
// single answer cites.
function shortenID(id: string): string {
  if (id.length <= 8) return id;
  return id.slice(0, 8) + "…";
}
