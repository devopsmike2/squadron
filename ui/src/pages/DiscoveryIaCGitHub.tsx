// DiscoveryIaCGitHub — the v0.89.3 #603 Stream 19 IaC connections list
// page. Sits next to /discovery/aws under the Discovery group; mirrors
// the AWS page's Account-tab posture: a list of connected repos with a
// "Connect IaC repo" CTA that opens the wizard inside a dialog.
//
// Slice 1 ships GitHub only. The page key is /discovery/iac/github so
// future slices (GitLab / Bitbucket) can land under /discovery/iac/<p>
// without renaming routes.
//
// v0.89.4 (#610) — query-param deep link. Landing on
//   /discovery/iac/github?connection_id=<uuid>&step=placement&kind=<resource_kind>
// auto-opens the wizard dialog in placement-only edit mode, pre-fills
// with the connection's existing rows, and scrolls the row for the
// target resource_kind into view + outlines it. The deep link is the
// land-target for the DiscoveryAWS "Configure placement" /
// NoPlacementMapping recovery hint (#610 closes the Phase-4 stopgap).
//
// Invariants:
//   - The bare /discovery/iac/github URL (no query params) still
//     renders the connections list — regression-guarded.
//   - A stale connection_id (no row in the SWR list) surfaces an
//     inline "That connection no longer exists" notice; the page
//     still renders the connections list.
//   - An unknown ?kind=... value is silently dropped; the wizard
//     still opens at the placement step, just without a focused row.
//   - On dialog close the query params are stripped via replaceState
//     so the back button doesn't re-trigger the wizard.

import { AlertTriangle, ExternalLink, Github, Sparkles, Trash2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import useSWR from "swr";

import {
  deleteIaCGitHubConnection,
  type IaCGitHubConnection,
  listIaCGitHubConnections,
} from "@/api/iacGithub";
import {
  IaCGitHubWizard,
  type IaCGitHubWizardEditPlacementMode,
} from "@/components/discovery/IaCGitHubWizard";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { IAC_GITHUB_PLACEMENT_KINDS } from "@/data/iacGithubWizard";

// Canonical resource_kind set (the seven slice-1 kinds in
// data/iacGithubWizard.ts). The deep link's ?kind=<...> param is
// validated against this set; an unknown value is silently dropped.
const CANONICAL_KINDS = new Set(
  IAC_GITHUB_PLACEMENT_KINDS.map((k) => k.resource_kind),
);

// Deep-link query-param keys. Re-declared so a typo at the link
// site flags at TS check time rather than at runtime.
const QP_CONNECTION_ID = "connection_id";
const QP_STEP = "step";
const QP_KIND = "kind";
const QP_STEP_PLACEMENT = "placement";

// SWR cache key for the connections list. Exported because the wizard
// dialog mutate()s on save so the new row lands without a refresh.
export const IAC_GITHUB_CONNECTIONS_SWR_KEY = "/iac/github/connections";

// formatTime mirrors the helper in DiscoveryAWS.tsx — relative for
// recent, locale date for older. Inlined to avoid a shared utility for
// two callers.
function formatTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return iso;
  const secs = Math.floor((Date.now() - t) / 1000);
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return new Date(iso).toLocaleDateString();
}

export default function DiscoveryIaCGitHubPage() {
  const { data, error, isLoading, mutate } = useSWR(
    IAC_GITHUB_CONNECTIONS_SWR_KEY,
    () => listIaCGitHubConnections(),
  );
  const location = useLocation();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [deleting, setDeleting] = useState<IaCGitHubConnection | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [deletePending, setDeletePending] = useState(false);
  // Stale-deep-link notice. When the URL points at a connection_id
  // that doesn't exist in the SWR list, the page renders the
  // connections list AND shows this banner so the operator's not
  // left wondering why the wizard didn't open.
  const [staleNotice, setStaleNotice] = useState<string | null>(null);

  // Wrap in useMemo so the array identity is stable across renders —
  // the useMemo/useEffect hooks below key off `connections` and a
  // fresh array per render would tank them.
  const connections = useMemo(
    () => data?.connections ?? [],
    [data?.connections],
  );

  // Parse the URL query params on every render (cheap). The three
  // shapes that lead to "auto-open the wizard in placement-edit
  // mode":
  //   1. connection_id present + step=placement + connection found:
  //      build editMode with the matched connection's rows.
  //   2. connection_id present + step=placement + connection NOT
  //      found (and list has finished loading): set staleNotice.
  //   3. connection_id present + step=placement + kind=<canonical>:
  //      focused row = kind. Unknown kind → focused row = null
  //      (open at placement step, but don't highlight anything).
  // Anything else (bare URL, partial query) renders the bare list.
  const editMode: IaCGitHubWizardEditPlacementMode | null = useMemo(() => {
    const params = new URLSearchParams(location.search);
    const connID = params.get(QP_CONNECTION_ID);
    const step = params.get(QP_STEP);
    if (!connID || step !== QP_STEP_PLACEMENT) return null;
    // Wait for the SWR list to settle before deciding stale-or-found.
    // While isLoading, return null and the open-effect below holds off.
    if (isLoading) return null;
    const conn = connections.find((c) => c.connection_id === connID);
    if (!conn) return null;
    const kindParam = params.get(QP_KIND);
    const focusedResourceKind =
      kindParam && CANONICAL_KINDS.has(kindParam) ? kindParam : null;
    return {
      kind: "edit-placement",
      connectionID: conn.connection_id,
      repoFullName: conn.repo_full_name,
      repoLayout:
        conn.repo_layout === "mono" || conn.repo_layout === "multi"
          ? conn.repo_layout
          : "multi",
      initialRows: conn.placement_map,
      focusedResourceKind,
    };
  }, [connections, isLoading, location.search]);

  // Stale-link detection runs separately so we can post the notice
  // even when editMode is null (the wizard won't auto-open).
  useEffect(() => {
    if (isLoading) return;
    const params = new URLSearchParams(location.search);
    const connID = params.get(QP_CONNECTION_ID);
    const step = params.get(QP_STEP);
    if (!connID || step !== QP_STEP_PLACEMENT) {
      setStaleNotice(null);
      return;
    }
    const found = connections.some((c) => c.connection_id === connID);
    if (!found) {
      setStaleNotice(
        "That IaC connection no longer exists. The link may be stale — pick a connection from the list below or run the wizard again.",
      );
    } else {
      setStaleNotice(null);
    }
  }, [connections, isLoading, location.search]);

  // stripQueryParams removes the deep-link params from the URL
  // without pushing a new history entry. We use this on dialog close
  // so the operator's back button doesn't re-trigger the wizard;
  // pushing a new entry would mean a duplicate Back press to leave
  // the page.
  const stripQueryParams = useCallback(() => {
    if (!location.search) return;
    navigate(location.pathname, { replace: true });
  }, [location.pathname, location.search, navigate]);

  // Auto-open the wizard when a valid deep link landed. The effect
  // fires once per editMode identity (which itself only changes when
  // connections / search params change).
  useEffect(() => {
    if (editMode) {
      setOpen(true);
    }
  }, [editMode]);

  const onWizardComplete = useCallback(() => {
    // Refresh the list so the new row lands. We delay the close by one
    // tick so the Connected card has a moment to render — same UX as
    // the AWS wizard. The deep-link's edit-mode flow also calls the
    // same mutate; the page's stale-notice effect will quiet down
    // once the list reflects the saved row.
    void mutate();
    setTimeout(() => {
      setOpen(false);
      stripQueryParams();
    }, 1200);
  }, [mutate, stripQueryParams]);

  // Dialog onOpenChange — when the operator closes via the X button
  // or ESC, strip the query params too so the back button doesn't
  // re-trigger.
  const onDialogOpenChange = useCallback(
    (next: boolean) => {
      setOpen(next);
      if (!next) stripQueryParams();
    },
    [stripQueryParams],
  );

  const onDeleteConfirm = useCallback(async () => {
    if (!deleting) return;
    setDeletePending(true);
    setDeleteError(null);
    try {
      await deleteIaCGitHubConnection(deleting.connection_id);
      await mutate();
      setDeleting(null);
    } catch (e) {
      setDeleteError(e instanceof Error ? e.message : String(e));
    } finally {
      setDeletePending(false);
    }
  }, [deleting, mutate]);

  return (
    <div className="space-y-4 p-6">
      <header>
        <div className="flex items-center gap-2">
          <Github className="h-5 w-5 text-violet-500" aria-hidden />
          <h1 className="text-2xl font-semibold">IaC Connections</h1>
        </div>
        <p className="text-sm text-muted-foreground">
          Connect a Terraform repository so Squadron can open PRs from
          recommendations.
        </p>
      </header>

      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-base font-semibold">Connected repositories</h2>
          <p className="text-xs text-muted-foreground">
            One row per IaC connection. Slice 1 supports GitHub only.
          </p>
        </div>
        <Dialog open={open} onOpenChange={onDialogOpenChange}>
          <Button onClick={() => setOpen(true)}>Connect IaC repo</Button>
          <DialogContent className="max-w-2xl">
            <DialogHeader>
              <DialogTitle>
                {editMode ? "Edit placement map" : "Connect IaC repository"}
              </DialogTitle>
              <DialogDescription>
                {editMode
                  ? "Update which Terraform file each resource kind appends to. The connection's token, branch prefix, and reviewer team are unchanged."
                  : "Walk through the six steps to grant Squadron PR access via a GitHub Personal Access Token."}
              </DialogDescription>
            </DialogHeader>
            <IaCGitHubWizard
              onComplete={onWizardComplete}
              editMode={editMode ?? undefined}
            />
          </DialogContent>
        </Dialog>
      </div>

      {staleNotice && (
        <Card role="alert" aria-live="polite">
          <CardContent className="flex items-start gap-2 p-4 text-sm">
            <AlertTriangle
              className="mt-0.5 h-4 w-4 shrink-0 text-yellow-600 dark:text-yellow-400"
              aria-hidden
            />
            <p>{staleNotice}</p>
          </CardContent>
        </Card>
      )}

      {error && (
        <Card>
          <CardContent className="p-4 text-sm text-destructive">
            Could not load IaC connections: {String(error)}
          </CardContent>
        </Card>
      )}

      {isLoading && (
        <div className="space-y-2">
          <Skeleton className="h-20 w-full" />
          <Skeleton className="h-20 w-full" />
        </div>
      )}

      {!isLoading && connections.length === 0 && <EmptyState />}

      {!isLoading && connections.length > 0 && (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          {connections.map((c) => (
            <ConnectionCard
              key={c.connection_id}
              conn={c}
              onDelete={() => setDeleting(c)}
            />
          ))}
        </div>
      )}

      {/* Delete-confirm modal. Mirrors the AWS page's posture; slice 1
          ships explicit confirm rather than a typed-confirmation
          challenge because the row is recoverable (re-run wizard). */}
      <Dialog
        open={!!deleting}
        onOpenChange={(o) => {
          if (!o) {
            setDeleting(null);
            setDeleteError(null);
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete IaC connection</DialogTitle>
            <DialogDescription>
              This removes the connection and its stored token. Open-PR
              actions for{" "}
              <code className="rounded bg-muted px-1 py-0.5 text-xs">
                {deleting?.repo_full_name}
              </code>{" "}
              will stop working until you re-run the wizard.
            </DialogDescription>
          </DialogHeader>
          {deleteError && (
            <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
              Delete failed: {deleteError}
            </div>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="ghost"
              onClick={() => setDeleting(null)}
              disabled={deletePending}
            >
              Cancel
            </Button>
            <Button
              type="button"
              variant="destructive"
              onClick={onDeleteConfirm}
              disabled={deletePending}
            >
              <Trash2 className="mr-1 h-4 w-4" aria-hidden />
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function ConnectionCard({
  conn,
  onDelete,
}: {
  conn: IaCGitHubConnection;
  onDelete: () => void;
}) {
  const repoHref = `https://github.com/${conn.repo_full_name}`;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <a
            href={repoHref}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 hover:underline"
          >
            {conn.repo_full_name}
            <ExternalLink className="h-3 w-3" aria-hidden />
          </a>
        </CardTitle>
        <CardDescription>
          Default branch{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
            {conn.default_branch}
          </code>
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        <div className="flex flex-wrap items-center gap-1">
          <Badge variant="outline" className="text-[10px]">
            {conn.repo_layout}
          </Badge>
          <Badge variant="outline" className="text-[10px]">
            {conn.auth_kind}
          </Badge>
          <Badge variant="secondary" className="text-[10px]">
            {conn.placement_map.length} placement
            {conn.placement_map.length === 1 ? "" : "s"}
          </Badge>
          {conn.branch_prefix && (
            <Badge variant="outline" className="text-[10px]">
              prefix <code>{conn.branch_prefix}</code>
            </Badge>
          )}
        </div>
        {conn.reviewer_team_handle && (
          <p className="text-xs text-muted-foreground">
            Reviewers:{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-[10px]">
              {conn.reviewer_team_handle}
            </code>
          </p>
        )}
        <p className="text-xs text-muted-foreground">
          Connected {formatTime(conn.created_at)}
        </p>
        <div className="pt-1">
          <Button
            type="button"
            size="sm"
            variant="ghost"
            className="text-xs text-destructive hover:text-destructive"
            onClick={onDelete}
            aria-label={`Delete connection for ${conn.repo_full_name}`}
          >
            <Trash2 className="mr-1 h-3.5 w-3.5" aria-hidden />
            Delete
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

function EmptyState() {
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-3 p-8 text-center">
        <Github className="h-10 w-10 text-muted-foreground" aria-hidden />
        <div>
          <h3 className="text-base font-semibold">
            No IaC repositories connected yet.
          </h3>
          <p className="mt-1 text-sm text-muted-foreground">
            Click &quot;Connect IaC repo&quot; to wire one up.
          </p>
        </div>
        <div className="rounded-md border bg-muted/30 p-3 text-xs text-muted-foreground">
          <Sparkles
            className="mr-1 inline-block h-3 w-3 text-violet-500"
            aria-hidden
          />
          Squadron opens pull requests against your repo&apos;s branches —
          never your default branch. Your branch protection and CI stay
          in charge of merges and applies.
        </div>
      </CardContent>
    </Card>
  );
}
