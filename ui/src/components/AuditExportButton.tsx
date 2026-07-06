import { Download } from "lucide-react";
import { useState } from "react";

import { downloadAuditExport, type AuditExportFormat } from "@/api/audit";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { AuditEventFilter } from "@/types/audit";

// AuditExportButton downloads the audit log as a CSV/JSON evidence file
// (ADR 0020). It reuses the caller's current filter but raises the limit to the
// store's page cap so an export captures the full available window, not just
// the rows shown on screen. The download itself is recorded in the audit log
// (audit.exported), so exports are on the record for compliance evidence.
export function AuditExportButton({ filter }: { filter?: AuditEventFilter }) {
  const [format, setFormat] = useState<AuditExportFormat>("csv");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const onExport = async () => {
    setBusy(true);
    setErr(null);
    try {
      const limit = filter?.limit && filter.limit > 1000 ? filter.limit : 1000;
      await downloadAuditExport({ ...filter, limit }, format);
    } catch (e) {
      setErr(e instanceof Error ? e.message : "export failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col items-end gap-1">
      <div className="flex items-center gap-2">
        <Select
          value={format}
          onValueChange={(v) => setFormat(v as AuditExportFormat)}
        >
          <SelectTrigger className="h-9 w-24">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="csv">CSV</SelectItem>
            <SelectItem value="json">JSON</SelectItem>
          </SelectContent>
        </Select>
        <Button onClick={onExport} disabled={busy} className="gap-1">
          <Download className="h-4 w-4" />
          {busy ? "Exporting…" : "Export"}
        </Button>
      </div>
      {err && <span className="text-xs text-red-600">{err}</span>}
    </div>
  );
}
