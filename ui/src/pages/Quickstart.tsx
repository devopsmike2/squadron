/**
 * Quickstart — v0.27.1 onboarding wizard.
 *
 * The single landing screen the SMB operator hits when they first
 * open Squadron (or via the sidebar later to onboard more agents).
 * Two clear branches:
 *
 *   1. "Start fresh" — pick a backend (Datadog/Honeycomb/etc.),
 *      get a starter collector config + per-platform install
 *      command, wait for the first agent to connect, drop to
 *      Fleet Status.
 *
 *   2. "I have collectors already running" — get the OpAMP
 *      extension snippet to paste into existing configs, plus a
 *      per-platform restart command. Bulk mode generates one
 *      ssh-ready command per hostname.
 *
 * Both branches end on the same "watching for agents" step that
 * polls /api/v1/agents/stats and flips when totalAgents goes up.
 *
 * The page is intentionally chunky in one file — wizard flows
 * benefit from local cohesion, and splitting this across many
 * components makes the linear reading harder than it needs to be.
 */

import {
  ArrowLeftIcon,
  CheckIcon,
  CopyIcon,
  ExternalLinkIcon,
  Loader2Icon,
  PartyPopperIcon,
  PlusCircleIcon,
  RocketIcon,
  ServerIcon,
  SparklesIcon,
} from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import useSWR from "swr";

import { getAgentStats } from "@/api/agents";
import {
  getOpAMPSnippet,
  getQuickstartCatalog,
  getStarterConfig,
  type QuickstartBackend,
  type QuickstartBackendInfo,
} from "@/api/quickstart";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Stepper } from "@/components/ui/stepper";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";

type Mode = "landing" | "fresh" | "adopt";

export default function QuickstartPage() {
  const [mode, setMode] = useState<Mode>("landing");

  if (mode === "landing") return <Landing onPick={setMode} />;
  if (mode === "fresh")
    return <FreshInstallFlow onBack={() => setMode("landing")} />;
  return <AdoptExistingFlow onBack={() => setMode("landing")} />;
}

// ----------------------------------------------------------------
// Landing — two big cards
// ----------------------------------------------------------------

function Landing({ onPick }: { onPick: (m: Mode) => void }) {
  return (
    <div className="flex flex-col gap-6 p-6">
      <header>
        <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
          Squadron
        </div>
        <h1 className="mt-1 text-2xl font-semibold tracking-tight">
          Get your first agent into Squadron
        </h1>
        <p className="mt-1 max-w-2xl text-sm text-muted-foreground">
          Squadron manages OpenTelemetry Collectors via the OpAMP protocol. To
          start managing agents, each collector needs the OpAMP extension
          pointed at this Squadron. Pick the path that matches where you are.
        </p>
      </header>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <button
          type="button"
          onClick={() => onPick("fresh")}
          className="text-left transition-transform hover:scale-[1.01]"
        >
          <Card className="h-full">
            <CardContent className="p-6">
              <div className="flex items-center gap-3">
                <div className="rounded-md border border-border bg-background/40 p-2">
                  <RocketIcon
                    className="h-5 w-5"
                    style={{ color: "var(--info)" }}
                  />
                </div>
                <div>
                  <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
                    Path A
                  </div>
                  <div className="text-base font-medium">Start fresh</div>
                </div>
              </div>
              <p className="mt-3 text-sm text-muted-foreground">
                You don't have an OpenTelemetry Collector running yet. Pick your
                backend (Datadog, Honeycomb, etc.) and Squadron generates a
                starter config + a one-line install command. Live in minutes.
              </p>
              <div className="mt-4 inline-flex items-center gap-1 text-xs font-medium">
                Get started <ArrowLeftIcon className="h-3 w-3 rotate-180" />
              </div>
            </CardContent>
          </Card>
        </button>

        <button
          type="button"
          onClick={() => onPick("adopt")}
          className="text-left transition-transform hover:scale-[1.01]"
        >
          <Card className="h-full">
            <CardContent className="p-6">
              <div className="flex items-center gap-3">
                <div className="rounded-md border border-border bg-background/40 p-2">
                  <ServerIcon
                    className="h-5 w-5"
                    style={{ color: "var(--success)" }}
                  />
                </div>
                <div>
                  <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
                    Path B
                  </div>
                  <div className="text-base font-medium">
                    I have collectors running
                  </div>
                </div>
              </div>
              <p className="mt-3 text-sm text-muted-foreground">
                You already have OTel Collectors deployed (bare binary, Docker,
                Helm, systemd). Squadron generates the OpAMP extension snippet
                to paste into your existing configs. No re-deploy needed.
              </p>
              <div className="mt-4 inline-flex items-center gap-1 text-xs font-medium">
                Adopt my fleet <ArrowLeftIcon className="h-3 w-3 rotate-180" />
              </div>
            </CardContent>
          </Card>
        </button>
      </div>

      <Card>
        <CardContent className="p-4 text-sm text-muted-foreground">
          <strong className="text-foreground">Not sure?</strong> Path A is
          quicker if you have a backend account but no collector setup yet. Path
          B preserves your existing pipelines and just adds Squadron's
          management — pick this if your collectors are already shipping
          telemetry you care about.
        </CardContent>
      </Card>
    </div>
  );
}

// ----------------------------------------------------------------
// Fresh install flow
// ----------------------------------------------------------------

function FreshInstallFlow({ onBack }: { onBack: () => void }) {
  const [step, setStep] = useState(0);
  const [backend, setBackend] = useState<QuickstartBackend | null>(null);

  const { data: catalog } = useSWR("quickstart-backends", getQuickstartCatalog);

  return (
    <div className="flex flex-col gap-6 p-6">
      <WizardHeader title="Start fresh — your first agent" onBack={onBack} />
      <Stepper
        steps={[
          { id: "backend", title: "Pick your backend" },
          { id: "install", title: "Install the agent" },
          { id: "connect", title: "Confirm it connected" },
        ]}
        currentStep={step}
      />

      {step === 0 && (
        <BackendPicker
          backends={catalog?.items ?? []}
          selected={backend}
          onSelect={(b) => {
            setBackend(b);
            setStep(1);
          }}
        />
      )}

      {step === 1 && backend && (
        <InstallStep
          backend={backend}
          info={catalog?.items.find((b) => b.id === backend)}
          onBack={() => setStep(0)}
          onNext={() => setStep(2)}
        />
      )}

      {step === 2 && <WatchForAgentsStep />}
    </div>
  );
}

function BackendPicker({
  backends,
  selected: _selected,
  onSelect,
}: {
  backends: QuickstartBackendInfo[];
  selected: QuickstartBackend | null;
  onSelect: (b: QuickstartBackend) => void;
}) {
  if (backends.length === 0) {
    return (
      <Card>
        <CardContent className="flex items-center gap-2 p-6 text-sm text-muted-foreground">
          <Loader2Icon className="h-4 w-4 animate-spin" />
          Loading backends…
        </CardContent>
      </Card>
    );
  }
  return (
    <div className="grid grid-cols-1 gap-3 md:grid-cols-2 lg:grid-cols-3">
      {backends.map((b) => (
        <button
          key={b.id}
          type="button"
          onClick={() => onSelect(b.id)}
          className="text-left transition-colors"
        >
          <Card className="h-full hover:border-foreground/40">
            <CardContent className="p-4">
              <div className="text-sm font-medium">{b.name}</div>
              <p className="mt-1 text-xs text-muted-foreground">
                {b.description}
              </p>
              {b.env_vars && b.env_vars.length > 0 && (
                <div className="mt-3 flex flex-wrap gap-1">
                  {b.env_vars
                    .filter((e) => e.required)
                    .map((e) => (
                      <span
                        key={e.name}
                        className="font-mono rounded-md border border-border px-1.5 py-0.5 text-[10px] text-muted-foreground"
                      >
                        {e.name}
                      </span>
                    ))}
                </div>
              )}
            </CardContent>
          </Card>
        </button>
      ))}
    </div>
  );
}

function InstallStep({
  backend,
  info,
  onBack,
  onNext,
}: {
  backend: QuickstartBackend;
  info?: QuickstartBackendInfo;
  onBack: () => void;
  onNext: () => void;
}) {
  const { data: starter } = useSWR(`quickstart-starter-${backend}`, () =>
    getStarterConfig(backend),
  );

  if (!starter) {
    return (
      <Card>
        <CardContent className="flex items-center gap-2 p-6 text-sm text-muted-foreground">
          <Loader2Icon className="h-4 w-4 animate-spin" />
          Generating starter config…
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-4">
      {info?.env_vars && info.env_vars.length > 0 && (
        <Card>
          <CardContent className="p-4">
            <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
              Required environment variables
            </div>
            <div className="mt-2 space-y-1.5 text-sm">
              {info.env_vars.map((e) => (
                <div key={e.name} className="flex items-baseline gap-3">
                  <span className="font-mono text-xs">{e.name}</span>
                  <span className="text-muted-foreground">{e.purpose}</span>
                  {!e.required && (
                    <Badge variant="outline" className="text-[10px]">
                      optional
                    </Badge>
                  )}
                </div>
              ))}
              {info.docs_url && (
                <a
                  href={info.docs_url}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
                >
                  <ExternalLinkIcon className="h-3 w-3" />
                  {info.name} OpenTelemetry docs
                </a>
              )}
            </div>
          </CardContent>
        </Card>
      )}

      <YamlBlock title="Starter collector config" yaml={starter.yaml} />

      <Card>
        <CardContent className="p-4">
          <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
            Run the agent
          </div>
          <p className="mt-1 text-sm text-muted-foreground">
            Save the config above to{" "}
            <code className="font-mono text-xs">otelcol-config.yaml</code> on
            the host you want to run the agent on, then run one of the commands
            below.
          </p>
          <InstallCommandTabs starterURL={starter.opamp_server_url} />
        </CardContent>
      </Card>

      <div className="flex justify-between">
        <Button variant="outline" size="sm" onClick={onBack}>
          <ArrowLeftIcon className="mr-1 h-3 w-3" /> Different backend
        </Button>
        <Button size="sm" onClick={onNext}>
          I've started the agent{" "}
          <ArrowLeftIcon className="ml-1 h-3 w-3 rotate-180" />
        </Button>
      </div>
    </div>
  );
}

function InstallCommandTabs({
  starterURL: _starterURL,
}: {
  starterURL: string;
}) {
  // Generic install commands. The starter URL is informational —
  // it's already inside the YAML the operator just saved.
  const dockerCmd = `docker run -d --name otelcol \\
  -v $(pwd)/otelcol-config.yaml:/etc/otelcol-contrib/config.yaml \\
  --env-file=./otelcol.env \\
  -p 4317:4317 -p 4318:4318 \\
  otel/opentelemetry-collector-contrib:latest`;

  const systemdCmd = `# 1. Install the collector binary (Linux x86_64):
curl -L -o /tmp/otelcol-contrib.tar.gz \\
  https://github.com/open-telemetry/opentelemetry-collector-releases/releases/latest/download/otelcol-contrib_linux_amd64.tar.gz
sudo tar -xzf /tmp/otelcol-contrib.tar.gz -C /usr/local/bin/

# 2. Save the config above to /etc/otelcol/config.yaml

# 3. Create a systemd unit and start:
sudo systemctl daemon-reload && sudo systemctl enable --now otelcol`;

  const helmCmd = `# values.yaml — paste the starter config YAML under \`config:\`,
# set your env vars under \`extraEnvs\`, then:
helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts
helm upgrade --install otelcol \\
  open-telemetry/opentelemetry-collector \\
  -f values.yaml`;

  return (
    <Tabs defaultValue="docker" className="mt-3">
      <TabsList>
        <TabsTrigger value="docker">Docker</TabsTrigger>
        <TabsTrigger value="systemd">Bare metal / systemd</TabsTrigger>
        <TabsTrigger value="helm">Kubernetes (Helm)</TabsTrigger>
      </TabsList>
      <TabsContent value="docker" className="mt-3">
        <CommandBlock cmd={dockerCmd} />
      </TabsContent>
      <TabsContent value="systemd" className="mt-3">
        <CommandBlock cmd={systemdCmd} />
      </TabsContent>
      <TabsContent value="helm" className="mt-3">
        <CommandBlock cmd={helmCmd} />
      </TabsContent>
    </Tabs>
  );
}

// ----------------------------------------------------------------
// Adopt existing fleet flow
// ----------------------------------------------------------------

function AdoptExistingFlow({ onBack }: { onBack: () => void }) {
  const { data: snippet } = useSWR("quickstart-opamp-snippet", () =>
    getOpAMPSnippet(),
  );
  const [hostList, setHostList] = useState("");

  if (!snippet) {
    return (
      <div className="p-6">
        <WizardHeader title="Adopt your existing collectors" onBack={onBack} />
        <Card className="mt-6">
          <CardContent className="flex items-center gap-2 p-6 text-sm text-muted-foreground">
            <Loader2Icon className="h-4 w-4 animate-spin" />
            Generating OpAMP snippet…
          </CardContent>
        </Card>
      </div>
    );
  }

  const hostnames = hostList
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);

  return (
    <div className="flex flex-col gap-6 p-6">
      <WizardHeader title="Adopt your existing collectors" onBack={onBack} />

      <Card>
        <CardContent className="p-4 text-sm">
          <p>
            Squadron talks to collectors via the OpenTelemetry{" "}
            <strong>OpAMP</strong> extension. Adopting existing collectors is
            two steps:{" "}
            <strong>
              merge the snippet below into each collector's config
            </strong>
            , then <strong>restart the collector</strong>. Once restarted, it
            shows up here within a few seconds.
          </p>
        </CardContent>
      </Card>

      <YamlBlock
        title="OpAMP extension — paste into your existing config"
        yaml={snippet.yaml}
      />

      <Card>
        <CardContent className="p-4">
          <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
            Then restart
          </div>
          <p className="mt-1 text-sm text-muted-foreground">
            Pick the deployment shape that matches your setup.
          </p>
          <Tabs defaultValue="systemd" className="mt-3">
            <TabsList>
              <TabsTrigger value="systemd">systemd</TabsTrigger>
              <TabsTrigger value="docker">Docker</TabsTrigger>
              <TabsTrigger value="helm">Helm</TabsTrigger>
              <TabsTrigger value="bare">Bare process</TabsTrigger>
            </TabsList>
            <TabsContent value="systemd" className="mt-3">
              <CommandBlock cmd="sudo systemctl restart otelcol" />
            </TabsContent>
            <TabsContent value="docker" className="mt-3">
              <CommandBlock cmd="docker restart $(docker ps -q --filter ancestor=otel/opentelemetry-collector-contrib)" />
            </TabsContent>
            <TabsContent value="helm" className="mt-3">
              <CommandBlock cmd="helm upgrade otelcol open-telemetry/opentelemetry-collector --reuse-values" />
            </TabsContent>
            <TabsContent value="bare" className="mt-3">
              <CommandBlock
                cmd="# Stop the running collector process (Ctrl-C or kill PID),&#10;# then re-run with the updated config file:&#10;./otelcol-contrib --config /etc/otelcol/config.yaml"
              />
            </TabsContent>
          </Tabs>
        </CardContent>
      </Card>

      <Card>
        <CardContent className="p-4">
          <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
            Bulk mode — generate per-host ssh commands
          </div>
          <p className="mt-1 text-sm text-muted-foreground">
            Paste a list of hostnames (one per line or comma-separated). We'll
            generate one-liner commands you can run from any host with ssh
            reach.
          </p>
          <Textarea
            value={hostList}
            onChange={(e) => setHostList(e.target.value)}
            placeholder={
              "web-01.prod.example.com\nweb-02.prod.example.com\napi-01.prod.example.com"
            }
            rows={4}
            className="mt-2 font-mono text-xs"
          />
          {hostnames.length > 0 && (
            <div className="mt-3 space-y-1.5">
              <div className="text-xs text-muted-foreground">
                {hostnames.length} host{hostnames.length === 1 ? "" : "s"} —
                one-liners that append the snippet and restart:
              </div>
              {hostnames.map((host) => (
                <CommandBlock
                  key={host}
                  cmd={`ssh ${host} 'sudo tee -a /etc/otelcol/config.yaml < squadron-opamp.yaml && sudo systemctl restart otelcol'`}
                />
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <WatchForAgentsStep />
    </div>
  );
}

// ----------------------------------------------------------------
// Shared: waiting-for-agents step + small UI helpers
// ----------------------------------------------------------------

function WatchForAgentsStep() {
  const { data: stats } = useSWR("agents-stats-wait", getAgentStats, {
    refreshInterval: 3000,
  });
  // Capture baseline once. The "first agent connected" moment is
  // when totalAgents goes ABOVE the baseline — that handles the
  // case where the operator already has some agents and is
  // onboarding more.
  const [baseline] = useState(() => stats?.totalAgents ?? null);
  const baselineN = baseline ?? stats?.totalAgents ?? 0;
  const newAgents = (stats?.totalAgents ?? 0) - baselineN;

  if (newAgents <= 0) {
    return (
      <Card>
        <CardContent className="p-6">
          <div className="flex items-center gap-2">
            <Loader2Icon className="h-4 w-4 animate-spin text-muted-foreground" />
            <div className="text-sm font-medium">Watching for new agents…</div>
          </div>
          <p className="mt-2 text-sm text-muted-foreground">
            Squadron polls every 3 seconds. Currently{" "}
            <span className="font-tabular font-medium text-foreground">
              {stats?.totalAgents ?? 0}
            </span>{" "}
            total agent{stats?.totalAgents === 1 ? "" : "s"} registered; we'll
            light up when a new one connects via OpAMP.
          </p>
        </CardContent>
      </Card>
    );
  }
  return (
    <Card>
      <CardContent className="p-6">
        <div className="flex items-center gap-2">
          <PartyPopperIcon
            className="h-5 w-5"
            style={{ color: "var(--success)" }}
          />
          <div className="text-base font-semibold">
            {newAgents} new agent{newAgents === 1 ? "" : "s"} connected!
          </div>
        </div>
        <p className="mt-2 text-sm text-muted-foreground">
          Squadron is now managing them via OpAMP. Open Fleet Status or Agents
          to see live state, push configs, and start saving on your telemetry
          bill.
        </p>
        <div className="mt-4 flex gap-2">
          <Link to="/">
            <Button size="sm">
              <SparklesIcon className="mr-1 h-3 w-3" /> Open Fleet Status
            </Button>
          </Link>
          <Link to="/agents">
            <Button variant="outline" size="sm">
              View agent
            </Button>
          </Link>
        </div>
      </CardContent>
    </Card>
  );
}

function WizardHeader({
  title,
  onBack,
}: {
  title: string;
  onBack: () => void;
}) {
  return (
    <div className="flex items-center gap-3">
      <Button variant="ghost" size="sm" onClick={onBack}>
        <ArrowLeftIcon className="mr-1 h-3 w-3" /> Back
      </Button>
      <div>
        <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
          Squadron Quickstart
        </div>
        <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
      </div>
    </div>
  );
}

function YamlBlock({ title, yaml }: { title: string; yaml: string }) {
  const [copied, setCopied] = useState(false);
  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(yaml);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      /* clipboard may be blocked in iframes; user can copy manually */
    }
  };
  return (
    <Card>
      <CardContent className="p-4">
        <div className="mb-2 flex items-center justify-between">
          <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
            {title}
          </div>
          <Button variant="ghost" size="sm" onClick={handleCopy}>
            {copied ? (
              <CheckIcon className="mr-1 h-3 w-3" />
            ) : (
              <CopyIcon className="mr-1 h-3 w-3" />
            )}
            {copied ? "Copied" : "Copy"}
          </Button>
        </div>
        <pre className="max-h-96 overflow-auto rounded-md bg-muted/60 p-3 font-mono text-[11px] leading-snug">
          {yaml}
        </pre>
      </CardContent>
    </Card>
  );
}

function CommandBlock({
  cmd,
  hidden: _hidden,
}: {
  cmd: string;
  hidden?: string;
}) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="flex items-start gap-2 rounded-md border border-border bg-muted/40 p-2">
      <pre className="flex-1 overflow-auto whitespace-pre font-mono text-[11px] leading-snug">
        {cmd}
      </pre>
      <Button
        variant="ghost"
        size="sm"
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(cmd);
            setCopied(true);
            setTimeout(() => setCopied(false), 1500);
          } catch {
            /* noop */
          }
        }}
      >
        {copied ? (
          <CheckIcon className="h-3 w-3" />
        ) : (
          <CopyIcon className="h-3 w-3" />
        )}
      </Button>
    </div>
  );
}

// Avoid an unused-import warning for PlusCircleIcon, useEffect,
// and useMemo in case the flow grows. Cheap; keeps the imports list
// stable when the page expands in v0.27.x.
const _quickstartKeepImports = [PlusCircleIcon, useEffect, useMemo];
void _quickstartKeepImports;
