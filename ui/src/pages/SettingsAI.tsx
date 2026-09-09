// SettingsAI is the AI-assist provider configuration page.
//
// An admin sets the AI provider API key here. The key is stored
// ENCRYPTED server-side (sealed with SQUADRON_SECRETS_KEY) and is
// WRITE-ONLY: it is never rendered back. The page shows only the
// non-secret status — whether AI is enabled, which provider/models are
// wired, where the key came from (env / stored), and the last 4
// characters as a rotation hint.
//
// Mounted at /settings/ai. Requires the ai:write scope to save/clear.

import { KeyRound, Sparkles } from "lucide-react";
import { useState } from "react";
import useSWR, { mutate } from "swr";

import {
  type AICapabilities,
  clearAiCredential,
  getAICapabilities,
  setAiCredential,
} from "@/api/ai";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

const CAPABILITIES_KEY = "ai-capabilities";

export default function SettingsAIPage() {
  const { data: caps, isLoading } = useSWR<AICapabilities>(
    CAPABILITIES_KEY,
    getAICapabilities,
  );

  // The key field is write-only: it starts empty and is never seeded
  // from the server (the plaintext never leaves the backend).
  const [apiKey, setApiKey] = useState("");
  const [provider, setProvider] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const keySource = caps?.key_source ?? "none";

  const save = async () => {
    if (apiKey.trim() === "") {
      setError("Enter an API key to save.");
      return;
    }
    setSubmitting(true);
    setError(null);
    setNotice(null);
    try {
      const updated = await setAiCredential(
        apiKey.trim(),
        provider || undefined,
      );
      await mutate(CAPABILITIES_KEY, updated, { revalidate: false });
      setApiKey("");
      setNotice("API key saved and applied.");
    } catch (e) {
      setError(e instanceof Error ? e.message : "save failed");
    } finally {
      setSubmitting(false);
    }
  };

  const clear = async () => {
    setSubmitting(true);
    setError(null);
    setNotice(null);
    try {
      const updated = await clearAiCredential();
      await mutate(CAPABILITIES_KEY, updated, { revalidate: false });
      setApiKey("");
      setNotice("API key cleared. AI assist is now disabled.");
    } catch (e) {
      setError(e instanceof Error ? e.message : "clear failed");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="space-y-4 p-6">
      <div>
        <h1 className="text-2xl font-semibold flex items-center gap-2">
          <Sparkles className="h-6 w-6" />
          AI assist
        </h1>
        <p className="text-muted-foreground text-sm max-w-2xl">
          Set the API key for your AI provider. Squadron stores it encrypted at
          rest (sealed with <code>SQUADRON_SECRETS_KEY</code>) and never returns
          it. To rotate, enter a new key below.
        </p>
      </div>

      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-base flex items-center gap-2">
            Status
            <Badge
              variant="outline"
              className={
                caps?.enabled
                  ? "bg-emerald-500/10 text-emerald-700 border-emerald-500/30"
                  : "bg-zinc-500/10 text-zinc-600 border-zinc-500/30"
              }
            >
              {caps?.enabled ? "Enabled" : "Disabled"}
            </Badge>
          </CardTitle>
        </CardHeader>
        <CardContent className="text-xs text-muted-foreground space-y-1">
          {isLoading ? (
            <div>Loading…</div>
          ) : (
            <>
              <div>
                Provider:{" "}
                <span className="font-mono">{caps?.provider ?? "—"}</span>
              </div>
              <div>
                Models:{" "}
                <span className="font-mono">
                  {caps?.explain_model ?? "—"} / {caps?.merge_model ?? "—"}
                </span>
              </div>
              <div>
                Key source: <span className="font-mono">{keySource}</span>
              </div>
              <div>
                Key:{" "}
                {caps?.key_last4 ? (
                  <span className="font-mono">••••••••{caps.key_last4}</span>
                ) : (
                  <span className="text-amber-700">not set</span>
                )}
              </div>
            </>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-base flex items-center gap-2">
            <KeyRound className="h-4 w-4" />
            {keySource === "stored" ? "Rotate API key" : "Set API key"}
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div>
            <Label htmlFor="ai-provider">
              Provider <span className="text-muted-foreground">(optional)</span>
            </Label>
            <select
              id="ai-provider"
              value={provider}
              onChange={(e) => setProvider(e.target.value)}
              className="mt-1 w-full rounded-md border border-border bg-background px-3 py-2 text-sm"
            >
              <option value="">Keep current</option>
              <option value="anthropic">Anthropic</option>
              <option value="openai">OpenAI-compatible</option>
            </select>
            <p className="mt-1 text-[11px] text-muted-foreground">
              A provider change is recorded now and takes effect on the next
              restart. The key applies immediately.
            </p>
          </div>

          <div>
            <Label htmlFor="ai-api-key">API key</Label>
            <Input
              id="ai-api-key"
              type="password"
              autoComplete="off"
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              placeholder="sk-…"
              className="font-mono text-xs"
            />
            <p className="mt-1 text-[11px] text-muted-foreground">
              Encrypted at rest. Squadron never returns the key after save.
            </p>
          </div>

          {error && <div className="text-sm text-red-600">{error}</div>}
          {notice && <div className="text-sm text-emerald-700">{notice}</div>}

          <div className="flex items-center gap-2">
            <Button onClick={save} disabled={submitting}>
              {submitting ? "Saving…" : "Save key"}
            </Button>
            <Button
              variant="outline"
              onClick={clear}
              disabled={submitting || keySource !== "stored"}
              title={
                keySource === "stored"
                  ? "Remove the stored key"
                  : "No stored key to clear"
              }
            >
              Clear stored key
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
