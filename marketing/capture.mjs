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
