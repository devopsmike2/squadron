import { AuditTimeline } from "@/components/AuditTimeline";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export default function AuditPage() {
  return (
    <div className="space-y-4 p-6">
      <div>
        <h1 className="text-2xl font-semibold">Audit log</h1>
        <p className="text-muted-foreground text-sm">
          Every state change in Squadron — config pushes, drift transitions,
          rule edits, alert firings — flows through this log. Newest first.
        </p>
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
