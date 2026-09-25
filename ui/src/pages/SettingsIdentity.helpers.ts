// Pure helpers for the RBAC role create drawer, split out of SettingsIdentity so
// the page file only exports its component (react-refresh) and so this logic is
// unit-testable without rendering the page.

import type { Permission } from "@/api/rbac";

// A permission row in the role create drawer. resource_ids is kept as raw text
// (comma / newline separated) and parsed on submit.
export interface PermRow {
  scope: string;
  resource_type: string;
  allResources: boolean;
  resourceIdsText: string;
  // ADR 0053 — optional cluster/environment scope for this permission.
  env: string;
  cluster: string;
}

export const emptyPermRow = (): PermRow => ({
  scope: "",
  resource_type: "",
  allResources: true,
  resourceIdsText: "",
  env: "",
  cluster: "",
});

// labelMatchSummary renders a permission's cluster/env scope as a compact suffix,
// e.g. " [env=prod, cluster=us-west-2]". Empty when the permission is
// label-agnostic.
export const labelMatchSummary = (lm?: Record<string, string>): string => {
  if (!lm) return "";
  const parts: string[] = [];
  if (lm["deployment.environment"])
    parts.push(`env=${lm["deployment.environment"]}`);
  if (lm["k8s.cluster.name"]) parts.push(`cluster=${lm["k8s.cluster.name"]}`);
  return parts.length ? ` [${parts.join(", ")}]` : "";
};

export const permLabel = (p: Permission): string => {
  const base = p.scope + (p.resource_type ? `@${p.resource_type}` : "");
  return (
    base +
    (p.all_resources ? " (all)" : ` (${p.resource_ids.length} ids)`) +
    labelMatchSummary(p.label_match)
  );
};

const parseIds = (text: string): string[] =>
  text
    .split(/[,\n]/)
    .map((s) => s.trim())
    .filter(Boolean);

// permRowsToPermissions converts the drawer's editable rows into the RBAC API
// Permission shape. Rows with a blank scope are dropped. label_match is emitted
// only when env and/or cluster is set (ADR 0053), so a label-agnostic permission
// carries no cluster/env key on the wire.
export const permRowsToPermissions = (rows: PermRow[]): Permission[] =>
  rows
    .filter((r) => r.scope.trim() !== "")
    .map((r) => {
      const labelMatch: Record<string, string> = {};
      if (r.env.trim()) labelMatch["deployment.environment"] = r.env.trim();
      if (r.cluster.trim()) labelMatch["k8s.cluster.name"] = r.cluster.trim();
      return {
        scope: r.scope.trim(),
        resource_type: r.resource_type.trim(),
        all_resources: r.allResources,
        resource_ids: r.allResources ? [] : parseIds(r.resourceIdsText),
        ...(Object.keys(labelMatch).length > 0
          ? { label_match: labelMatch }
          : {}),
      };
    });
