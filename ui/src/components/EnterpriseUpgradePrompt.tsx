import type { ReactNode } from "react";

import { UPGRADE_URL } from "@/api/license";

// EnterpriseUpgradePrompt is the shared "this is an enterprise feature" gate
// shown on OSS builds where an enterprise surface is unavailable (the seam 404s).
// It carries the upgrade CTA, whose destination is configurable via
// VITE_ENTERPRISE_UPGRADE_URL. (ADR 0032 S1c.)
export function EnterpriseUpgradePrompt({
  title,
  children,
  testId = "enterprise-upgrade-prompt",
}: {
  title: string;
  children?: ReactNode;
  testId?: string;
}) {
  return (
    <div
      className="text-muted-foreground rounded-md border border-dashed p-6 text-sm"
      data-testid={testId}
    >
      <p className="text-foreground font-medium">{title}</p>
      {children ? <p className="mt-1">{children}</p> : null}
      <a
        href={UPGRADE_URL}
        target="_blank"
        rel="noreferrer"
        data-testid="enterprise-upgrade-cta"
        className="text-primary mt-3 inline-block font-medium underline underline-offset-2"
      >
        Upgrade to Enterprise →
      </a>
    </div>
  );
}
