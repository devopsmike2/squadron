import type { Agent } from "@/types/agent";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { InfoCard } from "@/components/ui/info-card";

interface Metrics {
  metric_count: number;
  log_count: number;
  trace_count: number;
  throughput_rps: number;
}

interface AgentOverviewProps {
  agent: Agent;
  metrics?: Metrics;
}

export function AgentOverview({ agent, metrics }: AgentOverviewProps) {
  const renderConfigStatus = () => {
    const intent = agent.config_intent;
    const status = agent.drift_status ?? "unknown";

    let badgeClasses = "text-muted-foreground";
    let badgeLabel = "Unknown";

    switch (status) {
      case "synced":
        badgeClasses = "bg-emerald-500/10 text-emerald-700 border-emerald-500/20";
        badgeLabel = "Synced";
        break;
      case "drifted":
        badgeClasses = "bg-red-500/10 text-red-600 border-red-500/20";
        badgeLabel = "Drifted";
        break;
      case "no_intent":
        badgeClasses = "text-muted-foreground";
        badgeLabel = "No intent";
        break;
      case "no_effective":
        badgeClasses = "text-amber-600 dark:text-amber-300";
        badgeLabel = "No telemetry";
        break;
      default:
        badgeClasses = "text-muted-foreground";
        badgeLabel = "Unknown";
    }

    const sourceLabel = intent
      ? intent.source === "group"
        ? `Group: ${intent.source_name || agent.group_name || "unnamed"}`
        : "Agent override"
      : "No assigned config";

    const meta: string[] = [sourceLabel];
    if (intent?.version) {
      meta.push(`v${intent.version}`);
    }
    if (agent.drift_details?.checked_at) {
      meta.push(
        `checked ${new Date(agent.drift_details.checked_at).toLocaleTimeString()}`,
      );
    }

    return (
      <div className="flex flex-col gap-1">
        <Badge variant="outline" className={badgeClasses}>
          {badgeLabel}
        </Badge>
        <span className="text-xs text-muted-foreground">{meta.join(" • ")}</span>
      </div>
    );
  };

  return (
    <div className="space-y-4">
      <InfoCard
        title="Agent Information"
        items={[
          {
            label: "ID",
            value: <span className="font-mono">{agent.id}</span>,
          },
          { label: "Version", value: agent.version },
          {
            label: "Status",
            value: (
              <Badge
                variant={agent.status === "online" ? "default" : "secondary"}
              >
                {agent.status}
              </Badge>
            ),
          },
          {
            label: "Config",
            value: renderConfigStatus(),
          },
          { label: "Group", value: agent.group_name || "No Group" },
          {
            label: "Last Seen",
            value: new Date(agent.last_seen).toLocaleString(),
          },
        ]}
      />

      {metrics && (
        <InfoCard
          title="Telemetry Stats (Last 5 min)"
          items={[
            {
              label: "Metrics",
              value:
                metrics.metric_count === 0 ? (
                  <span className="text-muted-foreground text-xs">
                    No data received
                  </span>
                ) : (
                  <span className="font-semibold">{metrics.metric_count}</span>
                ),
            },
            {
              label: "Logs",
              value:
                metrics.log_count === 0 ? (
                  <span className="text-muted-foreground text-xs">
                    No data received
                  </span>
                ) : (
                  <span className="font-semibold">{metrics.log_count}</span>
                ),
            },
            {
              label: "Traces",
              value:
                metrics.trace_count === 0 ? (
                  <span className="text-muted-foreground text-xs">
                    No data received
                  </span>
                ) : (
                  <span className="font-semibold">{metrics.trace_count}</span>
                ),
            },
            {
              label: "Throughput",
              value: (
                <span className="font-semibold">
                  {metrics.throughput_rps.toFixed(2)} rps
                </span>
              ),
            },
          ]}
        />
      )}

      {agent.capabilities && agent.capabilities.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-lg font-semibold">
              Capabilities
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div className="flex flex-wrap gap-2">
              {agent.capabilities.map((capability) => (
                <Badge key={capability} variant="secondary">
                  {capability}
                </Badge>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      {agent.labels && Object.keys(agent.labels).length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-lg font-semibold">Labels</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="flex flex-wrap gap-2">
              {Object.entries(agent.labels).map(([key, value]) => (
                <Badge key={key} variant="outline">
                  {key}={String(value)}
                </Badge>
              ))}
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
