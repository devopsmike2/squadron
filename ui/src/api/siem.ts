// SIEM destinations API client (v0.50.3).
//
// Mirror of services.SiemDestinationView. Plaintext secrets only
// travel inbound (Create / Update); the View shape exposes
// has_secret instead.

import { apiDelete, apiGet, apiPost, apiPut } from "./base";

export type SiemDestinationType = "splunk_hec" | "webhook";

export interface SiemDestination {
  id: string;
  name: string;
  type: SiemDestinationType;
  url: string;
  has_secret: boolean;
  enabled: boolean;
  event_type_prefix: string[];
  last_event_sent_at?: string;
  last_error?: string;
  last_error_at?: string;
  created_at: string;
  updated_at: string;
}

export interface SiemDestinationInput {
  name: string;
  type: SiemDestinationType;
  url: string;
  plaintext_secret: string;
  enabled: boolean;
  event_type_prefix?: string[];
}

// SiemDestinationUpdate. Omit plaintext_secret to keep the existing
// secret — the only reason to send it is to rotate. Empty string
// is rejected server-side.
export interface SiemDestinationUpdate {
  name?: string;
  type?: SiemDestinationType;
  url?: string;
  plaintext_secret?: string;
  enabled?: boolean;
  event_type_prefix?: string[];
}

interface ListResponse {
  destinations: SiemDestination[];
}

export const listSiemDestinations = async (): Promise<SiemDestination[]> => {
  const resp = await apiGet<ListResponse>("/siem/destinations");
  return resp.destinations ?? [];
};

export const getSiemDestination = (id: string): Promise<SiemDestination> =>
  apiGet<SiemDestination>(`/siem/destinations/${id}`);

export const createSiemDestination = (
  input: SiemDestinationInput,
): Promise<SiemDestination> => apiPost<SiemDestination>("/siem/destinations", input);

export const updateSiemDestination = (
  id: string,
  input: SiemDestinationUpdate,
): Promise<SiemDestination> => apiPut<SiemDestination>(`/siem/destinations/${id}`, input);

export const deleteSiemDestination = (id: string): Promise<void> =>
  apiDelete<void>(`/siem/destinations/${id}`);

// testSiemDestination sends a synthetic event to the configured
// endpoint. Returns the server's "ok" payload on success; throws on
// any non-2xx (the API surfaces Splunk's error body on 502 so the
// UI can show "401 invalid token" rather than just "failed").
export const testSiemDestination = (id: string): Promise<{ result: string }> =>
  apiPost<{ result: string }>(`/siem/destinations/${id}/test`, {});
