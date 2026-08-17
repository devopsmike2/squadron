import { useState } from "react";
import useSWR from "swr";

import { updateAgentGroup } from "@/api/agents";
import { getGroups } from "@/api/groups";
import { ClearAgentConfigButton } from "@/components/agent-details/ClearAgentConfigButton";
import { AuditTimeline } from "@/components/AuditTimeline";
import { CommandSnippet } from "@/components/shared/CommandSnippet";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { InfoCard } from "@/components/ui/info-card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { clearGroupCommand, setGroupCommand } from "@/lib/squadronctl";
import type { Agent } from "@/types/agent";

interface Metrics {
  metric_count: number;
  log_count: number;
  trace_count: number;
  throughput_rps: number;
}

interface AgentOverviewProps {
  agent: Agent;
  metrics?: Metrics;
  // Called after a successful group change so the parent can refetch
  // the agent (its new group + any resulting config resolution).
  onAgentChanged?: () => void | Promise<unknown>;
}

// Sentinel for "no group" — Radix Select reserves the empty string, so
// the ungrouped option needs a real, non-empty value.
const NO_GROUP = "__none__";

// GroupSelector turns the read-only Group field into an editable
// dropdown of existing groups (plus a "No group" option). On change it
// PATCHes /agents/:id/group and asks the parent to refetch. Membership
// is explicit: this is the only UI path that moves an agent between
// groups.
function GroupSelector({
  agent,
  onAgentChanged,
}: {
  agent: Agent;
  onAgentChanged?: () => void | Promise<unknown>;
}) {
  const { data: groupsData, isLoading: groupsLoading } = useSWR(
    "groups",
    getGroups,
  );
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{
    type: "success" | "error";
    text: string;
  } | null>(null);

  const groups = groupsData?.groups ?? [];
  const current = agent.group_id ?? NO_GROUP;

  // Copy-as-command hint: the squadronctl equivalent of the current
  // membership. A grouped agent maps to `agents set-group` (preferring
  // the group name the CLI also accepts); an ungrouped one maps to
  // `agents clear-group`.
  const currentGroupName = agent.group_id
    ? (groups.find((g) => g.id === agent.group_id)?.name ??
      agent.group_name ??
      agent.group_id)
    : null;
  const groupCommand = currentGroupName
    ? setGroupCommand(agent.id, currentGroupName)
    : clearGroupCommand(agent.id);

  const handleChange = async (value: string) => {
    const nextGroupId = value === NO_GROUP ? null : value;
    if ((agent.group_id ?? null) === nextGroupId) return;

    setSaving(true);
    setMessage(null);
    try {
      await updateAgentGroup(agent.id, nextGroupId);
      await onAgentChanged?.();
      const name = nextGroupId
        ? (groups.find((g) => g.id === nextGroupId)?.name ?? "the group")
        : null;
      setMessage({
        type: "success",
        text: nextGroupId ? `Moved to ${name}` : "Removed from group",
      });
      setTimeout(() => setMessage(null), 4000);
    } catch (error) {
      setMessage({
        type: "error",
        text: error instanceof Error ? error.message : "Failed to update group",
      });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex flex-col items-end gap-1">
      <Select
        value={current}
        onValueChange={handleChange}
        disabled={saving || groupsLoading}
      >
        <SelectTrigger className="h-8 w-48 text-xs" aria-label="Group">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={NO_GROUP}>No group</SelectItem>
          {groups.map((g) => (
            <SelectItem key={g.id} value={g.id}>
              {g.name}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {message && (
        <span
          role={message.type === "error" ? "alert" : undefined}
          className={
            message.type === "error"
              ? "text-xs text-destructive"
              : "text-xs text-muted-foreground"
          }
        >
          {message.text}
        </span>
      )}
      <CommandSnippet variant="inline" command={groupCommand} />
    </div>
  );
}

export function AgentOverview({
  agent,
  metrics,
  onAgentChanged,
}: AgentOverviewProps) {
  const renderConfigStatus = () => {
    const intent = agent.config_intent;
    const status = agent.drift_status ?? "unknown";

    let badgeClasses = "text-muted-foreground";
    let badgeLabel = "Unknown";

    switch (status) {
      case "synced":
        badgeClasses =
          "bg-emerald-500/10 text-emerald-700 border-emerald-500/20";
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
        <span className="text-xs text-muted-foreground">
          {meta.join(" • ")}
        </span>
        {/* Clear an agent-scoped config so resolution falls back to the group
          config. Only meaningful (enabled) when the intent is an agent override
          and the agent is in a group — the button self-disables otherwise. */}
        {intent?.source === "agent" && (
          <ClearAgentConfigButton agent={agent} onCleared={onAgentChanged} />
        )}
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
          {
            label: "Group",
            value: (
              <GroupSelector agent={agent} onAgentChanged={onAgentChanged} />
            ),
          },
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

      <Card>
        <CardHeader>
          <CardTitle className="text-lg font-semibold">History</CardTitle>
        </CardHeader>
        <CardContent>
          <AuditTimeline targetType="agent" targetId={agent.id} limit={50} />
        </CardContent>
      </Card>

      {agent.drift_status === "drifted" && agent.drift_details?.diff && (
        <Card>
          <CardHeader>
            <CardTitle className="text-lg font-semibold flex items-center gap-2">
              <span className="text-red-600">Config drift</span>
              <span className="text-xs font-normal text-muted-foreground">
                intended → effective
              </span>
            </CardTitle>
          </CardHeader>
          <CardContent>
            <pre className="text-xs font-mono whitespace-pre-wrap break-all bg-muted/40 rounded p-3 overflow-auto max-h-96">
              {agent.drift_details.diff}
            </pre>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
