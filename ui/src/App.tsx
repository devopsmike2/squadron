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
import AgentsPage from "./pages/Agents";
import AlertsPage from "./pages/Alerts";
import AuditPage from "./pages/Audit";
import ConfigsPage from "./pages/Configs";
import CostInsightsPage from "./pages/CostInsights";
import DashboardPage from "./pages/Dashboard";
import DeployPage from "./pages/Deploy";
import FleetMapPage from "./pages/FleetMap";
import GroupsPage from "./pages/Groups";
import IncidentsPage from "./pages/Incidents";
import InventoryPage from "./pages/Inventory";
import LoginPage from "./pages/Login";
import QuickstartPage from "./pages/Quickstart";
import RolloutsPage from "./pages/Rollouts";
import SavingsPage from "./pages/Savings";
import SettingsSiemPage from "./pages/SettingsSiem";
import SettingsTokensPage from "./pages/SettingsTokens";
import TelemetryPage from "./pages/Telemetry";
import TimelinePage from "./pages/Timeline";

import "./App.css";
import {
  getAuthToken,
  subscribeAuthChallenge,
  subscribeAuthChange,
} from "@/api/auth-store";
import { CommandPalette } from "@/components/CommandPalette";
import { CommandPaletteHint } from "@/components/CommandPaletteHint";
import { EventSubscriber } from "@/components/EventSubscriber";
import { KeyboardShortcutsHelp } from "@/components/KeyboardShortcutsHelp";
import { ThemeProvider } from "@/components/ThemeProvider";
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

  const onLoginPage = location.pathname === "/login";
  const hasToken = Boolean(getAuthToken());

  // Login route mounts standalone (no Layout, no global subscriptions
  // that would try to fetch authenticated endpoints). Once authenticated
  // the operator gets the full app.
  if (onLoginPage) {
    return (
      <Routes>
        <Route path="/login" element={<LoginPage />} />
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
      <Routes>
        {/* Main application routes */}
        <Route element={<Layout />}>
          <Route path="/" element={<DashboardPage />} />
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
          <Route path="/deploy" element={<DeployPage />} />
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
