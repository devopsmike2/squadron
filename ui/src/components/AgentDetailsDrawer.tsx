import {
  CheckCircle,
  XCircle,
  AlertCircle,
  Server,
  RotateCw,
} from "lucide-react";
import { useState } from "react";
import useSWR from "swr";

import {
  decommissionAgent,
  dismissDuplicate,
  restartAgent,
} from "@/api/agents";
import { getAgentTopology } from "@/api/topology";
import { AgentConfigPipeline } from "@/components/agent-details/AgentConfigPipeline";
import { AgentLogs } from "@/components/agent-details/AgentLogs";
import { AgentMetrics } from "@/components/agent-details/AgentMetrics";
import { AgentOverview } from "@/components/agent-details/AgentOverview";
import { VolumePanel } from "@/components/insights/VolumePanel";
import { PipelineHealthAgentPanel } from "@/components/pipeline-health/PipelineHealthPanel";
import { RecommendationsPanel } from "@/components/recommendations/RecommendationsPanel";
import { CommandSnippet } from "@/components/shared/CommandSnippet";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { ErrorBoundary } from "@/components/ui/error-boundary";
import { LoadingSpinner } from "@/components/ui/loading-spinner";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { restartAgentCommand } from "@/lib/squadronctl";

interface AgentDetailsDrawerProps {
  agentId: string | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function AgentDetailsDrawer({
  agentId,
  open,
  onOpenChange,
}: AgentDetailsDrawerProps) {
  const [isRestarting, setIsRestarting] = useState(false);
  const [restartMessage, setRestartMessage] = useState<{
    type: "success" | "error";
    text: string;
  } | null>(null);
  const [isDismissing, setIsDismissing] = useState(false);

  const {
    data: agentTopology,
    isLoading,
    mutate: mutateAgent,
  } = useSWR(agentId && open ? `agent-topology-${agentId}` : null, () =>
    agentId ? getAgentTopology(agentId) : null,
  );

  const agent = agentTopology?.agent;
  const metrics = agentTopology?.metrics;
  const duplicate = agent?.suspected_duplicate_of;

  const handleDismissDuplicate = async () => {
    if (!agentId) return;
    setIsDismissing(true);
    try {
      await dismissDuplicate(agentId);
      // Re-fetch so the banner clears (the detector now skips this agent).
      await mutateAgent();
    } catch (e) {
      alert(String((e as Error).message ?? e));
    } finally {
      setIsDismissing(false);
    }
  };

  // Check if agent supports restart command
  const supportsRestart =
    agent?.capabilities?.includes("accepts_restart_command") ?? false;

  const handleRestart = async () => {
    if (!agentId) return;

    setIsRestarting(true);
    setRestartMessage(null);
    try {
      const response = await restartAgent(agentId);
      if (response.success) {
        setRestartMessage({ type: "success", text: response.message });
        // Clear message after 5 seconds
        setTimeout(() => setRestartMessage(null), 5000);
      } else {
        setRestartMessage({ type: "error", text: response.message });
      }
    } catch (error) {
      const errorMessage =
        error instanceof Error ? error.message : "Failed to restart agent";
      setRestartMessage({ type: "error", text: errorMessage });
    } finally {
      setIsRestarting(false);
    }
  };

  const getStatusIcon = (status: string) => {
    switch (status) {
      case "online":
        return (
          <CheckCircle className="h-5 w-5 text-green-600 dark:text-green-400" />
        );
      case "offline":
        return (
          <XCircle className="h-5 w-5 text-amber-600 dark:text-amber-400" />
        );
      case "error":
        return <AlertCircle className="h-5 w-5 text-red-500" />;
      default:
        return <Server className="h-5 w-5 text-gray-400" />;
    }
  };

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-full min-w[70vw] sm:max-w-2xl overflow-y-auto px-6">
        <SheetHeader>
          <div className="flex items-center justify-between">
            <div className="flex-1">
              <SheetTitle className="flex items-center gap-2">
                {agent && getStatusIcon(agent.status)}
                Agent Details
              </SheetTitle>
              <SheetDescription>{agent?.name || "Loading..."}</SheetDescription>
            </div>
            <div className="flex gap-2">
              {agent && supportsRestart && (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={handleRestart}
                  disabled={agent.status !== "online" || isRestarting}
                >
                  <RotateCw
                    className={`h-4 w-4 mr-2 ${isRestarting ? "animate-spin" : ""}`}
                  />
                  {isRestarting ? "Restarting..." : "Restart"}
                </Button>
              )}
              {agent && agentId && supportsRestart && (
                <CommandSnippet
                  variant="inline"
                  command={restartAgentCommand(agentId)}
                />
              )}
              {agent && agentId && (
                <Button
                  variant="outline"
                  size="sm"
                  title="Hard-delete this agent record. Used when the host has been physically retired from the fleet. Without this, offline agents linger forever in the inventory view."
                  onClick={async () => {
                    if (
                      !confirm(
                        `Decommission "${agent.name}"?\n\nThis hard-deletes the agent record. If the host ever checks in again via OpAMP, it'll re-register as a new agent. Telemetry already in DuckDB stays.`,
                      )
                    )
                      return;
                    try {
                      await decommissionAgent(agentId);
                      onOpenChange(false);
                    } catch (e) {
                      alert(String((e as Error).message ?? e));
                    }
                  }}
                >
                  Decommission
                </Button>
              )}
            </div>
          </div>
        </SheetHeader>

        {restartMessage && (
          <Alert
            variant={
              restartMessage.type === "error" ? "destructive" : "default"
            }
            className="mt-4"
          >
            <AlertDescription>{restartMessage.text}</AlertDescription>
          </Alert>
        )}

        {/* Backlog #5 — suspected duplicate-identity warning. Shown only when
            this telemetry-only agent looks like a phantom of an OpAMP-managed
            agent on the same host. Two outs: Decommission (the button in the
            header, for a real phantom) or Dismiss (below, for a legitimate
            separate agent). */}
        {duplicate && (
          <Alert variant="destructive" className="mt-4">
            <AlertCircle className="h-4 w-4" />
            <AlertDescription>
              <div className="font-medium">
                Likely duplicate of {duplicate.of_agent_name}
              </div>
              <div className="mt-1 text-xs opacity-90">
                {duplicate.reason}. If this is the same host, Decommission it
                (top right). If it is a legitimate separate agent, Dismiss to
                stop flagging it.
              </div>
              <div className="mt-3">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={handleDismissDuplicate}
                  disabled={isDismissing}
                >
                  {isDismissing ? "Dismissing..." : "Dismiss"}
                </Button>
              </div>
            </AlertDescription>
          </Alert>
        )}

        {isLoading ? (
          <div className="flex justify-center items-center py-8">
            <LoadingSpinner />
          </div>
        ) : agent ? (
          <ErrorBoundary
            fallback={
              <div
                role="alert"
                className="mt-6 rounded-md border border-destructive/40 bg-destructive/5 p-4 text-sm text-muted-foreground"
              >
                Failed to render agent details.
              </div>
            }
          >
            <Tabs defaultValue="overview" className="mt-6">
              <TabsList className="grid w-full grid-cols-4">
                <TabsTrigger value="overview">Overview</TabsTrigger>
                <TabsTrigger value="config">Config</TabsTrigger>
                <TabsTrigger value="metrics">Metrics</TabsTrigger>
                <TabsTrigger value="logs">Logs</TabsTrigger>
              </TabsList>

              <TabsContent value="overview" className="space-y-4 py-4">
                {/* v0.24 Volume panel sits at the top of Overview so
                  the cost/byte story is the first thing the
                  operator sees when they open the drawer. The
                  legacy AgentOverview keeps the inventory fields
                  below it. */}
                {agentId && (
                  <VolumePanel
                    mode="agent"
                    agentId={agentId}
                    window="24h"
                    compact
                  />
                )}
                {/* v0.31 pipeline health for this agent. Sits between the
                  volume panel and the recommendations so an operator
                  triaging "where's my data?" first sees throughput
                  ($ + bytes), then sees whether the pipeline is
                  healthy, then sees what to do about either. */}
                {agentId && (
                  <ErrorBoundary
                    fallback={
                      <div
                        role="alert"
                        className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-muted-foreground"
                      >
                        Failed to render self-metrics for this agent.
                      </div>
                    }
                  >
                    <PipelineHealthAgentPanel agentID={agentId} />
                  </ErrorBoundary>
                )}
                {/* v0.25 recommendations scoped to this agent. Sits
                  right under the byte panel so the "here's what to do
                  about it" story follows directly from the "here's
                  what you're spending" one. */}
                {agentId && (
                  <RecommendationsPanel
                    mode="agent"
                    agentId={agentId}
                    compact
                    limit={4}
                  />
                )}
                <AgentOverview
                  agent={agent}
                  metrics={metrics}
                  onAgentChanged={mutateAgent}
                />
              </TabsContent>

              <TabsContent value="config" className="space-y-4 py-4">
                {agentId && (
                  <AgentConfigPipeline
                    agentId={agentId}
                    effectiveConfig={agent?.effective_config}
                    agent={agent}
                    agentName={agent?.name}
                  />
                )}
              </TabsContent>

              <TabsContent value="metrics" className="space-y-4 py-4">
                {agentId && <AgentMetrics agentId={agentId} />}
              </TabsContent>

              <TabsContent value="logs" className="space-y-4 py-4">
                {agentId && <AgentLogs agentId={agentId} />}
              </TabsContent>
            </Tabs>
          </ErrorBoundary>
        ) : null}
      </SheetContent>
    </Sheet>
  );
}
