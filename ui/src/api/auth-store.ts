// auth-store is the single source of truth for the operator's API
// token in the browser. It's deliberately a tiny module — there's no
// state library or context provider in front of it because every
// caller is fine reading synchronously from localStorage, and the
// challenge subscription is just for the App-level redirect to /login.
//
// localStorage caveat: a successful XSS attack on the Squadron UI
// could read this token. That's a real but bounded risk — the UI
// is admin-only, so anyone with XSS already has equivalent access via
// the in-page session anyway. If you front Squadron with stricter
// auth (OIDC + a reverse proxy) you don't need this layer at all.

const STORAGE_KEY = "squadron.auth.token";

/** getAuthToken returns the persisted token or null if none is set. */
export function getAuthToken(): string | null {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    // Private mode / disabled storage. Treat as no token; the user
    // will get re-prompted on every page load, which is annoying but
    // not a security problem.
    return null;
  }
}

/** setAuthToken persists a new token and notifies subscribers. */
export function setAuthToken(token: string): void {
  try {
    localStorage.setItem(STORAGE_KEY, token);
  } catch {
    // ignore
  }
  notifyTokenChanged();
}

/** clearAuthToken removes the persisted token and notifies subscribers. */
export function clearAuthToken(): void {
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // ignore
  }
  notifyTokenChanged();
}

// Subscribers for "you got a 401 — go to login". The App component
// subscribes; everyone else fires it via onAuthChallenge.
type Listener = () => void;
const challengeListeners = new Set<Listener>();
const changeListeners = new Set<Listener>();

export function onAuthChallenge(): void {
  // Always clear the stored token on challenge — it's either bad or
  // revoked, and leaving it in place would cause the next refresh to
  // 401 again.
  clearAuthToken();
  for (const l of challengeListeners) l();
}

export function subscribeAuthChallenge(l: Listener): () => void {
  challengeListeners.add(l);
  return () => challengeListeners.delete(l);
}

export function subscribeAuthChange(l: Listener): () => void {
  changeListeners.add(l);
  return () => changeListeners.delete(l);
}

function notifyTokenChanged(): void {
  for (const l of changeListeners) l();
}
