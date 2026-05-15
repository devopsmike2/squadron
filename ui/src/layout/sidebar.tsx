import {
  Server,
  Users,
  FileText,
  BarChart3,
  GitBranch,
  Bell,
  Rocket,
  ScrollText,
  Sparkle,
} from "lucide-react";
import * as React from "react";
import { Link, useLocation } from "react-router-dom";

import { ModeToggle } from "./mode-toggle";

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
}

export function AppSidebar() {
  const location = useLocation();
  const { state } = useSidebar();

  const mainItems: MenuItem[] = [
    {
      key: "agents",
      title: "Agents",
      url: "/agents",
      icon: Server,
    },
    {
      key: "topology",
      title: "Topology",
      url: "/topology",
      icon: GitBranch,
    },
    {
      key: "groups",
      title: "Groups",
      url: "/groups",
      icon: Users,
    },
    {
      key: "configs",
      title: "Configs",
      url: "/configs",
      icon: FileText,
    },
    {
      key: "telemetry",
      title: "Telemetry",
      url: "/telemetry",
      icon: BarChart3,
    },
    {
      key: "alerts",
      title: "Alerts",
      url: "/alerts",
      icon: Bell,
    },
    {
      key: "rollouts",
      title: "Rollouts",
      url: "/rollouts",
      icon: Rocket,
    },
    {
      key: "audit",
      title: "Audit",
      url: "/audit",
      icon: ScrollText,
    },
  ];

  return (
    <Sidebar collapsible="icon" className="border-r border-border">
      <SidebarHeader className="border-b border-border h-16 flex items-center justify-center relative">
        <SidebarMenu>
          <SidebarMenuItem>
            {state === "collapsed" ? (
              <div className="relative group">
                <div className="flex items-center justify-center h-8 w-8 rounded-md transition-colors group-hover:opacity-0">
                  <Sparkle className="h-4 w-4 text-primary" />
                </div>
                <SidebarTrigger className="absolute inset-0 opacity-0 group-hover:opacity-100 transition-opacity" />
              </div>
            ) : (
              <SidebarMenuButton asChild>
                <a href="/" className="flex items-center space-x-2">
                  <Sparkle className="h-4 w-4 text-primary" />
                  <span>Squadron</span>
                </a>
              </SidebarMenuButton>
            )}
          </SidebarMenuItem>
        </SidebarMenu>
        {state === "expanded" && (
          <SidebarTrigger className="absolute right-2 top-1/2 -translate-y-1/2 h-6 w-6" />
        )}
      </SidebarHeader>

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Navigation</SidebarGroupLabel>
          <SidebarMenu>
            {mainItems.map((item) => {
              const isActive = location.pathname === item.url;
              return (
                <SidebarMenuItem key={item.title}>
                  <SidebarMenuButton
                    asChild
                    isActive={isActive}
                    tooltip={item.title}
                  >
                    <Link to={item.url} className="relative">
                      <item.icon />
                      {state === "expanded" && <span>{item.title}</span>}
                    </Link>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              );
            })}
          </SidebarMenu>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter className="border-t border-border">
        <SidebarMenu>
          {state === "expanded" && (
            <SidebarMenuItem>
              {/* Discoverability hint for the global command palette. The
                  keyboard handler lives in <CommandPalette />; this row is
                  purely a visual cue so first-time users find it. */}
              <button
                type="button"
                onClick={() => {
                  // Synthesize a ⌘K keypress so we route through the same
                  // global handler the keyboard does — keeps a single source
                  // of truth for what "open the palette" means.
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
                className="w-full flex items-center justify-between gap-2 px-2 py-1.5 rounded-md text-xs text-muted-foreground hover:bg-accent hover:text-accent-foreground"
                aria-label="Open command palette"
              >
                <span>Quick search</span>
                <kbd className="font-mono border rounded px-1.5 py-0.5">
                  ⌘K
                </kbd>
              </button>
            </SidebarMenuItem>
          )}
          <SidebarMenuItem>
            <ModeToggle iconOnly={state === "collapsed"} />
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
  );
}
