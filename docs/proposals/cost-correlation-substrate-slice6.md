# Cost-Correlation Substrate — slice 6 chunk 1 (substrate + guardrails)

Status: chunk 1 shipping in v0.89.183 (#825 Stream 222).

This chunk ships ONLY the money-touching plumbing — the read-only
cost-query interface, the per-account spend-bounding budget
governor, and the per-call-cost accounting — with NO per-cloud
billing API integration. Per-cloud chunks fan out from this
substrate in later releases, exactly as the per-cloud
MetricQuerier chunks fanned out from the cold-start substrate.
Isolating the spend-governing plumbing in its own reviewable
release is deliberate.

## 1. Guardrails (hard, non-negotiable)

These are enforced in code and restated here so the runbook
carries them upfront.

1. **Read-only only.** The cost substrate exposes exactly one
   verb — a cost *read*. It MUST NOT be used to issue any
   provisioning / mutating call (CreateOrder, ConfigureService,
   ChangeAccountTier, purchase, trial sign-up, or any call that
   incurs cost on the user's behalf). The `CostQuerier` interface
   has no write method and per-cloud implementations are
   restricted to the providers' cost *reporting* APIs.

2. **Bounded worst-case spend.** Some cost-reporting APIs charge
   per request (see §3). Every cost call is gated by a
   `CostBudgetGovernor` that tracks cumulative spend per connected
   account against a ceiling (default **$1.00 / 30-day rolling
   window / account**) and rejects any call that would exceed it
   with `ErrCostBudgetExceeded`. A scan that hits the ceiling
   degrades gracefully: cost fields stay at the not-measured
   sentinel, never blocking the rest of the scan.

3. **Report, don't editorialize.** Cost data surfaces in
   operator-facing recommendations as a plain figure ("this DLQ
   is costing ~$X/mo; here's how to drain it"). The substrate and
   the downstream proposer MUST NOT inflate, moralize, or
   editorialize about whether a cost is high or low — just report
   the number and the actionable next step.

## 2. Money representation

All money is represented as **micro-USD (`int64`)** — millionths
of a US dollar — never float. `$0.01 = 10_000` micro-USD;
`$1.00 = 1_000_000` micro-USD. Integer money avoids the rounding
drift that makes float dollars a known foot-gun in spend
accounting (where the governor's ceiling check must be exact).

## 3. Per-call-cost surface (HONEST, UPFRONT)

This table is the load-bearing honesty surface. It is the per-call
price of the read-only cost-reporting API each per-cloud chunk
will call. Per-cloud chunks pin these as package constants and
pass them to the governor; chunk 1 documents them so the cost is
visible before any integration ships.

| Cloud | Read-only cost API | Per-call cost (approx) | Notes |
|-------|--------------------|------------------------|-------|
| AWS | Cost Explorer `GetCostAndUsage` | **~$0.01 / request** (10_000 micro-USD) | The one that actually charges per call. The budget governor matters most here. |
| GCP | Cloud Billing / BigQuery billing export query | **$0 for the Billing API**; BigQuery export queries bill per TB scanned (≈ $0; cents on small billing tables) | Prefer the free Billing Catalog + exported-billing path; avoid unbounded BigQuery scans. |
| Azure | Cost Management `Query` | **$0** per call (subject to ARM throttling, not per-call billing) | Rate-limited, not per-call-priced. |
| OCI | Usage/Cost reports (object-storage cost-report objects) | **$0** per read beyond negligible object-storage GET | Reports are pre-generated objects; reads are effectively free. |

The numbers above are approximate and provider-pricing-dependent;
per-cloud chunks MUST confirm the current published price before
relying on it, and the governor ceiling is the backstop regardless
of the exact figure. AWS Cost Explorer is the only surface where
per-call cost is material at the default scan cadence — at
~$0.01/call and a $1/mo ceiling, the governor permits ~100
Cost Explorer calls per account per month (≈ 3/day at a daily
cadence), which is the budget the per-cloud AWS chunk must design
its query batching against.

## 4. CostBudgetGovernor

Thread-safe per-account spend governor:

- `Authorize(perCallMicroUSD int64) error` — called immediately
  before every cost API request. Resets the spend window if the
  rolling window has elapsed, then either records the spend and
  returns nil, or returns `ErrCostBudgetExceeded` if the call
  would push cumulative spend over the ceiling (the spend is NOT
  recorded on rejection).
- `Spent() / Remaining() int64` — observability for the runbook +
  tests.
- Construction pins the ceiling (default
  `DefaultMonthlyCostBudgetMicroUSD`) and the window
  (`DefaultCostBudgetWindow`, 30 days). A per-account governor is
  held by the per-cloud Scanner alongside the existing rate
  limiters.

A free-API call (per-call cost 0) is always authorized — the
governor only constrains the surfaces that actually charge.

## 5. CostQuerier interface

Mirrors the `MetricQuerier` shape (read-only, per-cloud Scanner
method, empty-result semantics). Chunk 1 ships the interface + the
`ErrCostNotImplemented` skeleton sentinel; per-cloud bodies land
in later chunks.

```
QueryCost(ctx, resourceID, dimension, window) (CostResult, error)
```

`CostResult` carries `AmountMicroUSD int64`, `Currency`,
`Granularity`, `Window`, `Covered bool` (the not-measured vs
real-zero distinction — same contract as the metric substrate's
SampleCount), and `ObservedAt`.

## 6. Chunk map

- **Chunk 1 (this slice, v0.89.183): substrate + guardrails.**
  `CostQuerier` interface, `CostResult`, `CostBudgetGovernor`,
  sentinels, per-call-cost surface table. NO per-cloud integration.
- **Chunk 2 (v0.89.184): AWS Cost Explorer `QueryCost` body.**
  Reads UnblendedCost for a SERVICE dimension over the window via
  GetCostAndUsage, gated through the CostBudgetGovernor
  (~$0.01/call). Refuses to issue a charged call without BOTH a
  client and a governor; over-budget returns
  `ErrCostBudgetExceeded` as a graceful skip. NOT wired into any
  scan yet — no charged request fires during a scan until the
  enrichment chunk enables it. Money parsed to integer micro-USD
  (no float).
- Chunk 3+: remaining per-cloud `QueryCost` bodies (GCP / Azure /
  OCI — all effectively free per call), then the cost-correlation
  enrichment that joins cost against the poison-rate / lag / DLQ
  axes ("this DLQ costs ~$X/mo") and wires the production Cost
  Explorer client + governor into the scan flow.

## 7. IAM

Per-cloud chunks require read-only cost/billing roles (AWS
`ce:GetCostAndUsage`, GCP `billing.resourceCosts.get` / BigQuery
read, Azure `Microsoft.CostManagement/query/read`, OCI usage-report
read). All read-only; no write/provisioning scope. Documented per
chunk.

## 8. Acceptance (chunk 1)

1. `CostBudgetGovernor.Authorize` records spend under the ceiling,
   rejects over it with `ErrCostBudgetExceeded` (without recording
   the rejected spend), and authorizes free ($0) calls
   unconditionally.
2. The governor resets cumulative spend when the rolling window
   elapses.
3. The governor is concurrency-safe (no double-spend under
   parallel Authorize calls).
4. `CostQuerier` skeleton returns `ErrCostNotImplemented`;
   sentinels are `errors.Is`-comparable.
