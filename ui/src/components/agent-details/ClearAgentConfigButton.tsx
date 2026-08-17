import { Undo2 } from "lucide-react";
import { useState } from "react";

import { clearAgentConfig } from "@/api/agents";
import { CommandSnippet } from "@/components/shared/CommandSnippet";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { clearConfigCommand } from "@/lib/squadronctl";
import type { Agent } from "@/types/agent";

interface ClearAgentConfigButtonProps {
  agent: Agent;
  /** Called after a successful clear so the parent can refetch the agent (its
   * new resolved config + drift). */
  onCleared?: () => void | Promise<unknown>;
}

/**
 * "Clear config (fall back to group)": delete this agent's OWN agent-scoped
 * config so config resolution falls back to the agent's GROUP config.
 *
 * Config resolution is agent-specific-wins (agent-scoped config beats group
 * config). A supervised agent that was auto-seeded an agent-scoped config on
 * first supervise therefore ignores any group config assigned later. Clearing
 * that agent-scoped config hands the agent back to group-level management.
 *
 * Disabled (with an explanatory tooltip) unless the agent actually has an
 * agent-scoped config to clear AND is in a group to fall back to. This deletes
 * only the agent's own config; the group config is never touched.
 */
export function ClearAgentConfigButton({
  agent,
  onCleared,
}: ClearAgentConfigButtonProps) {
  const [isClearing, setIsClearing] = useState(false);
  const [message, setMessage] = useState<{
    type: "success" | "error";
    text: string;
  } | null>(null);

  const hasAgentConfig = agent.config_intent?.source === "agent";
  const hasGroup = Boolean(agent.group_id);
  const canClear = hasAgentConfig && hasGroup;

  const title = !hasAgentConfig
    ? "This agent has no agent-scoped config to clear. It is already tracking its group config, or has no assigned config."
    : !hasGroup
      ? "This agent is in no group, so there is no group config to fall back to. Assign the agent to a group first."
      : "Delete this agent's own config so it falls back to its group's config. The group config is not changed.";

  const handleClear = async () => {
    if (!canClear) return;
    if (
      !window.confirm(
        "Clear this agent's own config so it falls back to its group config?\n\nThis deletes only the agent's agent-scoped config. The group config is not changed.",
      )
    ) {
      return;
    }

    setIsClearing(true);
    setMessage(null);
    try {
      const resp = await clearAgentConfig(agent.id);
      await onCleared?.();
      setMessage({ type: "success", text: resp.message });
      setTimeout(() => setMessage(null), 5000);
    } catch (error) {
      setMessage({
        type: "error",
        text:
          error instanceof Error
            ? error.message
            : "Failed to clear agent config",
      });
    } finally {
      setIsClearing(false);
    }
  };

  return (
    <div className="flex flex-col items-start gap-1">
      <div className="flex items-center gap-1">
        <Button
          size="sm"
          variant="outline"
          onClick={handleClear}
          disabled={!canClear || isClearing}
          title={title}
        >
          <Undo2 className="h-4 w-4 mr-2" />
          {isClearing ? "Clearing..." : "Clear config (fall back to group)"}
        </Button>
        <CommandSnippet
          variant="inline"
          command={clearConfigCommand(agent.id)}
        />
      </div>

      {message && (
        <Alert
          variant={message.type === "error" ? "destructive" : "default"}
          className="mt-1"
        >
          <AlertDescription>{message.text}</AlertDescription>
        </Alert>
      )}
    </div>
  );
}
