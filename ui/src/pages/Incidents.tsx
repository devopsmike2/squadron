// Squadron Incidents page (Move 3 of the engineer copilot roadmap).
//
// After Squadron's action runner restarts a service or applies a
// config push on a node, the AI drafter writes a postmortem-style
// ticket and the bridge persists it as an IncidentDraft. This page
// is the operator inbox: list the drafts, open one to see the
// rendered body, edit if needed, then publish through clipboard or
// to a tracked external system (Linear, Jira, GitHub).
//
// Two pane layout. Left: status tabs + drafts list. Right: detail
// with title, rendered markdown body, edit and publish actions. SWR
// polls every 30 seconds so a fresh draft from the bridge appears
// without a manual refresh.

import { useMemo, useState } from "react";
import ReactMarkdown from "react-markdown";
import useSWR, { mutate } from "swr";

import {
  dismissIncidentDraft,
  listIncidentDrafts,
  patchIncidentDraft,
  publishIncidentDraft,
} from "@/api/incidents";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import type {
  IncidentDraft,
  IncidentDraftStatus,
  IncidentProvider,
} from "@/types/incident";

const TABS: { value: IncidentDraftStatus; label: string }[] = [
  { value: "draft", label: "Drafts" },
  { value: "published", label: "Published" },
  { value: "dismissed", label: "Dismissed" },
];

const swrKey = (status: IncidentDraftStatus) => `/incidents/drafts?status=${status}`;

const statusToneClass: Record<IncidentDraftStatus, string> = {
  draft: "bg-amber-500/10 text-amber-700 border-amber-500/20",
  published: "bg-emerald-500/10 text-emerald-700 border-emerald-500/20",
  dismissed: "bg-zinc-500/10 text-zinc-600 border-zinc-500/20",
};

function StatusBadge({ status }: { status: IncidentDraftStatus }) {
  return (
    <Badge variant="outline" className={statusToneClass[status]}>
      {status}
    </Badge>
  );
}

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

export default function IncidentsPage() {
  const [activeTab, setActiveTab] = useState<IncidentDraftStatus>("draft");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [editTitle, setEditTitle] = useState("");
  const [editBody, setEditBody] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  // Publish form fields.
  const [publishProvider, setPublishProvider] =
    useState<IncidentProvider>("clipboard");
  const [publishExternalID, setPublishExternalID] = useState("");
  const [publishExternalURL, setPublishExternalURL] = useState("");

  const { data: drafts, isLoading, error } = useSWR<IncidentDraft[]>(
    swrKey(activeTab),
    () => listIncidentDrafts({ status: activeTab }),
    {
      // The bridge ticks every 60s; poll a little faster so a new
      // draft shows up within ~30s of the action completing.
      refreshInterval: 30_000,
    },
  );

  const selected = useMemo(() => {
    if (!selectedId || !drafts) return null;
    return drafts.find((d) => d.id === selectedId) ?? null;
  }, [drafts, selectedId]);

  const refreshAll = () => {
    TABS.forEach((t) => {
      void mutate(swrKey(t.value));
    });
  };

  const startEdit = () => {
    if (!selected) return;
    setEditTitle(selected.title);
    setEditBody(selected.body_markdown);
    setEditing(true);
    setActionError(null);
  };

  const cancelEdit = () => {
    setEditing(false);
    setActionError(null);
  };

  const saveEdit = async () => {
    if (!selected) return;
    setSubmitting(true);
    setActionError(null);
    try {
      await patchIncidentDraft(selected.id, {
        title: editTitle,
        body_markdown: editBody,
      });
      setEditing(false);
      refreshAll();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  };

  const dismiss = async () => {
    if (!selected) return;
    setSubmitting(true);
    setActionError(null);
    try {
      await dismissIncidentDraft(selected.id);
      setSelectedId(null);
      refreshAll();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  };

  const copyToClipboard = async () => {
    if (!selected) return;
    try {
      await navigator.clipboard.writeText(selected.body_markdown);
    } catch {
      setActionError(
        "Could not access the system clipboard. " +
          "Copy the body manually below.",
      );
    }
  };

  const publish = async () => {
    if (!selected) return;
    setSubmitting(true);
    setActionError(null);
    try {
      // For the clipboard provider we also push the body onto the
      // OS clipboard as a convenience. The handler stamps the row
      // as published regardless; if the user denied clipboard
      // access we still record their intent.
      if (publishProvider === "clipboard") {
        try {
          await navigator.clipboard.writeText(selected.body_markdown);
        } catch {
          // Non-fatal; the clipboard API can be blocked in some
          // contexts. Continue with the server-side publish so
          // the audit log captures the operator's decision.
        }
      }
      await publishIncidentDraft(selected.id, {
        provider: publishProvider,
        external_id: publishExternalID || undefined,
        external_url: publishExternalURL || undefined,
      });
      setSelectedId(null);
      setPublishExternalID("");
      setPublishExternalURL("");
      setPublishProvider("clipboard");
      refreshAll();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="flex h-full flex-col gap-4 p-6">
      <div>
        <h1 className="text-2xl font-semibold">Incidents</h1>
        <p className="text-muted-foreground text-sm">
          When Squadron's action runner restarts a service or applies a
          fix, the AI drafter writes a postmortem ticket here for
          review. Edit, dismiss, or publish to your team's ticketing
          system.
        </p>
      </div>

      <div className="flex gap-2">
        {TABS.map((t) => {
          const isActive = activeTab === t.value;
          return (
            <Button
              key={t.value}
              variant={isActive ? "default" : "outline"}
              size="sm"
              onClick={() => {
                setActiveTab(t.value);
                setSelectedId(null);
                setEditing(false);
              }}
            >
              {t.label}
            </Button>
          );
        })}
      </div>

      <div className="grid flex-1 grid-cols-1 gap-4 overflow-hidden lg:grid-cols-[360px_1fr]">
        <Card className="flex flex-col overflow-hidden">
          <CardHeader>
            <CardTitle className="text-base font-semibold">
              {TABS.find((t) => t.value === activeTab)?.label}
            </CardTitle>
          </CardHeader>
          <CardContent className="flex-1 overflow-auto p-0">
            {isLoading && (
              <div className="text-muted-foreground p-4 text-sm">
                Loading drafts...
              </div>
            )}
            {error && (
              <div className="text-destructive p-4 text-sm">
                Could not load drafts.
              </div>
            )}
            {!isLoading && drafts && drafts.length === 0 && (
              <EmptyState status={activeTab} />
            )}
            <ul className="divide-y">
              {(drafts ?? []).map((d) => {
                const isSelected = d.id === selectedId;
                return (
                  <li key={d.id}>
                    <button
                      type="button"
                      onClick={() => {
                        setSelectedId(d.id);
                        setEditing(false);
                        setActionError(null);
                      }}
                      className={`hover:bg-muted/50 flex w-full flex-col items-start gap-1 px-4 py-3 text-left ${
                        isSelected ? "bg-muted/70" : ""
                      }`}
                    >
                      <div className="flex w-full items-center justify-between gap-2">
                        <span className="line-clamp-2 text-sm font-medium">
                          {d.title}
                        </span>
                        <StatusBadge status={d.status} />
                      </div>
                      <div className="text-muted-foreground flex items-center gap-2 text-xs">
                        <span>{formatRelative(d.created_at)}</span>
                        {d.action_request_id && (
                          <span className="font-mono">
                            action {d.action_request_id.slice(0, 8)}
                          </span>
                        )}
                      </div>
                    </button>
                  </li>
                );
              })}
            </ul>
          </CardContent>
        </Card>

        <Card className="flex flex-col overflow-hidden">
          {!selected && (
            <CardContent className="text-muted-foreground flex flex-1 items-center justify-center text-sm">
              Select a draft to review.
            </CardContent>
          )}
          {selected && (
            <>
              <CardHeader className="flex flex-row items-start justify-between gap-3 space-y-0">
                <div className="space-y-2">
                  {editing ? (
                    <Input
                      value={editTitle}
                      onChange={(e) => setEditTitle(e.target.value)}
                      className="text-base font-semibold"
                    />
                  ) : (
                    <CardTitle className="text-base font-semibold">
                      {selected.title}
                    </CardTitle>
                  )}
                  <div className="text-muted-foreground flex flex-wrap items-center gap-2 text-xs">
                    <StatusBadge status={selected.status} />
                    <span>created {formatRelative(selected.created_at)}</span>
                    {selected.action_request_id && (
                      <span className="font-mono">
                        action {selected.action_request_id}
                      </span>
                    )}
                    {selected.rollout_id && (
                      <span className="font-mono">
                        rollout {selected.rollout_id}
                      </span>
                    )}
                    {selected.external_url && (
                      <a
                        className="text-primary underline"
                        href={selected.external_url}
                        target="_blank"
                        rel="noopener noreferrer"
                      >
                        {selected.external_id || "ticket"}
                      </a>
                    )}
                  </div>
                </div>
                <div className="flex shrink-0 flex-wrap gap-2">
                  {selected.status === "draft" && !editing && (
                    <>
                      <Button size="sm" variant="outline" onClick={startEdit}>
                        Edit
                      </Button>
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={copyToClipboard}
                      >
                        Copy markdown
                      </Button>
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={dismiss}
                        disabled={submitting}
                      >
                        Dismiss
                      </Button>
                    </>
                  )}
                  {editing && (
                    <>
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={cancelEdit}
                        disabled={submitting}
                      >
                        Cancel
                      </Button>
                      <Button
                        size="sm"
                        onClick={saveEdit}
                        disabled={submitting}
                      >
                        Save
                      </Button>
                    </>
                  )}
                </div>
              </CardHeader>
              <CardContent className="flex-1 space-y-4 overflow-auto">
                {actionError && (
                  <div className="text-destructive text-sm">{actionError}</div>
                )}

                {editing ? (
                  <Textarea
                    value={editBody}
                    onChange={(e) => setEditBody(e.target.value)}
                    rows={20}
                    className="font-mono text-sm"
                  />
                ) : (
                  <div className="prose prose-sm dark:prose-invert max-w-none">
                    <ReactMarkdown>{selected.body_markdown}</ReactMarkdown>
                  </div>
                )}

                {selected.status === "draft" && !editing && (
                  <PublishForm
                    provider={publishProvider}
                    externalID={publishExternalID}
                    externalURL={publishExternalURL}
                    submitting={submitting}
                    onProviderChange={setPublishProvider}
                    onExternalIDChange={setPublishExternalID}
                    onExternalURLChange={setPublishExternalURL}
                    onPublish={publish}
                  />
                )}
              </CardContent>
            </>
          )}
        </Card>
      </div>
    </div>
  );
}

function EmptyState({ status }: { status: IncidentDraftStatus }) {
  const text =
    status === "draft"
      ? "No pending drafts. After Squadron's action runner runs, a postmortem draft will appear here for review."
      : status === "published"
        ? "No published drafts yet."
        : "No dismissed drafts.";
  return (
    <div className="text-muted-foreground px-4 py-6 text-sm">{text}</div>
  );
}

interface PublishFormProps {
  provider: IncidentProvider;
  externalID: string;
  externalURL: string;
  submitting: boolean;
  onProviderChange: (v: IncidentProvider) => void;
  onExternalIDChange: (v: string) => void;
  onExternalURLChange: (v: string) => void;
  onPublish: () => void;
}

function PublishForm({
  provider,
  externalID,
  externalURL,
  submitting,
  onProviderChange,
  onExternalIDChange,
  onExternalURLChange,
  onPublish,
}: PublishFormProps) {
  return (
    <div className="border-border space-y-3 rounded-md border p-4">
      <div className="text-sm font-medium">Publish</div>
      <p className="text-muted-foreground text-xs">
        Publishing stamps the draft so the audit timeline records what
        you did with it. The clipboard provider also copies the body
        to your clipboard so you can paste it into Linear, Jira, or
        wherever you track work.
      </p>
      <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
        <div className="space-y-1">
          <Label htmlFor="publish-provider" className="text-xs">
            Provider
          </Label>
          <Select
            value={provider}
            onValueChange={(v) => onProviderChange(v as IncidentProvider)}
          >
            <SelectTrigger id="publish-provider">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="clipboard">Clipboard</SelectItem>
              <SelectItem value="linear">Linear</SelectItem>
              <SelectItem value="jira">Jira</SelectItem>
              <SelectItem value="github">GitHub Issues</SelectItem>
              <SelectItem value="generic">Generic</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1">
          <Label htmlFor="publish-external-id" className="text-xs">
            External ID
          </Label>
          <Input
            id="publish-external-id"
            placeholder="LIN-123"
            value={externalID}
            onChange={(e) => onExternalIDChange(e.target.value)}
          />
        </div>
        <div className="space-y-1">
          <Label htmlFor="publish-external-url" className="text-xs">
            External URL
          </Label>
          <Input
            id="publish-external-url"
            placeholder="https://linear.app/..."
            value={externalURL}
            onChange={(e) => onExternalURLChange(e.target.value)}
          />
        </div>
      </div>
      <div className="flex justify-end">
        <Button size="sm" onClick={onPublish} disabled={submitting}>
          Publish
        </Button>
      </div>
    </div>
  );
}
