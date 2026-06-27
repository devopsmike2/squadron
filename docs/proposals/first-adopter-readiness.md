# First-adopter readiness — honest gap analysis

Status: living notes. Written for Michael to read, not a spec. The goal is an
honest "what would actually break or disappoint the first real OSS adopter who
tries the cloud-discovery path," grounded in what was verified end-to-end this
week vs. what is inferred from the code. Confidence is flagged per item.

## What's verified working (high confidence — exercised e2e this week)

- **Cloud discovery, three of four clouds.** AWS, GCP, and Azure connection
  wizards → live scan against real terraform-provisioned infra → inventory
  verified against the known oracle (instrumented vs uninstrumented instances).
  Test repos exist (`squadron-test-{azure,gcp,oci}-terraform`).
- **Recommendations against the real LLM.** AWS (EC2 Graviton, Windows-bare)
  and the async job + poll flow produce shape-correct, deployable-ish Terraform.
  The sync-call HTTP-timeout bug is fixed (async job store, v0.89.209+).
- **First-run onboarding.** One-port `8080`, prod-like default compose + a dev
  variant, a default dev `SQUADRON_SECRETS_KEY`, AWS-creds mount documented.
- **First-user 500 sweep.** The incidents-handler nil-store class of
  eager-wiring bugs was found and fixed; a GET-endpoint sweep ran.
- **Demo mode (new, v0.89.239-241).** A first-user with NO cloud account and
  NO API key can click "Try the demo" on the AWS page and get a populated
  Inventory tab + seeded recommendations. Removes the single biggest trial
  barrier. AWS-only for now.

## Known backlog, already filed (high confidence — honest deferrals)

- **OCI is the one cloud not yet e2e-validated.** Connector + scanner +
  recommendations are implemented and unit-tested, but a live scan against real
  OCI infra has not been run (blocked on tenancy credentials). Until then, treat
  OCI discovery as "implemented, unproven."
- **Serverless cold-start detection needs a paid add-on — now recommended
  (v0.89.258).** AWS Lambda needs Lambda Insights and Azure Functions needs
  Application Insights for a real cold-start / error signal (#152 / #153).
  Squadron now *recommends enabling* the add-on (proposer kinds
  `lambda-insights-enable` / `azfunc-appinsights-enable`) with a why + cost
  explanation + IaC, rather than silently showing "needs Insights". Detection
  still can't fire until the operator enables it — recommending it is the
  unblock. AWS slice also offers the cheaper Logs metric-filter alternative.
- **Poison-rate depth signals deferred.** AWS SQS via DLQ depth (#156) and OCI
  Queue via `deadLetterQueueDeliveryCount` (#159) are designed but not built.
- **ADOT Lambda layer ARN freshness (#109).** The proposer emits layer ARNs
  free-form (frozen at the model's training time). A durable resolver (SSM
  public-parameter lookup) is designed but blocked on confirming the SSM path
  + adding an IAM action.

## Gaps to weigh before calling it adopter-ready (medium confidence — partly inferred from code/comments, worth confirming)

- **Discovery scans: persistence shipped (v0.89.250), scheduling still
  pending.** As of continuous-discovery slice 1, AWS scans are persisted
  (whole-scan records + `GET .../scans` history + `GET .../scans/:scanID`
  detail). As of slice 2 (v0.89.251) ALL FOUR clouds persist + expose scan
  history (`GET .../scans` + `.../scans/:scanID`). What remains for a true
  "continuous" story: (a) scheduled re-scans now EXIST as of slice 3a
  (v0.89.252) — opt-in via SQUADRON_DISCOVERY_SCAN_INTERVAL, all four clouds (slice 3b,
  v0.89.254), default off; (b) the on-demand POST scan is
  still synchronous (async-job HTTP API is separate); (c) drift shipped (slice 4, v0.89.253) — GET .../connections/:id/drift diffs
  the latest two scans (added/removed/instrumentation-flipped) on all four
  clouds. Remaining: drift de-dup/digest + routing drift events to a notification channel (audit events already forward to SIEM).
- **Single region per connection (slice 1).** The credstore + scanner are
  multi-region-shaped but ship single-entry region lists. A multi-region account
  needs one connection per region. CONFIRM current state.
- **Demo mode is AWS-only.** The other three clouds' empty states don't yet
  offer it. Cheap follow-up if the demo lands well: the demo connection is
  AWS-shaped, so GCP/Azure/OCI parity needs provider-shaped demo connections.
- **AI recommendations require `ANTHROPIC_API_KEY`.** Expected for a BYO-LLM
  product, and demo mode now covers the no-key trial — but worth stating plainly
  in onboarding so a keyless adopter isn't surprised when the live (non-demo)
  Recommendations button 503s.
- **AuthZ maturity.** Routes use scope middleware (`agents:read` / `:write`),
  but I have NOT audited the auth story for a multi-user / multi-tenant adopter
  (token issuance, RBAC granularity, default-open vs default-closed). CONFIRM
  before any shared deployment.

## Suggested sequencing for "first real adopter"

1. Close OCI e2e (needs Michael's credentials) so all four clouds are proven.
2. Confirm the scan persistence / scheduling story and either build it or set
   expectations in docs — this is the biggest "is it actually continuous?" gap.
3. Decide on the cold-start / poison-rate data-source items (#152/#153/#156/
   #159) — these are the recurring "honest-but-limited" detection gaps.
4. Extend demo mode to the other clouds if AWS demo telemetry shows uptake.

Nothing here is a blocker for a curious adopter kicking the tires (demo mode +
AWS/GCP/Azure discovery all work today). The items above are about the gap
between "impressive demo" and "I'd run this against my production account
continuously."
