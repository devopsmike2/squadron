# 90-second demo script

For the landing-page hero video / Twitter-X demo / conference
intro. Total runtime: ~90 seconds. Voice-over and on-screen text
in alternating columns; cuts at section breaks.

## Cold open (0–8s)

**Visual:** macOS terminal, fullscreen. Operator types:

```bash
docker compose up -d
```

Containers spin up. ~3-second time-lapse of logs. Then:

```bash
open http://localhost:8080
```

Browser opens to the Squadron Dashboard with the orange "No agents
yet — let's get your first one connected" banner.

**Voice-over:**
> "Squadron is an open-source OpenTelemetry control plane. One
> Docker command to start. Designed for small teams paying too
> much for telemetry."

## Quickstart (8–25s)

**Visual:** Operator clicks the banner. Lands on `/quickstart`.
Click "I have collectors running" (Path B). Snippet appears.
Operator hits "Copy". Cuts to a second terminal that pastes into
an existing `otelcol.yaml`, runs `systemctl restart otelcol`.

Back in the browser, the Quickstart page's "watching for agents…"
spinner flips to the green "Agent connected!" celebration.

**Voice-over:**
> "If you already have collectors deployed, paste this OpAMP
> snippet into your existing config, restart, and they show up in
> Squadron within seconds. No re-deploy. No vendor lock-in."

## Cost insights (25–45s)

**Visual:** Operator clicks the "Open Fleet Status" button.
Dashboard fills in: online agents, drift status, recent activity.
Cut to the Cost Insights page — shows the Volume panel (158 MB
metrics), Outlier Agents, Top Attributes with `code.stacktrace`
at 50%, `processor` dominating metrics.

**Voice-over:**
> "The moment your agents start reporting, Squadron breaks down
> exactly where your bytes are going. By signal. By agent. By
> attribute. Sampled estimates so you can spot the noisy keys
> before they spike your bill."

## Savings + recommendations (45–65s)

**Visual:** Operator clicks "Savings" in the sidebar.

Two hero cards appear:
- Estimated monthly spend: **$847/month**
- Potential monthly savings: **$312/month** (green)

Below, the Quick Wins panel. Top recommendation: "Drop attribute
`http.url` from traces" — **CRITICAL** — saves **$211/month**.

Operator clicks "Apply".

**Voice-over:**
> "Squadron projects your monthly spend in dollars and ranks fixes
> by what they'd save. Click Apply on any of them and you're in
> the config editor with the change pre-staged. Nothing rolls out
> until you review the diff and approve."

## AI assist (65–82s)

**Visual:** The config editor opens with the recommendation
banner at the top and the processor block already inserted. The
"AI Assist" dropdown opens. Operator clicks "Merge in a snippet".

Modal opens; operator pastes a custom snippet about k8s labels.
Click "Merge into editor". A 2-second spinner. Editor updates;
the diff highlights the new processor in the pipeline.

Cut to the Cost Insights page — operator clicks "Explain" on a
recommendation. Inline AI panel appears with a 2-sentence
explanation.

**Voice-over:**
> "Claude is built in. Explain any recommendation in plain English.
> Merge a snippet into your config without rewriting the YAML
> yourself. Every AI-generated change runs through lint, diff,
> and Squadron's existing staged rollout before it touches
> production."

## Close (82–90s)

**Visual:** Cut back to the Savings page. The hero number ticks
down: $847 → $635/month. Banner: "Saved $212/month".

**On-screen text (held):**
```
Squadron
Open-source. Self-hosted. Pays for itself.

squadron.dev   github.com/devopsmike2/squadron
```

**Voice-over:**
> "Squadron. Open-source, self-hosted, pays for itself. Get the
> code on GitHub."

---

## Production notes

- **Length:** 90 seconds is the hard limit. If a scene runs long
  in editing, cut the AI Assist Merge scene first (it's the most
  expendable; the Explain inline panel does similar work).
- **Voice:** Calm, technical, slightly dry. Not announcer-energy.
  The product speaks for itself; over-selling reads as a tell.
- **Captions:** Always-on, large, white-on-translucent-black.
  Most people watch with sound off.
- **Resolution:** 1440p minimum so YAML in the editor reads
  clean on retina.
- **Cursor:** Use a visible-cursor recorder (Screen Studio / kap /
  similar). Highlight clicks with a small ring animation.
- **Music:** None for the hero video. Optional for the social
  cuts; pick something instrumental and recede-into-background.
- **B-roll backup:** Record 2-3 minutes of footage at each stage
  so the editor has overlap. Re-record the cold open if any
  errors flash in the docker compose logs — operators notice.
- **Numbers:** Use a fleetsim run before recording so the Cost
  Insights / Savings numbers are non-trivial. The default demo
  fleet's numbers ($33/month) are fine but a $847 → $635
  reduction tells a punchier story for the close.

## Social-cut variations

Same source footage, three derived cuts:

| Audience | Length | Cuts |
|---|---|---|
| **Twitter/X hero** | 30s | Cold open + Savings hero + close |
| **Hacker News post header gif** | 6s loop | Cost Insights pan only |
| **r/devops** | 60s | Skip the AI Assist Merge; everything else |

All three reuse the original master, no separate shoots.
