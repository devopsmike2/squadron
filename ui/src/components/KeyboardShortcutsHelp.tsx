// KeyboardShortcutsHelp shows a list of every global shortcut Squadron
// honors. Bound to "?" — operators press it and discover the rest. Lists
// the same shortcuts that <App /> actually wires up via useGlobalShortcut.

import * as DialogPrimitive from "@radix-ui/react-dialog";
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";

import { useGlobalShortcut } from "@/hooks/useGlobalShortcut";

interface Shortcut {
  keys: string[];
  description: string;
}

const SHORTCUTS: Shortcut[] = [
  { keys: ["⌘K", "Ctrl+K"], description: "Open command palette" },
  { keys: ["G", "A"], description: "Go to Agents" },
  { keys: ["G", "T"], description: "Go to Topology" },
  { keys: ["G", "C"], description: "Go to Configs" },
  { keys: ["G", "L"], description: "Go to Alerts" },
  { keys: ["?"], description: "Show this help" },
  { keys: ["Esc"], description: "Close the active dialog" },
];

export function KeyboardShortcutsHelp() {
  const [open, setOpen] = useState(false);
  const navigate = useNavigate();

  // "?" opens the help overlay (Shift-/ on US layout).
  useGlobalShortcut({ key: "?", shift: true }, () => setOpen(true));

  // G-then-X "leader" shortcuts: type G, then one of [a t c l] within
  // 1.5s. Common Vim-y pattern. Implemented inline because it's two
  // sequential keypresses, not a single combination.
  useEffect(() => {
    let armed = false;
    let timer: number | null = null;

    const disarm = () => {
      armed = false;
      if (timer !== null) {
        window.clearTimeout(timer);
        timer = null;
      }
    };

    const isEditableTarget = (t: EventTarget | null) => {
      if (!(t instanceof HTMLElement)) return false;
      const tag = t.tagName.toLowerCase();
      return (
        tag === "input" ||
        tag === "textarea" ||
        tag === "select" ||
        t.isContentEditable
      );
    };

    const onKey = (e: KeyboardEvent) => {
      if (isEditableTarget(e.target)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;

      if (!armed) {
        if (e.key.toLowerCase() === "g") {
          armed = true;
          timer = window.setTimeout(disarm, 1500);
        }
        return;
      }

      // We're armed — the next key resolves the leader.
      switch (e.key.toLowerCase()) {
        case "a":
          navigate("/agents");
          break;
        case "t":
          navigate("/topology");
          break;
        case "c":
          navigate("/configs");
          break;
        case "l":
          navigate("/alerts");
          break;
      }
      disarm();
    };

    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("keydown", onKey);
      disarm();
    };
  }, [navigate]);

  return (
    <DialogPrimitive.Root open={open} onOpenChange={setOpen}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-black/40 data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0" />
        <DialogPrimitive.Content
          aria-label="Keyboard shortcuts"
          className="fixed left-1/2 top-[20vh] z-50 w-[92vw] max-w-md -translate-x-1/2 rounded-lg border bg-popover text-popover-foreground shadow-xl p-6 focus:outline-none"
        >
          <DialogPrimitive.Title className="text-base font-semibold mb-3">
            Keyboard shortcuts
          </DialogPrimitive.Title>
          <ul className="space-y-2 text-sm">
            {SHORTCUTS.map((s, i) => (
              <li key={i} className="flex items-center justify-between gap-3">
                <span className="text-muted-foreground">{s.description}</span>
                <span className="flex items-center gap-1">
                  {s.keys.map((k, j) => (
                    <kbd
                      key={j}
                      className="font-mono text-xs border rounded px-1.5 py-0.5"
                    >
                      {k}
                    </kbd>
                  ))}
                </span>
              </li>
            ))}
          </ul>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}
