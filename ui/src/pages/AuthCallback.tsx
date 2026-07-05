// AuthCallback is the landing route for the enterprise OIDC→frontend bearer
// handoff (ADR 0014). The enterprise callback redirects the browser to
//   <returnTo>#squadron_token=<url-escaped token>&expires_at=<RFC3339>
// so the minted Squadron bearer arrives in the URL FRAGMENT (never sent to the
// server, kept out of logs + Referer). On mount we parse window.location.hash,
// store the token exactly like a pasted operator token, scrub it from the URL +
// history, and land the operator on the app. No token present → back to /login.
//
// Mounted in the UNAUTHENTICATED <Routes> block and treated as a pre-auth route
// so no authenticated fetch (and its 401 bounce) fires before the token is
// stored.

import { useEffect } from "react";
import { useNavigate } from "react-router-dom";

import { setAuthToken } from "@/api/auth-store";

export default function AuthCallbackPage() {
  const nav = useNavigate();

  useEffect(() => {
    const params = new URLSearchParams(window.location.hash.slice(1));
    const token = params.get("squadron_token");
    if (token) {
      setAuthToken(token);
      // Scrub the token from the URL + browser history before anything else can
      // read the fragment.
      window.history.replaceState(null, "", "/auth/callback");
      nav("/", { replace: true });
    } else {
      nav("/login", { replace: true });
    }
  }, [nav]);

  return (
    <div className="min-h-screen flex items-center justify-center bg-background p-6">
      <p className="text-sm text-muted-foreground">Signing you in…</p>
    </div>
  );
}
