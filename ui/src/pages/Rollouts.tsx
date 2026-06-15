// Rollouts page — list + create + abort.
//
// Each row shows progress through the configured stages with a state
// badge. In-progress rollouts get an Abort button. The create form is
// inline at the top so operators can kick one off without navigating
// away.
//
// Stages support two selection modes:
//   - "percent": pick the first N% of the group's agents (legacy / default)
//   - "label":   match by AND'd key=value pairs against agent labels
// The form toggles between the two; live match-count against the target
// group gives operators feedback before they click Start.

import { Check, ChevronDown, ChevronRight, Pause, Play, Plus, RotateCcw, Trash2, X } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import useSWR, { mutate } from "swr";

import { getAgents } from "@/api/agents";
import { getGroups, type Group } from "@/api/groups";
import {
  abortRollout,
  approveRollout,
  createRollout,
  listAbortCriteriaRecipes,
  listRolloutTemplates,
  listRollouts,
  pauseRollout,
  rejectRollout,
  resumeRollout,
} from "@/api/rollouts";
import { AuditTimeline } from "@/components/AuditTimeline";
import { RolloutPreviewPane } from "@/components/rollouts/RolloutPreviewPane";
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
import type { Agent } from "@/types/agent";
import type {
  AbortCriteriaRecipe,
  Rollout,
  RolloutInput,
  RolloutStage,
  RolloutStageMode,
  RolloutState,
  RolloutTemplate,
} from "@/types/rollout";

const ROLLOUTS_KEY = "rollouts";

const stateBadge: Record<RolloutState, string> = {
  pending: "bg-slate-500/10 text-slate-700 border-slate-500/20",
  in_progress: "bg-blue-500/10 text-blue-700 border-blue-500/20",
  paused: "bg-violet-500/10 text-violet-700 border-violet-500/20",
  succeeded: "bg-emerald-500/10 text-emerald-700 border-emerald-500/20",
  aborted: "bg-amber-500/10 text-amber-700 border-amber-500/20",
  rolled_back: "bg-red-500/10 text-red-700 border-red-500/20",
  // v0.47 — approval workflow. Pending-approval gets a warm tone so it
  // stands out in the list (operators looking through a long list need
  // to spot the "needs me" entries fast). Rejected is muted — terminal,
  // and the requester usually wants to look past it on to the new run.
  pending_approval: "bg-orange-500/10 text-orange-700 border-orange-500/20",
  rejected: "bg-zinc-500/10 text-zinc-700 border-zinc-500/20",
};

interface GroupsList {
  groups: Group[];
}

// FormStage carries both modes' fields side-by-side. Whichever is "active"
// is determined by `mode`. We keep the inactive mode's values around so
// flipping the toggle doesn't wipe the operator's selector entries.
interface FormStage {
  mode: RolloutStageMode;
  percentage: string; // controlled <Input type="number"> wants a string
  // selector_rows is an editable list of {key, value} pairs. We keep it as
  // an array (not a Record) so duplicate-key keystrokes don't collide and
  // so empty rows persist while the operator is mid-edit.
  selector_rows: { key: string; value: string }[];
  dwell_seconds: string;
}

interface FormState {
  name: string;
  group_id: string;
  target_config_id: string;
  stages: FormStage[];
  max_drifted_agents: string;
  max_error_logs_per_minute: string;
  min_dwell_seconds_before_abort: string;
  // recipe_id and template_id remember which cookbook entries the
  // operator picked. They're UI memos for highlighting the current
  // selection — the form fields are the source of truth on submit.
  // recipe_id clears on hand-edits to the criteria fields (since
  // those edits desync from the recipe); template_id is sticky and
  // only changes when the operator picks a different template or
  // "Custom". This gives operators a "what was my starting point?"
  // breadcrumb without making the UI lie about whether what they're
  // submitting is exactly the template's shape.
  recipe_id: string;
  template_id: string;
  notification_url: string;
  // v0.47 — when true, the rollout enters pending_approval and waits
  // for an Approve API call before the engine advances. Operators with
  // SCM (NERC CIP) or change-management requirements turn this on as
  // a hard guarantee that no one (including the requester themselves)
  // can ship without a second pair of eyes.
  require_approval: boolean;
}

const emptyPercentStage = (percentage: string, dwell: string): FormStage => ({
  mode: "percent",
  percentage,
  selector_rows: [{ key: "", value: "" }],
  dwell_seconds: dwell,
});

const emptyInput = (): FormState => ({
  name: "",
  group_id: "",
  target_config_id: "",
  stages: [
    emptyPercentStage("10", "60"),
    emptyPercentStage("50", "120"),
    emptyPercentStage("100", "60"),
  ],
  // Defaults match the historical pre-cookbook form values so an
  // operator who skips the picker sees the same form as before.
  max_drifted_agents: "0",
  max_error_logs_per_minute: "0",
  min_dwell_seconds_before_abort: "30",
  recipe_id: "",
  template_id: "",
  notification_url: "",
  require_approval: false,
});

// applyTemplate returns a FormState with the template's stages,
// criteria, and default name overwritten in, preserving group_id and
// target_config_id (the operator-supplied bits). Stage shape is
// converted from the wire-typed RolloutStage to the FormStage variant
// so the editor's controlled inputs remain string-typed.
function applyTemplate(prev: FormState, tpl: RolloutTemplate): FormState {
  const stages: FormStage[] = tpl.stages.map((s) => {
    const dwell = String(s.dwell_seconds ?? 0);
    if (s.mode === "label") {
      const selectorRows = Object.entries(s.label_selector ?? {}).map(
        ([key, value]) => ({ key, value }),
      );
      return {
        mode: "label",
        percentage: "100",
        selector_rows:
          selectorRows.length > 0 ? selectorRows : [{ key: "", value: "" }],
        dwell_seconds: dwell,
      };
    }
    return {
      mode: "percent",
      percentage: String(s.percentage ?? 100),
      selector_rows: [{ key: "", value: "" }],
      dwell_seconds: dwell,
    };
  });
  const crit = tpl.abort_criteria;
  return {
    ...prev,
    name: tpl.default_name,
    stages,
    max_drifted_agents: String(crit.max_drifted_agents),
    max_error_logs_per_minute: String(crit.max_error_logs_per_minute ?? 0),
    min_dwell_seconds_before_abort: String(crit.min_dwell_seconds_before_abort ?? 0),
    template_id: tpl.id,
    // Picking a template fully owns the criteria, so the recipe
    // selection is no longer accurate — clear it.
    recipe_id: "",
  };
}

// Build the wire-shape RolloutStage from form state. Discards the
// inactive mode's fields so the API receives clean input.
const toWireStage = (s: FormStage): RolloutStage => {
  const base: RolloutStage = { dwell_seconds: parseInt(s.dwell_seconds, 10) || 0 };
  if (s.mode === "label") {
    const selector: Record<string, string> = {};
    for (const row of s.selector_rows) {
      const k = row.key.trim();
      const v = row.value.trim();
      if (k && v) selector[k] = v;
    }
    return { ...base, mode: "label", label_selector: selector };
  }
  return { ...base, mode: "percent", percentage: parseInt(s.percentage, 10) || 0 };
};

// Materialize a stage's effective selector at the moment of typing so
// the match-count widget can show a number. Mirrors backend AND-semantics.
const matchAgents = (agents: Agent[], groupId: string, stage: FormStage): Agent[] => {
  const inGroup = agents.filter((a) => a.group_id === groupId);
  if (stage.mode === "percent") {
    const pct = parseInt(stage.percentage, 10);
    if (!pct || pct <= 0) return [];
    // ceil division mirrors engine.canaryAgentsForStage percent math
    const n = Math.min(inGroup.length, Math.ceil((inGroup.length * pct) / 100));
    // Deterministic id sort like the backend so the preview matches.
    return [...inGroup].sort((a, b) => a.id.localeCompare(b.id)).slice(0, n);
  }
  // Label mode: AND-match across all completed (non-empty key+value) rows.
  // Incomplete rows are ignored — operators in mid-typing shouldn't see
  // their match-count crash to zero between keystrokes.
  const completed = stage.selector_rows.filter((r) => r.key.trim() && r.value.trim());
  if (completed.length === 0) return [];
  return inGroup.filter((a) =>
    completed.every((r) => a.labels?.[r.key.trim()] === r.value.trim()),
  );
};

export default function RolloutsPage() {
  const { data: rollouts, error, isLoading } = useSWR<Rollout[]>(
    ROLLOUTS_KEY,
    listRollouts,
  );
  const { data: groupsResp } = useSWR<GroupsList>("groups-list", getGroups);
  const { data: agentsResp } = useSWR("agents-list", getAgents);
  // Cookbook is server-defined and only changes on upgrade; fine to
  // fetch once and reuse without revalidation.
  const { data: recipes = [] } = useSWR<AbortCriteriaRecipe[]>(
    "abort-criteria-recipes",
    listAbortCriteriaRecipes,
    { revalidateOnFocus: false, revalidateIfStale: false },
  );
  const { data: templates = [] } = useSWR<RolloutTemplate[]>(
    "rollout-templates",
    listRolloutTemplates,
    { revalidateOnFocus: false, revalidateIfStale: false },
  );
  // Flatten the agents-by-id map the API returns. Used for label-mode
  // match counts and (eventually) hover-to-see-which-agents UX.
  const allAgents: Agent[] = useMemo(
    () => Object.values(agentsResp?.agents ?? {}),
    [agentsResp],
  );

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

  // Per-stage match count is recomputed off the (debounced) form state +
  // agent list. We debounce by holding a "settled" mirror that updates
  // 150ms after the last edit, so quick typing doesn't thrash useSWR or
  // re-render the agent list.
  const [settledStages, setSettledStages] = useState(form.stages);
  useEffect(() => {
    const h = setTimeout(() => setSettledStages(form.stages), 150);
    return () => clearTimeout(h);
  }, [form.stages]);

  const submit = async () => {
    setSubmitting(true);
    setSubmitError(null);

    // The form holds both mode's fields side-by-side; toWireStage drops
    // whichever isn't active so the API sees a clean shape.
    const stages: RolloutStage[] = form.stages
      .map(toWireStage)
      .filter((s) => {
        // Drop blank rows the operator never filled in (defensive — the
        // add/remove buttons should keep this from happening).
        if (s.mode === "label") {
          return s.label_selector && Object.keys(s.label_selector).length > 0;
        }
        return typeof s.percentage === "number" && s.percentage > 0;
      });

    const input: RolloutInput = {
      name: form.name.trim(),
      group_id: form.group_id,
      target_config_id: form.target_config_id.trim(),
      stages,
      abort_criteria: {
        max_drifted_agents: parseInt(form.max_drifted_agents, 10) || 0,
        max_error_logs_per_minute:
          parseInt(form.max_error_logs_per_minute, 10) || 0,
        min_dwell_seconds_before_abort:
          parseInt(form.min_dwell_seconds_before_abort, 10) || 0,
      },
      notification_url: form.notification_url.trim(),
      require_approval: form.require_approval,
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

  const handlePauseResume = async (r: Rollout) => {
    try {
      if (r.state === "paused") {
        await resumeRollout(r.id);
      } else {
        await pauseRollout(r.id);
      }
      await mutate(ROLLOUTS_KEY);
    } catch (e) {
      alert(e instanceof Error ? e.message : "pause/resume failed");
    }
  };

  // v0.47 — approve / reject. Both prompt for optional notes that
  // land in the audit log payload. The two-person rule is enforced
  // server-side; here we just surface the 409 if the approver is
  // also the requester.
  const handleApprove = async (r: Rollout) => {
    const notes = window.prompt(
      `Approve rollout "${r.name || r.id.slice(0, 8)}"?\n\nOptional approval notes (recorded in the audit log):`,
      "",
    );
    if (notes === null) return;
    try {
      await approveRollout(r.id, notes);
      await mutate(ROLLOUTS_KEY);
    } catch (e) {
      alert(e instanceof Error ? e.message : "approve failed");
    }
  };
  const handleReject = async (r: Rollout) => {
    const notes = window.prompt(
      `Reject rollout "${r.name || r.id.slice(0, 8)}"?\n\nThis is terminal — the requester will have to clone the rollout to retry.\n\nOptional rejection notes:`,
      "",
    );
    if (notes === null) return;
    try {
      await rejectRollout(r.id, notes);
      await mutate(ROLLOUTS_KEY);
    } catch (e) {
      alert(e instanceof Error ? e.message : "reject failed");
    }
  };

  // groupAgents count is shown next to each match-count so operators
  // immediately see "0 of 12 agents matched" rather than just "0".
  const groupAgentCount = form.group_id
    ? allAgents.filter((a) => a.group_id === form.group_id).length
    : 0;

  // Once the operator has picked a group, validate label-mode rollouts
  // have a non-zero match at stage 0. We warn but still allow Start so
  // they can launch ahead of an agent rolling in with the matching label.
  const stageZeroMatchesZero =
    form.group_id &&
    settledStages[0] &&
    settledStages[0].mode === "label" &&
    matchAgents(allAgents, form.group_id, settledStages[0]).length === 0;

  // Stages with all-same mode required by the API. We surface this in the
  // form too so operators don't get a 400 after Start.
  const mixedModes =
    new Set(form.stages.map((s) => s.mode)).size > 1;

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
            {/* Template picker — top of the form because picking one
                overwrites name/stages/criteria. Operator picks a
                template, then only has to fill in group + target
                config. Templates ship percent-mode only; label-mode
                rollouts must be hand-built for now (the comment in
                services/rollout_template.go explains why). */}
            <div className="space-y-2 rounded-md border border-dashed p-3">
              <Label htmlFor="template">Start from template (optional)</Label>
              <Select
                value={form.template_id || "blank"}
                onValueChange={(v) => {
                  if (v === "blank") {
                    setForm({ ...form, template_id: "" });
                    return;
                  }
                  const tpl = templates.find((t) => t.id === v);
                  if (!tpl) return;
                  setForm((prev) => applyTemplate(prev, tpl));
                }}
              >
                <SelectTrigger id="template">
                  <SelectValue placeholder="Build from scratch..." />
                </SelectTrigger>
                <SelectContent>
                  {templates.map((t) => (
                    <SelectItem key={t.id} value={t.id}>
                      {t.name}
                    </SelectItem>
                  ))}
                  <SelectItem value="blank">
                    Build from scratch (no template)
                  </SelectItem>
                </SelectContent>
              </Select>
              {(() => {
                const active = templates.find((t) => t.id === form.template_id);
                if (!active) {
                  return (
                    <p className="text-xs text-muted-foreground">
                      Pick a template to prefill name, stages, and abort
                      criteria — you'll still need to pick a group and
                      target config below.
                    </p>
                  );
                }
                return (
                  <div className="space-y-1">
                    <p className="text-xs text-foreground">
                      {active.description}
                    </p>
                    <p className="text-[11px] text-muted-foreground leading-relaxed">
                      {active.when_to_use}
                    </p>
                  </div>
                );
              })()}
            </div>

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
              {/* Preview pane: shows the diff + lint findings between
                  the group's current effective config and the target
                  the operator just typed in. Renders inline so the
                  diff lands in front of the operator at exactly the
                  decision point. */}
              <RolloutPreviewPane
                groupId={form.group_id}
                targetConfigId={form.target_config_id.trim()}
              />
            </div>

            <div className="space-y-2">
              <Label>Stages</Label>
              <div className="space-y-3">
                {form.stages.map((stage, i) => {
                  const matches = form.group_id
                    ? matchAgents(allAgents, form.group_id, settledStages[i] ?? stage)
                    : [];
                  return (
                    <StageEditor
                      key={i}
                      index={i}
                      stage={stage}
                      groupSelected={Boolean(form.group_id)}
                      groupAgentCount={groupAgentCount}
                      matchCount={matches.length}
                      onChange={(next) => {
                        const arr = [...form.stages];
                        arr[i] = next;
                        setForm({ ...form, stages: arr });
                      }}
                      onRemove={() => {
                        if (form.stages.length <= 1) return;
                        const arr = form.stages.filter((_, j) => j !== i);
                        setForm({ ...form, stages: arr });
                      }}
                      removable={form.stages.length > 1}
                    />
                  );
                })}
                <Button
                  variant="outline"
                  size="sm"
                  className="gap-1"
                  onClick={() => {
                    // New stages copy the previous stage's mode so the
                    // mixed-mode rejection rule doesn't surprise operators.
                    const lastMode = form.stages[form.stages.length - 1]?.mode ?? "percent";
                    const next: FormStage =
                      lastMode === "label"
                        ? {
                            mode: "label",
                            percentage: "100",
                            selector_rows: [{ key: "", value: "" }],
                            dwell_seconds: "60",
                          }
                        : emptyPercentStage("100", "60");
                    setForm({ ...form, stages: [...form.stages, next] });
                  }}
                >
                  <Plus className="h-3.5 w-3.5" />
                  Add stage
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">
                Percent-mode stages are cumulative and must reach 100. Label
                mode stages must all use the label selector — mixed modes
                aren't supported.
              </p>
              {mixedModes && (
                <p className="text-xs text-amber-600">
                  Stages mix percent and label modes. Pick one mode for the
                  whole rollout before starting.
                </p>
              )}
              {stageZeroMatchesZero && (
                <p className="text-xs text-amber-600">
                  Stage 1's label selector matches 0 agents in this group. The
                  rollout can still be started — Squadron will record an
                  empty-canary audit event and the rollout will sit at stage 1
                  until matching agents appear.
                </p>
              )}
            </div>

            <div className="space-y-3 rounded-md border p-3">
              <div className="space-y-2">
                <Label htmlFor="recipe">Abort criteria recipe</Label>
                <Select
                  value={form.recipe_id || "custom"}
                  onValueChange={(v) => {
                    if (v === "custom") {
                      setForm({ ...form, recipe_id: "" });
                      return;
                    }
                    const recipe = recipes.find((r) => r.id === v);
                    if (!recipe) return;
                    setForm({
                      ...form,
                      recipe_id: recipe.id,
                      max_drifted_agents: String(recipe.criteria.max_drifted_agents),
                      max_error_logs_per_minute: String(
                        recipe.criteria.max_error_logs_per_minute ?? 0,
                      ),
                      min_dwell_seconds_before_abort: String(
                        recipe.criteria.min_dwell_seconds_before_abort ?? 0,
                      ),
                    });
                  }}
                >
                  <SelectTrigger id="recipe">
                    <SelectValue placeholder="Pick a recipe to prefill criteria..." />
                  </SelectTrigger>
                  <SelectContent>
                    {recipes.map((r) => (
                      <SelectItem key={r.id} value={r.id}>
                        {r.name}
                      </SelectItem>
                    ))}
                    {/* "Custom" lets the operator hand-tune. Stored as
                        empty recipe_id so the next render's selectedRecipe
                        lookup returns nothing and the hint hides. */}
                    <SelectItem value="custom">Custom (no recipe)</SelectItem>
                  </SelectContent>
                </Select>
                {(() => {
                  // Render the description for the active recipe so
                  // operators know what they're picking before they
                  // commit. IIFE avoids leaking a const into the JSX
                  // body just for one line of conditional render.
                  const active = recipes.find((r) => r.id === form.recipe_id);
                  if (!active) {
                    return (
                      <p className="text-xs text-muted-foreground">
                        Pick a recipe to prefill the criteria below, or
                        leave on "Custom" and tune each field yourself.
                      </p>
                    );
                  }
                  return (
                    <div className="space-y-1">
                      <p className="text-xs text-foreground">
                        {active.description}
                      </p>
                      <p className="text-[11px] text-muted-foreground leading-relaxed">
                        {active.when_to_use}
                      </p>
                    </div>
                  );
                })()}
              </div>

              <div className="grid gap-3 md:grid-cols-3 pt-1">
                <div className="space-y-1">
                  <Label htmlFor="maxdrift" className="text-xs">
                    Max drifted agents
                  </Label>
                  <Input
                    id="maxdrift"
                    type="number"
                    min={0}
                    value={form.max_drifted_agents}
                    onChange={(e) =>
                      setForm({
                        ...form,
                        // Hand-edits clear the recipe selection — what's
                        // in the form no longer matches any cookbook entry.
                        recipe_id: "",
                        max_drifted_agents: e.target.value,
                      })
                    }
                  />
                </div>
                <div className="space-y-1">
                  <Label htmlFor="maxerr" className="text-xs">
                    Max errors / min
                  </Label>
                  <Input
                    id="maxerr"
                    type="number"
                    min={0}
                    value={form.max_error_logs_per_minute}
                    onChange={(e) =>
                      setForm({
                        ...form,
                        recipe_id: "",
                        max_error_logs_per_minute: e.target.value,
                      })
                    }
                  />
                </div>
                <div className="space-y-1">
                  <Label htmlFor="warmup" className="text-xs">
                    Warmup (s)
                  </Label>
                  <Input
                    id="warmup"
                    type="number"
                    min={0}
                    value={form.min_dwell_seconds_before_abort}
                    onChange={(e) =>
                      setForm({
                        ...form,
                        recipe_id: "",
                        min_dwell_seconds_before_abort: e.target.value,
                      })
                    }
                  />
                </div>
              </div>
              <p className="text-[11px] text-muted-foreground">
                Drift threshold is checked every tick. Error-rate threshold
                only fires after the warmup elapses so newly-pushed agents
                have time to flush startup noise. Set max errors/min to 0
                to disable the error-rate check.
              </p>
            </div>

            <div className="space-y-2">
              <Label htmlFor="notif">Notification webhook (optional)</Label>
              <Input
                id="notif"
                value={form.notification_url}
                onChange={(e) =>
                  setForm({ ...form, notification_url: e.target.value })
                }
                placeholder="https://hooks.example.com/squadron-rollouts"
              />
              <p className="text-xs text-muted-foreground">
                Squadron POSTs a JSON payload on every state transition.
              </p>
            </div>

            {/* v0.47 — approval workflow toggle. Defaults off so the
                form behaves as it did before. When checked, the rollout
                enters pending_approval; a second person (not the
                requester) has to call Approve on it before the engine
                advances. The two-person rule is enforced server-side. */}
            <div className="space-y-2 rounded-md border border-orange-500/20 bg-orange-500/5 p-3">
              <label className="flex items-start gap-2 cursor-pointer">
                <input
                  type="checkbox"
                  className="mt-0.5 h-4 w-4"
                  checked={form.require_approval}
                  onChange={(e) =>
                    setForm({ ...form, require_approval: e.target.checked })
                  }
                />
                <div className="space-y-0.5">
                  <div className="text-sm font-medium">
                    Require approval before rollout starts
                  </div>
                  <p className="text-[11px] text-muted-foreground leading-relaxed">
                    The rollout will enter <code>pending_approval</code> and
                    won't advance until a different operator approves it
                    (two-person rule). Use this for production-impacting or
                    NERC CIP–regulated changes that require change-control
                    sign-off.
                  </p>
                </div>
              </label>
            </div>

            {submitError && (
              <div className="text-sm text-red-600">{submitError}</div>
            )}

            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={reset} disabled={submitting}>
                Cancel
              </Button>
              <Button onClick={submit} disabled={submitting || mixedModes}>
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
            onPauseResume={handlePauseResume}
            onApprove={handleApprove}
            onReject={handleReject}
          />
        ))}
    </div>
  );
}

// StageEditor renders one stage row with a mode toggle, the appropriate
// editor for that mode, and a live "matches N of M" hint when a group is
// selected. Kept separate so the parent's render stays scannable.
interface StageEditorProps {
  index: number;
  stage: FormStage;
  groupSelected: boolean;
  groupAgentCount: number;
  matchCount: number;
  onChange: (next: FormStage) => void;
  onRemove: () => void;
  removable: boolean;
}

function StageEditor({
  index,
  stage,
  groupSelected,
  groupAgentCount,
  matchCount,
  onChange,
  onRemove,
  removable,
}: StageEditorProps) {
  const matchHint = groupSelected ? (
    matchCount === 0 ? (
      <span className="text-amber-600">
        matches 0 of {groupAgentCount} agents
      </span>
    ) : (
      <span className="text-emerald-700">
        matches {matchCount} of {groupAgentCount} agents
      </span>
    )
  ) : (
    <span>pick a group above to preview matches</span>
  );

  return (
    <div className="rounded-md border p-3 space-y-3">
      <div className="flex items-center justify-between">
        <div className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
          Stage {index + 1}
        </div>
        <div className="flex items-center gap-2">
          {/* Mode toggle: button group, not Select, so it's one click. */}
          <div className="inline-flex rounded-md border bg-muted/40 p-0.5">
            <button
              type="button"
              className={`px-2.5 py-0.5 text-xs rounded ${
                stage.mode === "percent"
                  ? "bg-background shadow text-foreground"
                  : "text-muted-foreground"
              }`}
              onClick={() => onChange({ ...stage, mode: "percent" })}
            >
              Percent
            </button>
            <button
              type="button"
              className={`px-2.5 py-0.5 text-xs rounded ${
                stage.mode === "label"
                  ? "bg-background shadow text-foreground"
                  : "text-muted-foreground"
              }`}
              onClick={() => onChange({ ...stage, mode: "label" })}
            >
              Label selector
            </button>
          </div>
          <Button
            variant="ghost"
            size="icon"
            onClick={onRemove}
            disabled={!removable}
            title="Remove stage"
          >
            <Trash2 className="h-4 w-4" />
          </Button>
        </div>
      </div>

      {stage.mode === "percent" ? (
        <div className="grid gap-2 md:grid-cols-2">
          <div className="space-y-1">
            <Label htmlFor={`stage-pct-${index}`} className="text-xs">
              % of group
            </Label>
            <Input
              id={`stage-pct-${index}`}
              type="number"
              min={1}
              max={100}
              value={stage.percentage}
              onChange={(e) => onChange({ ...stage, percentage: e.target.value })}
            />
          </div>
          <div className="space-y-1">
            <Label htmlFor={`stage-dwell-${index}`} className="text-xs">
              Dwell (s)
            </Label>
            <Input
              id={`stage-dwell-${index}`}
              type="number"
              min={0}
              value={stage.dwell_seconds}
              onChange={(e) => onChange({ ...stage, dwell_seconds: e.target.value })}
            />
          </div>
        </div>
      ) : (
        <div className="space-y-2">
          <Label className="text-xs">Label selector (all keys AND-matched)</Label>
          <div className="space-y-1.5">
            {stage.selector_rows.map((row, i) => (
              <div key={i} className="flex items-center gap-2">
                <Input
                  placeholder="key (e.g. host.name)"
                  value={row.key}
                  className="font-mono text-sm"
                  onChange={(e) => {
                    const rows = [...stage.selector_rows];
                    rows[i] = { ...rows[i], key: e.target.value };
                    onChange({ ...stage, selector_rows: rows });
                  }}
                />
                <span className="text-muted-foreground text-xs">=</span>
                <Input
                  placeholder="value (e.g. canary-1)"
                  value={row.value}
                  className="font-mono text-sm"
                  onChange={(e) => {
                    const rows = [...stage.selector_rows];
                    rows[i] = { ...rows[i], value: e.target.value };
                    onChange({ ...stage, selector_rows: rows });
                  }}
                />
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={() => {
                    if (stage.selector_rows.length <= 1) {
                      // Last row — clear it rather than removing so the
                      // editor always shows at least one input pair.
                      onChange({ ...stage, selector_rows: [{ key: "", value: "" }] });
                      return;
                    }
                    const rows = stage.selector_rows.filter((_, j) => j !== i);
                    onChange({ ...stage, selector_rows: rows });
                  }}
                  title="Remove selector row"
                >
                  <Trash2 className="h-4 w-4" />
                </Button>
              </div>
            ))}
            <Button
              variant="outline"
              size="sm"
              className="gap-1"
              onClick={() =>
                onChange({
                  ...stage,
                  selector_rows: [
                    ...stage.selector_rows,
                    { key: "", value: "" },
                  ],
                })
              }
            >
              <Plus className="h-3 w-3" />
              Add label
            </Button>
          </div>
          <div className="space-y-1">
            <Label htmlFor={`stage-dwell-${index}`} className="text-xs">
              Dwell (s)
            </Label>
            <Input
              id={`stage-dwell-${index}`}
              type="number"
              min={0}
              value={stage.dwell_seconds}
              onChange={(e) => onChange({ ...stage, dwell_seconds: e.target.value })}
              className="max-w-40"
            />
          </div>
        </div>
      )}

      <div className="text-xs text-muted-foreground">{matchHint}</div>
    </div>
  );
}

interface RolloutCardProps {
  rollout: Rollout;
  groupName: string;
  onAbort: (r: Rollout) => void;
  onPauseResume: (r: Rollout) => void;
  // v0.47 — approval workflow. Only used when rollout.state ===
  // "pending_approval"; the card hides them otherwise.
  onApprove: (r: Rollout) => void;
  onReject: (r: Rollout) => void;
}

function RolloutCard({
  rollout: r,
  groupName,
  onAbort,
  onPauseResume,
  onApprove,
  onReject,
}: RolloutCardProps) {
  const [historyOpen, setHistoryOpen] = useState(false);
  const totalStages = r.stages.length;
  const currentStage = r.stages[Math.min(r.current_stage, totalStages - 1)];
  const isActive =
    r.state === "in_progress" || r.state === "pending" || r.state === "paused";
  const isPaused = r.state === "paused";

  // Stage summary text varies by mode. We render this in both the header
  // and in the per-stage tooltips below.
  const summarizeStage = (s: RolloutStage, idx: number): string => {
    const mode = s.mode ?? "percent";
    const dwell = `${s.dwell_seconds}s dwell`;
    if (mode === "label") {
      const pairs = Object.entries(s.label_selector ?? {})
        .map(([k, v]) => `${k}=${v}`)
        .join(", ");
      return `Stage ${idx + 1}: label[${pairs || "?"}] · ${dwell}`;
    }
    return `Stage ${idx + 1}: ${s.percentage ?? 0}% · ${dwell}`;
  };

  const currentMode = currentStage?.mode ?? "percent";
  const currentSummary =
    currentMode === "label"
      ? `label selector: ${Object.entries(currentStage?.label_selector ?? {})
          .map(([k, v]) => `${k}=${v}`)
          .join(", ") || "(empty)"}`
      : `${currentStage?.percentage ?? 0}% of group`;

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
            <div className="flex items-center gap-2">
              <Button
                variant="outline"
                size="sm"
                className="gap-1"
                onClick={() => onPauseResume(r)}
              >
                {isPaused ? (
                  <>
                    <Play className="h-3.5 w-3.5" />
                    Resume
                  </>
                ) : (
                  <>
                    <Pause className="h-3.5 w-3.5" />
                    Pause
                  </>
                )}
              </Button>
              <Button
                variant="outline"
                size="sm"
                className="gap-1"
                onClick={() => onAbort(r)}
              >
                Abort
              </Button>
            </div>
          )}
          {/* v0.47 — approval action buttons. The server enforces the
              two-person rule (requester ≠ approver); we surface a
              tooltip so the requester knows why the buttons will 409. */}
          {r.state === "pending_approval" && (
            <div className="flex items-center gap-2">
              <Button
                variant="outline"
                size="sm"
                className="gap-1 border-emerald-500/30 text-emerald-700 hover:bg-emerald-500/10"
                onClick={() => onApprove(r)}
              >
                <Check className="h-3.5 w-3.5" />
                Approve
              </Button>
              <Button
                variant="outline"
                size="sm"
                className="gap-1 border-red-500/30 text-red-700 hover:bg-red-500/10"
                onClick={() => onReject(r)}
              >
                <X className="h-3.5 w-3.5" />
                Reject
              </Button>
            </div>
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
                title={summarizeStage(stage, i)}
              />
            );
          })}
        </div>
        <div className="text-xs text-muted-foreground">{currentSummary}</div>

        {/* v0.47 — approval banner. Surfaces the requester (for the
            two-person comparison) and, once an approval/rejection has
            happened, who did it and when. */}
        {(r.require_approval || r.state === "pending_approval" || r.approved_by || r.rejected_by) && (
          <div className="rounded border border-orange-500/20 bg-orange-500/5 px-2.5 py-1.5 text-[11px] space-y-0.5">
            {r.requested_by && (
              <div>
                <span className="text-muted-foreground">Requested by:</span>{" "}
                <span className="font-mono">{r.requested_by}</span>
              </div>
            )}
            {r.state === "pending_approval" && (
              <div className="text-orange-700">
                Waiting on a second approver. The requester cannot approve
                their own rollout.
              </div>
            )}
            {r.approved_by && (
              <div className="text-emerald-700">
                Approved by{" "}
                <span className="font-mono">{r.approved_by}</span>
                {r.approved_at &&
                  ` · ${new Date(r.approved_at).toLocaleString()}`}
                {r.approval_notes && ` · "${r.approval_notes}"`}
              </div>
            )}
            {r.rejected_by && (
              <div className="text-red-700">
                Rejected by{" "}
                <span className="font-mono">{r.rejected_by}</span>
                {r.rejected_at &&
                  ` · ${new Date(r.rejected_at).toLocaleString()}`}
                {r.approval_notes && ` · "${r.approval_notes}"`}
              </div>
            )}
          </div>
        )}

        {/* History toggle — mounts an AuditTimeline filtered to this
            rollout. Includes the per-stage resolved agent IDs so a
            post-mortem can answer "who got pushed at each stage?". */}
        <button
          type="button"
          className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          onClick={() => setHistoryOpen((v) => !v)}
        >
          {historyOpen ? (
            <ChevronDown className="h-3 w-3" />
          ) : (
            <ChevronRight className="h-3 w-3" />
          )}
          {historyOpen ? "Hide history" : "Show history"}
        </button>
        {historyOpen && (
          <div className="border-t pt-2">
            <AuditTimeline
              targetType="rollout"
              targetId={r.id}
              limit={100}
            />
          </div>
        )}
      </CardContent>
    </Card>
  );
}
