// CommandPalette is the global ⌘K (Ctrl+K) launcher. It folds together
// page navigation, common actions, and live entities (agents, alert rules)
// behind one fuzzy-search input so operators never have to hunt through
// menus to do common things.
//
// Mounted once at the App root so the same instance handles every keyboard
// trigger; it lives inside <Router> so useNavigate is available.

import * as DialogPrimitive from "@radix-ui/react-dialog";
import { Command } from "cmdk";
import {
  Bell,
  FileText,
  GitBranch,
  Plus,
  Search,
  Server,
  Users,
  BarChart3,
  Sparkle,
} from "lucide-react";
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import useSWR from "swr";

import { listAlertRules } from "@/api/alerts";

import type { Agent } from "@/types/agent";
import type { AlertRule } from "@/types/alert";

interface AgentsListResponse {
  agents: Record<string, Agent>;
}

// fetchAgentsRaw is a thin adapter around the same /agents endpoint the
// Agents page uses, but returns the raw map. Kept local so the palette
// doesn't pull in the broader agents API client surface.
const fetchAgentsRaw = async (): Promise<Agent[]> => {
  const { simpleRequest } = await import("@/api/base");
  const data = await simpleRequest<AgentsListResponse>("/agents");
  return Object.values(data.agents ?? {});
};

export function CommandPalette() {
  const [open, setOpen] = useState(false);
  const navigate = useNavigate();

  // Wire the global keyboard shortcut. Match the most common conventions
  // (cmd+k on mac, ctrl+k everywhere else). Toggling means a second press
  // closes the palette without needing Escape.
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((prev) => !prev);
      }
    };
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  }, []);

  // Live data — only fetched when the palette has been opened at least
  // once, so we don't pay a fleet listing round trip on every page load.
  // SWR's revalidateOnFocus will refresh when the user comes back.
  const { data: agents } = useSWR<Agent[]>(
    open ? "command-palette/agents" : null,
    fetchAgentsRaw,
  );
  const { data: alertRules } = useSWR<AlertRule[]>(
    open ? "command-palette/alert-rules" : null,
    listAlertRules,
  );

  const go = (path: string) => {
    setOpen(false);
    navigate(path);
  };

  return (
    <DialogPrimitive.Root open={open} onOpenChange={setOpen}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-black/40 data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0" />
        <DialogPrimitive.Content
          aria-label="Squadron command palette"
          className="fixed left-1/2 top-[12vh] z-50 w-[92vw] max-w-2xl -translate-x-1/2 rounded-lg border bg-popover text-popover-foreground shadow-xl overflow-hidden focus:outline-none"
        >
          <DialogPrimitive.Title className="sr-only">
            Squadron command palette
          </DialogPrimitive.Title>
          <Command label="Squadron command palette" className="flex flex-col">
            <div className="flex items-center gap-2 border-b px-3">
              <Search className="h-4 w-4 text-muted-foreground shrink-0" />
              <Command.Input
                autoFocus
                placeholder="Jump to a page, agent, rule — or run a command..."
                className="flex-1 h-11 bg-transparent text-sm outline-none placeholder:text-muted-foreground"
              />
              <kbd className="text-[10px] font-mono text-muted-foreground border rounded px-1.5 py-0.5 hidden sm:inline">
                esc
              </kbd>
            </div>

            <Command.List className="max-h-[60vh] overflow-y-auto p-2">
              <Command.Empty className="px-3 py-6 text-sm text-center text-muted-foreground">
                No matches.
              </Command.Empty>

              <Command.Group
                heading="Pages"
                className="text-xs text-muted-foreground [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:tracking-wider"
              >
                <PaletteItem
                  icon={<Server className="h-4 w-4" />}
                  onSelect={() => go("/agents")}
                >
                  Agents
                </PaletteItem>
                <PaletteItem
                  icon={<GitBranch className="h-4 w-4" />}
                  onSelect={() => go("/topology")}
                >
                  Topology
                </PaletteItem>
                <PaletteItem
                  icon={<Users className="h-4 w-4" />}
                  onSelect={() => go("/groups")}
                >
                  Groups
                </PaletteItem>
                <PaletteItem
                  icon={<FileText className="h-4 w-4" />}
                  onSelect={() => go("/configs")}
                >
                  Configs
                </PaletteItem>
                <PaletteItem
                  icon={<BarChart3 className="h-4 w-4" />}
                  onSelect={() => go("/telemetry")}
                >
                  Telemetry
                </PaletteItem>
                <PaletteItem
                  icon={<Bell className="h-4 w-4" />}
                  onSelect={() => go("/alerts")}
                >
                  Alerts
                </PaletteItem>
              </Command.Group>

              <Command.Group
                heading="Actions"
                className="text-xs text-muted-foreground [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:tracking-wider"
              >
                <PaletteItem
                  icon={<Plus className="h-4 w-4" />}
                  onSelect={() => go("/configs/new")}
                  keywords={["create", "new", "config"]}
                >
                  New collector config
                </PaletteItem>
                <PaletteItem
                  icon={<Plus className="h-4 w-4" />}
                  onSelect={() => go("/alerts")}
                  keywords={["create", "new", "alert", "rule"]}
                >
                  New alert rule
                </PaletteItem>
              </Command.Group>

              {agents && agents.length > 0 && (
                <Command.Group
                  heading="Agents"
                  className="text-xs text-muted-foreground [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:tracking-wider"
                >
                  {agents.map((a) => (
                    <PaletteItem
                      key={a.id}
                      icon={<Sparkle className="h-4 w-4 text-emerald-500" />}
                      onSelect={() => go(`/agents?agent=${a.id}`)}
                      keywords={[a.name, a.id, a.version, a.group_name ?? ""]}
                      rightHint={a.status}
                    >
                      {a.name}
                    </PaletteItem>
                  ))}
                </Command.Group>
              )}

              {alertRules && alertRules.length > 0 && (
                <Command.Group
                  heading="Alert rules"
                  className="text-xs text-muted-foreground [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:tracking-wider"
                >
                  {alertRules.map((r) => (
                    <PaletteItem
                      key={r.id}
                      icon={<Bell className="h-4 w-4 text-amber-500" />}
                      onSelect={() => go("/alerts")}
                      keywords={[r.name, r.severity]}
                      rightHint={r.enabled ? "enabled" : "disabled"}
                    >
                      {r.name}
                    </PaletteItem>
                  ))}
                </Command.Group>
              )}
            </Command.List>
          </Command>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

interface PaletteItemProps {
  icon: React.ReactNode;
  children: React.ReactNode;
  onSelect: () => void;
  keywords?: string[];
  rightHint?: string;
}

// PaletteItem standardizes the row visual so every group looks the same.
// cmdk's value prop is what its fuzzy search runs against — we set it to
// the children text plus any explicit keywords so users can find an
// "agents" page by typing "list" if we register that keyword.
function PaletteItem({
  icon,
  children,
  onSelect,
  keywords,
  rightHint,
}: PaletteItemProps) {
  const label = typeof children === "string" ? children : "";
  const value = [label, ...(keywords ?? [])].filter(Boolean).join(" ");
  return (
    <Command.Item
      value={value}
      onSelect={onSelect}
      className="flex items-center gap-2 rounded px-2 py-2 text-sm cursor-pointer aria-selected:bg-accent aria-selected:text-accent-foreground"
    >
      <span className="shrink-0">{icon}</span>
      <span className="flex-1 truncate">{children}</span>
      {rightHint && (
        <span className="text-[10px] uppercase tracking-wider text-muted-foreground">
          {rightHint}
        </span>
      )}
    </Command.Item>
  );
}
