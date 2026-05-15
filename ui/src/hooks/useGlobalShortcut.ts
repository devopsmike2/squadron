// useGlobalShortcut — register a global key handler that fires when the
// matching combination is pressed, regardless of where focus is. Skips
// firing when focus is inside an editable element so it doesn't steal
// keystrokes from form inputs and the Monaco editor.

import { useEffect, useRef } from "react";

export interface ShortcutSpec {
  /** The character to match (case-insensitive). */
  key: string;
  /** When true, requires the platform meta key (⌘ on macOS, Ctrl elsewhere). */
  mod?: boolean;
  /** When true, requires Shift to be held. */
  shift?: boolean;
}

export function useGlobalShortcut(
  spec: ShortcutSpec,
  handler: () => void,
  enabled: boolean = true,
): void {
  const handlerRef = useRef(handler);
  handlerRef.current = handler;

  useEffect(() => {
    if (!enabled) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key.toLowerCase() !== spec.key.toLowerCase()) return;
      if (Boolean(spec.mod) !== (e.metaKey || e.ctrlKey)) return;
      if (Boolean(spec.shift) !== e.shiftKey) return;
      if (isEditable(e.target)) return;
      e.preventDefault();
      handlerRef.current();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [spec.key, spec.mod, spec.shift, enabled]);
}

// isEditable reports whether the given event target is an editable element
// where global shortcuts should NOT trigger. Covers <input>, <textarea>,
// <select>, and any [contenteditable].
function isEditable(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  const tag = target.tagName.toLowerCase();
  if (tag === "input" || tag === "textarea" || tag === "select") return true;
  if (target.isContentEditable) return true;
  return false;
}
