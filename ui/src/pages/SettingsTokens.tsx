// SettingsTokens is the API token management page. Operators create,
// list, and revoke tokens here. The plaintext value is shown ONCE at
// creation time in a modal that warns the operator to copy it now —
// Squadron does not retain a recoverable copy.
//
// Mounted at /settings/tokens.

import { Copy, KeyRound, Plus, Trash2 } from "lucide-react";
import type React from "react";
import { useState } from "react";
import useSWR, { mutate } from "swr";

import {
  ALL_SCOPES,
  type APIToken,
  createAPIToken,
  listAPITokens,
  revokeAPIToken,
} from "@/api/auth";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

const TOKENS_KEY = "api-tokens";

export default function SettingsTokensPage() {
  const { data: tokens, error, isLoading } = useSWR<APIToken[]>(
    TOKENS_KEY,
    listAPITokens,
  );
  const [creating, setCreating] = useState(false);
  const [newLabel, setNewLabel] = useState("");
  // selectedScopes is the working set for the new-token form. Starts
  // empty so operators have to explicitly pick something — the
  // "Full access" shortcut just flips them all on plus the wildcard.
  const [selectedScopes, setSelectedScopes] = useState<Set<string>>(new Set());
  const [fullAccess, setFullAccess] = useState(false);
  // freshPlaintext holds the just-issued token plaintext for the
  // "copy this now" modal. Cleared when the operator dismisses the
  // modal — at which point Squadron has no way to recover it.
  const [freshPlaintext, setFreshPlaintext] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);

  const toggleScope = (id: string) => {
    setSelectedScopes((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const scopesForSubmit = (): string[] => {
    if (fullAccess) return ["*"];
    return Array.from(selectedScopes);
  };

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setSubmitError(null);
    const scopes = scopesForSubmit();
    if (scopes.length === 0) {
      setSubmitError("Pick at least one scope, or choose 'Full access'.");
      setSubmitting(false);
      return;
    }
    try {
      const resp = await createAPIToken(newLabel.trim(), scopes);
      setFreshPlaintext(resp.plaintext);
      setNewLabel("");
      setSelectedScopes(new Set());
      setFullAccess(false);
      setCreating(false);
      await mutate(TOKENS_KEY);
    } catch (err) {
      setSubmitError(err instanceof Error ? err.message : "create failed");
    } finally {
      setSubmitting(false);
    }
  };

  const handleRevoke = async (t: APIToken) => {
    if (!window.confirm(`Revoke token "${t.label}"? This cannot be undone.`)) return;
    try {
      await revokeAPIToken(t.id);
      await mutate(TOKENS_KEY);
    } catch (err) {
      alert(err instanceof Error ? err.message : "revoke failed");
    }
  };

  const copyToClipboard = (text: string) => {
    navigator.clipboard
      .writeText(text)
      .catch(() => {
        /* clipboard API blocked — operator can still select-and-copy */
      });
  };

  return (
    <div className="space-y-4 p-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold flex items-center gap-2">
            <KeyRound className="h-6 w-6 text-muted-foreground" />
            API tokens
          </h1>
          <p className="text-muted-foreground text-sm">
            Bearer tokens for the Squadron API. Used by the UI itself, the
            squadronctl CLI, and any automation that talks to Squadron.
          </p>
        </div>
        {!creating && (
          <Button onClick={() => setCreating(true)} className="gap-1">
            <Plus className="h-4 w-4" />
            New token
          </Button>
        )}
      </div>

      {creating && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">New token</CardTitle>
          </CardHeader>
          <CardContent>
            <form onSubmit={handleCreate} className="space-y-3">
              <div className="space-y-2">
                <Label htmlFor="label">Label</Label>
                <Input
                  id="label"
                  value={newLabel}
                  onChange={(e) => setNewLabel(e.target.value)}
                  placeholder="ci-bot, deploy-pipeline, alice@example.com"
                  required
                  autoFocus
                />
                <p className="text-xs text-muted-foreground">
                  Labels show up in the audit log as
                  <span className="font-mono">{" operator:<label>"}</span> so
                  pick something that identifies the bearer.
                </p>
              </div>

              {/* Scope picker. Default: nothing selected, so operators
                  must actively choose. "Full access" is the wildcard
                  shortcut — useful for the bootstrap / break-glass
                  case but discouraged for normal automation. */}
              <div className="space-y-2">
                <Label>Scopes</Label>
                <label className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={fullAccess}
                    onChange={(e) => {
                      setFullAccess(e.target.checked);
                      if (e.target.checked) setSelectedScopes(new Set());
                    }}
                  />
                  Full access (wildcard <span className="font-mono">*</span>)
                </label>
                {!fullAccess && (
                  <div className="grid gap-2 md:grid-cols-2 rounded-md border p-3">
                    {ALL_SCOPES.map((s) => (
                      <label
                        key={s.id}
                        className="flex items-start gap-2 text-sm"
                      >
                        <input
                          type="checkbox"
                          className="mt-0.5"
                          checked={selectedScopes.has(s.id)}
                          onChange={() => toggleScope(s.id)}
                        />
                        <span className="flex-1">
                          <span className="font-mono text-xs">{s.id}</span>
                          <span className="block text-[11px] text-muted-foreground">
                            {s.label}
                          </span>
                        </span>
                      </label>
                    ))}
                  </div>
                )}
                <p className="text-xs text-muted-foreground">
                  Token holders can only call endpoints in their granted
                  scopes. Match the scope to the bearer's job: a CI
                  pipeline that pushes configs and creates rollouts wants{" "}
                  <span className="font-mono">configs:write</span> +{" "}
                  <span className="font-mono">rollouts:write</span>, not
                  full access.
                </p>
              </div>

              {submitError && (
                <div className="text-sm text-red-600">{submitError}</div>
              )}
              <div className="flex justify-end gap-2">
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => {
                    setCreating(false);
                    setNewLabel("");
                    setSubmitError(null);
                  }}
                  disabled={submitting}
                >
                  Cancel
                </Button>
                <Button type="submit" disabled={submitting || !newLabel.trim()}>
                  {submitting ? "Issuing..." : "Issue token"}
                </Button>
              </div>
            </form>
          </CardContent>
        </Card>
      )}

      {/* One-time plaintext display. Modal-style banner so the operator
          can't miss it. Dismissing clears the value — Squadron itself
          retains only the hash, so a missed copy means "issue a new
          one". */}
      {freshPlaintext && (
        <Card className="border-emerald-500/50 bg-emerald-500/5">
          <CardContent className="py-4 space-y-2">
            <div className="text-sm font-medium">
              Your new token (shown once)
            </div>
            <div className="flex items-center gap-2">
              <code className="flex-1 font-mono text-xs bg-background border rounded px-2 py-1.5 break-all">
                {freshPlaintext}
              </code>
              <Button
                variant="outline"
                size="sm"
                className="gap-1"
                onClick={() => copyToClipboard(freshPlaintext)}
              >
                <Copy className="h-3.5 w-3.5" />
                Copy
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">
              Copy this now. Squadron stores only a hash — there's no way to
              retrieve the plaintext later. If you lose it, revoke and issue
              a new one.
            </p>
            <div className="flex justify-end">
              <Button
                size="sm"
                variant="ghost"
                onClick={() => setFreshPlaintext(null)}
              >
                I've copied it
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {error && (
        <div className="text-sm text-red-600">
          Failed to load tokens:{" "}
          {error instanceof Error ? error.message : String(error)}
        </div>
      )}

      {isLoading && (
        <div className="text-sm text-muted-foreground">Loading…</div>
      )}

      {tokens && tokens.length === 0 && !isLoading && (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            No tokens issued yet. Click "New token" to create one.
          </CardContent>
        </Card>
      )}

      {tokens && tokens.length > 0 && (
        <Card>
          <CardContent className="py-2">
            <table className="w-full text-sm">
              <thead>
                <tr className="text-left text-xs uppercase tracking-wider text-muted-foreground border-b">
                  <th className="py-2 pr-3">Label</th>
                  <th className="py-2 pr-3">Scopes</th>
                  <th className="py-2 pr-3">Created</th>
                  <th className="py-2 pr-3">Last used</th>
                  <th className="py-2 pr-3">Status</th>
                  <th className="py-2 w-1" />
                </tr>
              </thead>
              <tbody>
                {tokens.map((t) => (
                  <tr key={t.id} className="border-b last:border-0">
                    <td className="py-2 pr-3 font-medium">{t.label}</td>
                    <td className="py-2 pr-3 text-xs">
                      {renderScopes(t.scopes)}
                    </td>
                    <td className="py-2 pr-3 text-muted-foreground">
                      {formatTimestamp(t.created_at)}
                    </td>
                    <td className="py-2 pr-3 text-muted-foreground">
                      {t.last_used_at ? formatTimestamp(t.last_used_at) : "—"}
                    </td>
                    <td className="py-2 pr-3">
                      {t.revoked_at ? (
                        <Badge
                          variant="outline"
                          className="text-[10px] uppercase bg-muted text-muted-foreground"
                        >
                          revoked
                        </Badge>
                      ) : (
                        <Badge
                          variant="outline"
                          className="text-[10px] uppercase bg-emerald-500/10 text-emerald-700 border-emerald-500/20"
                        >
                          active
                        </Badge>
                      )}
                    </td>
                    <td className="py-2">
                      {!t.revoked_at && (
                        <Button
                          variant="ghost"
                          size="icon"
                          onClick={() => handleRevoke(t)}
                          title="Revoke"
                        >
                          <Trash2 className="h-4 w-4" />
                        </Button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </CardContent>
        </Card>
      )}
    </div>
  );
}

function formatTimestamp(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

// renderScopes shows a concise scope summary. Empty scopes = legacy
// full-access (pre-v0.10 row); wildcard = explicitly full. Anything
// else renders the count + a tooltip with the full list.
function renderScopes(scopes: string[] | undefined): React.ReactNode {
  if (!scopes || scopes.length === 0) {
    return (
      <Badge variant="outline" className="text-[10px] uppercase">
        legacy: full access
      </Badge>
    );
  }
  if (scopes.includes("*")) {
    return (
      <Badge
        variant="outline"
        className="text-[10px] uppercase bg-amber-500/10 text-amber-700 border-amber-500/20"
      >
        full access (*)
      </Badge>
    );
  }
  return (
    <span className="font-mono text-[11px]" title={scopes.join(", ")}>
      {scopes.length === 1 ? scopes[0] : `${scopes.length} scopes`}
    </span>
  );
}
