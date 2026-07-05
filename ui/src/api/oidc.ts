// OIDC SSO probe (ADR 0014 OIDC→frontend handoff). The enterprise pack mounts
// GET /auth/oidc/connections on the ROOT origin (pre-bearer), NOT under the
// /api/v1 base — so we derive the origin from BACKEND_HOSTNAME directly rather
// than reusing apiBaseUrl. In OSS the /auth/oidc/* wildcard is nil→404, so the
// probe naturally returns 404 and we treat it as "no SSO configured" ([]).

import { BACKEND_HOSTNAME } from "../config";

export interface OIDCConnection {
  id: string;
  display_name: string;
}

interface ConnectionsResponse {
  connections?: OIDCConnection[];
}

/**
 * listOIDCConnections probes the pre-bearer connections endpoint on the root
 * origin. Any failure (404 in OSS, network error, malformed body) resolves to
 * an empty list — the login screen simply renders no SSO buttons.
 */
export async function listOIDCConnections(): Promise<OIDCConnection[]> {
  try {
    const res = await fetch(`${BACKEND_HOSTNAME}/auth/oidc/connections`, {
      headers: { Accept: "application/json" },
    });
    if (!res.ok) {
      return [];
    }
    const body = (await res.json()) as ConnectionsResponse;
    return body.connections ?? [];
  } catch {
    return [];
  }
}
