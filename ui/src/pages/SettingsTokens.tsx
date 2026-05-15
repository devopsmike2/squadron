// SettingsTokens is the API token management page. Operators create,
// list, and revoke tokens here. The plaintext value is shown ONCE at
// creation time in a modal that warns the operator to copy it now —
// Squadron does not retain a recoverable copy.
//
// Mounted at /settings/tokens.

import { Copy, KeyRound, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import useSWR, { mutate } from "swr";

import {
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
  // freshPlaintext holds the just-issued token plaintext for the
  // "copy this now" modal. Cleared when the operator dismisses the
  // modal — at which point Squadron has no way to recover it.
  const [freshPlaintext, setFreshPlaintext] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setSubmitError(null);
    try {
      const resp = await createAPIToken(newLabel.trim());
      setFreshPlaintext(resp.plaintext);
      setNewLabel("");
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
