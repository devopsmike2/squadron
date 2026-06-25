# Discovery AWS — first-time setup

This is the operator-facing runbook for connecting a self-hosted
Squadron deployment to a real AWS account for the first time. It
covers the IAM bootstrap (user + role + trust policy + permissions
policy + access key + credentials file) plus the Squadron-side
restart and the in-product wizard walk.

If you're reading this as a Squadron developer wondering "wait, the
wizard already walks the operator through this?" — yes, the wizard
does the in-product steps, but it can't do the IAM clicks for the
operator. This runbook fills the gap on the AWS side and frames the
wizard inside the larger flow.

For a faster first test in a throwaway free-tier account, the
walkthrough takes roughly 15 minutes. For a production deployment
with tighter IAM scoping, budget 30 minutes plus your IAM review
cycle.

## What we're building

Three IAM objects in your AWS account:

1. **A dedicated IAM user (`squadron-bot`)** that owns the long-lived
   access key Squadron will read from `~/.aws/credentials`. This user
   has one permission and one only: call `sts:AssumeRole` against the
   discovery role below.
2. **A discovery IAM role (`SquadronDiscovery`)** that holds the
   read-only EC2 + Lambda + RDS + S3 + ELBv2 (ALB / NLB) describe
   permissions Squadron's scanner actually exercises. The trust
   policy on this role allows `squadron-bot` to assume it (gated by
   an `sts:ExternalId` condition).
3. **An inline `AssumeSquadronDiscovery` policy** on the
   `squadron-bot` user that authorizes the assume-role call. Scoped
   to the discovery role's ARN — not a wildcard.

This split is the AWS-recommended self-hosted bootstrap pattern. The
long-lived credentials Squadron holds belong to a principal that can
do exactly one thing (call `sts:AssumeRole`); everything else flows
through short-lived STS tokens minted on each scan.

## Prerequisites

- An AWS account where you can create IAM users and roles. Free-tier
  is fine for first-test.
- Your Squadron deployment running and reachable at
  `http://localhost:8090` (or wherever you've configured it).
- A terminal where you can write to `~/.aws/credentials`.

## Step 1 — Open the wizard

In the Squadron UI, open the **Discovery → AWS** page and click
**Connect new account**. The wizard starts at step 1 (Enter your AWS
account ID).

Enter your 12-digit AWS account ID and click Next. **Do not close
the wizard until you've finished step 6 (Save)** — the wizard holds
a per-deployment ExternalId in browser state. Closing or refreshing
mid-flow regenerates the ExternalId and forces you to update the
trust policy on the role to match.

If you do lose the ExternalId mid-flow, use the **Advanced: resume
with existing ExternalId** disclosure on the trust-policy step to
paste your existing UUID instead of accepting the regenerated one.

The wizard now sits on step 2 (the trust policy display). Leave the
tab open and switch to AWS.

## Step 2 — Create the `squadron-bot` IAM user

In AWS console, open IAM → Users → **Create user**.

- User name: `squadron-bot`
- **Uncheck** "Provide user access to the AWS Management Console" —
  this user is programmatic only.
- Permissions: **skip** for now. We attach the assume-role policy
  after the discovery role exists, because the policy needs to
  reference the discovery role's ARN.
- Tags: optional.

Click Next → Next → Create user.

After creation, AWS shows you the user's ARN. It looks like:

```
arn:aws:iam::<ACCOUNT_ID>:user/squadron-bot
```

Note this — the next step needs it.

## Step 3 — Create the `SquadronDiscovery` IAM role

Back in IAM, open Roles → **Create role**. Select
**Custom trust policy**.

### 3a. Trust policy

Copy the trust policy from the Squadron wizard's step 2 (use the
**Copy trust policy** button — this gets you the version with the
correct `<ACCOUNT_ID>` substitution and ExternalId).

By default, the wizard's trust policy uses the AWS account root as
the principal:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::<ACCOUNT_ID>:root"
      },
      "Action": "sts:AssumeRole",
      "Condition": {
        "StringEquals": {
          "sts:ExternalId": "<wizard-generated-UUID>"
        }
      }
    }
  ]
}
```

This means: any IAM identity in your account that has explicit
`sts:AssumeRole` permission on this role (and passes the ExternalId)
can assume it. Combined with the inline policy you'll add to
`squadron-bot` in step 5, this effectively scopes the assume-role to
`squadron-bot` alone.

If you want tighter scoping at the trust-policy layer, click the
wizard's **Advanced: scope to a specific IAM identity** disclosure
on step 2 and paste your IAM user's ARN
(`arn:aws:iam::<ACCOUNT_ID>:user/squadron-bot`). The trust policy
will then only allow that exact identity.

Paste the trust policy into the AWS Custom trust policy editor and
click **Next**.

### 3b. Permissions

On the Add permissions page, **skip** selecting any managed policy
— the inline policy we'll add after role creation is the cleaner
fit for a single-purpose role like this.

Click **Next**.

### 3c. Name and create

- Role name: `SquadronDiscovery` (exact case — Squadron's audit
  events expect this string)
- Description: anything memorable, e.g. "Read-only discovery role
  for Squadron. Assumed by squadron-bot. Slice 1+2+3a
  EC2/Lambda/RDS/S3/ALB."

Click **Create role**.

AWS lands you on the role's detail page. You'll see a warning that
the role has no permissions — that's expected.

### 3d. Attach the permissions policy

On the role's detail page, go to the **Permissions** tab → **Add
permissions** → **Create inline policy**. Switch to the JSON editor
and paste the policy from the Squadron wizard's step 3 (the new
permissions-policy step added in v0.87.1).

For reference, the v0.89.10 wizard's permissions policy is:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeInstanceStatus",
        "ec2:DescribeRegions",
        "ec2:DescribeTags",
        "lambda:ListFunctions",
        "lambda:GetFunction",
        "lambda:GetFunctionConfiguration",
        "lambda:ListTags",
        "rds:DescribeDBInstances",
        "s3:ListAllMyBuckets",
        "s3:GetBucketLocation",
        "s3:GetBucketLogging",
        "s3:GetBucketTagging",
        "s3:GetBucketRequestPayment",
        "elasticloadbalancing:DescribeLoadBalancers",
        "elasticloadbalancing:DescribeLoadBalancerAttributes",
        "elasticloadbalancing:DescribeTags",
        "eks:ListClusters",
        "eks:DescribeCluster",
        "eks:ListAddons",
        "eks:DescribeAddon",
        "eks:ListNodegroups",
        "eks:ListFargateProfiles",
        "dynamodb:ListTables",
        "dynamodb:DescribeTable",
        "dynamodb:DescribeContributorInsights",
        "dynamodb:ListTagsOfResource",
        "ecs:ListClusters",
        "ecs:DescribeClusters",
        "ecs:ListTagsForResource",
        "sqs:ListQueues",
        "sqs:GetQueueAttributes",
        "sns:ListTopics",
        "sns:GetTopicAttributes",
        "events:ListEventBuses",
        "events:ListRules",
        "events:ListTargetsByRule",
        "states:ListStateMachines",
        "states:DescribeStateMachine"
      ],
      "Resource": "*"
    }
  ]
}
```

39 actions total: 4 EC2 + 4 Lambda + 1 RDS + 5 S3 + 3 ELBv2 + 6 EKS + 4 DynamoDB + 3 ECS + 2 SQS + 2 SNS + 3 EventBridge + 2 Step Functions.
All Describe/List/Get; no write actions. Click **Next**. Policy name:
`SquadronDiscoveryReadOnly`. Click **Create policy**.

The role is now ready.

## Step 4 — Attach the AssumeRole policy to `squadron-bot`

Back to IAM → Users → `squadron-bot`. Permissions tab → **Add
permissions** → **Create inline policy** → JSON editor.

Paste:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "sts:AssumeRole",
      "Resource": "arn:aws:iam::<ACCOUNT_ID>:role/SquadronDiscovery"
    }
  ]
}
```

Replace `<ACCOUNT_ID>` with your 12-digit account ID. The Resource
must point at the exact role ARN — not a wildcard. This is the
authoritative boundary on what `squadron-bot` can do beyond its own
basic identity operations.

Click Next. Policy name: `AssumeSquadronDiscovery`. Create.

## Step 5 — Generate the access key

On the `squadron-bot` user page, go to the **Security credentials**
tab → **Create access key**.

- Use case: pick **Other**. AWS will warn it recommends an
  alternative; for self-hosted Squadron, "Other" is the correct
  pick.
- Description tag: optional, e.g. `squadron-localhost-bringup`.

Click Create access key. AWS shows you the **access key ID** and
**secret access key**. **Click Show** on the secret access key to
reveal it.

**Do not paste these values into any chat, message thread, or shared
log.** AWS shows the secret access key exactly once.

## Step 6 — Configure Squadron's credentials file

In your terminal:

```bash
mkdir -p ~/.aws
cat > ~/.aws/credentials <<'EOF'
[squadron-bot]
aws_access_key_id = REPLACE_ME
aws_secret_access_key = REPLACE_ME
EOF

cat > ~/.aws/config <<'EOF'
[profile squadron-bot]
region = us-east-1
EOF

chmod 600 ~/.aws/credentials ~/.aws/config
```

Adjust `region` to wherever your inventory lives.

Open `~/.aws/credentials` in an editor and replace the two
`REPLACE_ME` values with the access key ID and secret access key
from AWS. Save the file. Click **Done** in the AWS console.

(In AWS, you can now optionally also deactivate any older keys for
this user; if this was your first key creation, skip.)

## Step 7 — Restart Squadron with `AWS_PROFILE=squadron-bot`

Squadron's AWS SDK chain reads credentials from `~/.aws/credentials`
keyed by the `AWS_PROFILE` env var. Set it before launch.

If you use the bundled start script
(`~/.squadron/start-squadron.sh`), add `export AWS_PROFILE=squadron-bot`
before the `exec ./bin/squadron` line.

If you launch the binary directly:

```bash
export AWS_PROFILE=squadron-bot
./bin/squadron --config /tmp/squadron-local.yaml
```

If you use docker-compose, add `AWS_PROFILE=squadron-bot` to the
service's environment and mount `~/.aws` read-only into the
container.

## Step 8 — Validate the connection

Return to the Squadron wizard tab. Click through:

- Step 2 (trust policy display) — Next.
- Step 3 (permissions policy display) — Next.
- Step 4 (role ARN) — paste
  `arn:aws:iam::<ACCOUNT_ID>:role/SquadronDiscovery`. Next.
- Step 5 (validate) — click **Validate connection**.

The "What just happened" panel should show eight green checks:

- ✓ `sts:AssumeRole`
- ✓ `ec2 probe` (with a sample count — 0 if you have no EC2
  instances yet)
- ✓ `lambda probe`
- ✓ `rds probe`
- ✓ `s3 probe` (slice 3a, v0.88.0 — single `s3:ListAllMyBuckets`
  call)
- ✓ `alb probe` (slice 3a, v0.88.0 — single
  `elasticloadbalancing:DescribeLoadBalancers` call with
  PageSize=1)
- ✓ `eks probe` (slice 3b, v0.89.0 — single `eks:ListClusters`
  call with MaxResults=1)
- ✓ `dynamodb probe` (slice 4, v0.89.6 — single
  `dynamodb:ListTables` call with Limit=1)
- ✓ `ecs probe` (slice 5, v0.89.10 — single `ecs:ListClusters`
  call with MaxResults=1)

If any check fails, the panel renders a humanized error with a
`SuggestedStep` jump-back button — click it, fix the IAM
configuration named in the message, and re-validate.

Common failure modes:

- **`sts:AssumeRole` fails with "Access Denied"** — the
  `AssumeSquadronDiscovery` inline policy on `squadron-bot` is
  missing or scoped wrong. Re-check step 4 above.
- **`sts:AssumeRole` fails with "Invalid ExternalId"** — the trust
  policy on `SquadronDiscovery` references a different ExternalId
  than the wizard's current state. Update one to match the other.
- **`ec2 probe` / `lambda probe` / `rds probe` / `s3 probe` /
  `alb probe` / `eks probe` / `dynamodb probe` / `ecs probe` fails
  with AccessDenied** — the `SquadronDiscoveryReadOnly` inline
  policy on the role is missing or scoped wrong. Re-check step 3d
  above.
- **`sts:AssumeRole` hangs for 30+ seconds then fails with "no
  credentials"** — Squadron didn't see `~/.aws/credentials`. Check
  that `AWS_PROFILE=squadron-bot` is in the process environment
  (`ps eww -p <PID> | tr ' ' '\n' | grep AWS_`).

## Step 9 — Save the connection

Once Validate is green, click Next → Step 6 (Save the connection) →
**Save**. Squadron:

- Re-runs validate one final time (race protection).
- Encrypts the role ARN with the deployment's
  `SQUADRON_SECRETS_KEY` and persists it to credstore.
- Persists a `CloudConnection` record to the app DB.
- Emits a `discovery.aws.connection_created` audit event. The
  ExternalId is **not** in the audit payload (this is the
  load-bearing privacy promise from `docs/thesis.md`).

Wizard closes. You should now see the connection in the "Connected
accounts" list on `/discovery/aws`.

## Step 10 — Trigger your first scan

The Inventory tab on `/discovery/aws` triggers a scan against the
connection. If your account has EC2 / Lambda / RDS / S3 / ALB / EKS
/ DynamoDB / ECS resources, they populate the Compute / Functions /
Databases / Object stores / Load balancers / Clusters / DynamoDB
tables / ECS clusters sections. The Recommendations tab populates
from the proposer's analysis of the inventory.

If your account is empty (fresh test account), all eight sections
will show "no resources found" — that's expected and confirms the
scanner walked the API successfully with no items to return. Spin
up a free-tier t2.micro EC2 instance — or an empty S3 bucket — to
see the inventory populate.

## Rotation and cleanup

Treat the `squadron-bot` access key as a secret that rotates on a
schedule that matches the rest of your AWS credentials hygiene.

When testing is done in a throwaway account:

1. Deactivate the access key in IAM → Users → `squadron-bot` →
   Security credentials.
2. Delete the deactivated key.
3. Delete the `squadron-bot` user.
4. Delete the `SquadronDiscovery` role.

If you want to rotate the key without re-walking the whole flow:

1. Create a new access key on `squadron-bot`.
2. Edit `~/.aws/credentials` with the new key.
3. Restart Squadron.
4. Verify the next validate still succeeds.
5. Deactivate, then delete, the old key.

## Upgrading the IAM policy when Squadron ships a new slice

If you connected your AWS account on an earlier Squadron release and
have since upgraded, your inline `SquadronDiscoveryReadOnly` policy
may be missing actions that newer releases need. The symptom is a
partial scan: the audit event `discovery.aws.scan_completed` carries
`partial: true` and `failed_services: ["s3", "alb", "eks", "dynamodb", "ecs", ...]`
naming the service walks that hit `AccessDenied`. (v0.88.3 surfaces every
failed service in `partial_reason`, joined by `; ` — earlier releases
only showed the last one.)

The fix is operator-side: edit the inline policy in the IAM console
and add the missing actions. **Squadron does not auto-migrate your
role's IAM policy** — that's a write operation on your IAM, which
Squadron's discovery role explicitly does not have permission to do.

### Slice-to-IAM mapping

| Release | New actions added | Cumulative count |
| ------- | ----------------- | :-: |
| v0.85.0 (slice 1) | `ec2:DescribeInstances`, `ec2:DescribeInstanceStatus`, `ec2:DescribeRegions`, `ec2:DescribeTags`, `lambda:ListFunctions`, `lambda:GetFunction`, `lambda:GetFunctionConfiguration`, `lambda:ListTags` | 8 |
| v0.87.0 (slice 2 — RDS) | `rds:DescribeDBInstances` | 9 |
| v0.88.0 (slice 3a — S3 + ALB) | `s3:ListAllMyBuckets`, `s3:GetBucketLocation`, `s3:GetBucketLogging`, `s3:GetBucketTagging`, `s3:GetBucketRequestPayment`, `elasticloadbalancing:DescribeLoadBalancers`, `elasticloadbalancing:DescribeLoadBalancerAttributes`, `elasticloadbalancing:DescribeTags` | 17 |
| v0.89.0 (slice 3b — EKS) | `eks:ListClusters`, `eks:DescribeCluster`, `eks:ListAddons`, `eks:DescribeAddon`, `eks:ListNodegroups`, `eks:ListFargateProfiles` | 23 |
| v0.89.1 (hotfix — see #605) | (correction) v0.89.0 was published with 5 eks:* actions but the scanner also calls `eks:ListFargateProfiles`. Add the 6th action if you set up against the v0.89.0 template; no other change. | 23 |
| v0.89.6 (slice 4 — DynamoDB) | `dynamodb:ListTables`, `dynamodb:DescribeTable`, `dynamodb:DescribeContributorInsights`, `dynamodb:ListTagsOfResource` | 27 |
| v0.89.10 (slice 5 — ECS/Fargate) | `ecs:ListClusters`, `ecs:DescribeClusters`, `ecs:ListTagsForResource` | 30 |
| v0.89.207 (event-source tier — IAM fix) | `sqs:ListQueues`, `sqs:GetQueueAttributes`, `sns:ListTopics`, `sns:GetTopicAttributes`, `events:ListEventBuses`, `events:ListRules`, `events:ListTargetsByRule`, `states:ListStateMachines`, `states:DescribeStateMachine` — **power the SQS/SNS/EventBridge/Step Functions event-source recommendation tier. That tier shipped in v0.89.149–160 but these actions were missing from this template until v0.89.207, so a role created against the earlier template returns AccessDenied + an empty event-source inventory at scan time. Add these 9 actions to an existing role to enable it.** | 39 |

### How to update

1. AWS console → IAM → Roles → `SquadronDiscovery` → Permissions tab.
2. Click the **SquadronDiscoveryReadOnly** policy to expand, then
   **Edit**.
3. Replace the entire JSON with the latest policy block from
   **Step 3d** of this runbook (which always reflects the most
   recent shipped release). The full action list is cumulative —
   never delete an earlier slice's actions when adding a new
   slice's actions.
4. Click **Next** → **Save changes**. The next `sts:AssumeRole`
   Squadron makes picks up the new permissions immediately (STS
   tokens are issued fresh per scan).
5. Trigger a scan from `/discovery/aws` and confirm
   `partial: false` in the response and in the
   `discovery.aws.scan_completed` audit event.

### Verifying the policy is up to date

A quick health check after the update:

```bash
# Compares the action count in your live role to the expected
# v0.89.207+ count (39 actions). Run as the squadron-terraform or
# any IAM-read-capable profile (not squadron-bot — that profile
# only has sts:AssumeRole, not iam:GetRolePolicy).
AWS_PROFILE=<your-iam-read-profile> aws iam get-role-policy \
  --role-name SquadronDiscovery \
  --policy-name SquadronDiscoveryReadOnly \
  --query 'PolicyDocument.Statement[0].Action | length(@)'
```

Expected output: `39` for v0.89.207+ (`30` for v0.89.10–v0.89.206 —
these lacked the 9 event-source actions, so event-source discovery
returned AccessDenied; v0.89.6–v0.89.9 expected `27`; v0.89.0–v0.89.5
expected `23`; v0.85.0–v0.87.x ranged from 8 to 17). Anything less than
the expected value means you're missing actions for one of the shipped
slices.

## What this does NOT cover

The IAM permissions Squadron asks for cover slice 1 (EC2 + Lambda,
v0.85.0), slice 2 (+ RDS, v0.87.0), slice 3a (+ S3 + ALB,
v0.88.0), slice 3b (+ EKS, v0.89.0), slice 4 (+ DynamoDB, v0.89.6),
and slice 5 (+ ECS / Fargate, v0.89.10). ECS/Fargate landed in
v0.89.10 as slice 5. Each future slice expands the
permissions-policy template in the same place — the policy you
copied in step 3d.

**Honest scope limitation for DynamoDB (slice 4):** Squadron's
DynamoDB rule reads the resource-side Contributor Insights
status (`dynamodb:DescribeContributorInsights`). Squadron does
not detect SDK-side OpenTelemetry or X-Ray instrumentation in
your application code. If your DynamoDB SDK is OTel-wrapped on
the client side, Squadron will report the table as
uninstrumented — this is a known limitation of cloud-API-only
scanning. Operators in that posture can decline the
recommendation; the rule is the right one for the cloud-API
surface Squadron has access to, even when the operator's
application code is doing more.

**Honest scope limitation for ECS / Fargate (slice 5):** Squadron
detects cluster-level CloudWatch Container Insights. Squadron
does not detect task-definition-level instrumentation — X-Ray
daemon sidecars, ADOT collector sidecars, or FireLens log routing
in your task definitions. If your task defs include those
sidecars but the cluster does not have Container Insights
enabled, Squadron will report the cluster as uninstrumented —
this is a known limitation of cluster-level scanning. A future
slice can extend the rule to inspect task definitions if
operators request it. Both Fargate and EC2 launch types are
covered by the same per-cluster rule — Container Insights is
per-cluster, not per-launch-type.

The role does **not** include any write or modify permission. The
proposer surfaces recommendations as Terraform snippets or plan
steps; the operator executes the modifications in their own
tooling.

## Next: connect a Terraform repo

Once your AWS connection is working and you've generated your first
recommendations, the next step is connecting a GitHub-hosted
Terraform repository so the "Open PR" button on each recommendation
card lights up and Squadron can author the PR for you instead of
you copying snippets by hand.

See [discovery-iac-first-time-setup.md](discovery-iac-first-time-setup.md)
for the GitHub PAT bootstrap and the IaC wizard walk. The IaC
setup is independent of the AWS setup — you can keep scanning
without it — but the close-the-loop demo Squadron is built around
needs both halves wired.

## Scanning multiple accounts

Once you've connected more than one AWS account (re-run the wizard
once per account — same trust policy template, same external-ID
generation, same Save flow), v0.89.7a's multi-account orchestrator
lets you kick off scans across every connected account in one
call:

```bash
curl -X POST \
  -H "Authorization: Bearer $SQUADRON_API_TOKEN" \
  "http://localhost:8080/api/v1/discovery/aws/scan-all?concurrency=5"
```

No request body needed. The endpoint iterates every AWS connection
in the credstore and runs the same per-account scan you'd run from
the wizard, with bounded concurrency (default 3, max 8) so AWS
doesn't throttle the STS calls. The response is a single JSON
envelope with aggregate counts (`total_resources`,
`total_instrumented`, `total_uninstrumented`), one row per
succeeded account with its scan ID + counts, and one row per
failed account with `error_code` + `humanized_message` (a single
account's lapsed role won't block the rest).

Optional query parameters:

- `regions=us-east-1,eu-west-1` — override the per-call region
  list. Empty falls back to each connection's stored region list
  (the same posture as the per-account endpoint's empty-body
  branch).
- `concurrency=N` — maximum simultaneous per-account scans.
  Values above 8 are clamped silently; the effective value is
  echoed back in the response.

The audit timeline shows one
`discovery.aws.scan_all_completed` event linked to N per-account
`discovery.aws.scan_completed` events via the shared
`scan_all_id` payload field — a forensic reader can reconstruct
every per-account scan from the aggregate event ID.

The UI surface for multi-account scanning (the "Scan all" CTA on
the Inventory tab, the account selector, per-account badges on
the recommendation cards) lands in v0.89.7b. For now the endpoint
is curl-friendly and well-suited to ops scripts that run
overnight sweeps across an organization.
