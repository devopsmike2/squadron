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
];

async function main() {
  await fs.mkdir(OUTPUT_DIR, { recursive: true });

  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({
    viewport: VIEWPORT,
    deviceScaleFactor: 2, // retina for sharp screenshots
    colorScheme: "dark",
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
