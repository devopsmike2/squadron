# ADOT / third-party version-pinned ARN freshness — design

## Problem

The discovery proposer emits version-pinned third-party identifiers — most
visibly the **ADOT Lambda layer ARN** — free-form from the LLM's training
data, so they are frozen at the model's training cutoff and go stale. A
real validated PR (scan db91c23) carried
`arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-nodejs-amd64-ver-1-18-1:4`,
which resolves but dates to **2024-07 (ADOT JS v1.18.1)** — ~2 years stale.
Two failure modes for a first user who merges it: (a) an outdated ADOT
build, or (b) for a region where that exact layer/version was never
published, an **invalid** ARN.

The same class affects other version-pinned values the proposer emits free
form: the EC2 ADOT collector version and the EKS ADOT add-on version.

## Interim (shipped, v0.89.218)

Honest framing only: the prompt now forbids presenting the layer ARN as
authoritative. The model must emit a `# VERIFY: ADOT layer version may be
stale …` annotation and tell the operator to confirm the current ARN
before applying. This does not fix staleness — it makes it visible so a
user does not merge a stale ARN unknowingly.

## Durable answer: resolve the current ARN at scan time

The scanner already runs in the operator's account via assume-role and
knows each function's runtime, architecture, and region. It should resolve
the **current** ADOT layer ARN there and attach it to the candidate; the
proposer then uses the resolved value verbatim instead of guessing.

### Data source options

1. **AWS public SSM parameters** (`/aws/service/...`) — if AWS publishes
   ADOT layer ARNs as public SSM parameters (as it does for some managed
   layers), the scanner reads the latest ARN per runtime/arch in the
   function's region. Always current, zero maintenance. **UNVERIFIED:** a
   probe this pass was blocked — the test IAM principal lacks
   `ssm:GetParametersByPath` on the `/aws/` namespace, and web access to
   the ADOT docs timed out. **Action to unblock:** confirm the ADOT layer
   SSM parameter path exists (AWS docs / a principal with read on
   `/aws/service/*`); if it does, add read-only `ssm:GetParametersByPath`
   on `/aws/service/*` to the SquadronDiscovery template (39 → 40 actions)
   and build the reader.
2. **Maintained embedded ARN table** — a Go data table of current ADOT
   layer ARNs per (runtime-family, arch, region) with an explicit `as-of`
   date and a refresh procedure. Deterministic, no runtime dependency, but
   needs periodic maintenance and must handle per-region version-sequence
   differences (the `:N` suffix is not guaranteed identical across
   regions). A staleness test can flag when the `as-of` date is too old.
3. **Propose-time fetch** from the ADOT docs/GitHub releases — rejected:
   network dependency + brittle parsing on the request hot path.

**Recommendation:** option 1 if the SSM path is confirmed (cleanest,
always-current); option 2 as the maintained fallback if it is not.

### Substrate shape

```
type LayerARNResolver interface {
    // Resolve returns the current ADOT layer ARN for the runtime+arch in
    // region, plus the as-of time, or ok=false when unknown.
    Resolve(runtime, arch, region string) (arn string, asOf time.Time, ok bool)
}
```

- The AWS Lambda scanner calls it per uninstrumented function and sets a
  new `RecommendedLayerARN` (+ `RecommendedLayerAsOf`) on the candidate.
- The proposer user-message renders the resolved ARN; the prompt says
  "use the provided RecommendedLayerARN verbatim when present; only fall
  back to your own value (with the VERIFY annotation) when it is absent."
- Cold-start parity preserved: when the resolver returns nothing the
  rendered message is byte-identical to today, and the model uses the
  interim verify-this path.

### Why this is its own arc (not bundled with the per-runtime fixes)

The v0.89.213–217 fixes were prompt-only corrections of values that are
*constants* (exec wrapper per runtime, cert-manager prerequisite, log
delivery principal). The ARN is *not* a constant — it moves with upstream
releases — so the honest fix needs a live/maintained data source, IAM
surface (SSM read), scanner plumbing, and a refresh story. Bundling it
into a per-runtime correctness release would tangle a data-pipeline change
with one-line prompt fixes.

## Status

Blocked on confirming the data source (option 1 SSM path or option 2 table
seed). Interim honest-framing fix shipped. Ready to implement once the
source is confirmed.
