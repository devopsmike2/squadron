# Post 12: Why a control plane should never hold cloud write credentials

**Pillar:** Squadron
**Tag at publish:** v0.85.0
**Visual evidence:** A screenshot of the connector wizard's Step
2 ("Create the IAM role with this trust policy") on the live
deployment at the v0.85.0 tag. The trust-policy JSON is rendered
verbatim in the code block with the per-deployment ExternalId
already substituted in. The copy-to-clipboard button is visible.
A second smaller frame in the same image shows the relevant lines
from `internal/discovery/aws/scanner.go` listing the API calls
the slice-1 scanner actually makes — every one of them is a
Describe / List / Get / GetCallerIdentity.
**Hashtags:** #OpenTelemetry #SRE
**Target word count:** 200-400

## Draft

Most cloud-aware tools ask for write credentials and then turn
into a security review headache. The customer's security team
asks "what can this tool do in our AWS account?" and the answer
is "everything the credentials allow." Six months of paperwork
follow.

Squadron's slice-1 discovery posture is the deliberate inverse.

The IAM role the operator creates has a permissions policy of
`ec2:DescribeInstances`, `ec2:DescribeInstanceStatus`,
`ec2:DescribeRegions`, `ec2:DescribeTags`, `lambda:ListFunctions`,
`lambda:GetFunction`, `lambda:GetFunctionConfiguration`, and
`lambda:ListTags`. That is the entire list. No `*:Update*`. No
`*:Modify*`. No `*:Create*`. No `*:Delete*`. No `*:Put*`. No
`iam:*`. The principle of least privilege is enforced at the
policy level, so even a fully compromised Squadron cannot
escalate to write actions in the customer's account.

Recommendations land as Terraform snippets the customer's
existing IaC pipeline runs. Squadron orchestrates; the customer
executes. The proposer's system prompt states this to the model
in plain English — open
`internal/ai/proposer_discovery_prompt.go` and read the line
that begins SQUADRON DOES NOT EXECUTE THE TERRAFORM. The model
is told never to suggest an auto-apply path and never to imply
Squadron has write credentials. That is not policy enforced at
deploy time. It is policy stated to the engine that drafts the
recommendation.

The trust policy itself defends against the confused-deputy
problem with an `sts:ExternalId` condition the wizard generates
per deployment. The credential substrate stores the role ARN
and the ExternalId at rest, encrypted with AES-256-GCM. STS
session tokens live in memory for the duration of one scan and
are dropped. No long-lived AWS access keys ever enter Squadron's
schema. There is no field for them. A future contribution that
tries to add one gets rejected.

A security reviewer can verify all of this from the architecture
alone — the trust policy, the permissions policy, the prompt,
the scanner's import surface — without reading every code path.
That is what "no cloud write credentials" actually buys: a
review that takes weeks instead of quarters.

Repo at the v0.85.0 tag. The wizard is at
`/discovery/aws#account`.

#OpenTelemetry #SRE

## Visual asset spec

- **Filename:** `assets/post-12-trust-policy-wizard-plus-scanner-surface.png`
- **Surface — main frame:** the connector wizard's Step 2
  ("Create the IAM role with this trust policy") on the live
  deployment at the v0.85.0 tag, opened via the Account tab's
  "Connect new account" button. The trust-policy JSON code block
  shows the per-deployment ExternalId already substituted in;
  the copy-to-clipboard button is visible; the inline "why this
  step?" panel mentioning the confused-deputy problem is at
  least partially in frame.
- **Surface — inset frame:** a code-block screenshot from
  `internal/discovery/aws/scanner.go` showing the API calls
  the scanner makes. The `GetCallerIdentity`,
  `DescribeInstances`, and `ListFunctions` invocations are
  visible; together they prove the call surface is
  read-only.
- **Annotations:** one small marker on the trust-policy JSON's
  `sts:ExternalId` condition, captioned "per-deployment secret
  — defeats confused deputy". One marker on the inset code
  block's `DescribeInstances` line, captioned "every call is
  Describe / List / Get". Two markers total, both factual.
- **Crop:** include the wizard's step counter (Step 2 of 5) so
  the reader can see this is one of five guided steps, not a
  one-off dialog.

## Anti-pattern guard

Resists **the vision dump** from linkedin-rollout.md
"Anti-patterns to avoid". The pull is to publish the entire
threat-model section of `docs/universal-discovery-design.md`
inline. The post instead names the eight specific API actions
in the slice-1 policy, the six write-action prefixes that are
excluded, the one line in the discovery system prompt that
states the posture to the model, and the substrate's
AES-256-GCM at-rest encryption. Concrete details, not the full
strategy doc. The reader who wants more is one click away from
the design doc; the post is the entry point, not the document.
