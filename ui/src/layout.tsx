import { Outlet, useLocation } from "react-router-dom";

import { AppSidebar } from "./layout/sidebar";

import { LicenseBanner } from "@/components/LicenseBanner";
import { ErrorBoundary } from "@/components/ui/error-boundary";
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar";
import { getCookie } from "@/lib/utils";

export default function Layout() {
  // Read the sidebar state from cookie, default to true (expanded) if not found
  const sidebarCookie = getCookie("sidebar_state");
  const defaultOpen = sidebarCookie === null ? true : sidebarCookie === "true";

  // Reset the boundary on navigation. Without this, once a route throws
  // the boundary latches into its error state and every subsequent
  // route renders the fallback; keying on the pathname remounts a fresh
  // boundary per route so navigating away recovers cleanly.
  const location = useLocation();

  return (
    <SidebarProvider
      defaultOpen={defaultOpen}
      className="flex h-screen w-screen overflow-hidden"
    >
      <AppSidebar />
      <SidebarInset className="flex-1">
        <div className="flex flex-1 flex-col gap-4 p-4 w-full h-screen min-h-0 overflow-auto">
          <LicenseBanner />
          {/* A single crashing route must NOT blank the whole app.
              The Outlet (the active page) sits behind an error
              boundary so an unguarded render error degrades to an
              inline message while the sidebar, header and license
              banner above stay mounted — the durable lesson from the
              Groups `labels: null` blank-page field bug. */}
          <ErrorBoundary
            key={location.pathname}
            fallback={
              <div role="alert" className="container mx-auto p-6">
                <div className="rounded-md border border-destructive/40 bg-destructive/5 p-6">
                  <h1 className="text-xl font-semibold mb-2">
                    This page failed to render
                  </h1>
                  <p className="text-sm text-muted-foreground">
                    Something went wrong loading this view. The rest of Squadron
                    is still available — pick another page from the sidebar, or
                    reload to try again.
                  </p>
                </div>
              </div>
            }
          >
            <Outlet />
          </ErrorBoundary>
        </div>
      </SidebarInset>
    </SidebarProvider>
  );
}
