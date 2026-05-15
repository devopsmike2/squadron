// Rollouts page — list + create + abort.
//
// Each row shows progress through the configured stages with a state
// badge. In-progress rollouts get an Abort button. The create form is
// inline at the top so operators can kick one off without navigating
// away.

import { Pause, Plus, RotateCcw } from "lucide-react";
import { useState } from "react";
import useSWR, { mutate } from "swr";

import { getGroups, type Group } from "@/api/groups";
import { abortRollout, createRollout, listRollouts } from "@/api/rollouts";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type {
  Rollout,
  RolloutInput,
  RolloutStage,
  RolloutState,
} from "@/types/rollout";

const ROLLOUTS_KEY = "rollouts";

const stateBadge: Record<RolloutState, string> = {
  pending: "bg-slate-500/10 text-slate-700 border-slate-500/20",
  in_progress: "bg-blue-500/10 text-blue-700 border-blue-500/20",
  succeeded: "bg-emerald-500/10 text-emerald-700 border-emerald-500/20",
  aborted: "bg-amber-500/10 text-amber-700 border-amber-500/20",
  rolled_back: "bg-red-500/10 text-red-700 border-red-500/20",
};

interface GroupsList {
  groups: Group[];
}

const emptyInput = (): {
  name: string;
  group_id: string;
  target_config_id: string;
  stage_percents: string;
  dwell_seconds: string;
  max_drifted_agents: string;
} => ({
  name: "",
  group_id: "",
  target_config_id: "",
  stage_percents: "10, 50, 100",
  dwell_seconds: "60",
  max_drifted_agents: "0",
});

export default function RolloutsPage() {
  const { data: rollouts, error, isLoading } = useSWR<Rollout[]>(
    ROLLOUTS_KEY,
    listRollouts,
  );
  const { data: groupsResp } = useSWR<GroupsList>("groups-list", getGroups);
  // Surface group names for the row UI without an extra round trip.
  const groupName = (id: string): string =>
    groupsResp?.groups?.find((g) => g.id === id)?.name ?? id.slice(0, 8);

  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState(emptyInput());
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const reset = () => {
    setShowForm(false);
    setForm(emptyInput());
    setSubmitError(null);
  };

  const submit = async () => {
    setSubmitting(true);
    setSubmitError(null);

    // Parse the comma-separated percentages into ordered stages. All
    // stages share the same dwell — UI keeps this simple; the API
    // supports per-stage dwell if we expose it later.
    const percentages = form.stage_percents
      .split(",")
      .map((s) => parseInt(s.trim(), 10))
      .filter((n) => !Number.isNaN(n));
    const dwell = parseInt(form.dwell_seconds, 10) || 60;
    const stages: RolloutStage[] = percentages.map((p) => ({
      percentage: p,
      dwell_seconds: dwell,
    }));

    const input: RolloutInput = {
      name: form.name.trim(),
      group_id: form.group_id,
      target_config_id: form.target_config_id.trim(),
      stages,
      abort_criteria: {
        max_drifted_agents: parseInt(form.max_drifted_agents, 10) || 0,
      },
    };

    try {
      await createRollout(input);
      await mutate(ROLLOUTS_KEY);
      reset();
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : "create failed");
    } finally {
      setSubmitting(false);
    }
  };

  const handleAbort = async (r: Rollout) => {
    const reason = window.prompt(
      "Reason for aborting (will be shown in the audit log):",
      "aborted by operator",
    );
    if (reason === null) return;
    try {
      await abortRollout(r.id, reason);
      await mutate(ROLLOUTS_KEY);
    } catch (e) {
      alert(e instanceof Error ? e.message : "abort failed");
    }
  };

  return (
    <div className="space-y-4 p-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold">Rollouts</h1>
          <p className="text-muted-foreground text-sm">
            Stage a new config to a percentage of a group at a time. Squadron
            advances on dwell and rolls back automatically when a stage's
            abort criteria fire.
          </p>
        </div>
        {!showForm && (
          <Button onClick={() => setShowForm(true)} className="gap-1">
            <Plus className="h-4 w-4" />
            New rollout
          </Button>
        )}
      </div>

      {showForm && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">New rollout</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <Label htmlFor="name">Name</Label>
                <Input
                  id="name"
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="canary v0.150 to prod"
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="group">Target group</Label>
                <Select
                  value={form.group_id}
                  onValueChange={(v) => setForm({ ...form, group_id: v })}
                >
                  <SelectTrigger id="group">
                    <SelectValue placeholder="Pick a group..." />
                  </SelectTrigger>
                  <SelectContent>
                    {groupsResp?.groups?.map((g) => (
                      <SelectItem key={g.id} value={g.id}>
                        {g.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>

            <div className="space-y-2">
              <Label htmlFor="cfg">Target config ID</Label>
              <Input
                id="cfg"
                value={form.target_config_id}
                onChange={(e) =>
                  setForm({ ...form, target_config_id: e.target.value })
                }
                placeholder="UUID of the config to roll out"
                className="font-mono text-sm"
              />
              <p className="text-xs text-muted-foreground">
                Squadron snapshots the group's current config as the rollback
                target when the rollout is created.
              </p>
            </div>

            <div className="grid gap-4 md:grid-cols-3">
              <div className="space-y-2">
                <Label htmlFor="stages">Stages (% cumulative)</Label>
                <Input
                  id="stages"
                  value={form.stage_percents}
                  onChange={(e) =>
                    setForm({ ...form, stage_percents: e.target.value })
                  }
                  placeholder="10, 50, 100"
                />
                <p className="text-xs text-muted-foreground">
                  Final stage must reach 100.
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="dwell">Dwell per stage (s)</Label>
                <Input
                  id="dwell"
                  type="number"
                  min={0}
                  value={form.dwell_seconds}
                  onChange={(e) =>
                    setForm({ ...form, dwell_seconds: e.target.value })
                  }
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="maxdrift">Max drifted agents</Label>
                <Input
                  id="maxdrift"
                  type="number"
                  min={0}
                  value={form.max_drifted_agents}
                  onChange={(e) =>
                    setForm({ ...form, max_drifted_agents: e.target.value })
                  }
                />
                <p className="text-xs text-muted-foreground">
                  Auto-abort when more than this many canary agents drift.
                </p>
              </div>
            </div>

            {submitError && (
              <div className="text-sm text-red-600">{submitError}</div>
            )}

            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={reset} disabled={submitting}>
                Cancel
              </Button>
              <Button onClick={submit} disabled={submitting}>
                {submitting ? "Starting…" : "Start rollout"}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {error && (
        <div className="text-sm text-red-600">
          Failed to load rollouts:{" "}
          {error instanceof Error ? error.message : String(error)}
        </div>
      )}

      {isLoading && (
        <div className="text-sm text-muted-foreground">Loading…</div>
      )}

      {rollouts && rollouts.length === 0 && !isLoading && (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            No rollouts yet. Click "New rollout" to stage your first deploy.
          </CardContent>
        </Card>
      )}

      {rollouts &&
        rollouts.map((r) => (
          <RolloutCard
            key={r.id}
            rollout={r}
            groupName={groupName(r.group_id)}
            onAbort={handleAbort}
          />
        ))}
    </div>
  );
}

interface RolloutCardProps {
  rollout: Rollout;
  groupName: string;
  onAbort: (r: Rollout) => void;
}

function RolloutCard({ rollout: r, groupName, onAbort }: RolloutCardProps) {
  const totalStages = r.stages.length;
  const currentPct =
    totalStages > 0 ? r.stages[Math.min(r.current_stage, totalStages - 1)].percentage : 0;
  const isActive = r.state === "in_progress" || r.state === "pending";

  return (
    <Card>
      <CardContent className="py-4 space-y-3">
        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2 mb-1">
              <span className="font-medium">{r.name || r.id.slice(0, 8)}</span>
              <Badge variant="outline" className={stateBadge[r.state]}>
                {r.state.replace("_", " ")}
              </Badge>
            </div>
            <div className="text-xs text-muted-foreground">
              Group{" "}
              <span className="font-mono">{groupName}</span>
              {" · "}target config{" "}
              <span className="font-mono">
                {r.target_config_id.slice(0, 8)}…
              </span>
              {" · "}stage {r.current_stage + 1} of {totalStages}
              {r.abort_reason && (
                <>
                  {" · "}
                  <span className="text-amber-600">
                    {r.abort_reason}
                  </span>
                </>
              )}
            </div>
          </div>
          {isActive && (
            <Button
              variant="outline"
              size="sm"
              className="gap-1"
              onClick={() => onAbort(r)}
            >
              <Pause className="h-3.5 w-3.5" />
              Abort
            </Button>
          )}
          {r.state === "rolled_back" && (
            <Badge variant="outline" className="gap-1 text-xs">
              <RotateCcw className="h-3 w-3" />
              rolled back
            </Badge>
          )}
        </div>

        {/* Stage track: one bar per stage, filled up to current_stage. */}
        <div className="flex items-center gap-1">
          {r.stages.map((stage, i) => {
            const reached = i <= r.current_stage && r.state !== "pending";
            return (
              <div
                key={i}
                className={`flex-1 h-2 rounded ${
                  reached ? "bg-blue-500" : "bg-muted"
                }`}
                title={`Stage ${i + 1}: ${stage.percentage}% · ${stage.dwell_seconds}s dwell`}
              />
            );
          })}
        </div>
        <div className="text-xs text-muted-foreground">
          {currentPct}% of group at this stage
        </div>
      </CardContent>
    </Card>
  );
}

