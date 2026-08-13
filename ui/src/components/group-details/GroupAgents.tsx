import { CheckCircle, XCircle, AlertCircle, Server, X } from "lucide-react";
import { useState } from "react";
import useSWR from "swr";

import { getAgents, updateAgentGroup } from "@/api/agents";
import { getGroupAgents } from "@/api/groups";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { LoadingSpinner } from "@/components/ui/loading-spinner";
import { ScrollArea } from "@/components/ui/scroll-area";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

interface GroupAgentsProps {
  groupId: string;
  onAgentClick?: (agentId: string) => void;
}

export function GroupAgents({ groupId, onAgentClick }: GroupAgentsProps) {
  const {
    data: agentsData,
    isLoading,
    mutate,
  } = useSWR(`group-agents-${groupId}`, () => getGroupAgents(groupId), {
    revalidateOnFocus: false,
    revalidateOnReconnect: false,
  });

  // Candidate pool for the "Add agents" picker: a page of the fleet
  // that we filter down to agents not already in this group. Bounded
  // to keep the dropdown sane; the bulk path on the Agents page is the
  // tool for large moves (tracked as a follow-up).
  const { data: fleetData } = useSWR(`group-agents-picker-${groupId}`, () =>
    getAgents({ limit: 100 }),
  );

  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const getStatusIcon = (status: string) => {
    switch (status) {
      case "online":
        return <CheckCircle className="h-4 w-4 text-green-500" />;
      case "offline":
        return <XCircle className="h-4 w-4 text-gray-500" />;
      case "error":
        return <AlertCircle className="h-4 w-4 text-red-500" />;
      default:
        return <Server className="h-4 w-4 text-gray-500" />;
    }
  };

  const getStatusBadge = (status: string) => {
    switch (status) {
      case "online":
        return (
          <Badge variant="default" className="bg-green-100 text-green-800">
            Online
          </Badge>
        );
      case "offline":
        return <Badge variant="secondary">Offline</Badge>;
      case "error":
        return <Badge variant="destructive">Error</Badge>;
      default:
        return <Badge variant="outline">Unknown</Badge>;
    }
  };

  const agents = agentsData?.agents || [];
  const memberIds = new Set(agents.map((a) => a.id));
  const candidates = (fleetData?.items ?? []).filter(
    (a) => a.group_id !== groupId && !memberIds.has(a.id),
  );

  // Assign an agent to this group, then refresh the member list.
  const assign = async (agentId: string) => {
    setBusyId(agentId);
    setError(null);
    try {
      await updateAgentGroup(agentId, groupId);
      await mutate();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to add agent to group");
    } finally {
      setBusyId(null);
    }
  };

  // Clear an agent's membership (move it out of this group), then
  // refresh the member list.
  const remove = async (agentId: string) => {
    setBusyId(agentId);
    setError(null);
    try {
      await updateAgentGroup(agentId, null);
      await mutate();
    } catch (e) {
      setError(
        e instanceof Error ? e.message : "Failed to remove agent from group",
      );
    } finally {
      setBusyId(null);
    }
  };

  if (isLoading) {
    return <LoadingSpinner />;
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-4">
          <div>
            <CardTitle className="text-base">Group Agents</CardTitle>
            <CardDescription>
              {agents.length} agents in this group
            </CardDescription>
          </div>
          {/* Add agents: pick from the fleet's ungrouped / other-group
              agents. Selecting one assigns it immediately; the picker
              stays put so several can be added in a row. */}
          <Select
            value=""
            onValueChange={(v) => {
              if (v) assign(v);
            }}
            disabled={busyId !== null || candidates.length === 0}
          >
            <SelectTrigger className="h-8 w-56 text-xs" aria-label="Add agents">
              <SelectValue
                placeholder={
                  candidates.length === 0
                    ? "No agents to add"
                    : "Add agents to group"
                }
              />
            </SelectTrigger>
            <SelectContent>
              {candidates.map((a) => (
                <SelectItem key={a.id} value={a.id}>
                  {a.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        {error && (
          <p role="alert" className="mt-2 text-xs text-destructive">
            {error}
          </p>
        )}
      </CardHeader>
      <CardContent>
        {agents.length === 0 ? (
          <div className="text-center py-8 text-gray-500">
            No agents in this group
          </div>
        ) : (
          <ScrollArea className="h-96">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Status</TableHead>
                  <TableHead>Name</TableHead>
                  <TableHead>Version</TableHead>
                  <TableHead>Last Seen</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {agents.map((agent) => (
                  <TableRow
                    key={agent.id}
                    onClick={() => onAgentClick?.(agent.id)}
                    className={
                      onAgentClick ? "cursor-pointer hover:bg-muted/50" : ""
                    }
                  >
                    <TableCell>
                      <div className="flex items-center space-x-2">
                        {getStatusIcon(agent.status)}
                        {getStatusBadge(agent.status)}
                      </div>
                    </TableCell>
                    <TableCell className="font-medium">{agent.name}</TableCell>
                    <TableCell>{agent.version}</TableCell>
                    <TableCell>
                      {new Date(agent.last_seen).toLocaleString()}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button
                        variant="ghost"
                        size="sm"
                        aria-label={`Remove ${agent.name} from group`}
                        disabled={busyId === agent.id}
                        onClick={(e) => {
                          e.stopPropagation();
                          remove(agent.id);
                        }}
                      >
                        <X className="h-3.5 w-3.5" />
                        Remove
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </ScrollArea>
        )}
      </CardContent>
    </Card>
  );
}
