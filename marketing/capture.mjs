/**
 * Demo screenshot capture script.
 *
 * Drives a headless Chromium through Squadron's marketing-relevant
 * pages, takes a 1440x900 screenshot of each, and saves to
 * ./scenes/. Re-run any time the UI changes to refresh the
 * marketing gallery.
 *
 * Run from this directory:
 *   npx playwright install chromium    # one-time
 *   node capture.mjs
 */

import { chromium } from "playwright";
import fs from "node:fs/promises";
import path from "node:path";

const BASE = "http://localhost:5173";
const OUTPUT_DIR = path.join(import.meta.dirname, "scenes");
const VIEWPORT = { width: 1440, height: 900 };

const scenes = [
  {
    name: "01-quickstart-landing",
    url: "/quickstart",
    waitFor: "text=Get your first agent into Squadron",
    description: "Quickstart landing with both paths visible",
  },
  {
    name: "02-savings-hero",
    url: "/savings",
    waitFor: "text=Estimated monthly spend",
    description: "Savings hero numbers + Quick Wins panel",
  },
  {
    name: "03-cost-insights",
    url: "/cost-insights",
    waitFor: "text=Recommendations",
    description: "Cost Insights with recommendations panel",
    scrollY: 300,
  },
  {
    name: "04-recommendations",
    url: "/cost-insights",
    waitFor: "text=Recommendations",
    description: "Recommendations cards with AI Explain expanded",
    afterLoad: async (page) => {
      // Scroll to recommendations
      await page.evaluate(() => window.scrollBy(0, 400));
      await page.waitForTimeout(800);
      // Click "Show details" on the first recommendation to expand it
      await page.getByRole("button", { name: /show details/i }).first().click();
      await page.waitForTimeout(800);
    },
  },
  {
    name: "05-fleet-status",
    url: "/",
    waitFor: "text=Fleet",
    description: "Fleet Status dashboard",
  },
  {
    name: "06-config-editor",
    url: "/configs/new",
    waitFor: "text=AI Assist",
    description: "Config editor with AI Assist dropdown visible",
  },
  {
    // Discovery inventory on the built-in demo account. The deep-link
    // ?account=demo-000000000000 auto-selects the demo connection, lands on
    // the Inventory tab, and auto-runs the (canned) scan — no cloud creds,
    // no clicks. The result is a realistic mixed inventory: 5 EC2 (some
    // instrumented, some not, incl. a Windows box), 3 Lambda, 2 RDS.
    name: "07-discovery-inventory",
    url: "/discovery/aws?account=demo-000000000000",
    waitFor: "text=Scan result for account",
    description:
      "Discovery inventory — instrumented vs uninstrumented compute, functions & databases (AWS · GCP · Azure · OCI)",
    afterLoad: async (page) => {
      // Dismiss any first-run onboarding toast so it doesn't intrude.
      await page.keyboard.press("Escape").catch(() => {});
      // The Wizard tab is forceMount and sits above the results, so scroll
      // the populated scan result into frame.
      const anchor = page
        .getByText("Scan result for account", { exact: false })
        .first();
      await anchor.scrollIntoViewIfNeeded().catch(() => {});
      await page.evaluate(() => window.scrollBy(0, -90));
      await page.waitForTimeout(1000);
    },
  },
  {
    // Same demo account — after the auto-scan, the demo recommendations
    // auto-generate. Switch to the Recommendations tab to show the AI plan:
    // merge-ready Terraform steps (ADOT collector, Lambda layer, RDS levers)
    // an operator reviews and opens as a PR.
    name: "08-discovery-recommendations",
    url: "/discovery/aws?account=demo-000000000000",
    waitFor: "text=Scan result for account",
    description: "AI recommendations → merge-ready Terraform, review & open a PR",
    afterLoad: async (page) => {
      await page.keyboard.press("Escape").catch(() => {});
      // Demo recs auto-generate right after the auto-scan — give them a beat.
      await page.waitForTimeout(4500);
      try {
        await page
          .getByRole("tab", { name: /recommendations/i })
          .first()
          .click({ timeout: 3000 });
      } catch {}
      // Wait for the canned demo plan to render.
      const recAnchor = page
        .getByText("Install the ADOT Collector", { exact: false })
        .first();
      try {
        await recAnchor.waitFor({ timeout: 8000 });
        await recAnchor.scrollIntoViewIfNeeded();
        await page.evaluate(() => window.scrollBy(0, -120));
      } catch {}
      await page.waitForTimeout(1000);
    },
  },
  {
    name: "09-rollouts",
    url: "/rollouts",
    waitFor: "text=Stage a new config",
    description: "Staged config rollouts with AI reasoning + approval gates",
  },
  {
    name: "10-audit",
    url: "/audit",
    waitFor: "text=Every state change in Squadron",
    description: "Audit log — incidents, drift transitions, alerts, every action",
  },

  // ---------------------------------------------------------------------------
  // ENTERPRISE / COMPLIANCE scenes (11+).
  //
  // Unlike the OSS scenes above, these surfaces are enterprise-only: the
  // Identity admin page and the Audit "Access review" / "Integrity" tabs
  // feature-detect the enterprise API and render an "enterprise feature" gate
  // (not the real UI) when /api/v1/rbac, /api/v1/tenants, or the audit
  // review/verify endpoints return 404. To capture them you must point BASE at
  // a dev UI backed by the ENTERPRISE binary (make build-enterprise) with
  // auth enabled AND an authenticated session — i.e. a logged-in browser or a
  // bearer token in context. The OSS scenes need no auth; these do.
  //
  // The capture context here injects NO auth (mirroring the OSS scenes). If
  // your enterprise dev stack requires a bearer, add it once for all scenes,
  // e.g. via context.addInitScript to seed the session token into
  // localStorage, or context.setExtraHTTPHeaders({ Authorization: `Bearer ${TOKEN}` }).
  // Provision the tenants/roles these scenes show first — see
  // ../scripts/demo-seed.sh (Phase 2) and squadron-enterprise/docs/DEPLOYMENT.md.
  //
  // Identity is a Radix Tabs page (defaultValue="roles"); the tab is not in the
  // URL, so each scene clicks its TabsTrigger by name (same pattern as the
  // discovery Recommendations tab in scene 08).
  {
    name: "11-settings-tenants",
    url: "/settings/identity",
    waitFor: "text=Identity",
    description: "Settings ▸ Identity — Tenants tab (multi-tenant isolation)",
    afterLoad: async (page) => {
      try {
        await page.getByRole("tab", { name: /tenants/i }).first().click({ timeout: 3000 });
      } catch {}
      await page.waitForTimeout(1200);
    },
  },
  {
    name: "12-settings-roles",
    url: "/settings/identity",
    waitFor: "text=Identity",
    description: "Settings ▸ Identity — Roles tab (RBAC roles + permissions)",
    afterLoad: async (page) => {
      try {
        await page.getByRole("tab", { name: /^roles$/i }).first().click({ timeout: 3000 });
      } catch {}
      await page.waitForTimeout(1200);
    },
  },
  {
    name: "13-settings-usage",
    url: "/settings/identity",
    waitFor: "text=Identity",
    description: "Settings ▸ Identity — Usage tab (per-tenant usage / chargeback)",
    afterLoad: async (page) => {
      try {
        await page.getByRole("tab", { name: /usage/i }).first().click({ timeout: 3000 });
      } catch {}
      await page.waitForTimeout(1200);
    },
  },
  {
    name: "14-settings-budgets",
    url: "/settings/identity",
    waitFor: "text=Identity",
    description: "Settings ▸ Identity — Budgets tab (per-tenant trace budgets)",
    afterLoad: async (page) => {
      try {
        await page.getByRole("tab", { name: /budgets/i }).first().click({ timeout: 3000 });
      } catch {}
      await page.waitForTimeout(1200);
    },
  },
  {
    // Audit is a Radix Tabs page (defaultValue="activity"); click the
    // "Access review" tab. Enterprise-gated (SOC 2 quarterly access review).
    name: "15-audit-access-review",
    url: "/audit",
    waitFor: "text=Every state change in Squadron",
    description: "Audit ▸ Access review — per-actor / admin-action reviews (SOC 2)",
    afterLoad: async (page) => {
      try {
        await page.getByRole("tab", { name: /access review/i }).first().click({ timeout: 3000 });
      } catch {}
      await page.waitForTimeout(1200);
    },
  },
  {
    // Audit "Integrity" tab — the enterprise tamper-evidence surface
    // (hash-chain attestation / verify).
    name: "16-audit-integrity",
    url: "/audit",
    waitFor: "text=Every state change in Squadron",
    description: "Audit ▸ Integrity — tamper-evidence attestation / verify",
    afterLoad: async (page) => {
      try {
        await page.getByRole("tab", { name: /integrity/i }).first().click({ timeout: 3000 });
      } catch {}
      await page.waitForTimeout(1200);
    },
  },
];

async function main() {
  await fs.mkdir(OUTPUT_DIR, { recursive: true });

  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({
    viewport: VIEWPORT,
    deviceScaleFactor: 2, // retina for sharp screenshots
    colorScheme: "dark",
  });

  // Suppress the one-shot "Press ⌘K to jump anywhere" onboarding toast so it
  // doesn't intrude on any marketing screenshot. The hint fires once per
  // browser gated on this localStorage flag (CommandPaletteHint.tsx); setting
  // it before first paint keeps the toast from ever appearing.
  await context.addInitScript(() => {
    try {
      localStorage.setItem("squadron.hint.cmdk.v1", new Date().toISOString());
    } catch {
      /* storage disabled — nothing to do */
    }
  });

  for (const scene of scenes) {
    const page = await context.newPage();
    process.stdout.write(`  ${scene.name} ... `);
    try {
      // Vite dev server keeps a HMR WebSocket open so "networkidle"
      // never fires. Use domcontentloaded + an explicit content
      // wait instead.
      await page.goto(`${BASE}${scene.url}`, { waitUntil: "domcontentloaded", timeout: 15000 });
      try {
        await page.locator(scene.waitFor).first().waitFor({ timeout: 10000 });
      } catch {
        await page.waitForSelector(scene.waitFor, { timeout: 4000 });
      }
      // Settle: wait for any in-flight SWR fetches to render
      await page.waitForTimeout(1500);
      if (scene.scrollY) {
        await page.evaluate((y) => window.scrollBy(0, y), scene.scrollY);
        await page.waitForTimeout(500);
      }
      if (scene.afterLoad) {
        await scene.afterLoad(page);
      }
      const outPath = path.join(OUTPUT_DIR, `${scene.name}.png`);
      await page.screenshot({ path: outPath, fullPage: false });
      const stat = await fs.stat(outPath);
      console.log(`✓  ${(stat.size / 1024).toFixed(0)} KB  ${scene.description}`);
    } catch (e) {
      console.log(`✗  ${e.message}`);
    } finally {
      await page.close();
    }
  }

  await browser.close();
  console.log("\nDone. Screenshots in ./scenes/");
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
