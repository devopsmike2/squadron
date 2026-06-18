import { useState } from "react";
import useSWR, { mutate } from "swr";

import {
  createAlertRule,
  deleteAlertRule,
  listAlertRules,
  updateAlertRule,
} from "@/api/alerts";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import type {
  AlertRule,
  AlertRuleInput,
  AlertSeverity,
  ThresholdOperator,
} from "@/types/alert";

const ALERTS_KEY = "/api/v1/alerts/rules";

const emptyInput = (): AlertRuleInput => ({
  name: "",
  description: "",
  query: "",
  threshold_operator: ">",
  threshold_value: 0,
  interval_seconds: 60,
  severity: "warning",
  enabled: true,
  webhook_url: "",
});

const severityClass: Record<AlertSeverity, string> = {
  info: "bg-blue-500/10 text-blue-700 border-blue-500/20",
  warning: "bg-amber-500/10 text-amber-700 border-amber-500/20",
  critical: "bg-red-500/10 text-red-700 border-red-500/20",
};

export default function AlertsPage() {
  const {
    data: rules,
    isLoading,
    error,
  } = useSWR<AlertRule[]>(ALERTS_KEY, listAlertRules);

  const [editingId, setEditingId] = useState<string | null>(null);
  const [form, setForm] = useState<AlertRuleInput>(emptyInput());
  const [showForm, setShowForm] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const startCreate = () => {
    setEditingId(null);
    setForm(emptyInput());
    setSubmitError(null);
    setShowForm(true);
  };

  const startEdit = (r: AlertRule) => {
    setEditingId(r.id);
    setForm({
      name: r.name,
      description: r.description ?? "",
      query: r.query,
      threshold_operator: r.threshold_operator,
      threshold_value: r.threshold_value,
      interval_seconds: r.interval_seconds,
      severity: r.severity,
      enabled: r.enabled,
      webhook_url: r.webhook_url ?? "",
    });
    setSubmitError(null);
    setShowForm(true);
  };

  const cancelForm = () => {
    setShowForm(false);
    setEditingId(null);
    setSubmitError(null);
  };

  const submit = async () => {
    setSubmitting(true);
    setSubmitError(null);
    try {
      if (editingId) {
        await updateAlertRule(editingId, form);
      } else {
        await createAlertRule(form);
      }
      await mutate(ALERTS_KEY);
      setShowForm(false);
      setEditingId(null);
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : "request failed");
    } finally {
      setSubmitting(false);
    }
  };

  const remove = async (id: string) => {
    if (!confirm("Delete this alert rule?")) return;
    try {
      await deleteAlertRule(id);
      await mutate(ALERTS_KEY);
    } catch (e) {
      alert(e instanceof Error ? e.message : "delete failed");
    }
  };

  return (
    <div className="space-y-4 p-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold">Alerts</h1>
          <p className="text-muted-foreground text-sm">
            Squadron QL rules that fire when their threshold is satisfied.
          </p>
        </div>
        {!showForm && <Button onClick={startCreate}>New rule</Button>}
      </div>

      {showForm && (
        <Card>
          <CardHeader>
            <CardTitle>{editingId ? "Edit rule" : "New rule"}</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <Label htmlFor="name">Name</Label>
                <Input
                  id="name"
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="High error rate"
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="severity">Severity</Label>
                <Select
                  value={form.severity}
                  onValueChange={(v) =>
                    setForm({ ...form, severity: v as AlertSeverity })
                  }
                >
                  <SelectTrigger id="severity">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="info">Info</SelectItem>
                    <SelectItem value="warning">Warning</SelectItem>
                    <SelectItem value="critical">Critical</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>

            <div className="space-y-2">
              <Label htmlFor="description">Description</Label>
              <Input
                id="description"
                value={form.description}
                onChange={(e) =>
                  setForm({ ...form, description: e.target.value })
                }
                placeholder="What this rule fires on, free text"
              />
            </div>

            <div className="space-y-2">
              <Label htmlFor="query">Squadron QL query</Label>
              <Textarea
                id="query"
                value={form.query}
                onChange={(e) => setForm({ ...form, query: e.target.value })}
                placeholder='logs{severity="ERROR"}'
                rows={3}
                className="font-mono text-sm"
              />
              <p className="text-xs text-muted-foreground">
                The rule fires when the result row count satisfies the threshold
                below. Query runs against the last 5 minutes by default.
              </p>
            </div>

            <div className="grid gap-4 md:grid-cols-3">
              <div className="space-y-2">
                <Label htmlFor="op">Operator</Label>
                <Select
                  value={form.threshold_operator}
                  onValueChange={(v) =>
                    setForm({
                      ...form,
                      threshold_operator: v as ThresholdOperator,
                    })
                  }
                >
                  <SelectTrigger id="op">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value=">">&gt;</SelectItem>
                    <SelectItem value=">=">&ge;</SelectItem>
                    <SelectItem value="<">&lt;</SelectItem>
                    <SelectItem value="<=">&le;</SelectItem>
                    <SelectItem value="==">==</SelectItem>
                    <SelectItem value="!=">!=</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label htmlFor="value">Threshold value</Label>
                <Input
                  id="value"
                  type="number"
                  value={form.threshold_value}
                  onChange={(e) =>
                    setForm({
                      ...form,
                      threshold_value: parseFloat(e.target.value) || 0,
                    })
                  }
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="interval">Interval (seconds)</Label>
                <Input
                  id="interval"
                  type="number"
                  min={1}
                  value={form.interval_seconds}
                  onChange={(e) =>
                    setForm({
                      ...form,
                      interval_seconds: parseInt(e.target.value, 10) || 60,
                    })
                  }
                />
              </div>
            </div>

            <div className="space-y-2">
              <Label htmlFor="webhook">Webhook URL (optional)</Label>
              <Input
                id="webhook"
                value={form.webhook_url}
                onChange={(e) =>
                  setForm({ ...form, webhook_url: e.target.value })
                }
                placeholder="https://hooks.example.com/squadron"
              />
              <p className="text-xs text-muted-foreground">
                If set, Squadron POSTs the firing notification payload to this
                URL. Works with any webhook that accepts JSON (including Slack
                incoming webhooks).
              </p>
            </div>

            <div className="flex items-center gap-3">
              <Switch
                id="enabled"
                checked={form.enabled}
                onCheckedChange={(v) => setForm({ ...form, enabled: v })}
              />
              <Label htmlFor="enabled">Enabled</Label>
            </div>

            {submitError && (
              <div className="text-sm text-red-600">{submitError}</div>
            )}

            <div className="flex justify-end gap-2">
              <Button
                variant="outline"
                onClick={cancelForm}
                disabled={submitting}
              >
                Cancel
              </Button>
              <Button onClick={submit} disabled={submitting}>
                {submitting
                  ? "Saving…"
                  : editingId
                    ? "Save changes"
                    : "Create rule"}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {isLoading && (
        <p className="text-sm text-muted-foreground">Loading rules…</p>
      )}
      {error && (
        <p className="text-sm text-red-600">
          Failed to load rules:{" "}
          {error instanceof Error ? error.message : String(error)}
        </p>
      )}

      {rules && rules.length === 0 && !isLoading && (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            No alert rules yet. Click "New rule" to create one.
          </CardContent>
        </Card>
      )}

      {rules && rules.length > 0 && (
        <Card>
          <CardContent className="p-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Severity</TableHead>
                  <TableHead>Condition</TableHead>
                  <TableHead>Interval</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rules.map((r) => (
                  <TableRow
                    key={r.id}
                    className="cursor-pointer"
                    onClick={() => startEdit(r)}
                  >
                    <TableCell className="font-medium">{r.name}</TableCell>
                    <TableCell>
                      <Badge
                        variant="outline"
                        className={severityClass[r.severity]}
                      >
                        {r.severity}
                      </Badge>
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      count {r.threshold_operator} {r.threshold_value}
                    </TableCell>
                    <TableCell>{r.interval_seconds}s</TableCell>
                    <TableCell>
                      {r.enabled ? (
                        <Badge variant="default">Enabled</Badge>
                      ) : (
                        <Badge variant="secondary">Disabled</Badge>
                      )}
                    </TableCell>
                    <TableCell
                      className="text-right"
                      onClick={(e) => e.stopPropagation()}
                    >
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => remove(r.id)}
                      >
                        Delete
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
