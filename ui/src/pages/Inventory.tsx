/**
 * Inventory — full-page expected-vs-actual host reconciliation.
 *
 * The "See Inventory for details" link on Fleet Status's inventory
 * summary points here. Until this page existed the link rendered a
 * blank route — caught during the v0.41 UI tour.
 *
 * Wraps the existing <InventoryDetails/> component (the per-host
 * table) plus a page header that explains what the operator is
 * looking at. The summary stacked-bar already lives on Fleet Status;
 * this page is the drill-in.
 *
 * Added in v0.41.1 as a bug-fix for the dangling /inventory link.
 */

import {
  InventoryDetails,
  InventorySummary,
} from "@/components/inventory/InventoryPanel";
import { InfoTooltip } from "@/components/ui/info-tooltip";

export default function InventoryPage() {
  return (
    <div className="flex flex-col gap-4">
      <header>
        <div className="text-[10px] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
          Squadron
        </div>
        <h1 className="text-2xl font-semibold tracking-tight text-foreground">
          <span className="inline-flex items-center gap-1.5">
            Inventory
            <InfoTooltip label="About inventory" maxWidth={360}>
              Squadron compares the hosts that were <i>declared</i> (e.g. by
              your Ansible inventory.ini or a CI pipeline that POSTed to{" "}
              <code>/api/v1/inventory/expected</code>) against the agents that
              have actually checked in. Missing hosts are declared but absent;
              unexpected hosts are present but undeclared.
            </InfoTooltip>
          </span>
        </h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Expected-vs-actual reconciliation. Click "Deploy" on a target with an
          inventory_path set, and Squadron will auto-register every host the
          workflow promised to touch.
        </p>
      </header>

      {/* Re-render the summary at the top of the page for context.
          Cheap because both panels share the SWR cache key. */}
      <InventorySummary />

      {/* The detail table — one row per declared/discovered host. */}
      <InventoryDetails />
    </div>
  );
}
