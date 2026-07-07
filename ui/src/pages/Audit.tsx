import { X } from "lucide-react";
import { useState } from "react";

import { AuditExportButton } from "@/components/AuditExportButton";
import { AuditReviewPanel } from "@/components/AuditReviewPanel";
import { AuditTimeline } from "@/components/AuditTimeline";
import { AuditVerifyPanel } from "@/components/AuditVerifyPanel";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { AuditEventFilter } from "@/types/audit";

export default function AuditPage() {
  // Draft inputs vs the applied filter: typing edits the draft, "Apply"
  // commits it so the timeline + export refetch once, not per keystroke.
  const [actorDraft, setActorDraft] = useState("");
  const [eventTypeDraft, setEventTypeDraft] = useState("");
  const [applied, setApplied] = useState<AuditEventFilter>({});

  const apply = () =>
    setApplied({
      actor: actorDraft.trim() || undefined,
      event_type: eventTypeDraft.trim() || undefined,
    });
  const clear = () => {
    setActorDraft("");
    setEventTypeDraft("");
    setApplied({});
  };
  const hasFilter = Boolean(applied.actor || applied.event_type);

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
        {/* Export honors the applied filter, so a filtered view exports the
            same rows the operator is looking at. */}
        <AuditExportButton filter={applied} />
      </div>

      <Tabs defaultValue="activity">
        <TabsList>
          <TabsTrigger value="activity">Recent activity</TabsTrigger>
          <TabsTrigger value="review">Access review</TabsTrigger>
          <TabsTrigger value="verify">Integrity</TabsTrigger>
        </TabsList>

        <TabsContent value="activity" className="space-y-4">
          <Card>
            <CardHeader className="gap-3">
              <CardTitle className="text-base font-semibold">
                Recent activity
              </CardTitle>
              {/* Access-review filters: narrow to one actor's timeline or one
                  event type. Both use exact-match server-side params. */}
              <form
                className="flex flex-wrap items-center gap-2"
                onSubmit={(e) => {
                  e.preventDefault();
                  apply();
                }}
              >
                <Input
                  value={actorDraft}
                  onChange={(e) => setActorDraft(e.target.value)}
                  placeholder="actor (e.g. operator:alice@x.io)"
                  className="h-9 w-64"
                />
                <Input
                  value={eventTypeDraft}
                  onChange={(e) => setEventTypeDraft(e.target.value)}
                  placeholder="event type (e.g. config.applied)"
                  className="h-9 w-56"
                />
                <Button type="submit" variant="outline" className="h-9">
                  Apply
                </Button>
                {hasFilter && (
                  <Button
                    type="button"
                    variant="ghost"
                    className="h-9 gap-1"
                    onClick={clear}
                  >
                    <X className="h-4 w-4" />
                    Clear
                  </Button>
                )}
              </form>
            </CardHeader>
            <CardContent>
              <AuditTimeline
                actor={applied.actor}
                eventType={applied.event_type}
                limit={200}
              />
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="review" className="space-y-4">
          <Card>
            <CardHeader>
              <CardTitle className="text-base font-semibold">
                Access review
              </CardTitle>
              <p className="text-muted-foreground text-sm">
                Compliance access-review queries — per-actor timelines, per-
                resource access history, and a per-tenant admin-action rollup
                for a SOC 2 quarterly review. Cross-tenant reviews are
                themselves recorded to the audit trail.
              </p>
            </CardHeader>
            <CardContent>
              <AuditReviewPanel />
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="verify" className="space-y-4">
          <Card>
            <CardHeader>
              <CardTitle className="text-base font-semibold">
                Audit integrity
              </CardTitle>
              <p className="text-muted-foreground text-sm">
                Tamper-evidence verification — re-verify the audit log's hash
                chain per tenant or across the whole fleet, and download a
                sealed attestation as compliance evidence. Verification runs are
                themselves recorded to the audit trail.
              </p>
            </CardHeader>
            <CardContent>
              <AuditVerifyPanel />
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>
    </div>
  );
}
