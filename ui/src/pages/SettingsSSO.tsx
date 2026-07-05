// SettingsSSO is the SSO administration page (ADR 0016). Operators manage OIDC
// identity-provider connections (create / edit / delete) and mint SCIM service
// tokens for directory sync. Enterprise-only: in OSS the /api/v1/sso routes 404,
// and this page shows an enterprise-feature notice instead.
//
// The OIDC client secret only travels inbound (create / rotate); the list view
// never returns it. To rotate, re-enter it in the edit drawer. A minted SCIM
// token's plaintext is shown ONCE — the operator copies it into their IdP.
//
// Mounted at /settings/sso.

import { KeyRound, Lock, Plus, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import useSWR, { mutate } from "swr";

import {
  type OIDCConnection,
  type OIDCConnectionInput,
  createSSOConnection,
  deleteSSOConnection,
  listSSOConnections,
  mintSCIMToken,
  updateSSOConnection,
} from "@/api/sso";
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

const CONNECTIONS_KEY = "sso-connections";

type ConnForm = OIDCConnectionInput;

const emptyForm = (): ConnForm => ({
  issuer: "",
  client_id: "",
  client_secret: "",
  redirect_uri: `${window.location.origin}/auth/oidc/callback`,
  tenant_id: "default",
  scopes: ["openid", "email", "profile"],
  default_role: "",
  display_name: "",
  active: true,
});

export default function SettingsSSOPage() {
  const {
    data: connections,
    error,
    isLoading,
  } = useSWR<OIDCConnection[]>(CONNECTIONS_KEY, listSSOConnections, {
    // A 404 here means the SSO admin API is an enterprise feature not present
    // in this build; don't hammer it.
    shouldRetryOnError: false,
  });

  const notEnterprise =
    error != null && (error as { status?: number }).status === 404;

  const [drawerOpen, setDrawerOpen] = useState(false);
  const [editing, setEditing] = useState<OIDCConnection | null>(null);
  const [form, setForm] = useState<ConnForm>(emptyForm());
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<OIDCConnection | null>(
    null,
  );

  // SCIM-token minting.
  const [scimOpen, setScimOpen] = useState(false);
  const [scimTenant, setScimTenant] = useState("default");
  const [scimDescription, setScimDescription] = useState("");
  const [scimMinting, setScimMinting] = useState(false);
  const [scimError, setScimError] = useState<string | null>(null);
  const [scimToken, setScimToken] = useState<string | null>(null);

  useEffect(() => {
    if (!drawerOpen) return;
    if (editing) {
      setForm({
        issuer: editing.issuer,
        client_id: editing.client_id,
        client_secret: "", // blank = keep existing
        redirect_uri: editing.redirect_uri,
        tenant_id: editing.tenant_id,
        scopes: editing.scopes ?? [],
        default_role: editing.default_role,
        display_name: editing.display_name,
        active: editing.active,
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
  const openEdit = (c: OIDCConnection) => {
    setEditing(c);
    setDrawerOpen(true);
  };

  const submit = async () => {
    setSubmitting(true);
    setSubmitError(null);
    try {
      if (editing) {
        // Omit client_secret when blank so the existing sealed secret is kept.
        const { client_secret, ...rest } = form;
        const patch =
          client_secret.trim() !== "" ? { ...rest, client_secret } : rest;
        await updateSSOConnection(editing.id, patch);
      } else {
        await createSSOConnection(form);
      }
      setDrawerOpen(false);
      await mutate(CONNECTIONS_KEY);
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : "save failed");
    } finally {
      setSubmitting(false);
    }
  };

  const handleDelete = async () => {
    if (!confirmDelete) return;
    try {
      await deleteSSOConnection(confirmDelete.id);
      setConfirmDelete(null);
      await mutate(CONNECTIONS_KEY);
    } catch (e) {
      window.alert(e instanceof Error ? e.message : "delete failed");
    }
  };

  const openMint = () => {
    setScimTenant("default");
    setScimDescription("");
    setScimError(null);
    setScimToken(null);
    setScimOpen(true);
  };

  const mint = async () => {
    setScimMinting(true);
    setScimError(null);
    try {
      const res = await mintSCIMToken({
        tenant_id: scimTenant,
        description: scimDescription || undefined,
      });
      setScimToken(res.plaintext);
    } catch (e) {
      setScimError(e instanceof Error ? e.message : "mint failed");
    } finally {
      setScimMinting(false);
    }
  };

  return (
    <div className="space-y-4 p-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold">SSO &amp; identity</h1>
          <p className="text-muted-foreground text-sm max-w-2xl">
            Manage OIDC single sign-on connections and mint SCIM service tokens
            for directory provisioning. Users who log in through a connection
            are homed to its tenant; SCIM-provisioned users get their
            group-mapped roles at login.
          </p>
        </div>
        {!notEnterprise && (
          <div className="flex items-center gap-2">
            <Button variant="outline" onClick={openMint} className="gap-1">
              <KeyRound className="h-4 w-4" />
              Mint SCIM token
            </Button>
            <Button onClick={openCreate} className="gap-1">
              <Plus className="h-4 w-4" />
              New connection
            </Button>
          </div>
        )}
      </div>

      {notEnterprise && (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground space-y-2">
            <Lock className="h-8 w-8 mx-auto opacity-50" />
            <div className="font-medium text-foreground">
              SSO &amp; SCIM are enterprise features
            </div>
            <div className="text-sm max-w-md mx-auto">
              OIDC single sign-on and SCIM provisioning are available in the
              Squadron Enterprise edition. This build serves the open-source
              core.
            </div>
          </CardContent>
        </Card>
      )}

      {error && !notEnterprise && (
        <Card>
          <CardContent className="py-6 text-sm text-red-600">
            Failed to load connections:{" "}
            {error instanceof Error ? error.message : String(error)}
          </CardContent>
        </Card>
      )}

      {isLoading && !notEnterprise && (
        <div className="text-sm text-muted-foreground">Loading…</div>
      )}

      {connections && connections.length === 0 && !isLoading && (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground space-y-2">
            <KeyRound className="h-8 w-8 mx-auto opacity-50" />
            <div>No OIDC connections configured.</div>
            <Button variant="outline" onClick={openCreate}>
              Add your first connection
            </Button>
          </CardContent>
        </Card>
      )}

      {connections &&
        connections.map((c) => (
          <Card key={c.id}>
            <CardHeader className="flex flex-row items-start justify-between gap-2 pb-2">
              <div>
                <CardTitle className="text-base flex items-center gap-2">
                  {c.display_name || c.id}
                  <Badge
                    variant="outline"
                    className={
                      c.active
                        ? "bg-emerald-500/10 text-emerald-700 border-emerald-500/30"
                        : "bg-zinc-500/10 text-zinc-600 border-zinc-500/30"
                    }
                  >
                    {c.active ? "Active" : "Disabled"}
                  </Badge>
                  <Badge variant="outline">tenant: {c.tenant_id}</Badge>
                </CardTitle>
                <div className="mt-1 text-xs text-muted-foreground font-mono break-all">
                  {c.issuer}
                </div>
              </div>
              <div className="flex items-center gap-1">
                <Button variant="outline" size="sm" onClick={() => openEdit(c)}>
                  Edit
                </Button>
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={() => setConfirmDelete(c)}
                  title="Delete connection"
                >
                  <Trash2 className="h-4 w-4" />
                </Button>
              </div>
            </CardHeader>
            <CardContent className="text-xs text-muted-foreground space-y-1">
              <div className="flex flex-wrap items-center gap-4">
                <div>
                  Client ID:{" "}
                  <span className="font-mono text-foreground">
                    {c.client_id}
                  </span>
                </div>
                {c.default_role && (
                  <div>
                    Default role:{" "}
                    <code className="rounded bg-muted px-1">
                      {c.default_role}
                    </code>
                  </div>
                )}
                {c.scopes && c.scopes.length > 0 && (
                  <div>Scopes: {c.scopes.join(" ")}</div>
                )}
              </div>
              <div className="font-mono break-all">
                Redirect: {c.redirect_uri}
              </div>
            </CardContent>
          </Card>
        ))}

      {/* Create / Edit drawer */}
      <Sheet open={drawerOpen} onOpenChange={setDrawerOpen}>
        <SheetContent className="overflow-y-auto">
          <SheetHeader>
            <SheetTitle>
              {editing
                ? `Edit ${editing.display_name || editing.id}`
                : "New OIDC connection"}
            </SheetTitle>
            <SheetDescription>
              Connect an identity provider (Okta, Auth0, Entra, Google…). The
              client secret is sealed server-side and never returned.
            </SheetDescription>
          </SheetHeader>
          <div className="space-y-4 mt-6">
            <div>
              <Label htmlFor="sso-display">Display name</Label>
              <Input
                id="sso-display"
                value={form.display_name}
                onChange={(e) =>
                  setForm({ ...form, display_name: e.target.value })
                }
                placeholder="Acme Okta"
              />
              <p className="mt-1 text-[11px] text-muted-foreground">
                Shown on the sign-in page SSO button.
              </p>
            </div>

            <div>
              <Label htmlFor="sso-issuer">Issuer URL</Label>
              <Input
                id="sso-issuer"
                value={form.issuer}
                onChange={(e) => setForm({ ...form, issuer: e.target.value })}
                placeholder="https://acme.okta.com"
                className="font-mono text-xs"
              />
            </div>

            <div>
              <Label htmlFor="sso-client-id">Client ID</Label>
              <Input
                id="sso-client-id"
                value={form.client_id}
                onChange={(e) =>
                  setForm({ ...form, client_id: e.target.value })
                }
                className="font-mono text-xs"
              />
            </div>

            <div>
              <Label htmlFor="sso-secret">
                {editing ? "Rotate client secret" : "Client secret"}{" "}
                {editing && (
                  <span className="text-[11px] text-muted-foreground">
                    (leave blank to keep existing)
                  </span>
                )}
              </Label>
              <Input
                id="sso-secret"
                type="password"
                value={form.client_secret}
                onChange={(e) =>
                  setForm({ ...form, client_secret: e.target.value })
                }
                className="font-mono text-xs"
              />
              <p className="mt-1 text-[11px] text-muted-foreground">
                Sealed at rest with SQUADRON_SECRETS_KEY. Never returned after
                save.
              </p>
            </div>

            <div>
              <Label htmlFor="sso-redirect">Redirect URI</Label>
              <Input
                id="sso-redirect"
                value={form.redirect_uri}
                onChange={(e) =>
                  setForm({ ...form, redirect_uri: e.target.value })
                }
                className="font-mono text-xs"
              />
              <p className="mt-1 text-[11px] text-muted-foreground">
                Must match the callback registered at the IdP.
              </p>
            </div>

            <div>
              <Label htmlFor="sso-tenant">Tenant</Label>
              <Input
                id="sso-tenant"
                value={form.tenant_id}
                onChange={(e) =>
                  setForm({ ...form, tenant_id: e.target.value })
                }
                placeholder="default"
                className="font-mono text-xs"
              />
              <p className="mt-1 text-[11px] text-muted-foreground">
                Every user who logs in through this connection is homed here.
              </p>
            </div>

            <div>
              <Label htmlFor="sso-role">Default role id</Label>
              <Input
                id="sso-role"
                value={form.default_role}
                onChange={(e) =>
                  setForm({ ...form, default_role: e.target.value })
                }
                placeholder="r-…"
                className="font-mono text-xs"
              />
              <p className="mt-1 text-[11px] text-muted-foreground">
                Bound to a first-time user when SCIM has no better information.
              </p>
            </div>

            <div>
              <Label htmlFor="sso-scopes">Scopes</Label>
              <Input
                id="sso-scopes"
                value={(form.scopes ?? []).join(" ")}
                onChange={(e) =>
                  setForm({
                    ...form,
                    scopes: e.target.value.split(/\s+/).filter(Boolean),
                  })
                }
                placeholder="openid email profile"
                className="font-mono text-xs"
              />
            </div>

            <label className="flex items-center gap-2 cursor-pointer text-sm">
              <input
                type="checkbox"
                checked={form.active}
                onChange={(e) => setForm({ ...form, active: e.target.checked })}
                className="h-4 w-4"
              />
              Active
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
                  : "Create connection"}
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
            <DialogTitle>Delete OIDC connection</DialogTitle>
            <DialogDescription>
              Remove "{confirmDelete?.display_name || confirmDelete?.id}"? Users
              can no longer sign in through it. Already-issued sessions live out
              their expiry.
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

      {/* Mint SCIM token */}
      <Dialog open={scimOpen} onOpenChange={setScimOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Mint SCIM service token</DialogTitle>
            <DialogDescription>
              Issues a tenant-bound token your IdP uses to provision users and
              groups. The plaintext is shown once — copy it now.
            </DialogDescription>
          </DialogHeader>
          {!scimToken ? (
            <div className="space-y-4">
              <div>
                <Label htmlFor="scim-tenant">Tenant</Label>
                <Input
                  id="scim-tenant"
                  value={scimTenant}
                  onChange={(e) => setScimTenant(e.target.value)}
                  placeholder="default"
                  className="font-mono text-xs"
                />
              </div>
              <div>
                <Label htmlFor="scim-desc">
                  Description{" "}
                  <span className="text-muted-foreground">(optional)</span>
                </Label>
                <Input
                  id="scim-desc"
                  value={scimDescription}
                  onChange={(e) => setScimDescription(e.target.value)}
                  placeholder="Okta SCIM"
                />
              </div>
              {scimError && (
                <div className="text-sm text-red-600">{scimError}</div>
              )}
            </div>
          ) : (
            <div className="space-y-2">
              <Label>Service token (shown once)</Label>
              <textarea
                readOnly
                value={scimToken}
                onFocus={(e) => e.currentTarget.select()}
                className="w-full rounded-md border border-border bg-muted px-3 py-2 font-mono text-xs break-all"
                rows={3}
              />
              <p className="text-[11px] text-muted-foreground">
                Paste this into your IdP's SCIM connector as the bearer token.
                Squadron cannot show it again.
              </p>
            </div>
          )}
          <DialogFooter>
            {!scimToken ? (
              <>
                <Button variant="outline" onClick={() => setScimOpen(false)}>
                  Cancel
                </Button>
                <Button onClick={mint} disabled={scimMinting}>
                  {scimMinting ? "Minting…" : "Mint token"}
                </Button>
              </>
            ) : (
              <Button onClick={() => setScimOpen(false)}>Done</Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
