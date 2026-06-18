// Login is the auth-token entry screen. Squadron doesn't have user
// accounts or passwords — operators authenticate by pasting an API
// token issued from the settings page (or printed to stderr by the
// bootstrap-token-on-first-start flow). Successful validation stores
// the token in localStorage and redirects back to wherever the
// operator was headed.
//
// Mounted when:
//   - auth-store has no token (fresh load).
//   - A request returned 401 (token revoked or auth was just turned on).
//
// Mounted OUTSIDE the main Layout so the sidebar/nav don't render
// without authentication — the login screen is the only thing visible
// when unauthenticated.

import { KeyRound } from "lucide-react";
import { useState } from "react";
import { useNavigate } from "react-router-dom";

import { listAPITokens } from "@/api/auth";
import { setAuthToken } from "@/api/auth-store";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

export default function LoginPage() {
  const nav = useNavigate();
  const [token, setToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setError(null);

    // Optimistically store the token, then exercise an authenticated
    // endpoint to validate it. If the call 401s, simpleRequest will
    // already have cleared the token via onAuthChallenge — we surface
    // a friendly error and let the operator try again.
    setAuthToken(token.trim());
    try {
      await listAPITokens();
      // Good — the token was accepted. Redirect to the home page.
      nav("/", { replace: true });
    } catch (err) {
      const msg = err instanceof Error ? err.message : "login failed";
      // simpleRequest already cleared the token on a 401 path; we
      // explicitly clear here too for any other error so a stale token
      // doesn't sit in localStorage.
      setAuthToken("");
      setError(
        msg.toLowerCase().includes("unauthorized") ||
          msg.toLowerCase().includes("invalid")
          ? "That token wasn't accepted. Check for typos or paste a fresh one."
          : `Couldn't reach Squadron: ${msg}`,
      );
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="min-h-screen flex items-center justify-center bg-background p-6">
      <Card className="w-full max-w-md">
        <CardHeader>
          <div className="flex items-center gap-2">
            <KeyRound className="h-5 w-5 text-muted-foreground" />
            <CardTitle>Sign in to Squadron</CardTitle>
          </div>
          <p className="text-sm text-muted-foreground pt-1">
            Squadron uses API tokens for authentication. Paste an existing
            token, or look in your server's logs for the bootstrap token issued
            on first start.
          </p>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="token">API token</Label>
              <Input
                id="token"
                type="password"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder="sqd_..."
                className="font-mono text-sm"
                autoFocus
                required
              />
              <p className="text-xs text-muted-foreground">
                Tokens are stored in this browser's localStorage. Clear it via
                your browser's developer tools to sign out.
              </p>
            </div>
            {error && <div className="text-sm text-red-600">{error}</div>}
            <Button
              type="submit"
              disabled={submitting || !token.trim()}
              className="w-full"
            >
              {submitting ? "Verifying..." : "Sign in"}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
