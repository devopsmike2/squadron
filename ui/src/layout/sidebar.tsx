/**
 * Squadron sidebar.
 *
 * Re-designed in v0.19 to compete with Bindplane / Grafana Fleet on
 * the chrome alone:
 *
 *  - Two-tone brand block at the top: Squadron logomark + tracked
 *    wordmark. Collapses to just the mark in the icon-rail state.
 *  - Nav items are grouped by intent (Fleet / Operations / Admin)
 *    so operators see structure, not a flat list. Each item picks
 *    up an active-state accent bar on the left edge so the current
 *    location is visible at a glance even on a quiet color theme.
 *  - Status awareness: the Agents item shows a small count badge
 *    when there are agents online; Alerts shows an attention dot
 *    when rules are configured. Cheap to query and gives the
 *    sidebar a "command bridge" feel.
 *  - The footer still hosts the quick-search hint and theme
 *    toggle, but they're visually quieter so they don't compete
 *    with the navigation.
 */

import {
  Server,
  Users,
  FileText,
  BarChart3,
  GitBranch,
  Bell,
  Rocket,
  ScrollText,
  KeyRound,
  LayoutDashboard,
} from "lucide-react";
import * as React from "react";
import { Link, useLocation } from "react-router-dom";
import useSWR from "swr";

import { ModeToggle } from "./mode-toggle";

import { getAgentStats } from "@/api/agents";
import { listAlertRules } from "@/api/alerts";
import { SquadronMark } from "@/components/brand/SquadronMark";
import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarFooter,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarTrigger,
  useSidebar,
} from "@/components/ui/sidebar";

interface MenuItem {
  key: string;
  title: string;
  url: string;
  icon: React.ComponentType<{ className?: string }>;
  /** Optional status decoration shown next to the label. */
  status?: React.ReactNode;
}

interface MenuGroup {
  label: string;
  items: MenuItem[];
}

export function AppSidebar() {
  const location = useLocation();
  const { state } = useSidebar();
  const collapsed = state === "collapsed";

  // Status-aware decorations. Cheap fetches kept here (not in each
  // page) so the sidebar can render a count even before the user
  // navigates anywhere. SWR dedupes against page-level keys.
  const { data: stats } = useSWR("/agents/stats", () => getAgentStats(), {
    refreshInterval: 30000,
  });
  const { data: alerts } = useSWR("/alerts/rules", () => listAlertRules(), {
    refreshInterval: 30000,
  });

  const activeAlerts = (alerts ?? []).filter((a) => a.enabled).length;

  const groups: MenuGroup[] = [
    {
      label: "Fleet",
      items: [
        {
          key: "dashboard",
          title: "Fleet Status",
          url: "/",
          icon: LayoutDashboard,
        },
        {
          key: "agents",
          title: "Agents",
          url: "/agents",
          icon: Server,
          status:
            stats && stats.totalAgents > 0 ? (
              <NavCount
                value={`${stats.onlineAgents}/${stats.totalAgents}`}
                tone={stats.onlineAgents === stats.totalAgents ? "good" : "warn"}
              />
            ) : null,
        },
        {
          key: "fleet-map",
          title: "Fleet Map",
          url: "/fleet-map",
          icon: GitBranch,
        },
        {
          key: "groups",
          title: "Groups",
          url: "/groups",
          icon: Users,
        },
      ],
    },
    {
      label: "Operations",
      items: [
        {
          key: "configs",
          title: "Configs",
          url: "/configs",
          icon: FileText,
        },
        {
          key: "rollouts",
          title: "Rollouts",
          url: "/rollouts",
          icon: Rocket,
        },
        {
          key: "alerts",
          title: "Alerts",
          url: "/alerts",
          icon: Bell,
          status:
            activeAlerts > 0 ? (
              <NavCount value={activeAlerts} tone="info" />
            ) : null,
        },
        {
          key: "telemetry",
          title: "Telemetry",
          url: "/telemetry",
          icon: BarChart3,
        },
      ],
    },
    {
      label: "Admin",
      items: [
        {
          key: "audit",
          title: "Audit",
          url: "/audit",
          icon: ScrollText,
        },
        {
          key: "settings-tokens",
          title: "API tokens",
          url: "/settings/tokens",
          icon: KeyRound,
        },
      ],
    },
  ];

  return (
    <Sidebar
      collapsible="icon"
      className="border-r border-sidebar-border bg-sidebar"
    >
      <SidebarHeader className="h-16 border-b border-sidebar-border px-3">
        <div className="flex h-full items-center">
          {collapsed ? (
            // Collapsed state: mark only. Tooltip on hover reveals
            // the expand affordance.
            <div className="group relative mx-auto h-9 w-9">
              <div className="flex h-9 w-9 items-center justify-center rounded-md transition-colors group-hover:opacity-0">
                <SquadronMark className="h-5 w-5" />
              </div>
              <SidebarTrigger className="absolute inset-0 h-9 w-9 opacity-0 transition-opacity group-hover:opacity-100" />
            </div>
          ) : (
            <div className="flex w-full items-center justify-between">
              <Link
                to="/"
                className="brand-glow flex items-center gap-2 outline-none focus-visible:ring-2 focus-visible:ring-ring rounded-md"
              >
                <SquadronMark className="h-5 w-5" />
                <span className="text-sm font-semibold tracking-[0.18em] uppercase text-sidebar-foreground">
                  Squadron
                </span>
              </Link>
              <SidebarTrigger className="h-6 w-6 opacity-60 hover:opacity-100" />
            </div>
          )}
        </div>
      </SidebarHeader>

      <SidebarContent className="gap-1 pt-2">
        {groups.map((group, gi) => (
          <SidebarGroup key={group.label} className={gi === 0 ? "" : "mt-1"}>
            {!collapsed && (
              <SidebarGroupLabel className="text-[10px] uppercase tracking-[0.16em] text-sidebar-foreground/50 font-semibold">
                {group.label}
              </SidebarGroupLabel>
            )}
            <SidebarMenu>
              {group.items.map((item) => {
                const isActive =
                  item.url === "/"
                    ? location.pathname === "/"
                    : location.pathname.startsWith(item.url);
                return (
                  <SidebarMenuItem key={item.key}>
                    <SidebarMenuButton
                      asChild
                      isActive={isActive}
                      tooltip={item.title}
                      className="relative h-8"
                    >
                      <Link to={item.url}>
                        {/* Left accent bar — only renders for the
                            active route. Subtle but unmistakable, and
                            independent of the cell background so it
                            survives hover. */}
                        {isActive && (
                          <span
                            aria-hidden
                            className="absolute left-0 top-1/2 h-4 w-0.5 -translate-y-1/2 rounded-r-sm bg-primary"
                          />
                        )}
                        <item.icon className="h-4 w-4" />
                        {!collapsed && (
                          <>
                            <span>{item.title}</span>
                            {item.status && (
                              <span className="ml-auto">{item.status}</span>
                            )}
                          </>
                        )}
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                );
              })}
            </SidebarMenu>
          </SidebarGroup>
        ))}
      </SidebarContent>

      <SidebarFooter className="border-t border-sidebar-border">
        <SidebarMenu>
          {!collapsed && (
            <SidebarMenuItem>
              {/* Quick search hint — synthesizes a ⌘K keydown so the
                  global handler is the single source of truth. */}
              <button
                type="button"
                onClick={() => {
                  const isMac = navigator.platform.toLowerCase().includes("mac");
                  document.dispatchEvent(
                    new KeyboardEvent("keydown", {
                      key: "k",
                      metaKey: isMac,
                      ctrlKey: !isMac,
                      bubbles: true,
                    }),
                  );
                }}
                className="flex w-full items-center justify-between gap-2 rounded-md px-2 py-1.5 text-xs text-sidebar-foreground/60 hover:bg-sidebar-accent hover:text-sidebar-accent-foreground"
                aria-label="Open command palette"
              >
                <span>Quick search</span>
                <kbd className="rounded border border-sidebar-border bg-sidebar/40 px-1.5 py-0.5 font-mono text-[10px]">
                  ⌘K
                </kbd>
              </button>
            </SidebarMenuItem>
          )}
          <SidebarMenuItem>
            <ModeToggle iconOnly={collapsed} />
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
  );
}

/**
 * NavCount renders a tiny count or status pill next to a sidebar
 * label. Tone picks a token color so it threads through theme
 * switches without per-component tweaks.
 */
function NavCount({
  value,
  tone,
}: {
  value: React.ReactNode;
  tone: "good" | "warn" | "info" | "bad";
}) {
  const color = {
    good: "var(--success)",
    warn: "var(--warning)",
    info: "var(--info)",
    bad: "var(--destructive)",
  }[tone];
  return (
    <span
      className="font-tabular text-[10px] font-medium rounded-md border px-1.5 py-0.5"
      style={{
        color,
        borderColor: `color-mix(in oklch, ${color} 40%, transparent)`,
        background: `color-mix(in oklch, ${color} 12%, transparent)`,
      }}
    >
      {value}
    </span>
  );
}
