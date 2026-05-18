// API client for v0.27.1 Quickstart wizard.
// Mirrors internal/quickstart/quickstart.go shapes.

import { apiGet } from "./base";

export type QuickstartBackend =
  | "datadog"
  | "honeycomb"
  | "newrelic"
  | "signoz"
  | "grafana"
  | "otlp";

export interface QuickstartEnvVar {
  name: string;
  purpose: string;
  required: boolean;
}

export interface QuickstartBackendInfo {
  id: QuickstartBackend;
  name: string;
  description: string;
  env_vars?: QuickstartEnvVar[];
  docs_url?: string;
}

export interface QuickstartCatalog {
  items: QuickstartBackendInfo[];
}

export interface QuickstartStarterConfig {
  backend: QuickstartBackend;
  opamp_server_url: string;
  yaml: string;
}

export interface QuickstartOpAMPSnippet {
  opamp_server_url: string;
  yaml: string;
}

export function getQuickstartCatalog(): Promise<QuickstartCatalog> {
  return apiGet<QuickstartCatalog>("/quickstart/backends");
}

export function getStarterConfig(
  backend: QuickstartBackend,
  host?: string,
): Promise<QuickstartStarterConfig> {
  const q = new URLSearchParams({ backend });
  if (host) q.set("host", host);
  return apiGet<QuickstartStarterConfig>(
    `/quickstart/starter-config?${q.toString()}`,
  );
}

export function getOpAMPSnippet(
  host?: string,
): Promise<QuickstartOpAMPSnippet> {
  const q = new URLSearchParams();
  if (host) q.set("host", host);
  const qs = q.toString();
  return apiGet<QuickstartOpAMPSnippet>(
    `/quickstart/opamp-snippet${qs ? "?" + qs : ""}`,
  );
}
