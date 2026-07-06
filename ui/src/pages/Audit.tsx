import { AuditExportButton } from "@/components/AuditExportButton";
import { AuditTimeline } from "@/components/AuditTimeline";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export default function AuditPage() {
  return (
    <div className="space-y-4 p-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Audit log</h1>
          <p className="text-muted-foreground text-sm">
            Every state change in Squadron — config pushes, drift transitions,
            rule edits, alert firings — flows through this log. Newest first.
          </p>
          <p className="text-muted-foreground mt-1 text-xs">
            Exports are themselves recorded here (format, row count, filters),
            so the evidence trail shows who exported what and when.
          </p>
        </div>
        <AuditExportButton />
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base font-semibold">
            Recent activity
          </CardTitle>
        </CardHeader>
        <CardContent>
          <AuditTimeline limit={200} />
        </CardContent>
      </Card>
    </div>
  );
}
