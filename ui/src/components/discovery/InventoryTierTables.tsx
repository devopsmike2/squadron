import type {
  LoadBalancerSnapshot,
  ObjectStoreSnapshot,
} from "@/api/discovery";

// Shared inventory tables for the object-store + load-balancer tiers
// (coverage-parity arc slice 5). Rendered in the GCP/Azure/OCI Inventory
// sub-tabs so the tiers AWS already surfaced appear uniformly across all
// four clouds.

function emptyRow(colSpan: number, label: string) {
  return (
    <tr>
      <td
        colSpan={colSpan}
        className="p-3 text-center text-xs text-muted-foreground"
      >
        {label}
      </td>
    </tr>
  );
}

export function ObjectStoresTable({ rows }: { rows?: ObjectStoreSnapshot[] }) {
  const data = rows ?? [];
  return (
    <table className="w-full text-xs">
      <thead>
        <tr className="border-b text-left text-muted-foreground">
          <th className="p-2">Name</th>
          <th className="p-2">Region</th>
          <th className="p-2">Access logging</th>
        </tr>
      </thead>
      <tbody>
        {data.length === 0
          ? emptyRow(3, "No object stores found in this scan.")
          : data.map((o) => (
              <tr key={o.resource_id} className="border-b">
                <td className="p-2 font-mono">{o.resource_id}</td>
                <td className="p-2">{o.region}</td>
                <td className="p-2">
                  {o.server_access_logging_enabled ? (
                    <span className="text-emerald-600">Enabled</span>
                  ) : (
                    <span className="text-amber-600">Off</span>
                  )}
                </td>
              </tr>
            ))}
      </tbody>
    </table>
  );
}

export function LoadBalancersTable({
  rows,
}: {
  rows?: LoadBalancerSnapshot[];
}) {
  const data = rows ?? [];
  return (
    <table className="w-full text-xs">
      <thead>
        <tr className="border-b text-left text-muted-foreground">
          <th className="p-2">Name</th>
          <th className="p-2">Type</th>
          <th className="p-2">Scheme</th>
          <th className="p-2">Region</th>
          <th className="p-2">Access logs</th>
        </tr>
      </thead>
      <tbody>
        {data.length === 0
          ? emptyRow(5, "No load balancers found in this scan.")
          : data.map((lb) => (
              <tr key={lb.resource_id} className="border-b">
                <td className="p-2 font-mono">{lb.name || lb.resource_id}</td>
                <td className="p-2">{lb.type}</td>
                <td className="p-2">{lb.scheme}</td>
                <td className="p-2">{lb.region}</td>
                <td className="p-2">
                  {lb.access_logs_enabled ? (
                    <span className="text-emerald-600">Enabled</span>
                  ) : (
                    <span className="text-amber-600">Off</span>
                  )}
                </td>
              </tr>
            ))}
      </tbody>
    </table>
  );
}
