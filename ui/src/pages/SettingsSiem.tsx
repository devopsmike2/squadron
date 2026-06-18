// SettingsSiem is the SIEM destination management page (v0.50.3).
//
// Operators register downstream SIEM endpoints (Splunk HEC, signed
// webhook receivers) here. Squadron forwards every audit event to
// every enabled destination matching the optional event-type prefix
// filter. The local audit log is unchanged — SIEM export is an
// addition, not a replacement, so a SIEM outage never loses
// compliance evidence.
//
// Plaintext secrets only leave the operator's browser inbound: the
// list view never reveals them and the API never returns them. To
// rotate, re-enter the secret in the edit drawer.
//
// Mounted at /settings/siem.

import { CheckCircle, Plus, ServerCog, Trash2, XCircle } from "lucide-react";
import { useEffect, useState } from "react";
import useSWR, { mutate } from "swr";

import {
  type SiemDestination,
  type SiemDestinationInput,
  type SiemDestinationType,
  createSiemDestination,
  deleteSiemDestination,
  listSiemDestinations,
  testSiemDestination,
  updateSiemDestination,
} from "@/api/siem";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";

const DESTINATIONS_KEY = "siem-destinations";

// emptyForm gives the create drawer a sane starting point.
// America/Chicago is a sensible default because of the v0.49 change-
// window default; doesn't apply here directly but matches the
// "regulated utility" assumption baked into the whole page.
const emptyForm = (): SiemDestinationInput => ({
  name: "",
  type: "splunk_hec",
  url: "",
  plaintext_secret: "",
  enabled: true,
  event_type_prefix: [],
});

export default function SettingsSiemPage() {
  const {
    data: destinations,
    error,
    isLoading,
  } = useSWR<SiemDestination[]>(DESTINATIONS_KEY, listSiemDestinations, {
    refreshInterval: 15000,
  });

  // Form state for create / edit. `editing` non-null switches the
  // drawer into edit mode; plaintext_secret stays empty in edit
  // mode unless the operator explicitly types one (= rotate).
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [editing, setEditing] = useState<SiemDestination | null>(null);
  const [form, setForm] = useState<SiemDestinationInput>(emptyForm());
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);

  const [confirmDelete, setConfirmDelete] = useState<SiemDestination | null>(
    null,
  );
  const [testStatus, setTestStatus] = useState<
    Record<string, "ok" | "fail" | "pending">
  >({});
  const [testError, setTestError] = useState<Record<string, string>>({});

  // Re-seed the form when the operator opens the drawer in a new mode.
  useEffect(() => {
    if (!drawerOpen) return;
    if (editing) {
      setForm({
        name: editing.name,
        type: editing.type,
        url: editing.url,
        plaintext_secret: "",
        enabled: editing.enabled,
        event_type_prefix: editing.event_type_prefix ?? [],
      });
    } else {
      setForm(emptyForm());
    }
    setSubmitError(null);
  }, [drawerOpen, editing]);

  const openCreate = () => {
    setEditing(null);
    setDrawerOpen(true);
  };
  const openEdit = (d: SiemDestination) => {
    setEditing(d);
    setDrawerOpen(true);
  };

  const submit = async () => {
    setSubmitting(true);
    setSubmitError(null);
    try {
      if (editing) {
        const patch: Partial<SiemDestinationInput> = {
          name: form.name,
          type: form.type,
          url: form.url,
          enabled: form.enabled,
          event_type_prefix: form.event_type_prefix,
        };
        // Only send plaintext_secret if the operator typed one —
        // empty means "keep the existing secret".
        if (form.plaintext_secret.trim() !== "") {
          patch.plaintext_secret = form.plaintext_secret;
        }
        await updateSiemDestination(editing.id, patch);
      } else {
        await createSiemDestination(form);
      }
      setDrawerOpen(false);
      await mutate(DESTINATIONS_KEY);
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : "save failed");
    } finally {
      setSubmitting(false);
    }
  };

  const handleDelete = async () => {
    if (!confirmDelete) return;
    try {
      await deleteSiemDestination(confirmDelete.id);
      setConfirmDelete(null);
      await mutate(DESTINATIONS_KEY);
    } catch (e) {
      window.alert(e instanceof Error ? e.message : "delete failed");
    }
  };

  const handleTest = async (d: SiemDestination) => {
    setTestStatus((prev) => ({ ...prev, [d.id]: "pending" }));
    setTestError((prev) => ({ ...prev, [d.id]: "" }));
    try {
      await testSiemDestination(d.id);
      setTestStatus((prev) => ({ ...prev, [d.id]: "ok" }));
      await mutate(DESTINATIONS_KEY); // last_event_sent_at updates
    } catch (e) {
      setTestStatus((prev) => ({ ...prev, [d.id]: "fail" }));
      setTestError((prev) => ({
        ...prev,
        [d.id]: e instanceof Error ? e.message : "test failed",
      }));
      await mutate(DESTINATIONS_KEY); // last_error updates
    }
  };

  return (
    <div className="space-y-4 p-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold">SIEM destinations</h1>
          <p className="text-muted-foreground text-sm max-w-2xl">
            Every audit event Squadron records is also forwarded to every
            enabled destination below. Use this for centralized retention (3–7
            year SOX / NERC CIP windows) or to feed compliance dashboards. The
            local audit log is unchanged.
          </p>
        </div>
        <Button onClick={openCreate} className="gap-1">
          <Plus className="h-4 w-4" />
          New destination
        </Button>
      </div>

      {error && (
        <Card>
          <CardContent className="py-6 text-sm text-red-600">
            Failed to load destinations:{" "}
            {error instanceof Error ? error.message : String(error)}
            {String(error).includes("503") && (
              <div className="mt-2 text-muted-foreground">
                SIEM export is disabled. Set <code>SQUADRON_SIEM_KEY</code>{" "}
                (base64-encoded 32-byte key) and restart Squadron.
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {isLoading && (
        <div className="text-sm text-muted-foreground">Loading…</div>
      )}

      {destinations && destinations.length === 0 && !isLoading && (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground space-y-2">
            <ServerCog className="h-8 w-8 mx-auto opacity-50" />
            <div>No SIEM destinations configured.</div>
            <Button variant="outline" onClick={openCreate}>
              Configure your first destination
            </Button>
          </CardContent>
        </Card>
      )}

      {destinations &&
        destinations.map((d) => (
          <Card key={d.id}>
            <CardHeader className="flex flex-row items-start justify-between gap-2 pb-2">
              <div>
                <CardTitle className="text-base flex items-center gap-2">
                  {d.name}
                  <Badge
                    variant="outline"
                    className={
                      d.enabled
                        ? "bg-emerald-500/10 text-emerald-700 border-emerald-500/30"
                        : "bg-zinc-500/10 text-zinc-600 border-zinc-500/30"
                    }
                  >
                    {d.enabled ? "Enabled" : "Disabled"}
                  </Badge>
                  <Badge variant="outline">
                    {d.type === "splunk_hec" ? "Splunk HEC" : "Signed webhook"}
                  </Badge>
                </CardTitle>
                <div className="mt-1 text-xs text-muted-foreground font-mono break-all">
                  {d.url}
                </div>
              </div>
              <div className="flex items-center gap-1">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => handleTest(d)}
                  disabled={testStatus[d.id] === "pending"}
                >
                  {testStatus[d.id] === "pending"
                    ? "Testing…"
                    : testStatus[d.id] === "ok"
                      ? "Test ✓"
                      : "Test"}
                </Button>
                <Button variant="outline" size="sm" onClick={() => openEdit(d)}>
                  Edit
                </Button>
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={() => setConfirmDelete(d)}
                  title="Delete destination"
                >
                  <Trash2 className="h-4 w-4" />
                </Button>
              </div>
            </CardHeader>
            <CardContent className="text-xs text-muted-foreground space-y-1">
              <div className="flex items-center gap-4">
                <div>
                  Secret:{" "}
                  {d.has_secret ? (
                    <span className="inline-flex items-center gap-1 text-emerald-700">
                      <CheckCircle className="h-3 w-3" /> configured
                    </span>
                  ) : (
                    <span className="inline-flex items-center gap-1 text-amber-700">
                      <XCircle className="h-3 w-3" /> missing
                    </span>
                  )}
                </div>
                {d.event_type_prefix && d.event_type_prefix.length > 0 && (
                  <div>
                    Filter:{" "}
                    {d.event_type_prefix.map((p) => (
                      <code key={p} className="mr-1 rounded bg-muted px-1">
                        {p}*
                      </code>
                    ))}
                  </div>
                )}
              </div>
              <div className="flex flex-wrap items-center gap-4">
                <div>
                  Last delivered:{" "}
                  {d.last_event_sent_at ? (
                    <span className="text-emerald-700">
                      {new Date(d.last_event_sent_at).toLocaleString()}
                    </span>
                  ) : (
                    <span className="text-muted-foreground">never</span>
                  )}
                </div>
                {d.last_error && (
                  <div className="text-red-700">
                    Last error:{" "}
                    <span className="font-mono">{d.last_error}</span>
                    {d.last_error_at && (
                      <> ({new Date(d.last_error_at).toLocaleString()})</>
                    )}
                  </div>
                )}
              </div>
              {testStatus[d.id] === "fail" && testError[d.id] && (
                <div className="text-red-700">
                  Test failed:{" "}
                  <span className="font-mono">{testError[d.id]}</span>
                </div>
              )}
            </CardContent>
          </Card>
        ))}

      {/* Create / Edit drawer */}
      <Sheet open={drawerOpen} onOpenChange={setDrawerOpen}>
        <SheetContent className="overflow-y-auto">
          <SheetHeader>
            <SheetTitle>
              {editing ? `Edit ${editing.name}` : "New SIEM destination"}
            </SheetTitle>
            <SheetDescription>
              Squadron forwards every audit event to every enabled destination
              matching the optional event-type filter.
            </SheetDescription>
          </SheetHeader>
          <div className="space-y-4 mt-6">
            <div>
              <Label htmlFor="siem-name">Name</Label>
              <Input
                id="siem-name"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                placeholder="prod-splunk"
              />
            </div>

            <div>
              <Label htmlFor="siem-type">Type</Label>
              <select
                id="siem-type"
                value={form.type}
                onChange={(e) =>
                  setForm({
                    ...form,
                    type: e.target.value as SiemDestinationType,
                  })
                }
                className="mt-1 w-full rounded-md border border-border bg-background px-3 py-2 text-sm"
              >
                <option value="splunk_hec">Splunk HTTP Event Collector</option>
                <option value="webhook">Signed webhook (HMAC-SHA256)</option>
              </select>
              <p className="mt-1 text-[11px] text-muted-foreground">
                {form.type === "splunk_hec"
                  ? "Auth header: Splunk <token>. Secret below is the HEC token."
                  : "Body is HMAC-SHA256 signed; receivers verify the X-Squadron-Signature header. Secret below is the signing key."}
              </p>
            </div>

            <div>
              <Label htmlFor="siem-url">URL</Label>
              <Input
                id="siem-url"
                value={form.url}
                onChange={(e) => setForm({ ...form, url: e.target.value })}
                placeholder={
                  form.type === "splunk_hec"
                    ? "https://splunk.example.com:8088/services/collector/event"
                    : "https://hooks.example.com/squadron-audit"
                }
                className="font-mono text-xs"
              />
            </div>

            <div>
              <Label htmlFor="siem-secret">
                {editing ? "Rotate secret" : "Secret"}{" "}
                {editing && (
                  <span className="text-[11px] text-muted-foreground">
                    (leave blank to keep existing)
                  </span>
                )}
              </Label>
              <Input
                id="siem-secret"
                type="password"
                value={form.plaintext_secret}
                onChange={(e) =>
                  setForm({ ...form, plaintext_secret: e.target.value })
                }
                placeholder={
                  form.type === "splunk_hec"
                    ? "HEC token (e.g. 00000000-0000-0000-0000-000000000000)"
                    : "HMAC signing key"
                }
                className="font-mono text-xs"
              />
              <p className="mt-1 text-[11px] text-muted-foreground">
                Encrypted at rest with the SQUADRON_SIEM_KEY master key.
                Squadron never returns the plaintext after save — to rotate,
                re-enter here.
              </p>
            </div>

            <div>
              <Label htmlFor="siem-prefix">
                Event-type filter{" "}
                <span className="text-muted-foreground">(optional)</span>
              </Label>
              <Input
                id="siem-prefix"
                value={(form.event_type_prefix ?? []).join(", ")}
                onChange={(e) =>
                  setForm({
                    ...form,
                    event_type_prefix: e.target.value
                      .split(",")
                      .map((s) => s.trim())
                      .filter(Boolean),
                  })
                }
                placeholder="rollout., config."
                className="font-mono text-xs"
              />
              <p className="mt-1 text-[11px] text-muted-foreground">
                Comma-separated prefixes. Only events whose type starts with one
                of these is forwarded to this destination. Empty = forward
                everything.
              </p>
            </div>

            <label className="flex items-center gap-2 cursor-pointer text-sm">
              <input
                type="checkbox"
                checked={form.enabled}
                onChange={(e) =>
                  setForm({ ...form, enabled: e.target.checked })
                }
                className="h-4 w-4"
              />
              Enabled
            </label>

            {submitError && (
              <div className="text-sm text-red-600">{submitError}</div>
            )}
          </div>
          <SheetFooter className="mt-6">
            <Button variant="outline" onClick={() => setDrawerOpen(false)}>
              Cancel
            </Button>
            <Button onClick={submit} disabled={submitting}>
              {submitting
                ? "Saving…"
                : editing
                  ? "Save changes"
                  : "Create destination"}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      {/* Delete confirmation */}
      <Dialog
        open={!!confirmDelete}
        onOpenChange={(o) => !o && setConfirmDelete(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete SIEM destination</DialogTitle>
            <DialogDescription>
              Stop forwarding audit events to "{confirmDelete?.name}"?
              Squadron's local audit log is unaffected; only the downstream pipe
              is removed.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmDelete(null)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={handleDelete}>
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
