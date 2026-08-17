import { Check, Copy, Terminal } from "lucide-react";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

interface CommandSnippetProps {
  /** The full command string to display and copy. */
  command: string;
  /**
   * "block" renders the command in a full-width monospaced pill with a
   * Copy button beside it (used for the onboarding examples). "inline"
   * renders a compact "CLI" affordance to sit next to an existing UI
   * action without redesigning it.
   */
  variant?: "block" | "inline";
  /**
   * Label for the inline affordance. Defaults to "CLI". Pass null for an
   * icon-only button (useful in dense table rows).
   */
  label?: string | null;
  /** Accessible name override. Defaults to `Copy command: <command>`. */
  ariaLabel?: string;
  className?: string;
}

// Shared copy behavior. Falls back silently when the clipboard API is
// blocked (the command text stays selectable in the block variant).
function useCopyCommand(command: string) {
  const [copied, setCopied] = useState(false);
  const copy = async (e?: React.MouseEvent) => {
    e?.stopPropagation();
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      // clipboard blocked; nothing else to do.
    }
  };
  return { copied, copy };
}

/**
 * CommandSnippet renders a squadronctl command with a copy-to-clipboard
 * affordance. It is the reusable primitive behind both the
 * copy-as-command hints on agent/group actions and the example
 * workflow on the CLI onboarding page.
 */
export function CommandSnippet({
  command,
  variant = "block",
  label = "CLI",
  ariaLabel,
  className,
}: CommandSnippetProps) {
  const { copied, copy } = useCopyCommand(command);
  const accessibleName = ariaLabel ?? `Copy command: ${command}`;

  if (variant === "inline") {
    return (
      <Button
        type="button"
        variant="ghost"
        size="sm"
        onClick={copy}
        title={command}
        aria-label={accessibleName}
        className={cn(
          "h-7 gap-1 px-2 text-xs text-muted-foreground",
          className,
        )}
      >
        {copied ? (
          <Check className="h-3 w-3 text-green-600" aria-hidden />
        ) : (
          <Terminal className="h-3 w-3" aria-hidden />
        )}
        {label !== null && <span>{copied ? "Copied" : label}</span>}
        {/* Keep the exact command in the DOM (visually hidden) so it is
            discoverable to assistive tech and testable even when the
            button only shows an icon/short label. */}
        <span className="sr-only">{command}</span>
      </Button>
    );
  }

  return (
    <div
      className={cn(
        "flex items-center gap-2 rounded-md border bg-muted/40 p-2",
        className,
      )}
    >
      <code className="flex-1 overflow-x-auto whitespace-pre font-mono text-xs">
        {command}
      </code>
      <Button
        type="button"
        variant="ghost"
        size="sm"
        onClick={copy}
        aria-label={accessibleName}
        className="h-7 shrink-0 gap-1 px-2 text-xs"
      >
        {copied ? (
          <Check className="h-3 w-3 text-green-600" aria-hidden />
        ) : (
          <Copy className="h-3 w-3" aria-hidden />
        )}
        {copied ? "Copied" : "Copy"}
      </Button>
    </div>
  );
}
