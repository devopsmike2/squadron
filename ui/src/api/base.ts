import { apiBaseUrl } from "../config";
import { getAuthToken, onAuthChallenge } from "./auth-store";

// Common types for API responses
export interface ApiResponse<T = unknown> {
  success?: boolean;
  error?: string;
  data?: T;
}

// Base API configuration
export const apiConfig = {
  baseUrl: apiBaseUrl,
  defaultHeaders: {
    "Content-Type": "application/json",
  },
};

// simpleRequest is the canonical fetch wrapper for every Squadron API
// call. Two responsibilities beyond plain fetch:
//   1. Attach the Bearer token (from auth-store / localStorage) when
//      one exists, so authenticated Squadron instances see attributed
//      requests.
//   2. On a 401, clear the stored token and emit an auth-challenge so
//      the app can route to the login screen. This handles both "auth
//      was just turned on" and "operator's token was revoked".
export const simpleRequest = async <T = unknown>(
  endpoint: string,
  options: RequestInit = {},
): Promise<T> => {
  const url = `${apiConfig.baseUrl}${endpoint}`;

  const headers: Record<string, string> = {
    ...apiConfig.defaultHeaders,
    ...(options.headers as Record<string, string> | undefined),
  };
  const token = getAuthToken();
  if (token && !("Authorization" in headers)) {
    headers["Authorization"] = `Bearer ${token}`;
  }

  const response = await fetch(url, {
    ...options,
    headers,
  });

  if (response.status === 401) {
    // The server thinks we're unauthenticated. Either auth was just
    // turned on, the token was revoked, or it never existed. Clear
    // the local copy and let the app re-prompt.
    onAuthChallenge();
  }

  if (!response.ok) {
    // Try to get detailed error message from response body
    let errorMessage = `API request failed: ${response.status} ${response.statusText}`;
    try {
      const errorData = await response.json();
      if (errorData.error) {
        errorMessage = errorData.error;
        if (errorData.details) {
          errorMessage += `: ${errorData.details}`;
        }
      }
    } catch {
      // If we can't parse the error response, use the default message
    }
    const error = new Error(errorMessage) as Error & { status: number };
    error.status = response.status;
    throw error;
  }

  // 204 No Content responses have empty bodies. fetch().json() throws
  // in that case, which would surface as a confusing error in handlers
  // that don't read the response. Short-circuit here.
  if (response.status === 204) {
    return undefined as T;
  }

  return response.json();
};

// HTTP method helpers
export const apiGet = <T = unknown>(
  endpoint: string,
  params?: Record<string, string>,
): Promise<T> => {
  const url = params ? `${endpoint}?${new URLSearchParams(params)}` : endpoint;
  return simpleRequest<T>(url, { method: "GET" });
};

export const apiPost = <T = unknown>(
  endpoint: string,
  data?: unknown,
): Promise<T> => {
  return simpleRequest<T>(endpoint, {
    method: "POST",
    body: data ? JSON.stringify(data) : undefined,
  });
};

export const apiPut = <T = unknown>(
  endpoint: string,
  data?: unknown,
): Promise<T> => {
  return simpleRequest<T>(endpoint, {
    method: "PUT",
    body: data ? JSON.stringify(data) : undefined,
  });
};

export const apiDelete = <T = unknown>(endpoint: string): Promise<T> => {
  return simpleRequest<T>(endpoint, { method: "DELETE" });
};

export const apiPatch = <T = unknown>(
  endpoint: string,
  data?: unknown,
): Promise<T> => {
  return simpleRequest<T>(endpoint, {
    method: "PATCH",
    body: data ? JSON.stringify(data) : undefined,
  });
};
