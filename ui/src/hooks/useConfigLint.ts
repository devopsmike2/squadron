// useConfigLint — debounced wrapper around POST /api/v1/configs/lint.
//
// The hook is intentionally not built on SWR: every keystroke produces a
// fresh body, so SWR's request dedupe / caching would either explode the
// cache or cache the wrong shape. A plain debounced effect with an
// AbortController on outdated requests is the right primitive.

import { useEffect, useRef, useState } from "react";

import { lintConfig } from "@/api/config-tools";

import type { LintFinding } from "@/types/config-tools";

export interface UseConfigLintResult {
  findings: LintFinding[];
  isLinting: boolean;
  error: string | null;
}

const DEFAULT_DEBOUNCE_MS = 500;

export function useConfigLint(
  value: string,
  options: { debounceMs?: number; enabled?: boolean } = {},
): UseConfigLintResult {
  const { debounceMs = DEFAULT_DEBOUNCE_MS, enabled = true } = options;
  const [findings, setFindings] = useState<LintFinding[]>([]);
  const [isLinting, setIsLinting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    if (!enabled) {
      setFindings([]);
      setIsLinting(false);
      setError(null);
      return;
    }

    // Cancel any in-flight request so we don't apply a stale response on top
    // of a newer keystroke's reply.
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    setIsLinting(true);
    const handle = window.setTimeout(async () => {
      try {
        const result = await lintConfig(value);
        if (!controller.signal.aborted) {
          setFindings(result);
          setError(null);
        }
      } catch (e) {
        if (!controller.signal.aborted) {
          setError(e instanceof Error ? e.message : "lint request failed");
        }
      } finally {
        if (!controller.signal.aborted) {
          setIsLinting(false);
        }
      }
    }, debounceMs);

    return () => {
      controller.abort();
      window.clearTimeout(handle);
    };
  }, [value, debounceMs, enabled]);

  return { findings, isLinting, error };
}
