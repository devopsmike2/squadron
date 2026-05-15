import { BrowserRouter as Router, Routes, Route } from "react-router-dom";

import Layout from "./layout";
import AgentsPage from "./pages/Agents";
import AlertsPage from "./pages/Alerts";
import ConfigsPage from "./pages/Configs";
import GroupsPage from "./pages/Groups";
import TelemetryPage from "./pages/Telemetry";
import TopologyPage from "./pages/Topology";

import "./App.css";
import { CommandPalette } from "@/components/CommandPalette";
import { EventSubscriber } from "@/components/EventSubscriber";
import { ThemeProvider } from "@/components/ThemeProvider";
import { SWRProvider } from "@/lib/swr-provider";
import { ApiProvider } from "@/providers/ApiProvider";

function App() {
  return (
    <ThemeProvider defaultTheme="system">
      <SWRProvider>
        <ApiProvider>
          <Router>
            {/* Global ⌘K command palette. Mounted inside the Router so its
                items can use useNavigate. */}
            <CommandPalette />
            {/* SSE-driven cache invalidator. Listens to Squadron's event
                stream and revalidates the relevant SWR caches so pages stay
                live without each one wiring its own subscription. */}
            <EventSubscriber />
            <Routes>
              {/* Main application routes */}
              <Route element={<Layout />}>
                <Route path="/" element={<AgentsPage />} />
                <Route path="/agents" element={<AgentsPage />} />
                <Route path="/groups" element={<GroupsPage />} />
                <Route path="/configs" element={<ConfigsPage />} />
                <Route
                  path="/configs/new"
                  element={<ConfigsPage mode="create" />}
                />
                <Route
                  path="/configs/:configId/edit"
                  element={<ConfigsPage mode="edit" />}
                />
                <Route path="/telemetry" element={<TelemetryPage />} />
                <Route path="/topology" element={<TopologyPage />} />
                <Route path="/alerts" element={<AlertsPage />} />
              </Route>
            </Routes>
          </Router>
        </ApiProvider>
      </SWRProvider>
    </ThemeProvider>
  );
}

export default App;
