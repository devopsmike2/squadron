# Post 10: What's in your fleet that you're not observing?

**Pillar:** Dynamic discovery
**Tag at publish:** v0.85.0
**Visual evidence:** A screenshot of the `/discovery/aws`
Inventory tab on the live deployment at the v0.85.0 tag, taken
right after a real scan completed. The tab shows the EC2 and
Lambda groupings populated with rows from the connected account.
The OTel detection column shows a mix of green check (instrumented)
and gray X (uninstrumented) badges so the coverage gap is visible
at a glance. The instrumented / uninstrumented counts in the page
header are non-zero.
**Hashtags:** #OpenTelemetry #SRE
**Target word count:** 200-400

## Draft

What is in your fleet that you are not observing?

Most platform teams cannot answer this in under a week. The
honest answer requires a Jira ticket, a spreadsheet, a list of
accounts from someone in finance, a tag-policy audit, and a
walkthrough with whoever maintains the Terraform modules. By
the time the answer is in hand, the fleet has drifted.

v0.85.0 answers the question natively for AWS.

Connect an account via read-only IAM assume-role. Pick a region.
Click Scan. The scanner paginates `ec2:DescribeInstances` and
`lambda:ListFunctions`, applies an OTel detection heuristic per
resource — any EC2 tag key beginning with `otel`, any Lambda
layer ARN containing `otel` or `opentelemetry` — and renders the
result on the Inventory tab grouped by service. The instrumented
and uninstrumented counts are right there in the page header.

That is the first answer. It is small (slice 1 is EC2 and
Lambda, one region per scan), but it is concrete. The SRE who
walks past the desk on Monday morning, opens the laptop, clicks
`Connect new account`, follows the five-step wizard, and runs a
scan can give a number to their manager by lunch. The number was
not knowable without an audit project a month ago. It is now.

The detection heuristic is intentionally conservative — a tag
check and a layer ARN check, both case-insensitive — because
slice 1 is the answer for the resources Squadron can see, not the
last word on whether a process is emitting telemetry. False
positives are the failure mode the operator can verify by hand
in five minutes; false negatives become recommendations on the
next tab. The proposer reasons about the gap.

Slices 2 through 6 expand the surface (RDS, S3, ALB; multi-
region; GCP; Azure; on-prem). The question is the same. The
answer keeps getting bigger.

Repo at the v0.85.0 tag. The inventory tab is at
`/discovery/aws#inventory`.

#OpenTelemetry #SRE

## Visual asset spec

- **Filename:** `assets/post-10-discovery-aws-inventory-tab.png`
- **Surface:** the `/discovery/aws` Inventory tab on the live
  deployment at the v0.85.0 tag. Run a scan against a demo
  account that has a deliberate mix of OTel-tagged and
  untagged EC2 instances and a mix of Lambda functions with and
  without an `otel` layer ARN. The mix is the point — a 100%
  instrumented fleet is the boring screenshot.
- **What must be visible in the crop:** the tab header with the
  instrumented and uninstrumented count chips; both the EC2 and
  Lambda groupings, each with at least three rows; the OTel
  detection column rendering a visible mix of green check and
  gray X badges. The last-scan timestamp at the top is the proof
  the data is fresh.
- **Annotations:** one small marker on a row showing a gray X
  badge, with the caption "uninstrumented — feeds the
  Recommendations tab" added in post-processing. One marker on
  the green-check side with the caption "OTel layer detected on
  ARN". The two markers make the heuristic visible without
  spelling out the code path.
- **Crop:** include the route in the browser address bar so the
  reader can verify the surface.

## Anti-pattern guard

Resists **the hype follow-up** from linkedin-rollout.md
"Anti-patterns to avoid". The pull, right after a shipping
moment like v0.85.0, is to lead with "Squadron now scans your
entire AWS fleet across every region in seconds." The post
instead narrows the claim to what slice 1 actually does (EC2 +
Lambda, one region, one account), names the heuristic (tag and
layer ARN, case-insensitive), and is honest about the false-
positive failure mode. The reader who tests it on Monday gets the
behavior the post described. That is what builds trust over the
drumbeat.
