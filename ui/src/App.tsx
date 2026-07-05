import { useEffect, useState } from "react";
import {
  BrowserRouter as Router,
  Navigate,
  Route,
  Routes,
  useLocation,
  useNavigate,
} from "react-router-dom";

import Layout from "./layout";
import ActionsPage from "./pages/Actions";
import AgentsPage from "./pages/Agents";
import AlertsPage from "./pages/Alerts";
import AuditPage from "./pages/Audit";
import AuthCallbackPage from "./pages/AuthCallback";
import ConfigsPage from "./pages/Configs";
import CostInsightsPage from "./pages/CostInsights";
import DashboardPage from "./pages/Dashboard";
import DeployPage from "./pages/Deploy";
import DiscoveryPage from "./pages/Discovery";
import DiscoveryAWSPage from "./pages/DiscoveryAWS";
import DiscoveryAzurePage from "./pages/DiscoveryAzure";
import DiscoveryGCPPage from "./pages/DiscoveryGCP";
import DiscoveryIaCGitHubPage from "./pages/DiscoveryIaCGitHub";
import DiscoveryOCIPage from "./pages/DiscoveryOCI";
import FleetMapPage from "./pages/FleetMap";
import GroupsPage from "./pages/Groups";
import IncidentsPage from "./pages/Incidents";
import InventoryPage from "./pages/Inventory";
import LoginPage from "./pages/Login";
import PlanPage from "./pages/Plan";
import ProposerPlaygroundPage from "./pages/ProposerPlayground";
import QuickstartPage from "./pages/Quickstart";
import RolloutsPage from "./pages/Rollouts";
import RunnersPage from "./pages/Runners";
import SavingsPage from "./pages/Savings";
import SettingsSiemPage from "./pages/SettingsSiem";
import SettingsTokensPage from "./pages/SettingsTokens";
import TelemetryPage from "./pages/Telemetry";
import TimelinePage from "./pages/Timeline";
import UseCasesPage from "./pages/UseCases";

import "./App.css";
import {
  getAuthToken,
  subscribeAuthChallenge,
  subscribeAuthChange,
} from "@/api/auth-store";
import { AskSquadronDialog } from "@/components/AskSquadronDialog";
import { CommandPalette } from "@/components/CommandPalette";
import { CommandPaletteHint } from "@/components/CommandPaletteHint";
import { EventSubscriber } from "@/components/EventSubscriber";
import { KeyboardShortcutsHelp } from "@/components/KeyboardShortcutsHelp";
import { ThemeProvider } from "@/components/ThemeProvider";
import { TourHost } from "@/components/tour/TourHost";
import { SWRProvider } from "@/lib/swr-provider";
import { ApiProvider } from "@/providers/ApiProvider";

function App() {
  return (
    <ThemeProvider defaultTheme="dark">
      <SWRProvider>
        <ApiProvider>
          <Router>
            {/* AuthBoundary watches for 401s and redirects to /login.
                Mounted inside Router so it can navigate. It also
                renders the routes itself so the unauthenticated state
                doesn't briefly flash the protected UI. */}
            <AuthBoundary />
          </Router>
        </ApiProvider>
      </SWRProvider>
    </ThemeProvider>
  );
}

// AuthBoundary handles two cases:
//   1. A 401 from any API call → emit a challenge → redirect to /login.
//   2. The operator explicitly setting/clearing a token → re-render so
//      the protected UI mounts immediately after a successful sign-in.
//
// Auth is opt-in on the server, so requests succeeding without a token
// is a legitimate state. We don't gate the protected routes on
// "has token in localStorage" because in dev-mode (auth disabled)
// there will never be one. Instead we route to /login only when the
// server has actually 401'd us — that's the signal that auth is on.
function AuthBoundary() {
  const location = useLocation();
  const nav = useNavigate();
  // tick is a render-trigger; subscribe handlers bump it so the global
  // mounts (palette, events, shortcuts) re-evaluate.
  const [, setTick] = useState(0);

  useEffect(() => {
    const offChallenge = subscribeAuthChallenge(() => {
      // Carry the pre-login path so we can return there after sign-in
      // (future enhancement — for now we just dump the operator on
      // the home page).
      nav("/login", { replace: true });
      setTick((t) => t + 1);
    });
    const offChange = subscribeAuthChange(() => setTick((t) => t + 1));
    return () => {
      offChallenge();
      offChange();
    };
  }, [nav]);

  // Pre-auth routes render standalone (no Layout, no global subscriptions that
  // would fire authenticated fetches). /auth/callback is the OIDC→frontend
  // handoff landing (ADR 0014): it must be treated as pre-auth so no authed
  // fetch / 401 bounce fires before it stores the minted bearer.
  const isPreAuthRoute =
    location.pathname === "/login" || location.pathname === "/auth/callback";
  const hasToken = Boolean(getAuthToken());

  // Login + callback routes mount standalone (no Layout, no global
  // subscriptions that would try to fetch authenticated endpoints). Once
  // authenticated the operator gets the full app.
  if (isPreAuthRoute) {
    return (
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/auth/callback" element={<AuthCallbackPage />} />
        {/* Other paths under unauthenticated bounce back to /login.
            React-router needs a fallback that doesn't cause a render
            loop with our redirect, so this is just defensive. */}
        <Route path="*" element={<Navigate to="/login" replace />} />
      </Routes>
    );
  }

  return (
    <>
      {/* Global ⌘K command palette. Mounted inside the Router so its
          items can use useNavigate. */}
      <CommandPalette />
      {/* v0.64 — Ask Squadron conversational dialog. Mounted at
          App root so any caller can open it by dispatching the
          ASK_OPEN_EVENT; ⌘K's Ask Squadron entry is the first
          dispatch site. */}
      <AskSquadronDialog />
      {/* First-session hint pointing at ⌘K. localStorage-flagged so
          it fires exactly once per browser. v0.38 — discoverability
          pass after the command palette went underutilized in
          early usage. */}
      <CommandPaletteHint />
      {/* SSE-driven cache invalidator. Listens to Squadron's event
          stream and revalidates the relevant SWR caches so pages stay
          live without each one wiring its own subscription. */}
      <EventSubscriber />
      {/* Global keyboard shortcut system + ? help overlay. */}
      <KeyboardShortcutsHelp />
      {/* Guided use-case tours. Mounted at root so it can navigate across
          routes and portal a coach-mark overlay over any page. Idle until a
          TOUR_START_EVENT fires (from the Use Cases page). */}
      <TourHost />
      <Routes>
        {/* Main application routes */}
        <Route element={<Layout />}>
          <Route path="/" element={<DashboardPage />} />
          <Route path="/use-cases" element={<UseCasesPage />} />
          <Route path="/quickstart" element={<QuickstartPage />} />
          <Route path="/agents" element={<AgentsPage />} />
          <Route path="/groups" element={<GroupsPage />} />
          <Route path="/configs" element={<ConfigsPage />} />
          <Route path="/configs/new" element={<ConfigsPage mode="create" />} />
          <Route
            path="/configs/:configId/edit"
            element={<ConfigsPage mode="edit" />}
          />
          <Route path="/telemetry" element={<TelemetryPage />} />
          <Route path="/savings" element={<SavingsPage />} />
          <Route path="/cost-insights" element={<CostInsightsPage />} />
          <Route path="/fleet-map" element={<FleetMapPage />} />
          {/* Back-compat alias for v0.19 bookmarks; Fleet Map is the
              canonical URL going forward. */}
          <Route path="/topology" element={<FleetMapPage />} />
          <Route path="/alerts" element={<AlertsPage />} />
          <Route path="/rollouts" element={<RolloutsPage />} />
          {/* v0.75 — multi step plan detail. Reached via the Plan
              badge on any rollout card whose plan_id is set. */}
          <Route path="/plans/:id" element={<PlanPage />} />
          {/* v0.84 — proposer playground. Operator-facing tool for
              dogfooding the proposer without seeding a real spike.
              Closes Arc C (proposer plan output → bench →
              playground). */}
          <Route
            path="/playground/proposer"
            element={<ProposerPlaygroundPage />}
          />
          <Route path="/deploy" element={<DeployPage />} />
          {/* v0.89.62 #689 Stream 87 (slice-1 chunk 2) — unified
              Discovery dashboard. Aggregates connection / instance /
              recommendation counts across all four clouds into a
              single landing screen. Mounted at /discovery so the
              sidebar Discovery group header can navigate here as the
              default; the per-provider /discovery/<cloud> routes
              still host the wizard surfaces. Closes the unified
              dashboard slice 1 (design doc
              docs/proposals/unified-discovery-dashboard-slice1.md). */}
          <Route path="/discovery" element={<DiscoveryPage />} />
          {/* v0.85 Stream 2E — universal-discovery first user-facing
              payoff. /discovery/aws hosts the Account / Inventory /
              Recommendations triptych the design doc calls out. */}
          <Route path="/discovery/aws" element={<DiscoveryAWSPage />} />
          {/* v0.89.48 #670 Stream 68 (slice-1 chunk 4) — GCP discovery
              page parallels DiscoveryAWS. Wizard / Inventory /
              Recommendations triptych under /discovery/gcp so the
              namespace stays uniform across providers. */}
          <Route path="/discovery/gcp" element={<DiscoveryGCPPage />} />
          {/* v0.89.53 #677 Stream 75 (slice-1 chunk 4) — Azure discovery
              page parallels DiscoveryGCP. Same Wizard / Inventory /
              Recommendations triptych under /discovery/azure. Sits
              between GCP and the IaC GitHub entry so the
              /discovery/<cloud> routes group together. */}
          <Route path="/discovery/azure" element={<DiscoveryAzurePage />} />
          {/* v0.89.58 #684 Stream 82 (slice-1 chunk 4) — OCI discovery
              page parallels DiscoveryAzure. Same Wizard / Inventory /
              Recommendations triptych under /discovery/oci. Sits next
              to Azure so all four /discovery/<cloud> routes (AWS,
              GCP, Azure, OCI) cluster together before the IaC GitHub
              entry. */}
          <Route path="/discovery/oci" element={<DiscoveryOCIPage />} />
          {/* v0.89.3 Stream 19 — IaC connections list + Connect-IaC-repo
              wizard. /discovery/iac/<provider> namespace leaves room
              for slice-2 GitLab / Bitbucket without renaming routes. */}
          <Route
            path="/discovery/iac/github"
            element={<DiscoveryIaCGitHubPage />}
          />
          {/* v0.55 SQ-2.8 / N4 — operational visibility into the action
              runner system. /actions lists every signed request
              Squadron dispatched; /runners lists the daemons that
              accepted them. Sit next to Rollouts in Operations since
              they answer the same operator question: what changed and
              who applied it. */}
          <Route path="/actions" element={<ActionsPage />} />
          <Route path="/runners" element={<RunnersPage />} />
          {/* v0.54 Move 3 — incident drafter inbox. Sits in the
              Operations group next to Rollouts and Deploy because
              that's where the operator already lives during an
              incident response. */}
          <Route path="/incidents" element={<IncidentsPage />} />
          <Route path="/audit" element={<AuditPage />} />
          {/* v0.41.1 — full Inventory drill-in page. The "See
              inventory for details" link on Fleet Status pointed
              here before but the route didn't exist, so the link
              rendered blank. */}
          <Route path="/inventory" element={<InventoryPage />} />
          {/* v0.40 postmortem timeline — merges audit, deploy,
              cost-spike events onto one horizontal axis. */}
          <Route path="/timeline" element={<TimelinePage />} />
          <Route path="/settings/tokens" element={<SettingsTokensPage />} />
          <Route path="/settings/siem" element={<SettingsSiemPage />} />
        </Route>
      </Routes>
      {/* hasToken is referenced so React's lint doesn't strip the
          subscription; the actual auth gate is the simpleRequest 401
          handler that triggers a navigation to /login. */}
      {hasToken && null}
    </>
  );
}

export default App;
