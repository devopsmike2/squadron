package demo

import (
	"time"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// GCP demo connection. Unlike AWS (keyed on a fixed account id in the shared
// credstore), GCP connections live in their own store and get a generated UUID
// at create time — so the demo connection is identified by a sentinel
// ProjectID instead. The scan + recommendations handlers short-circuit when the
// resolved connection's ProjectID matches GCPProjectID.
const (
	GCPProjectID   = "squadron-demo"
	GCPRegion      = "us-central1"
	GCPDisplayName = "Demo Project (sample data)"
	GCPScanID      = "demo-gcp-scan-0001"
)

// IsGCPDemoProject reports whether projectID addresses the demo GCP connection.
func IsGCPDemoProject(projectID string) bool { return projectID == GCPProjectID }

// GCPResult returns the deterministic GCP sample inventory: 3 GCE instances
// (1 instrumented, 2 gaps) and 2 Cloud SQL instances (1 with Query Insights
// off). The GCP scan response surfaces compute + databases (+ clusters); the
// mix here keeps the recommendations non-trivial.
func GCPResult() *scanner.Result {
	now := time.Now().UTC()
	return &scanner.Result{
		ScanID:          GCPScanID,
		ScanStartedAt:   now.Add(-2 * time.Second),
		ScanCompletedAt: now,
		Provider:        credstore.ProviderGCP,
		AccountID:       GCPProjectID,
		Regions:         []string{GCPRegion},
		Partial:         false,
		Compute: []scanner.ComputeInstanceSnapshot{
			{
				ResourceID:   "web-prod-1",
				InstanceType: "e2-standard-2",
				OSFamily:     "linux",
				Region:       GCPRegion,
				HasOTel:      true,
				Tags:         map[string]string{"app": "web", "env": "prod", "otel": "ops-agent"},
			},
			{
				ResourceID:   "web-prod-2",
				InstanceType: "e2-standard-2",
				OSFamily:     "linux",
				Region:       GCPRegion,
				HasOTel:      false,
				Tags:         map[string]string{"app": "web", "env": "prod"},
			},
			{
				ResourceID:   "api-prod-1",
				InstanceType: "n2-standard-4",
				OSFamily:     "linux",
				Region:       GCPRegion,
				HasOTel:      false,
				Tags:         map[string]string{"app": "api", "env": "prod"},
			},
		},
		Databases: []scanner.DatabaseInstanceSnapshot{
			{
				ResourceID:           "squadron-demo:us-central1:orders-db",
				Engine:               "postgres",
				EngineVersion:        "15",
				InstanceClass:        "db-custom-2-7680",
				Region:               GCPRegion,
				Provider:             "gcp",
				QueryInsightsEnabled: true,
				Tags:                 map[string]string{"app": "orders", "env": "prod"},
			},
			{
				ResourceID:           "squadron-demo:us-central1:analytics-db",
				Engine:               "mysql",
				EngineVersion:        "8.0",
				InstanceClass:        "db-custom-2-7680",
				Region:               GCPRegion,
				Provider:             "gcp",
				QueryInsightsEnabled: false,
				Tags:                 map[string]string{"app": "analytics", "env": "staging"},
			},
		},
	}
}

// GCPRecommendationSteps backs the demo GCP recommendations. Same
// buildDiscoveryRecommendations walk as live output; snippets are illustrative
// samples that target the real gaps in GCPResult.
func GCPRecommendationSteps() []ai.PlanStepCandidate {
	return []ai.PlanStepCandidate{
		{
			Name:              "Install the Ops Agent (OpenTelemetry) on 2 uninstrumented GCE instances",
			GroupID:           "demo-gcp-compute",
			AffectedResources: []string{"web-prod-2", "api-prod-1"},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Rolls the Google Cloud Ops Agent out
# to the targeted GCE instances via an OS Config agent policy. The Ops Agent
# collects metrics + logs and can export OTLP to a collector.
resource "google_os_config_os_policy_assignment" "ops_agent" {
  name     = "ops-agent-rollout"
  location = "us-central1"
  instance_filter {
    inclusion_labels {
      labels = { app = "web" }
    }
  }
  # ...os_policies installing google-cloud-ops-agent...
  rollout {
    disruption_budget { fixed = 1 }
    min_wait_duration = "300s"
  }
}`,
		},
		{
			Name:              "Enable Query Insights on the analytics Cloud SQL instance",
			GroupID:           "demo-gcp-databases",
			AffectedResources: []string{"squadron-demo:us-central1:analytics-db"},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Turns on Cloud SQL Query Insights so
# the analytics database surfaces per-query latency + plans.
resource "google_sql_database_instance" "analytics_db" {
  # ...existing instance config...
  settings {
    insights_config {
      query_insights_enabled  = true
      query_string_length     = 1024
      record_application_tags = true
      record_client_address   = true
    }
  }
}`,
		},
	}
}
