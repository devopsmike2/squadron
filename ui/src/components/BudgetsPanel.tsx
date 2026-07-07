import { useState } from "react";
import useSWR from "swr";

import {
  listBudgets,
  putTenantBudget,
  deleteTenantBudget,
  type Budget,
} from "@/api/budgets";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useBudgetCapabilities } from "@/hooks/useBudgetCapabilities";

// BudgetsPanel is the enterprise per-tenant trace-index budget admin surface,
// mounted as the "Budgets" tab on SettingsIdentity. It feature-detects the
// enterprise /api/v1/budgets surface: in OSS (404) it shows an enterprise-
// feature notice; in enterprise it shows the caller's own-tenant budget, and —
// when cross_tenant is available — an editable row per tenant so an operator
// can set/clear each tenant's max_rows cap. canWrite is a client hint only; the
// backend 403 is the real write enforcement. A max_rows of 0 means "no override
// (the global cap applies)".
export function BudgetsPanel() {
  const { isEnterprise, capabilities, isLoading } = useBudgetCapabilities();

  const crossTenantCapable =
    isEnterprise && capabilities?.cross_tenant === true;
  const canWrite = capabilities?.scopes?.includes("budgets:write") ?? false;

  const {
    data: budgets,
    error: budgetsError,
    isLoading: budgetsLoading,
    mutate,
  } = useSWR<Budget[]>(
    isEnterprise
      ? crossTenantCapable
        ? "budgets-list"
        : ["budget-own"]
      : null,
    listBudgets,
    { shouldRetryOnError: false },
  );

  if (isLoading) {
    return (
      <p className="text-muted-foreground text-sm">
        Checking budgets availability…
      </p>
    );
  }

  if (!isEnterprise) {
    return (
      <div
        className="text-muted-foreground rounded-md border border-dashed p-6 text-sm"
        data-testid="budgets-enterprise-gate"
      >
        <p className="text-foreground font-medium">
          Per-tenant budgets are an enterprise feature
        </p>
        <p className="mt-1">
          Per-tenant trace-index budgets are an enterprise feature. Cap how many
          trace-index rows each tenant retains — available in the enterprise
          edition.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-4" data-testid="budgets-controls">
      {crossTenantCapable && (
        <p
          className="text-muted-foreground text-xs"
          data-testid="budgets-scope"
        >
          Editing budgets across all tenants.
        </p>
      )}

      {budgetsError && (
        <p className="text-xs text-red-600" data-testid="budgets-list-error">
          {budgetsError instanceof Error
            ? budgetsError.message
            : "failed to load budgets"}
        </p>
      )}

      {budgetsLoading && !budgets && (
        <p className="text-muted-foreground text-sm">Loading budgets…</p>
      )}

      {budgets && budgets.length === 0 && (
        <p
          className="text-muted-foreground text-sm"
          data-testid="budgets-empty"
        >
          No budgets configured.
        </p>
      )}

      {budgets && budgets.length > 0 && (
        <div className="space-y-3" data-testid="budgets-rows">
          {budgets.map((b) => (
            <BudgetRow
              key={b.tenant}
              budget={b}
              canWrite={canWrite}
              onChanged={() => mutate()}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function BudgetRow({
  budget,
  canWrite,
  onChanged,
}: {
  budget: Budget;
  canWrite: boolean;
  onChanged: () => void;
}) {
  const tenant = budget.tenant;
  const [value, setValue] = useState(String(budget.max_rows));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const dirty = value.trim() !== String(budget.max_rows);

  const save = async () => {
    const n = Number(value);
    if (!Number.isFinite(n) || n <= 0) {
      setErr("must be a positive number");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await putTenantBudget(tenant, n);
      onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "save failed");
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    setBusy(true);
    setErr(null);
    try {
      await deleteTenantBudget(tenant);
      onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "delete failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          <Badge variant="outline">tenant: {tenant}</Badge>
          {budget.max_rows === 0 && (
            <Badge variant="secondary">no override (global cap)</Badge>
          )}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-1">
        <div className="flex items-end gap-1">
          <div className="max-w-[12rem]">
            <Label htmlFor={`budgets-maxrows-${tenant}`}>Max rows</Label>
            <Input
              id={`budgets-maxrows-${tenant}`}
              type="number"
              min={1}
              value={value}
              onChange={(e) => setValue(e.target.value)}
              className="h-8"
              disabled={busy || !canWrite}
              data-testid={`budgets-maxrows-${tenant}`}
            />
          </div>
          <Button
            size="sm"
            variant="outline"
            className="h-8"
            onClick={save}
            disabled={busy || !dirty || !canWrite}
            data-testid={`budgets-save-${tenant}`}
          >
            Save
          </Button>
          <Button
            size="sm"
            variant="destructive"
            className="h-8"
            onClick={remove}
            disabled={busy || !canWrite}
            data-testid={`budgets-delete-${tenant}`}
          >
            Delete
          </Button>
        </div>
        {budget.max_rows === 0 && (
          <p className="text-muted-foreground text-xs">
            0 = no override (the global cap applies).
          </p>
        )}
        {err && (
          <p
            className="text-xs text-red-600"
            data-testid={`budgets-error-${tenant}`}
          >
            {err}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
