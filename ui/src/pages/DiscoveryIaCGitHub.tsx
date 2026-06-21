// DiscoveryIaCGitHub — the v0.89.3 #603 Stream 19 IaC connections list
// page. Sits next to /discovery/aws under the Discovery group; mirrors
// the AWS page's Account-tab posture: a list of connected repos with a
// "Connect IaC repo" CTA that opens the wizard inside a dialog.
//
// Slice 1 ships GitHub only. The page key is /discovery/iac/github so
// future slices (GitLab / Bitbucket) can land under /discovery/iac/<p>
// without renaming routes.

import { ExternalLink, Github, Sparkles, Trash2 } from "lucide-react";
import { useCallback, useState } from "react";
import useSWR from "swr";

import {
  deleteIaCGitHubConnection,
  type IaCGitHubConnection,
  listIaCGitHubConnections,
} from "@/api/iacGithub";
import { IaCGitHubWizard } from "@/components/discovery/IaCGitHubWizard";
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
  const [open, setOpen] = useState(false);
  const [deleting, setDeleting] = useState<IaCGitHubConnection | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [deletePending, setDeletePending] = useState(false);

  const connections = data?.connections ?? [];

  const onWizardComplete = useCallback(() => {
    // Refresh the list so the new row lands. We delay the close by one
    // tick so the Connected card has a moment to render — same UX as
    // the AWS wizard.
    void mutate();
    setTimeout(() => setOpen(false), 1200);
  }, [mutate]);

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
        <Dialog open={open} onOpenChange={setOpen}>
          <Button onClick={() => setOpen(true)}>Connect IaC repo</Button>
          <DialogContent className="max-w-2xl">
            <DialogHeader>
              <DialogTitle>Connect IaC repository</DialogTitle>
              <DialogDescription>
                Walk through the six steps to grant Squadron PR access
                via a GitHub Personal Access Token.
              </DialogDescription>
            </DialogHeader>
            <IaCGitHubWizard onComplete={onWizardComplete} />
          </DialogContent>
        </Dialog>
      </div>

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
