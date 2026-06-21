// Static data for the v0.89.3 #603 Stream 19 Connect-IaC-repo wizard.
//
// The eight canonical resource_kind rows match docs/proposals/603-
// connect-iac-repo.md §6 and the canonical kinds used by the proposer
// in internal/ai/proposer_discovery.go. Slice 4 (v0.89.6) added the
// eighth row (dynamodb-contributor-insights). The wizard pre-populates
// the placement map with these rows; operators set per-row file paths
// (or flip per-row Skip toggles for kinds they don't manage in this
// repo).
//
// Description lines are engineer-to-engineer terse — one sentence per
// row, no marketing tone, the same posture as the AWS wizard's step
// descriptions in awsWizard.ts.

// PlacementKindRow drives one row of the wizard's placement-map step.
// The wizard maps each row to one IaCPlacementEntry on validate / save.
export interface PlacementKindRow {
  // Wire shape: provider + resource_kind go straight onto the request.
  provider: string;
  resource_kind: string;
  // Display fields.
  display_name: string;
  description: string;
}

export const IAC_GITHUB_PLACEMENT_KINDS: PlacementKindRow[] = [
  {
    provider: "aws",
    resource_kind: "ec2-otel-layer",
    display_name: "EC2 OTel layer",
    description:
      "Installs an OpenTelemetry collector or agent on EC2 instances that lack one.",
  },
  {
    provider: "aws",
    resource_kind: "lambda-otel-layer",
    display_name: "Lambda OTel layer",
    description:
      "Attaches the AWS-managed OTel Lambda layer to functions missing instrumentation.",
  },
  {
    provider: "aws",
    resource_kind: "rds-pi-em",
    display_name: "RDS Performance Insights + Enhanced Monitoring",
    description:
      "Enables PI and EM on RDS instances missing either lever.",
  },
  {
    provider: "aws",
    resource_kind: "s3-access-logging",
    display_name: "S3 access logging",
    description:
      "Turns on server-access logging for buckets without it; logs land in an existing bucket Squadron already sees.",
  },
  {
    provider: "aws",
    resource_kind: "alb-access-logs",
    display_name: "ALB / NLB access logs",
    description:
      "Enables access logs on load balancers, targeting an existing logging bucket where possible.",
  },
  {
    provider: "aws",
    resource_kind: "eks-cluster-logging",
    display_name: "EKS control-plane logging",
    description:
      "Turns on the api + audit control-plane log types on EKS clusters.",
  },
  {
    provider: "aws",
    resource_kind: "eks-observability-addon",
    display_name: "EKS observability addon",
    description:
      "Installs the adot or amazon-cloudwatch-observability addon on clusters without one ACTIVE.",
  },
  {
    // Slice 4 (v0.89.6) — DynamoDB Contributor Insights. Single-axis
    // observability rule per the proposer prompt: a table is covered
    // iff contributor_insights_status == "ENABLED". The proposer's
    // Terraform shape per uncovered table is
    // resource "aws_dynamodb_contributor_insights" "<name>" {
    //   table_name = "..."
    // } — placement lands in the operator-declared file path.
    provider: "aws",
    resource_kind: "dynamodb-contributor-insights",
    display_name: "DynamoDB Contributor Insights",
    description:
      "Enables CloudWatch Contributor Insights on DynamoDB tables to surface top-accessed keys and most-throttled keys.",
  },
];

// GitHub deep-link to the create-PAT page, pre-filled with the `repo`
// scope and a Squadron description so the operator can confirm what
// they're creating before clicking Generate.
export const GITHUB_CREATE_PAT_URL =
  "https://github.com/settings/tokens/new?scopes=repo&description=Squadron+OSS";

// REPO_FULL_NAME_RE matches "owner/repo" — GitHub allows
// alphanumerics, dashes, dots, and underscores in both segments. The
// regex is intentionally identical to the one the Go handler uses for
// the splitRepoFullName helper so client + server agree on the parse.
export const REPO_FULL_NAME_RE = /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/;

// Default branch-prefix string the wizard shows in its placeholder.
// The substrate's actual default lives in iacconnstore.DefaultBranchPrefix
// — we re-declare it here rather than fetch from the server because
// the wizard renders it before any round-trip.
export const DEFAULT_BRANCH_PREFIX = "squadron/rec";
