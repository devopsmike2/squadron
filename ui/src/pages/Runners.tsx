// Squadron Runners page — operational visibility into the action
// runner fleet (Move 2 of the engineer copilot roadmap).
//
// Each runner that registers with Squadron lands here. Operators
// need this page to answer: which runners are currently connected,
// which actions are they willing to run, who has gone silent. A
// runner that has not polled in a few minutes is almost certainly
// dead from Squadron's side even if it is still running on the host.
//
// One card per runner. The capability list is parsed from the
// runner's declared CapabilitiesJSON so operators see exactly what
// each runner accepts, not what the action type registry has on
// offer in general.

import { useState } from "react";
import useSWR, { mutate } from "swr";

import {
  listActionRunners,
  revokeActionRunner,
} from "@/api/actions";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import type { ActionCapability, ActionRunner } from "@/types/action";

function formatRelative(iso: string): string {
  const then = new Date(iso).getTime();
  const now = Date.now();
  const seconds = Math.round((now - then) / 1000);
  if (seconds < 60) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  return `${days}d ago`;
}

function parseCapabilities(json: string): ActionCapability[] {
  if (!json) return [];
  try {
    const parsed = JSON.parse(json);
    if (Array.isArray(parsed)) return parsed as ActionCapability[];
    return [];
  } catch {
    return [];
  }
}

// Health heuristic. We consider a runner stale if it has not polled
// in more than 90 seconds, because the default runner poll interval
// is 30 seconds plus jitter and three missed polls is the line where
// a reader should worry. The threshold is intentionally permissive;
// the goal is to surface obvious stoppages, not to chase flakes.
function isStale(lastSeenISO: string): boolean {
  const ageSeconds = (Date.now() - new Date(lastSeenISO).getTime()) / 1000;
  return ageSeconds > 90;
}

function HealthBadge({ runner }: { runner: ActionRunner }) {
  if (runner.revoked_at) {
    return (
      <Badge
        variant="outline"
        className="bg-zinc-500/10 text-zinc-600 border-zinc-500/20"
      >
        revoked
      </Badge>
    );
  }
  if (isStale(runner.last_seen_at)) {
    return (
      <Badge
        variant="outline"
        className="bg-amber-500/10 text-amber-700 border-amber-500/20"
      >
        stale
      </Badge>
    );
  }
  return (
    <Badge
      variant="outline"
      className="bg-emerald-500/10 text-emerald-700 border-emerald-500/20"
    >
      online
    </Badge>
  );
}

export default function RunnersPage() {
  const [expanded, setExpanded] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const { data: runners, isLoading } = useSWR<ActionRunner[]>(
    "/runners",
    () => listActionRunners(),
    { refreshInterval: 15000 },
  );

  const onRevoke = async (id: string) => {
    if (
      !window.confirm(
        `Revoke runner ${id}? Squadron will refuse to dispatch any further actions to it.`,
      )
    ) {
      return;
    }
    setActionError(null);
    setRevoking(id);
    try {
      await revokeActionRunner(id);
      await mutate("/runners");
    } catch (err) {
      const msg =
        err instanceof Error ? err.message : "Revoke failed for unknown reason";
      setActionError(msg);
    } finally {
      setRevoking(null);
    }
  };

  const visible = runners ?? [];
  const onlineCount = visible.filter(
    (r) => !r.revoked_at && !isStale(r.last_seen_at),
  ).length;

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Runners</h1>
        <p className="text-sm text-muted-foreground mt-1 max-w-2xl">
          Hosts running the squadron-action-runner daemon. Each one declares
          a capability list at registration; Squadron will only dispatch an
          action to a runner that has accepted it. {onlineCount} of{" "}
          {visible.length} online.
        </p>
      </div>

      {actionError ? (
        <Card>
          <CardContent className="py-3 text-sm text-rose-600">
            {actionError}
          </CardContent>
        </Card>
      ) : null}

      {isLoading && !runners ? (
        <Card>
          <CardContent className="py-8 text-sm text-muted-foreground">
            Loading runners...
          </CardContent>
        </Card>
      ) : visible.length === 0 ? (
        <Card>
          <CardContent className="py-8 text-sm text-muted-foreground">
            No runners registered yet. Install the squadron-action-runner
            daemon on a host, point it at this server, and it will appear
            here after its first poll.
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-2">
          {visible.map((r) => {
            const open = expanded === r.runner_id;
            const caps = parseCapabilities(r.capabilities_json);
            return (
              <Card key={r.runner_id}>
                <CardHeader
                  className="cursor-pointer hover:bg-accent/40 transition-colors"
                  onClick={() => setExpanded(open ? null : r.runner_id)}
                >
                  <div className="flex items-center justify-between gap-3 flex-wrap">
                    <div className="flex items-center gap-2 flex-wrap min-w-0">
                      <CardTitle className="text-sm font-medium truncate">
                        {r.hostname}
                      </CardTitle>
                      <HealthBadge runner={r} />
                      <span className="text-xs text-muted-foreground">
                        {caps.length} capabilit
                        {caps.length === 1 ? "y" : "ies"}
                      </span>
                    </div>
                    <div className="text-xs text-muted-foreground shrink-0">
                      last seen {formatRelative(r.last_seen_at)}
                    </div>
                  </div>
                </CardHeader>
                {open ? (
                  <CardContent className="pt-0 space-y-4">
                    <div className="space-y-1 text-xs">
                      <KV k="Runner ID" v={r.runner_id} mono />
                      <KV k="Registered" v={r.registered_at} />
                      <KV k="Last seen" v={r.last_seen_at} />
                      {r.revoked_at ? (
                        <KV k="Revoked" v={r.revoked_at} />
                      ) : null}
                    </div>

                    <div className="space-y-2">
                      <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                        Accepted actions
                      </div>
                      {caps.length === 0 ? (
                        <p className="text-xs text-muted-foreground">
                          No capabilities declared. This runner cannot
                          accept any action and will deny every dispatch.
                        </p>
                      ) : (
                        <ul className="space-y-1">
                          {caps.map((cap, idx) => (
                            <li key={`${cap.action_type}-${idx}`}>
                              <div className="flex items-center gap-2">
                                <code className="text-xs bg-muted/40 rounded px-1.5 py-0.5">
                                  {cap.action_type}
                                </code>
                                {cap.policy && Object.keys(cap.policy).length ? (
                                  <span className="text-xs text-muted-foreground">
                                    constrained
                                  </span>
                                ) : null}
                              </div>
                              {cap.policy && Object.keys(cap.policy).length ? (
                                <pre className="text-xs bg-muted/40 rounded p-2 mt-1 overflow-x-auto whitespace-pre-wrap break-all">
                                  {JSON.stringify(cap.policy, null, 2)}
                                </pre>
                              ) : null}
                            </li>
                          ))}
                        </ul>
                      )}
                    </div>

                    {!r.revoked_at ? (
                      <div className="pt-2">
                        <Button
                          variant="destructive"
                          size="sm"
                          disabled={revoking === r.runner_id}
                          onClick={(e) => {
                            e.stopPropagation();
                            void onRevoke(r.runner_id);
                          }}
                        >
                          {revoking === r.runner_id ? "Revoking..." : "Revoke"}
                        </Button>
                      </div>
                    ) : null}
                  </CardContent>
                ) : null}
              </Card>
            );
          })}
        </div>
      )}
    </div>
  );
}

function KV({ k, v, mono = false }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="flex items-baseline gap-2 text-xs">
      <span className="text-muted-foreground shrink-0 w-24">{k}</span>
      <span className={mono ? "font-mono break-all" : "break-all"}>{v}</span>
    </div>
  );
}
