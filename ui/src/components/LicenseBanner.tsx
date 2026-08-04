// LicenseBanner shows a top-of-app notice ONLY when the enterprise license needs
// attention: expired/invalid, in its grace period, or a trial/term expiring soon
// (<= 14 days). It is silent for a healthy enterprise license and for the OSS
// edition (no nagging). Backed by GET /api/v1/license/status. (ADR 0032 S1.)

import useSWR from "swr";

import { getLicenseStatus, UPGRADE_URL, type LicenseStatus } from "@/api/license";

const WARN_WITHIN_DAYS = 14;

type Severity = "none" | "warn" | "error";

function severityOf(s: LicenseStatus | undefined): Severity {
  if (!s || s.edition !== "enterprise") return "none";
  if (s.state === "expired" || s.state === "invalid") return "error";
  if (s.state === "grace" || s.in_grace) return "error";
  if (
    s.state === "valid" &&
    typeof s.days_remaining === "number" &&
    s.days_remaining <= WARN_WITHIN_DAYS
  ) {
    return "warn";
  }
  return "none";
}

function messageOf(s: LicenseStatus): string {
  if (s.state === "expired" || s.state === "invalid") {
    return "Your Squadron Enterprise license has expired. Enterprise features are now limited.";
  }
  if (s.state === "grace" || s.in_grace) {
    return "Your Squadron Enterprise license has lapsed and is in its grace period. Renew to keep enterprise features.";
  }
  const d = s.days_remaining ?? 0;
  const kind = s.trial ? "trial" : "license";
  return `Your Squadron Enterprise ${kind} expires in ${d} day${d === 1 ? "" : "s"}.`;
}

export function LicenseBanner() {
  const { data } = useSWR<LicenseStatus>("license-status", getLicenseStatus, {
    revalidateOnFocus: false,
    shouldRetryOnError: false,
  });

  const severity = severityOf(data);
  if (severity === "none" || !data) return null;

  const isError = severity === "error";
  return (
    <div
      role="alert"
      data-testid="license-banner"
      className={`flex items-center justify-between gap-4 rounded-md border px-4 py-2 text-sm ${
        isError
          ? "border-red-300 bg-red-50 text-red-900 dark:border-red-800 dark:bg-red-950 dark:text-red-100"
          : "border-amber-300 bg-amber-50 text-amber-900 dark:border-amber-800 dark:bg-amber-950 dark:text-amber-100"
      }`}
    >
      <span>{messageOf(data)}</span>
      <a
        href={UPGRADE_URL}
        target="_blank"
        rel="noreferrer"
        className="whitespace-nowrap font-medium underline underline-offset-2"
      >
        {isError ? "Renew license" : "Manage license"}
      </a>
    </div>
  );
}
