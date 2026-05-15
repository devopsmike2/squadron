import { SquadronQLInterface } from "@/components/squadron-ql/SquadronQLInterface";

export default function TelemetryPage() {
  return (
    <div className="container mx-auto p-6 space-y-6">
      <div className="flex justify-between items-center">
        <div>
          <h1 className="text-3xl font-bold">Telemetry Explorer</h1>
          <p className="text-muted-foreground">
            Query and explore metrics, logs, and traces using Squadron QL
          </p>
        </div>
      </div>

      {/* Squadron QL Interface */}
      <div className="min-h-[600px]">
        <SquadronQLInterface />
      </div>
    </div>
  );
}
