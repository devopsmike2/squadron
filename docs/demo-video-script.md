# GA demo — screencast / voice-over script

A narration-ready script for a 5–8 minute Squadron Enterprise screencast.
Michael reads the **VOICE-OVER** lines while recording the **ON SCREEN**
actions. It mirrors, scene for scene, the live
[demo-walkthrough.md](./demo-walkthrough.md) — record with the walkthrough
open on a second monitor so the on-screen actions stay in lockstep.

For the 90-second landing-page hero cut, use
[demo-script.md](./demo-script.md) as the short companion — it is the
cut-down of this same story (cold open → savings → close) and reuses the
same footage conventions (visible-cursor recorder, always-on captions,
1440p minimum). This script is the long-form, compliance-audience cut.

**Tone:** crisp, benefit-led, non-hyperbolic. The audience is technical
and compliance-minded; overselling reads as a tell. Keep each scene's VO
to a few sentences. Land the tamper-evident verification as the close.

**Preconditions** (see the walkthrough's Setup preamble): enterprise
binary (`edition=squadron-enterprise`), state pre-seeded via
`scripts/demo-seed.sh` (tenants `acme` + `beta-corp`, the two seeded
rollouts, the +312% spike), running on a throwaway config on alternate
ports (UI `http://localhost:8090`) so it never touches the soak's
`8080/4320/4317/4318`. Build the offline verifier before recording.

---

## Scene 1 — Fleet status (0:00–0:40)

**ON SCREEN:** Open on the Dashboard at `http://localhost:8090/`. Cursor
traces the online-agent count, then the fleet health / recent-activity
strip.

**VOICE-OVER:**
> This is Squadron — an OpenTelemetry control plane. One screen shows the
> whole fleet: every collector it controls, their config drift state, and
> what changed recently. Squadron sits beside your pipeline, not inside
> it — your telemetry never flows through it, only control.

---

## Scene 2 — Multi-tenant isolation (0:40–1:55)

**ON SCREEN:** Navigate to `/settings/identity`. Click the **Tenants**
tab (show `acme` and `beta-corp`). Click through **Roles** and
**Bindings**. Open **Directory**, pick a tenant, show the SCIM users and
groups.

**VOICE-OVER:**
> Enterprise Squadron is multi-tenant. Acme and Beta-Corp are separate
> isolation boundaries — separate agents, tokens, budgets, and audit
> trails. Access is role-based: a role is a set of scoped permissions,
> bound to a principal — an SSO identity, a token, or a break-glass
> admin. Users and groups sync in over SCIM, and a group carries the role
> its members inherit at login.

---

## Scene 3 — SSO (1:55–2:40)

**ON SCREEN:** Navigate to `/settings/sso`. Show the seeded OIDC
connection: issuer, client ID, redirect URI, and the tenant it homes
users into. Open `/login` in a second tab to show the "Sign in with SSO"
button.

**VOICE-OVER:**
> Single sign-on is configured per tenant — one identity provider maps to
> one tenant. A user who signs in here is provisioned into that tenant
> with the role their directory group grants. It's standard OIDC and
> SCIM, so a customer's existing IdP plugs straight in.

**NOTE — not fully seedable:** a live OIDC redirect needs a real IdP,
which the demo does not stand up. On camera, show the configured
connection and the login button, and state plainly that completing a
sign-in requires the customer's own provider. Do not click through to a
live login. If you have a real IdP wired for the recording, you may film
the redirect; otherwise cut here and let the Directory view from Scene 2
stand in for "where an SSO user lands."

---

## Scene 4 — Agent discovery and the AI-proposed rollout (2:40–4:25)

This is the flagship scene. Slow down; let the approval gate breathe.

**ON SCREEN:** Open `/agents`, pan the discovered agents and their
groups. Navigate to `/rollouts`. Find `rlo-demo-ai-proposal` — highlight
the violet "AI proposal" badge and the `pending_approval` state. Expand
it to reveal the AI reasoning, the Evidence list citing the cost spike,
and the config diff. Click **Approve**; watch it begin staging. Then
point at `rlo-demo-inflight` mid-canary.

**VOICE-OVER:**
> Squadron discovered these collectors over OpAMP — no re-deploy. And
> here's the part that matters: this rollout was drafted by the AI
> proposer, off a cost spike, and it's parked at "pending approval." It
> shows its reasoning and cites the exact evidence it acted on. A human
> approves — and only then does it stage through the canary. The AI
> proposes; a person decides; every step is recorded.

---

## Scene 5 — Cost spike to savings (4:25–5:40)

**ON SCREEN:** Navigate to `/cost-insights`. Highlight the +312% spike
(roughly $400 to $1,648 a month) and the attribution — the driving agent
and attributes. Navigate to `/savings`; show recommendations ranked by
dollars saved, and note that **Apply** stages the change in the editor.

**VOICE-OVER:**
> The moment costs move, Squadron breaks the spike down — by signal, by
> agent, by attribute — so you see the noisy key, not just a bigger bill.
> On Savings, the fixes are ranked by the dollars they'd save. Apply one
> and you land in the config editor with the change staged — and it still
> goes through the same approval gate before it touches production.

---

## Scene 6 — Compliance finale: tamper-evident audit (5:40–6:30)

The mic-drop. Do not undersell the offline check — it is the strongest
claim in the demo.

**ON SCREEN:** Navigate to `/audit`, open the **Integrity** tab. Click
**Verify fleet**; show all tenants intact. Pick `acme`, click **Download
attestation**. Cut to a terminal and run the export curl, then the
offline verifier. Let the `chain: OK` and `head match: PASS` lines hold
on screen.

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8090/api/v1/audit/events?include_chain=1&format=json" \
  > /tmp/audit-chain.json

bin/squadron-audit-verify \
  -export /tmp/audit-chain.json \
  -attestation ~/Downloads/attestation-acme.json \
  -tenant acme
```

**VOICE-OVER:**
> Every change you just saw is in a tamper-evident audit log. One click
> re-verifies the hash chain across every tenant, and seals an
> attestation. Now watch the auditor's independent check: I export the
> chain, and I run an open-source verifier — no database, no keys, no
> running server. It re-hashes the rows and confirms the chain tip
> matches what Squadron attested. Chain intact. Head matches. Zero
> secrets. That's the receipt a SOC 2 auditor can run themselves.

**Hold the final frame** on `head match: PASS` for a beat before cutting.

---

## Production notes

- **Match the walkthrough.** Keep [demo-walkthrough.md](./demo-walkthrough.md)
  open while recording so on-screen actions and the "expected UI state"
  callouts stay accurate.
- **Captions on, 1440p minimum**, visible-cursor recorder — same as the
  90-second script's production notes in [demo-script.md](./demo-script.md).
- **Record overlap.** Grab a few extra seconds of footage at each scene
  boundary so the editor has room to cut.
- **If a scene runs long**, Scene 3 (SSO) is the most compressible —
  the configured-connection shot alone carries the point. Never cut
  Scene 4 (the approval gate) or Scene 6 (the offline verification);
  they are the demo's two load-bearing moments.
- **Numbers.** The seed's +312% spike ($400 → $1,648) is the on-screen
  figure; keep the VO consistent with whatever the seeded Savings page
  actually shows on the day.
