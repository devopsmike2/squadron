/**
 * Inventory Reconciliation surface — v0.32.
 *
 * Two components, both reading /api/v1/inventory/reconciliation:
 *
 *   - <InventorySummary/>   stacked bar + counts; lives on the Dashboard
 *   - <InventoryDetails/>   per-host table; lives on a dedicated page
 *
 * The status bucket distinction matters:
 *   healthy    — expected & recently seen
 *   missing    — expected but never connected, or quiet for > 10 min
 *   unexpected — connected but not in the expected list
 *
 * Refresh interval is 30s (matches the typical OpAMP heartbeat) so
 * the dashboard doesn't lag too far behind a real outage but also
 * doesn't pester DuckDB every 5 seconds.
 */

import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import useSWR from "swr";

import {
  fetchInventoryReport,
  statusColor,
  statusLabel,
  type InventoryStatus,
} from "@/api/inventory";
import { AdoptDrawer } from "@/components/inventory/AdoptDrawer";
import { BulkAdoptModal } from "@/components/inventory/BulkAdoptModal";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { InfoTooltip } from "@/components/ui/info-tooltip";

const REFRESH_MS = 30_000;

export function InventorySummary() {
  const { data } = useSWR("inventory-report", () => fetchInventoryReport(""), {
    refreshInterval: REFRESH_MS,
    shouldRetryOnError: false,
  });
  if (!data || data.total === 0) {
    return null;
  }
  const segs: { status: InventoryStatus; count: number }[] = [
    { status: "missing", count: data.missing },
    { status: "unexpected", count: data.unexpected },
    { status: "healthy", count: data.healthy },
  ];
  return (
    <Card>
      <CardContent className="space-y-3 p-4">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-1.5">
            <h3 className="text-sm font-semibold">Fleet inventory</h3>
            <InfoTooltip label="About fleet inventory" maxWidth={320}>
              Reconciliation between your declared inventory (e.g.
              an Ansible <code>inventory.ini</code> file) and the
              agents that actually checked in. <b>Missing</b> means a
              host was declared but hasn't checked in for ~10 minutes
              — often expected during a deploy, but worth checking
              afterward. <b>Unexpected</b> means an agent showed up
              that isn't in your declared inventory.
            </InfoTooltip>
          </div>
          <span className="text-xs text-muted-foreground">
            {data.total} tracked
          </span>
        </div>
        <div className="flex h-3 w-full overflow-hidden rounded">
          {segs.map((s) =>
            s.count === 0 ? null : (
              <div
                key={s.status}
                title={`${s.count} ${s.status}`}
                style={{
                  background: statusColor(s.status),
                  width: `${(s.count / data.total) * 100}%`,
                }}
              />
            ),
          )}
        </div>
        <div className="grid grid-cols-3 gap-2 text-[11px]">
          {segs.map((s) => (
            <div key={s.status} className="flex items-center gap-1.5">
              <span
                className="inline-block h-2 w-2 rounded-full"
                style={{ background: statusColor(s.status) }}
              />
              <span className="font-tabular">{s.count}</span>
              <span className="text-muted-foreground">{statusLabel(s.status)}</span>
            </div>
          ))}
        </div>
        {data.missing > 0 && (
          <div className="text-xs text-muted-foreground">
            {data.missing} expected{" "}
            {data.missing === 1 ? "host hasn't" : "hosts haven't"} checked in
            recently. See{" "}
            <Link
              to="/inventory"
              className="underline hover:text-foreground"
            >
              Inventory
            </Link>{" "}
            for details.
          </div>
        )}
      </CardContent>
    </Card>
  );
}

export function InventoryDetails() {
  const { data } = useSWR("inventory-report-detail", () => fetchInventoryReport(""), {
    refreshInterval: REFRESH_MS,
  });
  const rows = useMemo(() => data?.rows ?? [], [data]);
  // v0.45 — adoption drawer state. Opens when the operator clicks
  // "Adopt" on a missing row and shows the per-host snippet.
  const [adoptHost, setAdoptHost] = useState<{
    hostname: string;
    labels: Record<string, string>;
  } | null>(null);
  // v0.46 — bulk adoption via deploy pipeline.
  const [bulkOpen, setBulkOpen] = useState(false);
  const missingHosts = useMemo(
    () => rows.filter((r) => r.status === "missing").map((r) => r.hostname),
    [rows],
  );
  if (!data) {
    return (
      <Card>
        <CardContent className="p-4 text-sm text-muted-foreground">
          Loading inventory…
        </CardContent>
      </Card>
    );
  }
  if (!rows.length) {
    return (
      <Card>
        <CardContent className="p-4 text-sm text-muted-foreground">
          <p className="font-medium">No inventory yet.</p>
          <p className="mt-1">
            Your CI/CD pipeline can register the hosts it deployed by
            POSTing to <code>/api/v1/inventory/expected</code>.
            Squadron will then diff that list against connected agents
            and flag missing or unexpected hosts.
          </p>
        </CardContent>
      </Card>
    );
  }
  return (
    <Card>
      {/* v0.46 — Bulk adopt button appears in the table header when
          there are missing hosts to act on. Fires the configured
          adoption pipeline against the selected hosts. */}
      {missingHosts.length > 0 && (
        <div className="flex items-center justify-between border-b border-border/40 px-3 py-2">
          <span className="text-xs text-muted-foreground">
            {missingHosts.length} missing host
            {missingHosts.length === 1 ? "" : "s"} eligible for
            bulk adoption
          </span>
          <Button
            size="sm"
            variant="default"
            onClick={() => setBulkOpen(true)}
            className="h-7 px-3 text-xs"
          >
            Bulk adopt via pipeline…
          </Button>
        </div>
      )}
      <CardContent className="p-0">
        <table className="w-full text-sm">
          <thead className="border-b text-left text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th className="px-3 py-2">Hostname</th>
              <th className="px-3 py-2">Status</th>
              <th className="px-3 py-2">Source</th>
              <th className="px-3 py-2">Last seen</th>
              <th className="px-3 py-2">Notes</th>
              <th className="px-3 py-2 text-right">Action</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.hostname} className="border-b">
                <td className="px-3 py-2 font-mono text-xs">{r.hostname}</td>
                <td className="px-3 py-2">
                  <Badge
                    variant="outline"
                    style={{
                      borderColor: statusColor(r.status),
                      color: statusColor(r.status),
                    }}
                  >
                    {statusLabel(r.status)}
                  </Badge>
                </td>
                <td className="px-3 py-2 text-xs text-muted-foreground">
                  {r.source || "—"}
                </td>
                <td className="px-3 py-2 text-xs font-tabular text-muted-foreground">
                  {r.last_seen
                    ? new Date(r.last_seen).toLocaleString()
                    : "never"}
                </td>
                <td className="px-3 py-2 text-xs text-muted-foreground">
                  {r.notes || ""}
                </td>
                <td className="px-3 py-2 text-right">
                  {/* v0.45 — Adopt only shows for missing hosts.
                      For healthy and unexpected hosts the action
                      doesn't apply (healthy is already managed;
                      unexpected is already checked in via OpAMP). */}
                  {r.status === "missing" && (
                    <Button
                      size="sm"
                      variant="outline"
                      className="h-7 px-2 text-xs"
                      onClick={() =>
                        setAdoptHost({
                          hostname: r.hostname,
                          labels: extractAdoptionLabels(r),
                        })
                      }
                    >
                      Adopt
                    </Button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </CardContent>
      <AdoptDrawer
        open={adoptHost !== null}
        onClose={() => setAdoptHost(null)}
        hostname={adoptHost?.hostname ?? ""}
        labels={adoptHost?.labels}
      />
      <BulkAdoptModal
        open={bulkOpen}
        onClose={() => setBulkOpen(false)}
        candidates={missingHosts}
      />
    </Card>
  );
}

// extractAdoptionLabels pulls the kv labels we want to bake into the
// per-host snippet. The inventory row carries a source string (which
// CI/CD pipeline registered the host) and any labels the pipeline
// attached. We forward both so Squadron's agent card shows the same
// labels you set at deploy time.
function extractAdoptionLabels(
  r: { source?: string; labels?: Record<string, string> },
): Record<string, string> {
  const out: Record<string, string> = {};
  if (r.source) out["squadron.source"] = r.source;
  for (const [k, v] of Object.entries(r.labels ?? {})) {
    out[k] = v;
  }
  return out;
}
