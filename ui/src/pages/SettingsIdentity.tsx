// SettingsIdentity is the enterprise identity administration page. Operators
// manage RBAC roles and role-bindings, tenants, and view the SCIM-provisioned
// directory. Enterprise-only: in OSS the /api/v1/rbac, /api/v1/tenants and
// /api/v1/sso routes 404, and this page shows an enterprise-feature notice
// instead of the tabs.
//
// Mounted at /settings/identity.

import { Lock, Plus, Trash2, X } from "lucide-react";
import { useEffect, useState } from "react";
import useSWR, { mutate } from "swr";

import {
  type Binding,
  type BindingInput,
  type Permission,
  type Role,
  type RoleInput,
  createBinding,
  createRole,
  deleteBinding,
  deleteRole,
  listBindings,
  listRoles,
} from "@/api/rbac";
import {
  type DirectoryGroup,
  type DirectoryUser,
  clearGroupRole,
  listDirectoryGroups,
  listDirectoryUsers,
  setGroupRole,
} from "@/api/sso";
import {
  type Tenant,
  type TenantInput,
  createTenant,
  deleteTenant,
  listTenants,
} from "@/api/tenants";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";

const ROLES_KEY = "rbac-roles";
const BINDINGS_KEY = "rbac-bindings";
const TENANTS_KEY = "tenants";

// A permission row in the role create drawer. resource_ids is kept as raw text
// (comma / newline separated) and parsed on submit.
interface PermRow {
  scope: string;
  resource_type: string;
  allResources: boolean;
  resourceIdsText: string;
}

const emptyPermRow = (): PermRow => ({
  scope: "",
  resource_type: "",
  allResources: true,
  resourceIdsText: "",
});

const permLabel = (p: Permission): string => {
  const base = p.scope + (p.resource_type ? `@${p.resource_type}` : "");
  return (
    base + (p.all_resources ? " (all)" : ` (${p.resource_ids.length} ids)`)
  );
};

const parseIds = (text: string): string[] =>
  text
    .split(/[,\n]/)
    .map((s) => s.trim())
    .filter(Boolean);

export default function SettingsIdentityPage() {
  const { data: roles, error: rolesError } = useSWR<Role[]>(
    ROLES_KEY,
    listRoles,
    {
      // A 404 here means the identity admin API is an enterprise feature not
      // present in this build; don't hammer it.
      shouldRetryOnError: false,
    },
  );

  const notEnterprise =
    rolesError != null && (rolesError as { status?: number }).status === 404;

  const { data: bindings } = useSWR<Binding[]>(
    notEnterprise ? null : BINDINGS_KEY,
    listBindings,
    { shouldRetryOnError: false },
  );
  const { data: tenants } = useSWR<Tenant[]>(
    notEnterprise ? null : TENANTS_KEY,
    listTenants,
    { shouldRetryOnError: false },
  );

  // Role create drawer.
  const [roleOpen, setRoleOpen] = useState(false);
  const [roleName, setRoleName] = useState("");
  const [permRows, setPermRows] = useState<PermRow[]>([emptyPermRow()]);
  const [roleSubmitting, setRoleSubmitting] = useState(false);
  const [roleError, setRoleError] = useState<string | null>(null);
  const [confirmDeleteRole, setConfirmDeleteRole] = useState<Role | null>(null);

  // Binding create drawer.
  const [bindingOpen, setBindingOpen] = useState(false);
  const [bindRoleId, setBindRoleId] = useState("");
  const [bindKind, setBindKind] = useState("token_label");
  const [bindRef, setBindRef] = useState("");
  const [bindSubmitting, setBindSubmitting] = useState(false);
  const [bindError, setBindError] = useState<string | null>(null);
  const [confirmDeleteBinding, setConfirmDeleteBinding] =
    useState<Binding | null>(null);

  // Tenant create drawer.
  const [tenantOpen, setTenantOpen] = useState(false);
  const [tenantName, setTenantName] = useState("");
  const [tenantId, setTenantId] = useState("");
  const [tenantSubmitting, setTenantSubmitting] = useState(false);
  const [tenantError, setTenantError] = useState<string | null>(null);
  const [confirmDeleteTenant, setConfirmDeleteTenant] = useState<Tenant | null>(
    null,
  );

  // Directory tab.
  const [dirTenant, setDirTenant] = useState("");

  useEffect(() => {
    if (!roleOpen) return;
    setRoleName("");
    setPermRows([emptyPermRow()]);
    setRoleError(null);
  }, [roleOpen]);

  useEffect(() => {
    if (!bindingOpen) return;
    setBindRoleId("");
    setBindKind("token_label");
    setBindRef("");
    setBindError(null);
  }, [bindingOpen]);

  useEffect(() => {
    if (!tenantOpen) return;
    setTenantName("");
    setTenantId("");
    setTenantError(null);
  }, [tenantOpen]);

  const submitRole = async () => {
    setRoleSubmitting(true);
    setRoleError(null);
    try {
      const permissions: Permission[] = permRows
        .filter((r) => r.scope.trim() !== "")
        .map((r) => ({
          scope: r.scope.trim(),
          resource_type: r.resource_type.trim(),
          all_resources: r.allResources,
          resource_ids: r.allResources ? [] : parseIds(r.resourceIdsText),
        }));
      const input: RoleInput = { name: roleName, permissions };
      await createRole(input);
      setRoleOpen(false);
      await mutate(ROLES_KEY);
    } catch (e) {
      setRoleError(e instanceof Error ? e.message : "save failed");
    } finally {
      setRoleSubmitting(false);
    }
  };

  const handleDeleteRole = async () => {
    if (!confirmDeleteRole) return;
    try {
      await deleteRole(confirmDeleteRole.id);
      setConfirmDeleteRole(null);
      await mutate(ROLES_KEY);
    } catch (e) {
      window.alert(e instanceof Error ? e.message : "delete failed");
    }
  };

  const submitBinding = async () => {
    setBindSubmitting(true);
    setBindError(null);
    try {
      const input: BindingInput = {
        role_id: bindRoleId,
        principal_kind: bindKind,
        principal_ref: bindRef.trim(),
      };
      await createBinding(input);
      setBindingOpen(false);
      await mutate(BINDINGS_KEY);
    } catch (e) {
      setBindError(e instanceof Error ? e.message : "save failed");
    } finally {
      setBindSubmitting(false);
    }
  };

  const handleDeleteBinding = async () => {
    if (!confirmDeleteBinding) return;
    try {
      await deleteBinding(confirmDeleteBinding.id);
      setConfirmDeleteBinding(null);
      await mutate(BINDINGS_KEY);
    } catch (e) {
      window.alert(e instanceof Error ? e.message : "delete failed");
    }
  };

  const submitTenant = async () => {
    setTenantSubmitting(true);
    setTenantError(null);
    try {
      const input: TenantInput = {
        name: tenantName,
        tenant_id: tenantId.trim() || undefined,
      };
      await createTenant(input);
      setTenantOpen(false);
      await mutate(TENANTS_KEY);
    } catch (e) {
      setTenantError(e instanceof Error ? e.message : "save failed");
    } finally {
      setTenantSubmitting(false);
    }
  };

  const handleDeleteTenant = async () => {
    if (!confirmDeleteTenant) return;
    try {
      await deleteTenant(confirmDeleteTenant.tenant_id);
      setConfirmDeleteTenant(null);
      await mutate(TENANTS_KEY);
    } catch (e) {
      window.alert(e instanceof Error ? e.message : "delete failed");
    }
  };

  const roleName_ = (id: string): string =>
    (roles ?? []).find((r) => r.id === id)?.name ?? id;

  return (
    <div className="space-y-4 p-6">
      <div>
        <h1 className="text-2xl font-semibold">Identity</h1>
        <p className="text-muted-foreground text-sm max-w-2xl">
          Manage RBAC roles and bindings, tenants, and view the SCIM directory.
        </p>
      </div>

      {notEnterprise ? (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground space-y-2">
            <Lock className="h-8 w-8 mx-auto opacity-50" />
            <div className="font-medium text-foreground">
              These are enterprise features
            </div>
            <div className="text-sm max-w-md mx-auto">
              RBAC roles and bindings, tenants, and the SCIM directory are
              available in the Squadron Enterprise edition. This build serves
              the open-source core.
            </div>
          </CardContent>
        </Card>
      ) : (
        <Tabs defaultValue="roles">
          <TabsList>
            <TabsTrigger value="roles">Roles</TabsTrigger>
            <TabsTrigger value="bindings">Bindings</TabsTrigger>
            <TabsTrigger value="tenants">Tenants</TabsTrigger>
            <TabsTrigger value="directory">Directory</TabsTrigger>
          </TabsList>

          {/* Roles */}
          <TabsContent value="roles" className="space-y-4">
            <div className="flex justify-end">
              <Button onClick={() => setRoleOpen(true)} className="gap-1">
                <Plus className="h-4 w-4" />
                New role
              </Button>
            </div>
            <Card>
              <CardContent className="p-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Name</TableHead>
                      <TableHead>Permissions</TableHead>
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(roles ?? []).length === 0 && (
                      <TableRow>
                        <TableCell
                          colSpan={3}
                          className="text-center text-muted-foreground py-8"
                        >
                          No roles defined.
                        </TableCell>
                      </TableRow>
                    )}
                    {(roles ?? []).map((r) => (
                      <TableRow key={r.id}>
                        <TableCell className="font-medium">{r.name}</TableCell>
                        <TableCell>
                          <div className="flex flex-wrap gap-1">
                            {r.permissions.map((p, i) => (
                              <Badge key={i} variant="outline">
                                {permLabel(p)}
                              </Badge>
                            ))}
                          </div>
                        </TableCell>
                        <TableCell className="text-right">
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => setConfirmDeleteRole(r)}
                            title="Delete role"
                          >
                            <Trash2 className="h-4 w-4" />
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          </TabsContent>

          {/* Bindings */}
          <TabsContent value="bindings" className="space-y-4">
            <div className="flex justify-end">
              <Button onClick={() => setBindingOpen(true)} className="gap-1">
                <Plus className="h-4 w-4" />
                New binding
              </Button>
            </div>
            <Card>
              <CardContent className="p-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Role</TableHead>
                      <TableHead>Principal kind</TableHead>
                      <TableHead>Principal ref</TableHead>
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(bindings ?? []).length === 0 && (
                      <TableRow>
                        <TableCell
                          colSpan={4}
                          className="text-center text-muted-foreground py-8"
                        >
                          No bindings defined.
                        </TableCell>
                      </TableRow>
                    )}
                    {(bindings ?? []).map((b) => (
                      <TableRow key={b.id}>
                        <TableCell className="font-medium">
                          {roleName_(b.role_id)}
                        </TableCell>
                        <TableCell>
                          <code className="rounded bg-muted px-1 text-xs">
                            {b.principal_kind}
                          </code>
                        </TableCell>
                        <TableCell className="font-mono text-xs break-all">
                          {b.principal_ref}
                        </TableCell>
                        <TableCell className="text-right">
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => setConfirmDeleteBinding(b)}
                            title="Delete binding"
                          >
                            <Trash2 className="h-4 w-4" />
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          </TabsContent>

          {/* Tenants */}
          <TabsContent value="tenants" className="space-y-4">
            <div className="flex justify-end">
              <Button onClick={() => setTenantOpen(true)} className="gap-1">
                <Plus className="h-4 w-4" />
                New tenant
              </Button>
            </div>
            <Card>
              <CardContent className="p-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Tenant ID</TableHead>
                      <TableHead>Name</TableHead>
                      <TableHead>Created</TableHead>
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(tenants ?? []).length === 0 && (
                      <TableRow>
                        <TableCell
                          colSpan={4}
                          className="text-center text-muted-foreground py-8"
                        >
                          No tenants defined.
                        </TableCell>
                      </TableRow>
                    )}
                    {(tenants ?? []).map((t) => (
                      <TableRow key={t.tenant_id}>
                        <TableCell className="font-mono text-xs">
                          {t.tenant_id}
                        </TableCell>
                        <TableCell className="font-medium">{t.name}</TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          {new Date(t.created_at).toLocaleString()}
                        </TableCell>
                        <TableCell className="text-right">
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => setConfirmDeleteTenant(t)}
                            title="Delete tenant"
                          >
                            <Trash2 className="h-4 w-4" />
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          </TabsContent>

          {/* Directory */}
          <TabsContent value="directory" className="space-y-4">
            <div className="max-w-sm">
              <Label htmlFor="dir-tenant">Tenant</Label>
              <Select value={dirTenant} onValueChange={setDirTenant}>
                <SelectTrigger id="dir-tenant">
                  <SelectValue placeholder="Select a tenant" />
                </SelectTrigger>
                <SelectContent>
                  {(tenants ?? []).map((t) => (
                    <SelectItem key={t.tenant_id} value={t.tenant_id}>
                      {`${t.name} (${t.tenant_id})`}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {dirTenant ? (
              <DirectoryView tenant={dirTenant} />
            ) : (
              <div className="text-sm text-muted-foreground">
                Select a tenant to view its SCIM-provisioned directory.
              </div>
            )}
          </TabsContent>
        </Tabs>
      )}

      {/* New role drawer */}
      <Sheet open={roleOpen} onOpenChange={setRoleOpen}>
        <SheetContent className="overflow-y-auto">
          <SheetHeader>
            <SheetTitle>New role</SheetTitle>
            <SheetDescription>
              A role is a named set of scoped permissions. Bind it to a
              principal on the Bindings tab.
            </SheetDescription>
          </SheetHeader>
          <div className="space-y-4 mt-6">
            <div>
              <Label htmlFor="role-name">Name</Label>
              <Input
                id="role-name"
                value={roleName}
                onChange={(e) => setRoleName(e.target.value)}
                placeholder="rollout-operator"
              />
            </div>

            <div className="space-y-3">
              <Label>Permissions</Label>
              {permRows.map((row, i) => (
                <div
                  key={i}
                  className="rounded-md border border-border p-3 space-y-2"
                >
                  <div className="flex items-start justify-between gap-2">
                    <div className="flex-1 space-y-2">
                      <Input
                        value={row.scope}
                        onChange={(e) =>
                          setPermRows((rows) =>
                            rows.map((r, j) =>
                              j === i ? { ...r, scope: e.target.value } : r,
                            ),
                          )
                        }
                        placeholder="rollouts:read or *"
                        className="font-mono text-xs"
                      />
                      <Input
                        value={row.resource_type}
                        onChange={(e) =>
                          setPermRows((rows) =>
                            rows.map((r, j) =>
                              j === i
                                ? { ...r, resource_type: e.target.value }
                                : r,
                            ),
                          )
                        }
                        placeholder="blank = any type"
                        className="font-mono text-xs"
                      />
                    </div>
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() =>
                        setPermRows((rows) => rows.filter((_, j) => j !== i))
                      }
                      title="Remove permission"
                    >
                      <X className="h-4 w-4" />
                    </Button>
                  </div>
                  <label className="flex items-center gap-2 cursor-pointer text-sm">
                    <Checkbox
                      checked={row.allResources}
                      onCheckedChange={(v) =>
                        setPermRows((rows) =>
                          rows.map((r, j) =>
                            j === i ? { ...r, allResources: v === true } : r,
                          ),
                        )
                      }
                    />
                    All resources
                  </label>
                  {!row.allResources && (
                    <Textarea
                      value={row.resourceIdsText}
                      onChange={(e) =>
                        setPermRows((rows) =>
                          rows.map((r, j) =>
                            j === i
                              ? { ...r, resourceIdsText: e.target.value }
                              : r,
                          ),
                        )
                      }
                      placeholder="Resource ids, comma or newline separated"
                      className="font-mono text-xs"
                    />
                  )}
                </div>
              ))}
              <Button
                variant="outline"
                size="sm"
                onClick={() => setPermRows((rows) => [...rows, emptyPermRow()])}
              >
                + Add permission
              </Button>
            </div>

            {roleError && (
              <div className="text-sm text-red-600">{roleError}</div>
            )}
          </div>
          <SheetFooter className="mt-6">
            <Button variant="outline" onClick={() => setRoleOpen(false)}>
              Cancel
            </Button>
            <Button onClick={submitRole} disabled={roleSubmitting}>
              {roleSubmitting ? "Saving…" : "Create role"}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      {/* New binding drawer */}
      <Sheet open={bindingOpen} onOpenChange={setBindingOpen}>
        <SheetContent className="overflow-y-auto">
          <SheetHeader>
            <SheetTitle>New binding</SheetTitle>
            <SheetDescription>Attach a role to a principal.</SheetDescription>
          </SheetHeader>
          <div className="space-y-4 mt-6">
            <div>
              <Label htmlFor="bind-role">Role</Label>
              <Select value={bindRoleId} onValueChange={setBindRoleId}>
                <SelectTrigger id="bind-role">
                  <SelectValue placeholder="Select a role" />
                </SelectTrigger>
                <SelectContent>
                  {(roles ?? []).map((r) => (
                    <SelectItem key={r.id} value={r.id}>
                      {r.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div>
              <Label htmlFor="bind-kind">Principal kind</Label>
              <Select value={bindKind} onValueChange={setBindKind}>
                <SelectTrigger id="bind-kind">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="token_label">token_label</SelectItem>
                  <SelectItem value="token_id">token_id</SelectItem>
                </SelectContent>
              </Select>
            </div>

            <div>
              <Label htmlFor="bind-ref">Principal ref</Label>
              <Input
                id="bind-ref"
                value={bindRef}
                onChange={(e) => setBindRef(e.target.value)}
                className="font-mono text-xs"
              />
              <p className="mt-1 text-[11px] text-muted-foreground">
                For a user who logs in via SSO, use kind token_label and ref{" "}
                <code className="rounded bg-muted px-1">
                  oidc:&lt;subject&gt;
                </code>
                . Use <code className="rounded bg-muted px-1">bootstrap</code>{" "}
                to bind the break-glass admin, or a token id for a specific
                token.
              </p>
            </div>

            {bindError && (
              <div className="text-sm text-red-600">{bindError}</div>
            )}
          </div>
          <SheetFooter className="mt-6">
            <Button variant="outline" onClick={() => setBindingOpen(false)}>
              Cancel
            </Button>
            <Button onClick={submitBinding} disabled={bindSubmitting}>
              {bindSubmitting ? "Saving…" : "Create binding"}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      {/* New tenant drawer */}
      <Sheet open={tenantOpen} onOpenChange={setTenantOpen}>
        <SheetContent className="overflow-y-auto">
          <SheetHeader>
            <SheetTitle>New tenant</SheetTitle>
            <SheetDescription>
              A tenant is an isolation boundary that owns API tokens and homes
              SSO users.
            </SheetDescription>
          </SheetHeader>
          <div className="space-y-4 mt-6">
            <div>
              <Label htmlFor="tenant-name">Name</Label>
              <Input
                id="tenant-name"
                value={tenantName}
                onChange={(e) => setTenantName(e.target.value)}
                placeholder="Acme"
              />
            </div>

            <div>
              <Label htmlFor="tenant-id">Tenant ID (optional)</Label>
              <Input
                id="tenant-id"
                value={tenantId}
                onChange={(e) => setTenantId(e.target.value)}
                className="font-mono text-xs"
              />
              <p className="mt-1 text-[11px] text-muted-foreground">
                Leave blank to auto-generate. Set this to match an SSO
                connection's tenant_claim value so claim-based login homes users
                here.
              </p>
            </div>

            {tenantError && (
              <div className="text-sm text-red-600">{tenantError}</div>
            )}
          </div>
          <SheetFooter className="mt-6">
            <Button variant="outline" onClick={() => setTenantOpen(false)}>
              Cancel
            </Button>
            <Button onClick={submitTenant} disabled={tenantSubmitting}>
              {tenantSubmitting ? "Saving…" : "Create tenant"}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      {/* Delete role */}
      <Dialog
        open={!!confirmDeleteRole}
        onOpenChange={(o) => !o && setConfirmDeleteRole(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete role</DialogTitle>
            <DialogDescription>
              Remove "{confirmDeleteRole?.name}"? Bindings referencing it will
              no longer grant access.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setConfirmDeleteRole(null)}
            >
              Cancel
            </Button>
            <Button variant="destructive" onClick={handleDeleteRole}>
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete binding */}
      <Dialog
        open={!!confirmDeleteBinding}
        onOpenChange={(o) => !o && setConfirmDeleteBinding(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete binding</DialogTitle>
            <DialogDescription>
              Remove the binding for "{confirmDeleteBinding?.principal_ref}"?
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setConfirmDeleteBinding(null)}
            >
              Cancel
            </Button>
            <Button variant="destructive" onClick={handleDeleteBinding}>
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete tenant */}
      <Dialog
        open={!!confirmDeleteTenant}
        onOpenChange={(o) => !o && setConfirmDeleteTenant(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete tenant</DialogTitle>
            <DialogDescription>
              Remove "{confirmDeleteTenant?.name}"? A tenant that still owns API
              tokens cannot be deleted.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setConfirmDeleteTenant(null)}
            >
              Cancel
            </Button>
            <Button variant="destructive" onClick={handleDeleteTenant}>
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

// DirectoryView renders the SCIM-provisioned users and groups for a tenant.
// Split out so the two SWR fetches key cleanly on the selected tenant.
function DirectoryView({ tenant }: { tenant: string }) {
  const { data: users } = useSWR<DirectoryUser[]>(
    ["directory-users", tenant],
    () => listDirectoryUsers(tenant),
    { shouldRetryOnError: false },
  );
  const { data: groups, mutate: mutateGroups } = useSWR<DirectoryGroup[]>(
    ["directory-groups", tenant],
    () => listDirectoryGroups(tenant),
    { shouldRetryOnError: false },
  );

  return (
    <div className="space-y-4">
      <div>
        <h2 className="text-sm font-medium mb-2">Users</h2>
        <Card>
          <CardContent className="p-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>User name</TableHead>
                  <TableHead>Email</TableHead>
                  <TableHead>External ID</TableHead>
                  <TableHead>Active</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(users ?? []).length === 0 && (
                  <TableRow>
                    <TableCell
                      colSpan={4}
                      className="text-center text-muted-foreground py-8"
                    >
                      No users provisioned.
                    </TableCell>
                  </TableRow>
                )}
                {(users ?? []).map((u) => (
                  <TableRow key={u.id}>
                    <TableCell className="font-medium">{u.user_name}</TableCell>
                    <TableCell>{u.email}</TableCell>
                    <TableCell className="font-mono text-xs break-all">
                      {u.external_id}
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant="outline"
                        className={
                          u.active
                            ? "bg-emerald-500/10 text-emerald-700 border-emerald-500/30"
                            : "bg-zinc-500/10 text-zinc-600 border-zinc-500/30"
                        }
                      >
                        {u.active ? "Active" : "Disabled"}
                      </Badge>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      </div>

      <div>
        <h2 className="text-sm font-medium mb-2">Groups</h2>
        <Card>
          <CardContent className="p-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Display name</TableHead>
                  <TableHead>Role mapping</TableHead>
                  <TableHead>External ID</TableHead>
                  <TableHead>Members</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(groups ?? []).length === 0 && (
                  <TableRow>
                    <TableCell
                      colSpan={4}
                      className="text-center text-muted-foreground py-8"
                    >
                      No groups provisioned.
                    </TableCell>
                  </TableRow>
                )}
                {(groups ?? []).map((g) => (
                  <TableRow key={g.id}>
                    <TableCell className="font-medium">
                      {g.display_name}
                    </TableCell>
                    <TableCell>
                      <GroupRoleCell
                        tenant={tenant}
                        group={g}
                        onChanged={() => mutateGroups()}
                      />
                    </TableCell>
                    <TableCell className="font-mono text-xs break-all">
                      {g.external_id}
                    </TableCell>
                    <TableCell>{g.members.length}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

// GroupRoleCell is the inline editor for a SCIM group's explicit RBAC role
// mapping (ADR 0019 slice 5a). The operator types a role id and Saves — the
// backend validates the role exists in the tenant before persisting on the
// group; Clear removes the mapping. The mapping overrides the create-time
// displayName==role-name convention and is re-read at each login, so a user
// provisioned into this group lands with these scopes automatically.
function GroupRoleCell({
  tenant,
  group,
  onChanged,
}: {
  tenant: string;
  group: DirectoryGroup;
  onChanged: () => void;
}) {
  const [value, setValue] = useState(group.role_id);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const dirty = value.trim() !== group.role_id;

  const save = async () => {
    setBusy(true);
    setErr(null);
    try {
      await setGroupRole(tenant, group.external_id, value.trim());
      onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "save failed");
    } finally {
      setBusy(false);
    }
  };

  const clear = async () => {
    setBusy(true);
    setErr(null);
    try {
      await clearGroupRole(tenant, group.external_id);
      setValue("");
      onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "clear failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-1">
      <div className="flex items-center gap-1">
        <Input
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder="role id (e.g. r-…)"
          className="h-8 font-mono text-xs w-44"
          disabled={busy}
        />
        <Button
          size="sm"
          variant="outline"
          className="h-8"
          onClick={save}
          disabled={busy || !dirty || value.trim() === ""}
        >
          Save
        </Button>
        <Button
          size="sm"
          variant="ghost"
          className="h-8"
          onClick={clear}
          disabled={busy || group.role_id === ""}
        >
          Clear
        </Button>
      </div>
      {err && <p className="text-xs text-red-600">{err}</p>}
    </div>
  );
}
