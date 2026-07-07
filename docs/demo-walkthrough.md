# GA demo walkthrough

An operator-facing, live-presenter script for the 5–8 minute Squadron
Enterprise demo. It follows a single narrative: a multi-tenant control
plane that discovers a fleet, lets an AI propose a fix behind an
approval gate, cuts a real cost spike, and closes with a SOC 2-grade,
tamper-evident audit receipt an auditor can re-verify offline with zero
secrets.

For the 90-second landing-page cut-down, see
[demo-script.md](./demo-script.md). For the underlying seed scenario,
see [demo.md](./demo.md). For the enterprise build/enable framing, see
[oss-to-enterprise-migration.md](./oss-to-enterprise-migration.md) and
[deployment.md](./deployment.md).

## Setup preamble

Read this once before you present; none of it is on camera.

- **Run the enterprise binary, not OSS.** Multi-tenant and SSO are
  enterprise features — in OSS the `/api/v1/tenants`, `/api/v1/rbac`,
  and `/api/v1/sso` routes 404 and the Identity / SSO pages show an
  enterprise-feature notice instead of tabs. Build it with the private
  pack (`make build-enterprise`; see
  [oss-to-enterprise-migration.md](./oss-to-enterprise-migration.md)).
  Confirm the startup log shows `edition=squadron-enterprise`.
- **Pre-seed the state.** Run `scripts/demo-seed.sh` before you start.
  It provisions the two demo tenants (`acme`, `beta-corp`) plus the OSS
  demo scenario (demo group, baseline config, synthetic agent, the
  +312% cost spike), and lands the two seeded rollouts described below.
  The seed is idempotent; `--force` reseeds the cost spike.
- **Use a throwaway config on alternate ports.** The 24h soak owns the
  default ports `8080` (UI/API), `4320` (OpAMP), and `4317`/`4318`
  (OTLP). Do **not** reuse them. Point the demo binary at a throwaway
  config on non-colliding ports — for example UI `8090`, OpAMP `4420`,
  OTLP `4417`/`4418`. The port block in `deployment.md` (the Single VM
  `squadron.yaml`) is the template; just change the four `*_addr`
  values. All URLs below assume the UI on `http://localhost:8090`.
- **Have a terminal ready** for the finale (the chain export curl and
  the offline verifier). Build the verifier once ahead of time:
  `go build -o bin/squadron-audit-verify ./cmd/squadron-audit-verify`.
- **Set an operator token** for the curl step. Enterprise runs with
  auth on; export it as `$TOKEN` before you present so the export
  command doesn't 401 on camera.

**Total runtime target: 5–8 minutes.** The act timings below sum to
~6:30, leaving slack for questions.

## Act 1 — Fleet status (0:00–0:45)

**Open:** `http://localhost:8090/` (Dashboard / Fleet Status).

**Do / say:**

- Point at the online-agent count and the fleet health summary.
- One line: "This is the whole fleet at a glance — every collector
  Squadron controls, its config drift state, and recent activity."

**Expected UI state:** the Dashboard renders with at least the seeded
synthetic agent online (green), no error banners, and a populated
recent-activity strip.

**Why it matters:** Squadron is the control plane beside your pipeline,
not in the hot path — one screen answers "is my fleet healthy?"

## Act 2 — Multi-tenant isolation (0:45–2:00)

**Open:** `http://localhost:8090/settings/identity` (Identity — the six
enterprise tabs: Roles, Bindings, Tenants, Directory, Usage, Budgets).

**Do / say:**

- Open the **Tenants** tab. Show `acme` and `beta-corp` as separate
  isolation boundaries.
- Switch to **Roles** and **Bindings**. Explain that a role is a named
  set of scoped permissions and a binding attaches it to a principal
  (an SSO subject via `oidc:<subject>`, a token label, or the
  break-glass `bootstrap` admin).
- Open the **Directory** tab, pick a tenant, and show the
  SCIM-provisioned users/groups (groups carry an explicit role mapping
  re-read at each login).
- Optional: glance at **Budgets** (per-tenant trace-index budgets) and
  **Usage** (per-tenant chargeback) to show the isolation extends to
  cost controls.

**Expected UI state:** the six tabs render (not the "enterprise
feature" lock notice — if you see the lock, you booted OSS). Tenants
lists `acme` and `beta-corp`. Directory shows seeded users/groups for
the selected tenant.

**Why it matters:** every per-tenant query is tenant-filtered under the
enterprise wire; one tenant can never see another's agents, tokens,
budgets, or audit trail.

## Act 3 — SSO (2:00–2:45)

**Open:** `http://localhost:8090/settings/sso` (SSO — OIDC connections).

**Do / say:**

- Show the seeded OIDC connection row: issuer, client_id, per-connection
  `redirect_uri`, and the `tenant_id` it homes users into.
- The line to land: "SSO is configured per tenant — one IdP maps to one
  tenant. A user who signs in through this connection is provisioned
  into that tenant with the role their directory group maps to."
- Then open `http://localhost:8090/login` in a second tab to show the
  "Sign in with SSO" button the connection renders on the login screen.

**Not fully seedable — say this on camera or skip the redirect:** a live
OIDC round-trip needs a real identity provider. The seed can create the
connection row and render the SSO button, but clicking it redirects to a
real IdP we don't stand up for the demo. Honest alternative: show the
configured connection and the login button, and point back to the
Directory/Bindings from Act 2 as "this is where an SSO user lands." Do
not click through to a live login unless you have a real IdP wired.

**Why it matters:** enterprise identity is per-tenant and standards-based
(OIDC + SCIM), so customer IdPs plug in without Squadron minting
credentials.

## Act 4 — Agent discovery and the AI-proposed rollout (2:45–4:30)

This is the flagship moment. Do not rush it.

**Open first:** `http://localhost:8090/agents`.

- Show the discovered agents and their group membership. One line:
  "Squadron discovered these over OpAMP — no re-deploy."

**Then open:** `http://localhost:8090/rollouts`.

**Do / say:**

- Find `rlo-demo-ai-proposal`. Call out the violet **AI proposal**
  badge (the Sparkles chip) and that the rollout sits in
  `pending_approval` — it is waiting on a human.
- Expand it. Show the **AI reasoning** section and the **Evidence**
  list linking the cost spike that triggered the proposal, plus the
  config diff against the baseline.
- Click **Approve**. Watch the rollout leave `pending_approval` and
  begin staging through its canary.
- Then point at `rlo-demo-inflight`, already mid-canary, to show a
  staged rollout in motion.

**Expected UI state:** `rlo-demo-ai-proposal` shows the violet "AI
proposal" badge, a `pending_approval` state pill, an expandable AI
reasoning + Evidence panel citing the spike, and Approve/Reject
controls. After approval it advances to the first stage.
`rlo-demo-inflight` shows a canary in progress.

**Why it matters:** the AI drafts the fix and cites its evidence, but a
human holds the gate — nothing reaches production without an approval,
and every step is recorded.

## Act 5 — Cost spike to savings (4:30–5:45)

**Open:** `http://localhost:8090/cost-insights`.

**Do / say:**

- Point at the seeded cost spike: **+312%**, roughly `$400 → $1,648`
  per month. Show the attribution — which agent and which attributes
  are driving the bytes.
- One line: "The moment costs move, Squadron breaks it down by signal,
  by agent, by attribute — so you see the noisy key before the bill
  does."

**Then open:** `http://localhost:8090/savings`.

- Show the recommendations ranked by dollars saved per month. Note that
  clicking **Apply** on any of them drops you into the config editor
  with the change pre-staged — it does not roll out until reviewed and
  approved (tie this back to the gate from Act 4).

**Expected UI state:** Cost Insights shows the +312% spike under recent
spikes with attribution; Savings shows a projected monthly spend and a
ranked Quick Wins / recommendations list with per-item dollar figures.

**Why it matters:** cost control is quantified in dollars and turned
into ranked, one-click, reviewable actions — not a dashboard you have
to interpret.

## Act 6 — Compliance finale: tamper-evident audit (5:45–6:30)

The strongest close. This is the SOC 2 receipt.

**Open:** `http://localhost:8090/audit`, then the **Integrity** tab
(the three tabs are Recent activity, Access review, Integrity).

**Do / say:**

- Click **Verify fleet**. Show every tenant's hash chain coming back
  intact in one pass. Note that the verification run is itself recorded
  to the audit trail.
- Pick a tenant (e.g. `acme`) and click **Download attestation** to pull
  the sealed attestation JSON — the compliance evidence artifact.
- Now drop to the terminal for the auditor's independent check.

**Export the chain and run the offline verifier:**

```bash
# 1. Export the chain-column audit events (the raw rows + hash chain).
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8090/api/v1/audit/events?include_chain=1&format=json" \
  > /tmp/audit-chain.json

# 2. The attestation you just downloaded from the Integrity tab, e.g.
#    ~/Downloads/attestation-acme.json

# 3. Re-verify OFFLINE — zero secrets, standard library only.
bin/squadron-audit-verify \
  -export /tmp/audit-chain.json \
  -attestation ~/Downloads/attestation-acme.json \
  -tenant acme
```

**Expected terminal output** (the lines that matter):

```
Squadron offline attestation verifier (ADR 0027)
  ...
  chain:              OK (covers from seq <n>)
  recomputed head:    seq=<n> hash=<...>
  attested head:      seq=<n> hash=<...>
  tenant cross-check: OK (acme)
  head match:         PASS (recomputed tip matches the attestation)
```

**Say:** "The auditor didn't need our database, our keys, or our
running server. They re-hashed the exported rows with an open-source
binary and confirmed the chain tip matches what Squadron attested. That
is the tamper-evidence receipt — chain intact, head matches, zero
secrets."

**Expected UI state:** Integrity tab shows all tenants verified OK after
"Verify fleet"; the attestation downloads as a JSON file; the CLI prints
`chain: OK` and `head match: PASS`.

**Why it matters:** independent, offline verifiability is what turns an
audit log into audit *evidence* — the SOC 2 auditor's own check, not a
claim they have to trust.

## Recovery notes

- **Rollout doesn't show as `pending_approval`:** you likely booted OSS
  or forgot the seed. Confirm `edition=squadron-enterprise` in the
  startup log and re-run `scripts/demo-seed.sh --force`.
- **Identity/SSO pages show the lock notice:** same cause — you're on the
  OSS binary. Restart against the enterprise build.
- **Export curl 401s:** `$TOKEN` is unset or unscoped. Auth is on under
  enterprise; export a valid operator token first.
- **Port conflict on boot:** the soak owns `8080/4320/4317/4318`. Change
  the four `*_addr` values in the throwaway config.
