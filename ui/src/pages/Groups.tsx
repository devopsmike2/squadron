import { CalendarClock, Plus, RefreshCw, Server, ShieldCheck, Trash2, Users } from "lucide-react";
import { useState } from "react";
import useSWR from "swr";

import { getGroups, createGroup, deleteGroup, updateGroup } from "@/api/groups";
import type { Group, CreateGroupRequest } from "@/api/groups";
import { GroupDetailsDrawer } from "@/components/GroupDetailsDrawer";
import { ChangeWindowsModal } from "@/components/groups/ChangeWindowsModal";
import { PageTable } from "@/components/shared/PageTable";
import { Button } from "@/components/ui/button";
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
import { TableCell } from "@/components/ui/table";

export default function GroupsPage() {
  const [refreshing, setRefreshing] = useState(false);
  const [createDrawerOpen, setCreateDrawerOpen] = useState(false);
  const [deleteDialogOpen, setDeleteDialogOpen] = useState(false);
  const [selectedGroup, setSelectedGroup] = useState<Group | null>(null);
  const [selectedGroupId, setSelectedGroupId] = useState<string | null>(null);
  const [groupDrawerOpen, setGroupDrawerOpen] = useState(false);
  // v0.49 — change-windows modal. Keyed by group so opening from
  // a different row re-snapshots the windows.
  const [windowsGroup, setWindowsGroup] = useState<Group | null>(null);
  const [windowsOpen, setWindowsOpen] = useState(false);
  const [createForm, setCreateForm] = useState<CreateGroupRequest>({
    name: "",
    labels: {},
    require_approval: false,
  });

  const {
    data: groupsData,
    error: groupsError,
    mutate: mutateGroups,
  } = useSWR("groups", getGroups, { refreshInterval: 30000 });

  const handleRefresh = async () => {
    setRefreshing(true);
    await mutateGroups();
    setRefreshing(false);
  };

  const handleCreateGroup = async () => {
    try {
      await createGroup(createForm);
      setCreateDrawerOpen(false);
      setCreateForm({ name: "", labels: {}, require_approval: false });
      await mutateGroups();
    } catch (error) {
      console.error("Failed to create group:", error);
    }
  };

  // v0.48 — toggle the require_approval policy on an existing group.
  // The list refetches afterward so the badge updates immediately.
  // Errors are surfaced via window.alert because the table row has
  // no obvious place to put a toast — UX can iterate on this later.
  const handleToggleApprovalPolicy = async (
    group: Group,
    e: React.MouseEvent,
  ) => {
    e.stopPropagation();
    try {
      await updateGroup(group.id, { require_approval: !group.require_approval });
      await mutateGroups();
    } catch (error) {
      console.error("Failed to update group policy:", error);
      window.alert(
        error instanceof Error
          ? error.message
          : "Failed to update approval policy",
      );
    }
  };

  const handleDeleteGroup = async () => {
    if (!selectedGroup) return;
    try {
      await deleteGroup(selectedGroup.id);
      setDeleteDialogOpen(false);
      setSelectedGroup(null);
      await mutateGroups();
    } catch (error) {
      console.error("Failed to delete group:", error);
    }
  };

  const handleGroupClick = (groupId: string) => {
    setSelectedGroupId(groupId);
    setGroupDrawerOpen(true);
  };

  const handleDeleteClick = (group: Group, e: React.MouseEvent) => {
    e.stopPropagation();
    setSelectedGroup(group);
    setDeleteDialogOpen(true);
  };

  if (groupsError) {
    return (
      <div className="container mx-auto p-6">
        <div className="text-center">
          <h1 className="text-2xl font-bold text-red-600 mb-4">
            Error Loading Groups
          </h1>
          <p className="text-muted-foreground">{groupsError.message}</p>
          <Button onClick={handleRefresh} className="mt-4">
            <RefreshCw className="h-4 w-4 mr-2" />
            Retry
          </Button>
        </div>
      </div>
    );
  }

  const groups = groupsData?.groups || [];

  return (
    <>
      <PageTable
        pageTitle="Groups"
        pageDescription="Organize agents into groups for easier management"
        pageActions={[
          {
            label: "Refresh",
            icon: RefreshCw,
            onClick: handleRefresh,
            disabled: refreshing,
            variant: "ghost" as const,
          },
          {
            label: "Create Group",
            icon: Plus,
            onClick: () => setCreateDrawerOpen(true),
            variant: "default" as const,
          },
        ]}
        cardTitle={`Groups (${groups.length})`}
        cardDescription="All agent groups and their details"
        columns={[
          { header: "Name", key: "name" },
          { header: "Agents", key: "agents" },
          { header: "Config", key: "config" },
          { header: "Policy", key: "policy" },
          { header: "Created", key: "created" },
          { header: "Updated", key: "updated" },
          { header: "Labels", key: "labels" },
          { header: "Actions", key: "actions" },
        ]}
        data={groups}
        getRowKey={(group: Group) => group.id}
        onRowClick={(group: Group) => handleGroupClick(group.id)}
        renderRow={(group: Group) => (
          <>
            <TableCell className="font-medium">{group.name}</TableCell>
            <TableCell>
              <div className="flex items-center gap-2">
                <Server className="h-4 w-4 text-muted-foreground" />
                <span>{group.agent_count}</span>
              </div>
            </TableCell>
            <TableCell>
              {group.config_name ? (
                <span className="text-sm font-mono text-muted-foreground">
                  {group.config_name}
                </span>
              ) : (
                <span className="text-xs text-muted-foreground">No config</span>
              )}
            </TableCell>
            <TableCell>
              {/* v0.48 — approval-policy toggle. Click flips the
                  require_approval flag on this group; when on, every
                  rollout to this group is forced into pending_approval
                  regardless of what the requester sets on the create
                  form. The badge is intentionally clickable (rather
                  than a row-action) so policy changes are one click
                  from the list view.
                  v0.49 — change-windows count + edit button. The
                  count badge surfaces 'this group has N blackout
                  rules' so operators can see policy density before
                  drilling in. */}
              <div className="flex items-center gap-1">
                <button
                  type="button"
                  onClick={(e) => handleToggleApprovalPolicy(group, e)}
                  className={`inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-medium transition-colors ${
                    group.require_approval
                      ? "border-orange-500/30 bg-orange-500/10 text-orange-700 hover:bg-orange-500/20"
                      : "border-border bg-muted/40 text-muted-foreground hover:bg-muted"
                  }`}
                  title={
                    group.require_approval
                      ? "Approval required — click to disable"
                      : "Approval optional — click to require for all rollouts"
                  }
                >
                  <ShieldCheck className="h-3 w-3" />
                  {group.require_approval ? "Required" : "Optional"}
                </button>
                <button
                  type="button"
                  onClick={(e) => {
                    e.stopPropagation();
                    setWindowsGroup(group);
                    setWindowsOpen(true);
                  }}
                  className={`inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-medium transition-colors ${
                    (group.change_windows?.length ?? 0) > 0
                      ? "border-orange-500/30 bg-orange-500/10 text-orange-700 hover:bg-orange-500/20"
                      : "border-border bg-muted/40 text-muted-foreground hover:bg-muted"
                  }`}
                  title="Manage change windows"
                >
                  <CalendarClock className="h-3 w-3" />
                  {group.change_windows?.length ?? 0}
                </button>
              </div>
            </TableCell>
            <TableCell>
              {new Date(group.created_at).toLocaleDateString()}
            </TableCell>
            <TableCell>
              {new Date(group.updated_at).toLocaleDateString()}
            </TableCell>
            <TableCell>
              <div className="flex flex-wrap gap-1">
                {Object.entries(group.labels).map(([key, value]) => (
                  <span
                    key={key}
                    className="text-xs bg-gray-100 dark:bg-gray-800 px-2 py-1 rounded"
                  >
                    {key}={value}
                  </span>
                ))}
                {Object.keys(group.labels).length === 0 && (
                  <span className="text-xs text-muted-foreground">
                    No labels
                  </span>
                )}
              </div>
            </TableCell>
            <TableCell>
              <Button
                variant="ghost"
                size="sm"
                onClick={(e) => handleDeleteClick(group, e)}
                className="text-red-600 hover:text-red-700 hover:bg-red-50 dark:text-red-400 dark:hover:text-red-300 dark:hover:bg-red-950/30"
              >
                <Trash2 className="h-4 w-4" />
              </Button>
            </TableCell>
          </>
        )}
        emptyState={{
          icon: Users,
          title: "No Groups Found",
          description: "Create your first group to organize your agents.",
          action: {
            label: "Create Group",
            onClick: () => setCreateDrawerOpen(true),
          },
        }}
      />

      {/* Create Group Drawer */}
      <Sheet open={createDrawerOpen} onOpenChange={setCreateDrawerOpen}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>Create New Group</SheetTitle>
            <SheetDescription>
              Create a new group to organize your agents.
            </SheetDescription>
          </SheetHeader>
          <div className="space-y-4 mt-6">
            <div>
              <Label htmlFor="name">Group Name</Label>
              <Input
                id="name"
                value={createForm.name}
                onChange={(e) =>
                  setCreateForm({ ...createForm, name: e.target.value })
                }
                placeholder="Enter group name"
              />
            </div>
            {/* v0.48 — approval-policy toggle. Defaults off so the
                form behaves as it did before. When on, every rollout
                to this group is forced into pending_approval at
                create time regardless of what the rollout form sets.
                Used to mark production-tier groups for NERC CIP-style
                separation of duties. */}
            <div className="rounded-md border border-orange-500/20 bg-orange-500/5 p-3">
              <label className="flex items-start gap-2 cursor-pointer">
                <input
                  type="checkbox"
                  className="mt-0.5 h-4 w-4"
                  checked={createForm.require_approval ?? false}
                  onChange={(e) =>
                    setCreateForm({
                      ...createForm,
                      require_approval: e.target.checked,
                    })
                  }
                />
                <div className="space-y-0.5">
                  <div className="text-sm font-medium">
                    Require approval for all rollouts to this group
                  </div>
                  <p className="text-[11px] text-muted-foreground leading-relaxed">
                    Forces every rollout targeting this group into
                    pending_approval. A second operator (not the
                    requester) must approve before the engine
                    advances. Use for production-tier or NERC
                    CIP-regulated groups.
                  </p>
                </div>
              </label>
            </div>
          </div>
          <SheetFooter className="mt-6">
            <Button
              variant="outline"
              onClick={() => setCreateDrawerOpen(false)}
            >
              Cancel
            </Button>
            <Button onClick={handleCreateGroup} disabled={!createForm.name}>
              Create Group
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      {/* Delete Confirmation Dialog */}
      <Dialog open={deleteDialogOpen} onOpenChange={setDeleteDialogOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete Group</DialogTitle>
            <DialogDescription>
              Are you sure you want to delete the group "{selectedGroup?.name}"?
              This action cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setDeleteDialogOpen(false)}
            >
              Cancel
            </Button>
            <Button variant="destructive" onClick={handleDeleteGroup}>
              Delete Group
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <GroupDetailsDrawer
        groupId={selectedGroupId}
        open={groupDrawerOpen}
        onOpenChange={setGroupDrawerOpen}
      />

      {/* v0.49 — change-windows modal. Opens from the policy
          column's calendar badge. Saves via PUT /groups/:id; the
          mutate refreshes the list so the count badge updates. */}
      <ChangeWindowsModal
        group={windowsGroup}
        open={windowsOpen}
        onClose={() => setWindowsOpen(false)}
        onSaved={() => {
          void mutateGroups();
        }}
      />
    </>
  );
}
