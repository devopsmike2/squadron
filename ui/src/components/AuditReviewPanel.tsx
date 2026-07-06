import { Search } from "lucide-react";
import { useState } from "react";
import useSWR from "swr";

import {
  getActorTimeline,
  getAdminActions,
  getResourceAccess,
  type AdminActionsResult,
  type ReviewBucket,
  type ReviewEvent,
  type ReviewResult,
} from "@/api/auditReview";
import { listTenants } from "@/api/tenants";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useAuditReviewCapabilities } from "@/hooks/useAuditReviewCapabilities";

type ReviewMode = "actor" | "resource" | "admin";
const SELF_TENANT = "__self__";

type ReviewOutcome =
  | { kind: "review"; data: ReviewResult }
  | { kind: "admin"; data: AdminActionsResult };

// AuditReviewPanel is the enterprise access-review surface (ADR 0020 6c / ADR
// 0022), mounted as the "Access review" tab on /audit. It feature-detects via
// the capabilities probe: in OSS (404) it shows an enterprise-feature notice; in
// enterprise it offers the three review patterns — per-actor timeline, per-
// resource access history, and the per-tenant admin-action rollup — with a cross-
// tenant tenant picker gated on capabilities.cross_tenant (the backend 403 is the
// real enforcement; the UI can't see the operator's scopes).
export function AuditReviewPanel() {
  const { isEnterprise, capabilities, isLoading } =
    useAuditReviewCapabilities();

  const [mode, setMode] = useState<ReviewMode>("actor");
  const [actor, setActor] = useState("");
  const [resourceType, setResourceType] = useState("");
  const [resourceId, setResourceId] = useState("");
  const [since, setSince] = useState("");
  const [until, setUntil] = useState("");
  const [tenant, setTenant] = useState<string>(SELF_TENANT); // actor/resource cross-tenant
  const [adminTenant, setAdminTenant] = useState<string>(""); // admin-actions target
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [outcome, setOutcome] = useState<ReviewOutcome | null>(null);

  const crossTenantCapable =
    isEnterprise && capabilities?.cross_tenant === true;

  // Tenant list backs both the cross-tenant picker (actor/resource) and the
  // required admin-actions tenant target. Enterprise-only; 404s in OSS.
  const { data: tenants } = useSWR(
    isEnterprise ? "audit-review-tenants" : null,
    listTenants,
    { shouldRetryOnError: false },
  );

  if (isLoading) {
    return (
      <p
        className="text-muted-foreground text-sm"
        data-testid="audit-review-loading"
      >
        Checking access-review availability…
      </p>
    );
  }

  if (!isEnterprise) {
    return (
      <div
        className="text-muted-foreground rounded-md border border-dashed p-6 text-sm"
        data-testid="audit-review-enterprise-gate"
      >
        <p className="text-foreground font-medium">
          Access review is an enterprise feature
        </p>
        <p className="mt-1">
          Per-actor timelines, per-resource access history, and per-tenant
          admin-action reviews (SOC 2 quarterly access review) are available in
          the enterprise edition. The single-tenant audit log and CSV/JSON
          export on the “Recent activity” tab remain available here.
        </p>
      </div>
    );
  }

  const run = async () => {
    setBusy(true);
    setErr(null);
    setOutcome(null);
    try {
      const w = {
        since: since.trim() || undefined,
        until: until.trim() || undefined,
        tenant:
          crossTenantCapable && tenant !== SELF_TENANT ? tenant : undefined,
      };
      if (mode === "actor") {
        if (!actor.trim()) throw new Error("actor is required");
        setOutcome({
          kind: "review",
          data: await getActorTimeline(actor.trim(), w),
        });
      } else if (mode === "resource") {
        if (!resourceType.trim() || !resourceId.trim())
          throw new Error("resource type and id are required");
        setOutcome({
          kind: "review",
          data: await getResourceAccess(
            resourceType.trim(),
            resourceId.trim(),
            w,
          ),
        });
      } else {
        if (!adminTenant) throw new Error("select a tenant to review");
        setOutcome({
          kind: "admin",
          data: await getAdminActions(adminTenant, {
            since: since.trim() || undefined,
            until: until.trim() || undefined,
          }),
        });
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : "review failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4" data-testid="audit-review-controls">
      <div className="flex flex-wrap items-end gap-2">
        <div>
          <Label htmlFor="review-mode" className="text-xs">
            Review
          </Label>
          <Select value={mode} onValueChange={(v) => setMode(v as ReviewMode)}>
            <SelectTrigger id="review-mode" className="h-9 w-52">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="actor">Per-actor timeline</SelectItem>
              <SelectItem value="resource">Per-resource access</SelectItem>
              <SelectItem value="admin">Tenant admin-actions</SelectItem>
            </SelectContent>
          </Select>
        </div>

        {mode === "actor" && (
          <Input
            value={actor}
            onChange={(e) => setActor(e.target.value)}
            placeholder="actor (e.g. operator:alice@x.io)"
            className="h-9 w-64"
            aria-label="actor"
          />
        )}
        {mode === "resource" && (
          <>
            <Input
              value={resourceType}
              onChange={(e) => setResourceType(e.target.value)}
              placeholder="resource type (e.g. oidc_connection)"
              className="h-9 w-52"
              aria-label="resource type"
            />
            <Input
              value={resourceId}
              onChange={(e) => setResourceId(e.target.value)}
              placeholder="resource id"
              className="h-9 w-56"
              aria-label="resource id"
            />
          </>
        )}
        {mode === "admin" && (
          <div data-testid="audit-review-admin-tenant">
            <Label htmlFor="review-admin-tenant" className="sr-only">
              Tenant
            </Label>
            <Select value={adminTenant} onValueChange={setAdminTenant}>
              <SelectTrigger id="review-admin-tenant" className="h-9 w-56">
                <SelectValue placeholder="select a tenant" />
              </SelectTrigger>
              <SelectContent>
                {(tenants ?? []).map((t) => (
                  <SelectItem key={t.tenant_id} value={t.tenant_id}>
                    {`${t.name} (${t.tenant_id})`}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        )}

        {/* Cross-tenant picker for actor/resource reviews (own tenant by default). */}
        {crossTenantCapable && mode !== "admin" && (
          <div data-testid="audit-review-tenant">
            <Label htmlFor="review-tenant" className="sr-only">
              Tenant
            </Label>
            <Select value={tenant} onValueChange={setTenant}>
              <SelectTrigger id="review-tenant" className="h-9 w-44">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={SELF_TENANT}>My tenant</SelectItem>
                {(tenants ?? []).map((t) => (
                  <SelectItem key={t.tenant_id} value={t.tenant_id}>
                    {`${t.name} (${t.tenant_id})`}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        )}
      </div>

      <div className="flex flex-wrap items-end gap-2">
        <div>
          <Label htmlFor="review-since" className="text-xs">
            Since (RFC3339, optional)
          </Label>
          <Input
            id="review-since"
            value={since}
            onChange={(e) => setSince(e.target.value)}
            placeholder="2026-04-01T00:00:00Z"
            className="h-9 w-56"
          />
        </div>
        <div>
          <Label htmlFor="review-until" className="text-xs">
            Until (RFC3339, optional)
          </Label>
          <Input
            id="review-until"
            value={until}
            onChange={(e) => setUntil(e.target.value)}
            placeholder="2026-07-01T00:00:00Z"
            className="h-9 w-56"
          />
        </div>
        <Button
          onClick={run}
          disabled={busy}
          className="h-9 gap-1"
          data-testid="audit-review-run"
        >
          <Search className="h-4 w-4" />
          {busy ? "Running…" : "Run review"}
        </Button>
      </div>

      {err && (
        <p className="text-xs text-red-600" data-testid="audit-review-error">
          {err}
        </p>
      )}

      {outcome && (
        <div data-testid="audit-review-results" className="space-y-4">
          {outcome.kind === "review" ? (
            <ReviewResultView data={outcome.data} />
          ) : (
            <AdminResultView data={outcome.data} />
          )}
        </div>
      )}
    </div>
  );
}

function Rollup({
  title,
  buckets,
}: {
  title: string;
  buckets: ReviewBucket[];
}) {
  if (buckets.length === 0) return null;
  return (
    <div>
      <p className="text-muted-foreground mb-1 text-xs font-medium">{title}</p>
      <div className="flex flex-wrap gap-1.5">
        {buckets.map((b) => (
          <Badge key={b.key} variant="secondary" className="font-normal">
            {b.key}
            <span className="text-muted-foreground ml-1">· {b.count}</span>
          </Badge>
        ))}
      </div>
    </div>
  );
}

function EventsTable({ events }: { events: ReviewEvent[] }) {
  if (events.length === 0) {
    return (
      <p className="text-muted-foreground text-sm">
        No matching events in the window.
      </p>
    );
  }
  return (
    <div className="max-h-96 overflow-auto rounded-md border">
      <table className="w-full text-left text-xs">
        <thead className="bg-muted/50 text-muted-foreground sticky top-0">
          <tr>
            <th className="p-2 font-medium">Timestamp</th>
            <th className="p-2 font-medium">Actor</th>
            <th className="p-2 font-medium">Event</th>
            <th className="p-2 font-medium">Target</th>
            <th className="p-2 font-medium">Action</th>
          </tr>
        </thead>
        <tbody>
          {events.map((e) => (
            <tr key={e.id} className="border-t align-top">
              <td className="text-muted-foreground p-2 whitespace-nowrap">
                {e.timestamp}
              </td>
              <td className="p-2">{e.actor}</td>
              <td className="p-2 font-mono">{e.event_type}</td>
              <td className="p-2">
                {e.target_type}
                {e.target_id ? `/${e.target_id}` : ""}
              </td>
              <td className="p-2">{e.action}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function ReviewResultView({ data }: { data: ReviewResult }) {
  return (
    <>
      <div className="flex flex-wrap items-center gap-4">
        <span className="text-sm font-medium">{data.count} events</span>
        {data.cross_tenant && data.tenant && (
          <Badge variant="outline">tenant: {data.tenant}</Badge>
        )}
        {data.truncated && (
          <span className="text-xs text-amber-600">
            truncated — narrow the window for the full set
          </span>
        )}
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <Rollup title="By event type" buckets={data.by_event_type} />
        <Rollup title="By actor" buckets={data.by_actor} />
      </div>
      <EventsTable events={data.events} />
    </>
  );
}

function AdminResultView({ data }: { data: AdminActionsResult }) {
  return (
    <>
      <div className="flex flex-wrap items-center gap-4">
        <span className="text-sm font-medium">
          {data.total} admin actions · tenant {data.tenant}
        </span>
        {data.cross_tenant && <Badge variant="outline">cross-tenant</Badge>}
        {data.truncated && (
          <span className="text-xs text-amber-600">
            truncated — counts are a floor
          </span>
        )}
      </div>
      <Rollup
        title="By event type (admin actions)"
        buckets={data.by_event_type}
      />
      <Rollup title="By actor" buckets={data.by_actor} />
      <div>
        <p className="text-muted-foreground mb-1 text-xs font-medium">
          Sample timeline
        </p>
        <EventsTable events={data.sample} />
      </div>
    </>
  );
}
