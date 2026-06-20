// DiscoveryAWS — v0.85 Stream 2E first user-facing payoff for the
// universal-observation arc. Lands the Account / Inventory /
// Recommendations triptych the design doc's "Recommendation surface"
// section calls out.
//
// Slice 1 honesty:
//   - Account tab is fully wired: list connections + open the wizard
//     dialog to connect a new one.
//   - Inventory tab is on-demand only: scans are not persisted, the
//     operator re-scans to see fresh data. The note under the picker
//     calls this out so an SRE doesn't expect a scan history.
//   - Recommendations tab is a placeholder for the proposer's
//     observation mode (lands in the next slice).
//
// The page reuses the v0.84 ProposerPlayground's result-panel idiom:
// a summary header, then collapsible sections per resource category,
// with badges for OTel detection. Same UX shape the operator already
// recognizes from Squadron's AI surface.

import {
  Check,
  ChevronDown,
  ChevronRight,
  Cloud,
  Loader2,
  Play,
  Sparkles,
  X,
} from "lucide-react";
import { useCallback, useMemo, useState } from "react";
import useSWR from "swr";

import {
  listAWSConnections,
  runAWSScan,
  saveAWSConnection,
  validateAWSConnection,
  type CloudConnection,
  type ScanResult,
} from "@/api/discovery";
import { ConnectorWizard } from "@/components/discovery/ConnectorWizard";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { awsWizard } from "@/data/awsWizard";

// ACCOUNT_TAB / INVENTORY_TAB / RECS_TAB — string literals used both as
// Radix Tabs values and as a stable key for tests to query by role.
const ACCOUNT_TAB = "account";
const INVENTORY_TAB = "inventory";
const RECS_TAB = "recommendations";

// formatTime renders an ISO timestamp as a relative ("3m ago") string
// for recent values and falls back to a locale date for older ones.
// Inlined to avoid a date-fns dependency on a single-use page —
// mirrors the helper Plan.tsx ships.
function formatTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return iso;
  const secs = Math.floor((Date.now() - t) / 1000);
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return new Date(iso).toLocaleDateString();
}

export default function DiscoveryAWSPage() {
  return (
    <div className="space-y-4 p-6">
      <header>
        <div className="flex items-center gap-2">
          <Cloud className="h-5 w-5 text-violet-500" />
          <h1 className="text-2xl font-semibold">AWS Discovery</h1>
        </div>
        <p className="text-sm text-muted-foreground">
          Connect AWS accounts and discover what&apos;s uninstrumented.
        </p>
      </header>

      <Tabs defaultValue={ACCOUNT_TAB}>
        <TabsList>
          <TabsTrigger value={ACCOUNT_TAB}>Account</TabsTrigger>
          <TabsTrigger value={INVENTORY_TAB}>Inventory</TabsTrigger>
          <TabsTrigger value={RECS_TAB}>Recommendations</TabsTrigger>
        </TabsList>

        <TabsContent value={ACCOUNT_TAB} className="mt-4">
          <AccountTab />
        </TabsContent>
        <TabsContent value={INVENTORY_TAB} className="mt-4">
          <InventoryTab />
        </TabsContent>
        <TabsContent value={RECS_TAB} className="mt-4">
          <RecommendationsTab />
        </TabsContent>
      </Tabs>
    </div>
  );
}

// --- Account tab ---------------------------------------------------

function AccountTab() {
  const { data, error, isLoading, mutate } = useSWR(
    "/discovery/aws/connections",
    () => listAWSConnections(),
  );
  const [open, setOpen] = useState(false);

  const connections = data?.connections ?? [];

  const onWizardComplete = useCallback(() => {
    // Refresh the SWR cache so the new connection card lands without a
    // page reload. The dialog auto-closes after a short delay so the
    // operator can read the wizard's success card.
    void mutate();
    setOpen(false);
  }, [mutate]);

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-base font-semibold">Connected accounts</h2>
          <p className="text-xs text-muted-foreground">
            One row per AWS account Squadron is configured to scan.
          </p>
        </div>
        <Dialog open={open} onOpenChange={setOpen}>
          <Button onClick={() => setOpen(true)}>Connect new account</Button>
          <DialogContent className="max-w-2xl">
            <DialogHeader>
              <DialogTitle>Connect AWS account</DialogTitle>
              <DialogDescription>
                Walk through the five steps to grant Squadron read-only access
                via IAM assume-role.
              </DialogDescription>
            </DialogHeader>
            <ConnectorWizard
              wizard={awsWizard}
              onValidate={(req) => validateAWSConnection(req)}
              onSave={(req) =>
                saveAWSConnection(req).then((r) => ({
                  connection_id: r.connection_id,
                }))
              }
              onComplete={onWizardComplete}
            />
          </DialogContent>
        </Dialog>
      </div>

      {error && (
        <Card>
          <CardContent className="p-4 text-sm text-destructive">
            Could not load connections: {String(error)}
          </CardContent>
        </Card>
      )}

      {isLoading && (
        <div className="space-y-2">
          <Skeleton className="h-20 w-full" />
          <Skeleton className="h-20 w-full" />
        </div>
      )}

      {!isLoading && connections.length === 0 && <AccountEmptyState />}

      {!isLoading && connections.length > 0 && (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          {connections.map((c) => (
            <ConnectionCard key={c.account_id} conn={c} />
          ))}
        </div>
      )}
    </div>
  );
}

function ConnectionCard({ conn }: { conn: CloudConnection }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{conn.display_name}</CardTitle>
        <CardDescription>
          AWS account{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
            {conn.account_id}
          </code>
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        <div className="flex flex-wrap gap-1">
          {conn.regions.map((r) => (
            <Badge key={r} variant="secondary">
              {r}
            </Badge>
          ))}
        </div>
        <p className="text-xs text-muted-foreground">
          Connected {formatTime(conn.created_at)}
        </p>
      </CardContent>
    </Card>
  );
}

function AccountEmptyState() {
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-3 p-8 text-center">
        <Cloud className="h-10 w-10 text-muted-foreground" aria-hidden />
        <div>
          <h3 className="text-base font-semibold">
            No accounts connected yet.
          </h3>
          <p className="mt-1 text-sm text-muted-foreground">
            Click &quot;Connect new account&quot; to start.
          </p>
        </div>
        <div className="rounded-md border bg-muted/30 p-3 text-xs text-muted-foreground">
          <Sparkles
            className="mr-1 inline-block h-3 w-3 text-violet-500"
            aria-hidden
          />
          Squadron never holds your AWS write credentials — recommendations land
          as Terraform snippets for your existing pipeline.
        </div>
      </CardContent>
    </Card>
  );
}

// --- Inventory tab -------------------------------------------------

function InventoryTab() {
  const { data: connData } = useSWR("/discovery/aws/connections", () =>
    listAWSConnections(),
  );
  const connections = connData?.connections ?? [];

  const [selected, setSelected] = useState<string>("");
  const [scanning, setScanning] = useState(false);
  const [result, setResult] = useState<ScanResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  const onRun = useCallback(async () => {
    if (!selected || scanning) return;
    setScanning(true);
    setError(null);
    setResult(null);
    try {
      const r = await runAWSScan(selected);
      setResult(r);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setScanning(false);
    }
  }, [selected, scanning]);

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Run an inventory scan</CardTitle>
          <CardDescription>
            Pick a connected account and trigger an on-demand scan.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="flex flex-col gap-3 md:flex-row md:items-end">
            <div className="flex-1">
              <Select value={selected} onValueChange={setSelected}>
                <SelectTrigger
                  aria-label="Connected account"
                  disabled={connections.length === 0}
                >
                  <SelectValue
                    placeholder={
                      connections.length === 0
                        ? "Connect an account first"
                        : "Select an account"
                    }
                  />
                </SelectTrigger>
                <SelectContent>
                  {connections.map((c) => (
                    <SelectItem key={c.account_id} value={c.account_id}>
                      {c.display_name} ({c.account_id})
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Button
              onClick={onRun}
              disabled={!selected || scanning}
              className="gap-1"
            >
              {scanning ? (
                <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
              ) : (
                <Play className="h-4 w-4" aria-hidden />
              )}
              {scanning ? "Scanning…" : "Run scan"}
            </Button>
          </div>
          <p className="text-xs text-muted-foreground">
            Slice 1 doesn&apos;t persist scan history; run a scan to see current
            state. Each scan emits audit events.
          </p>
        </CardContent>
      </Card>

      {error && (
        <Card>
          <CardContent className="p-4 text-sm text-destructive">
            Scan failed: {error}
          </CardContent>
        </Card>
      )}

      {scanning && !result && <ScanSkeleton />}

      {!scanning && !result && !error && <InventoryEmptyState />}

      {result && <ScanResultPanel result={result} />}
    </div>
  );
}

function ScanSkeleton() {
  return (
    <Card>
      <CardContent className="space-y-2 p-4">
        <Skeleton className="h-6 w-1/2" />
        <Skeleton className="h-32 w-full" />
      </CardContent>
    </Card>
  );
}

function InventoryEmptyState() {
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-2 p-8 text-center">
        <p className="text-sm text-muted-foreground">
          Run a scan to see current inventory.
        </p>
      </CardContent>
    </Card>
  );
}

function ScanResultPanel({ result }: { result: ScanResult }) {
  return (
    <div className="space-y-4">
      {/* Summary header */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            Scan result for account{" "}
            <code className="rounded bg-muted px-1 py-0.5 font-mono text-sm">
              {result.account_id}
            </code>
          </CardTitle>
          <CardDescription>
            Regions {result.regions.join(", ") || "(none)"} · completed{" "}
            {formatTime(result.scan_completed_at)} · scanned{" "}
            {result.compute.length} compute instances and{" "}
            {result.functions.length} functions.
          </CardDescription>
        </CardHeader>
      </Card>

      {/* Partial warning */}
      {result.partial && (
        <Card className="border-yellow-500/50 bg-yellow-500/5">
          <CardContent className="p-4 text-sm">
            <span className="font-medium">Scan was partial:</span>{" "}
            {result.partial_reason ?? "the walk did not cover the full inventory"}.
            Re-run to capture missed resources.
          </CardContent>
        </Card>
      )}

      {/* Compute section */}
      <ComputeSection compute={result.compute} />

      {/* Functions section */}
      <FunctionsSection functions={result.functions} />
    </div>
  );
}

function ComputeSection({
  compute,
}: {
  compute: ScanResult["compute"];
}) {
  const [open, setOpen] = useState(compute.length > 0);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
        >
          <CardTitle className="text-base">
            Compute instances ({compute.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {compute.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No compute instances visible in the scanned regions.
            </p>
          ) : (
            <ul className="space-y-2">
              {compute.map((c) => (
                <li
                  key={c.resource_id}
                  className="rounded-md border bg-muted/20 p-3"
                >
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div>
                      <code className="font-mono text-sm">{c.resource_id}</code>
                      <p className="text-xs text-muted-foreground">
                        {c.instance_type} · {c.region}
                      </p>
                    </div>
                    <OtelBadge ok={c.has_otel} />
                  </div>
                  <TagPills tags={c.tags} />
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      )}
    </Card>
  );
}

function FunctionsSection({
  functions,
}: {
  functions: ScanResult["functions"];
}) {
  const [open, setOpen] = useState(functions.length > 0);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
        >
          <CardTitle className="text-base">
            Functions ({functions.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {functions.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No functions visible in the scanned regions.
            </p>
          ) : (
            <ul className="space-y-2">
              {functions.map((f) => (
                <li
                  key={f.resource_id}
                  className="flex flex-wrap items-center justify-between gap-2 rounded-md border bg-muted/20 p-3"
                >
                  <div>
                    <p className="font-medium">{f.name}</p>
                    <p className="text-xs text-muted-foreground">
                      {f.runtime} · {f.region}
                    </p>
                  </div>
                  <OtelBadge ok={f.has_otel_layer} />
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      )}
    </Card>
  );
}

function OtelBadge({ ok }: { ok: boolean }) {
  if (ok) {
    return (
      <Badge
        variant="outline"
        className="border-green-600/50 text-green-700 dark:text-green-400"
      >
        <Check className="h-3 w-3" aria-hidden />
        OTel detected
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className="text-muted-foreground">
      <X className="h-3 w-3" aria-hidden />
      No OTel
    </Badge>
  );
}

function TagPills({ tags }: { tags: Record<string, string> }) {
  const entries = useMemo(() => Object.entries(tags ?? {}), [tags]);
  if (entries.length === 0) return null;
  const visible = entries.slice(0, 5);
  const overflow = entries.length - visible.length;
  return (
    <div className="mt-2 flex flex-wrap items-center gap-1">
      {visible.map(([k, v]) => (
        <Badge
          key={k}
          variant="outline"
          className="text-[10px] font-normal text-muted-foreground"
        >
          {k}
          {v ? `=${v}` : ""}
        </Badge>
      ))}
      {overflow > 0 && (
        <span className="text-[10px] text-muted-foreground">
          and {overflow} more
        </span>
      )}
    </div>
  );
}

// --- Recommendations tab -------------------------------------------

function RecommendationsTab() {
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-3 p-8 text-center">
        <Sparkles className="h-8 w-8 text-violet-500" aria-hidden />
        <div>
          <h3 className="text-base font-semibold">
            Recommendations coming in the next slice.
          </h3>
          <p className="mt-2 max-w-md text-sm text-muted-foreground">
            The v0.66 generalized recommendation surface (Stream 2B) is ready
            to receive discovery-source recommendations once the proposer&apos;s
            observation mode ships. For now, the Inventory tab is the surface
            for understanding coverage.
          </p>
          <p className="mt-3 max-w-md text-xs text-muted-foreground">
            Heads up: when they land, recommendations will arrive as Terraform
            snippets you paste into your existing pipeline — Squadron never
            mutates your cloud.
          </p>
        </div>
      </CardContent>
    </Card>
  );
}
