// Static data for the v0.89.3 #603 Stream 19 Connect-IaC-repo wizard.
//
// The nine canonical resource_kind rows match docs/proposals/603-
// connect-iac-repo.md §6 and the canonical kinds used by the proposer
// in internal/ai/proposer_discovery.go. Slice 4 (v0.89.6) added the
// eighth row (dynamodb-contributor-insights); slice 5 (v0.89.10) added
// the ninth (ecs-container-insights). The wizard pre-populates the
// placement map with these rows; operators set per-row file paths
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
    description: "Enables PI and EM on RDS instances missing either lever.",
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
  {
    // Slice 5 (v0.89.10) — ECS / Fargate Container Insights.
    // Single-axis observability rule per the proposer prompt: a
    // cluster is covered iff container_insights_status == "enabled"
    // (case-insensitive against the cluster's
    // settings[name=containerInsights].value). Both Fargate and EC2
    // launch types covered by the same per-cluster rule — Container
    // Insights is per-cluster, not per-launch-type. The proposer's
    // Terraform shape per uncovered cluster is
    // resource "aws_ecs_cluster" "<cluster_name>" {
    //   name = "<cluster_name>"
    //   setting { name = "containerInsights" value = "enabled" }
    // } — placement lands in the operator-declared file path.
    provider: "aws",
    resource_kind: "ecs-container-insights",
    display_name: "ECS / Fargate Container Insights",
    description:
      "Enables CloudWatch Container Insights on ECS clusters to surface task and service metrics.",
  },

  // --- GCP (#182) ---
  {
    provider: "gcp",
    resource_kind: "gce-otel-label",
    display_name: "GCE OTel label",
    description:
      "Adds the OTel-collector label to Compute Engine instances missing it.",
  },
  {
    provider: "gcp",
    resource_kind: "cloudsql-pi-enable",
    display_name: "Cloud SQL Query Insights",
    description: "Enables Query Insights on Cloud SQL instances missing it.",
  },
  {
    provider: "gcp",
    resource_kind: "gke-mp-enable",
    display_name: "GKE Managed Prometheus",
    description:
      "Enables Google Cloud Managed Service for Prometheus on GKE clusters.",
  },
  {
    provider: "gcp",
    resource_kind: "gcs-logging-enable",
    display_name: "GCS bucket logging",
    description:
      "Enables storage logging to a log-sink bucket for Cloud Storage buckets without it.",
  },
  {
    provider: "gcp",
    resource_kind: "gclb-logging-enable",
    display_name: "Cloud Load Balancing logging",
    description:
      "Enables backend-service logging (log_config) on Google Cloud load balancers.",
  },

  // --- Azure (#182) ---
  {
    provider: "azure",
    resource_kind: "vm-otel-tag",
    display_name: "Azure VM OTel tag",
    description:
      "Tags Azure VMs (linux/windows resource picked by OS) for OTel-collector instrumentation.",
  },
  {
    provider: "azure",
    resource_kind: "azsql-diag-enable",
    display_name: "Azure SQL diagnostic settings",
    description:
      "Adds a diagnostic setting routing the SQLInsights log category for Azure SQL databases.",
  },
  {
    provider: "azure",
    resource_kind: "aks-monitor-enable",
    display_name: "AKS monitoring",
    description:
      "Enables Azure Monitor / managed observability on AKS clusters.",
  },
  {
    provider: "azure",
    resource_kind: "azblob-diag-enable",
    display_name: "Azure Blob diagnostic settings",
    description:
      "Adds a diagnostic setting routing blob StorageRead/Write/Delete logs for storage accounts.",
  },
  {
    provider: "azure",
    resource_kind: "azlb-diag-enable",
    display_name: "Azure Load Balancer diagnostic settings",
    description:
      "Adds a diagnostic setting routing load-balancer log/metric categories.",
  },

  // --- OCI (#182) ---
  {
    provider: "oci",
    resource_kind: "compute-otel-tag",
    display_name: "OCI Compute OTel tag",
    description:
      "Adds the OTel freeform tag to OCI Compute instances missing it.",
  },
  {
    provider: "oci",
    resource_kind: "ocidb-perfhub-enable",
    display_name: "OCI Database Operations Insights",
    description:
      "Enrolls OCI DB Systems / Autonomous Databases in Operations Insights / Database Management.",
  },
  {
    provider: "oci",
    resource_kind: "oke-ops-insights-enable",
    display_name: "OKE Operations Insights",
    description: "Enrolls OKE clusters in Operations Insights.",
  },
  {
    provider: "oci",
    resource_kind: "ocibucket-logging-enable",
    display_name: "OCI Object Storage logging",
    description:
      "Creates an OCI Logging service log for Object Storage buckets without access logging.",
  },
  {
    provider: "oci",
    resource_kind: "ocilb-logging-enable",
    display_name: "OCI Load Balancer logging",
    description:
      "Creates an OCI Logging service access log for load balancers without one.",
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

// v0.89.32 #651 Stream 49 — Connect IaC repo wizard now surfaces the
// webhook-secret + GitHub-webhook-config flow that used to be runbook-
// only. The constants below seed the new step bodies. The full
// operator runbook still lives at docs/webhook-listener.md and remains
// the source of truth for the contract.

// WEBHOOK_LISTENER_DOC_LINK is the relative docs path the wizard's
// "learn more" links point at. Co-located with the wizard rather than
// hardcoded inline so a future docs reshuffle is one-edit.
export const WEBHOOK_LISTENER_DOC_LINK = "/docs/webhook-listener.md";

// WEBHOOK_LISTENER_PATH is the path component the GitHub webhook
// "Payload URL" field needs. The wizard's "Configure the webhook on
// GitHub" step joins this with window.location.origin to surface a
// best-effort default; operators on multi-host deployments edit the
// host portion by hand. Matches the route registered in
// internal/api/server.go.
export const WEBHOOK_LISTENER_PATH = "/api/v1/webhooks/github";

// WEBHOOK_SECRET_BYTE_LEN matches the runbook's "32 random bytes" guidance
// (docs/webhook-listener.md §"Step 1 — Generate the webhook secret").
// crypto.getRandomValues(new Uint8Array(WEBHOOK_SECRET_BYTE_LEN)) →
// 64-character hex string, matching what `openssl rand -hex 32` emits.
export const WEBHOOK_SECRET_BYTE_LEN = 32;

// WebhookSecretSource is the operator's choice in the new "Set up the
// webhook secret" wizard step.
//
//   - "generate"   — client-side crypto.getRandomValues; PATCHed onto
//                    the connection after createConnection succeeds.
//   - "use_global" — operator relies on the env-var
//                    SQUADRON_GITHUB_WEBHOOK_SECRET fallback. No PATCH.
//   - "skip"       — operator defers webhook setup; wizard skips the
//                    "Configure the webhook on GitHub" walkthrough and
//                    surfaces a reminder on the success card.
export type WebhookSecretSource = "generate" | "use_global" | "skip";
