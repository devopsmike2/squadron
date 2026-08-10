import { FileDown } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";

import { adoptAgentConfig, type AdoptConfigResponse } from "@/api/agents";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";

interface AdoptConfigButtonProps {
  agentId: string;
  /** The agent's reported effective config. When empty/absent the button
   * is disabled — there is nothing to adopt until the agent reports
   * report-only over OpAMP. */
  effectiveConfig?: string;
}

/**
 * "Convert to template": adopt this agent's reported effective config as
 * a standalone managed Config template. The created config is unassigned
 * and is not pushed to any agent — the operator assigns it afterward via
 * the normal flow. This is the bridge from a report-only brownfield agent
 * to a Squadron-managed config without re-authoring by hand.
 */
export function AdoptConfigButton({
  agentId,
  effectiveConfig,
}: AdoptConfigButtonProps) {
  const [isAdopting, setIsAdopting] = useState(false);
  const [result, setResult] = useState<AdoptConfigResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  const hasEffectiveConfig = Boolean(effectiveConfig && effectiveConfig.trim());

  const handleAdopt = async () => {
    setIsAdopting(true);
    setError(null);
    setResult(null);
    try {
      const resp = await adoptAgentConfig(agentId);
      setResult(resp);
    } catch (e) {
      setError(
        e instanceof Error ? e.message : "Failed to convert config to template",
      );
    } finally {
      setIsAdopting(false);
    }
  };

  return (
    <div className="space-y-2">
      <Button
        size="sm"
        variant="outline"
        onClick={handleAdopt}
        disabled={!hasEffectiveConfig || isAdopting}
        title={
          hasEffectiveConfig
            ? "Create a managed config template seeded from this agent's reported effective config. The template is created unassigned and is not pushed to any agent."
            : "This agent has not reported an effective config yet, so there is nothing to convert. It must register report-only over OpAMP first."
        }
      >
        <FileDown className="h-4 w-4 mr-2" />
        {isAdopting ? "Converting..." : "Convert to template"}
      </Button>

      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      {result && (
        <Alert>
          <AlertTitle>Template created</AlertTitle>
          <AlertDescription>
            <div className="space-y-1">
              <p>
                Created managed config{" "}
                <span className="font-medium">{result.config.name}</span>. It is
                unassigned. Assign it to this agent or a group from the configs
                page.
              </p>
              {result.redacted && result.note ? (
                <p className="text-amber-700 dark:text-amber-400">
                  {result.note}
                </p>
              ) : null}
              <Link
                to={`/configs/${result.config.id}/edit`}
                className="inline-block text-primary underline"
              >
                View config
              </Link>
            </div>
          </AlertDescription>
        </Alert>
      )}
    </div>
  );
}
