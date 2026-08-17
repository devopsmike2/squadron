// CLI onboarding page (/settings/cli).
//
// The web UI deliberately funnels operators to squadronctl for the
// repeatable, scriptable fleet operations. This page is the on-ramp:
// what the CLI is, how to install and authenticate it, and the example
// workflow (the pilot loop) as copyable commands. The copy-as-command
// hints scattered across the agent/group actions all lead here for the
// one-time auth setup.

import { Download, ExternalLink, KeyRound, Terminal } from "lucide-react";
import { Link } from "react-router-dom";

import { CommandSnippet } from "@/components/shared/CommandSnippet";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { BACKEND_HOSTNAME } from "@/config";

const RELEASES_URL = "https://github.com/devopsmike2/squadron/releases/latest";
const DOCS_URL =
  "https://github.com/devopsmike2/squadron/blob/main/docs/squadronctl.md";

// Published release assets, one per platform. Names match the CLI's
// GitHub release artifacts (squadronctl-<os>-<arch>).
const BINARIES: { platform: string; asset: string }[] = [
  { platform: "macOS (Apple silicon)", asset: "squadronctl-darwin-arm64" },
  { platform: "macOS (Intel)", asset: "squadronctl-darwin-amd64" },
  { platform: "Linux (amd64)", asset: "squadronctl-linux-amd64" },
  { platform: "Linux (arm64)", asset: "squadronctl-linux-arm64" },
  { platform: "Windows (amd64)", asset: "squadronctl-windows-amd64.exe" },
];

export default function CliPage() {
  // Prefill SQUADRON_URL with the base URL the UI is talking to, so the
  // operator can copy the export line as-is.
  const exportSetup = `export SQUADRON_URL=${BACKEND_HOSTNAME}\nexport SQUADRON_TOKEN=<your-token>`;
  const configFile = `# ~/.squadronctl/config.yaml\nserver: ${BACKEND_HOSTNAME}\ntoken: <your-token>`;

  return (
    <div className="space-y-4 p-6">
      <div>
        <h1 className="flex items-center gap-2 text-2xl font-semibold">
          <Terminal className="h-6 w-6 text-muted-foreground" />
          squadronctl CLI
        </h1>
        <p className="mt-1 max-w-3xl text-sm text-muted-foreground">
          squadronctl is the Squadron command-line client. It wraps the REST API
          so you can drive Squadron from CI pipelines, shell scripts, and ad-hoc
          terminals without opening the UI. The web console is for seeing and
          deciding; the CLI is for repeatable, scriptable fleet operations. Many
          actions in this UI show a copy-as-command hint that drops you straight
          into the matching squadronctl command.{" "}
          <a
            href={DOCS_URL}
            target="_blank"
            rel="noreferrer"
            className="inline-flex items-center gap-1 text-primary underline"
          >
            Full reference
            <ExternalLink className="h-3 w-3" aria-hidden />
          </a>
        </p>
      </div>

      {/* Download / install. */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Download className="h-4 w-4 text-muted-foreground" />
            Install
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-sm text-muted-foreground">
            Pre-built binaries are attached to every GitHub release. Download
            the asset for your platform from the{" "}
            <a
              href={RELEASES_URL}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-primary underline"
            >
              latest release
              <ExternalLink className="h-3 w-3" aria-hidden />
            </a>
            , make it executable, and put it on your PATH.
          </p>
          <div className="overflow-hidden rounded-md border">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-xs uppercase tracking-wider text-muted-foreground">
                  <th className="px-3 py-2">Platform</th>
                  <th className="px-3 py-2">Release asset</th>
                </tr>
              </thead>
              <tbody>
                {BINARIES.map((b) => (
                  <tr key={b.asset} className="border-b last:border-0">
                    <td className="px-3 py-2 text-muted-foreground">
                      {b.platform}
                    </td>
                    <td className="px-3 py-2">
                      <code className="font-mono text-xs">{b.asset}</code>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <CommandSnippet
            command={`curl -L -o squadronctl ${RELEASES_URL}/download/squadronctl-darwin-arm64\nchmod +x squadronctl\nsudo mv squadronctl /usr/local/bin/`}
          />
          <p className="text-xs text-muted-foreground">
            No published binary for your platform? Build from source with{" "}
            <code className="font-mono">make build-cli</code> (the binary lands
            at <code className="font-mono">bin/squadronctl</code>).
          </p>
        </CardContent>
      </Card>

      {/* Auth setup. */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <KeyRound className="h-4 w-4 text-muted-foreground" />
            Authenticate
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-sm text-muted-foreground">
            squadronctl authenticates with a scoped bearer token. Mint one on
            the{" "}
            <Link to="/settings/tokens" className="text-primary underline">
              API tokens
            </Link>{" "}
            page (scope it to the job the CLI does, for example{" "}
            <code className="font-mono">agents:write</code>), then point the CLI
            at this Squadron instance. Set the environment variables:
          </p>
          <CommandSnippet command={exportSetup} />
          <p className="text-sm text-muted-foreground">
            Or write them once to a config file:
          </p>
          <CommandSnippet command={configFile} />
          <p className="text-sm text-muted-foreground">Verify connectivity:</p>
          <CommandSnippet command="squadronctl auth whoami" />
        </CardContent>
      </Card>

      {/* Example workflow — the pilot loop. */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Terminal className="h-4 w-4 text-muted-foreground" />
            Example workflow
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-sm text-muted-foreground">
            The pilot loop: find agents, move one into a group, assign the group
            a config, hand an over-ridden agent back to its group, inspect what
            an agent actually resolved, and restart it. These assume
            SQUADRON_URL and SQUADRON_TOKEN are set from the step above.
          </p>
          <div className="space-y-2">
            <ExampleRow label="List agents" command="squadronctl agents list" />
            <ExampleRow
              label="Assign an agent to a group"
              command="squadronctl agents set-group <agent-id> --group <group-name-or-id>"
            />
            <ExampleRow
              label="Assign a config to a group"
              command="squadronctl groups assign-config <group-id> --config <config-id>"
            />
            <ExampleRow
              label="Clear an agent's own config (fall back to group)"
              command="squadronctl agents clear-config <agent-id>"
            />
            <ExampleRow
              label="Show an agent's effective config"
              command="squadronctl agents get <agent-id> --effective"
            />
            <ExampleRow
              label="Restart an agent"
              command="squadronctl agents restart <agent-id>"
            />
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

function ExampleRow({ label, command }: { label: string; command: string }) {
  return (
    <div className="space-y-1">
      <p className="text-xs font-medium text-muted-foreground">{label}</p>
      <CommandSnippet command={command} />
    </div>
  );
}
