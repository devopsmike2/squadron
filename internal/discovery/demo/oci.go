package demo

import (
	"time"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// OCI demo connection. Like GCP / Azure, OCI connections live in their own store
// with generated UUIDs, so the demo is identified by a sentinel TenancyOCID.
const (
	OCITenancyOCID = "ocid1.tenancy.oc1..demo"
	OCIRegion      = "us-ashburn-1"
	OCIDisplayName = "Demo Tenancy (sample data)"
	OCIScanID      = "demo-oci-scan-0001"
)

// IsOCIDemoTenancy reports whether tenancyOCID addresses the demo OCI connection.
func IsOCIDemoTenancy(tenancyOCID string) bool { return tenancyOCID == OCITenancyOCID }

// OCIResult returns the deterministic OCI sample inventory: 3 compute instances
// (1 instrumented, 2 gaps) and 2 databases (1 with Database Management off).
func OCIResult() *scanner.Result {
	now := time.Now().UTC()
	return &scanner.Result{
		ScanID:          OCIScanID,
		ScanStartedAt:   now.Add(-2 * time.Second),
		ScanCompletedAt: now,
		Provider:        credstore.ProviderOCI,
		AccountID:       OCITenancyOCID,
		Regions:         []string{OCIRegion},
		Partial:         false,
		Compute: []scanner.ComputeInstanceSnapshot{
			{
				ResourceID:   "ocid1.instance.oc1.iad.demo-web-prod-1",
				InstanceType: "VM.Standard.E4.Flex",
				OSFamily:     "linux",
				Region:       OCIRegion,
				HasOTel:      true,
				Tags:         map[string]string{"app": "web", "env": "prod", "otel-instrumented": "true"},
			},
			{
				ResourceID:   "ocid1.instance.oc1.iad.demo-web-prod-2",
				InstanceType: "VM.Standard.E4.Flex",
				OSFamily:     "linux",
				Region:       OCIRegion,
				HasOTel:      false,
				Tags:         map[string]string{"app": "web", "env": "prod"},
			},
			{
				ResourceID:   "ocid1.instance.oc1.iad.demo-api-prod-1",
				InstanceType: "VM.Standard.E4.Flex",
				OSFamily:     "linux",
				Region:       OCIRegion,
				HasOTel:      false,
				Tags:         map[string]string{"app": "api", "env": "prod"},
			},
		},
		Databases: []scanner.DatabaseInstanceSnapshot{
			{
				ResourceID:                "ocid1.database.oc1.iad.demo-orders-db",
				Engine:                    "oracle",
				EngineVersion:             "19c",
				InstanceClass:             "VM.Standard2.2",
				Region:                    OCIRegion,
				Provider:                  "oci",
				DatabaseManagementEnabled: true,
				Tags:                      map[string]string{"app": "orders", "env": "prod"},
			},
			{
				ResourceID:                "ocid1.database.oc1.iad.demo-analytics-db",
				Engine:                    "mysql",
				EngineVersion:             "8.0",
				InstanceClass:             "MySQL.VM.Standard.E3.1.8GB",
				Region:                    OCIRegion,
				Provider:                  "oci",
				DatabaseManagementEnabled: false,
				Tags:                      map[string]string{"app": "analytics", "env": "staging"},
			},
		},
	}
}

// OCIRecommendationSteps backs the demo OCI recommendations. Snippets are
// illustrative samples targeting the real gaps in OCIResult. All demo compute
// is Linux, so the agent step is explicitly Linux-scoped (guarding against the
// OS-assumption #148 flagged for OCI compute snippets).
func OCIRecommendationSteps() []ai.PlanStepCandidate {
	return []ai.PlanStepCandidate{
		{
			Name:    "Install the OpenTelemetry Collector on 2 uninstrumented OCI compute instances",
			GroupID: "demo-oci-compute",
			AffectedResources: []string{
				"ocid1.instance.oc1.iad.demo-web-prod-2",
				"ocid1.instance.oc1.iad.demo-api-prod-1",
			},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Installs the OpenTelemetry Collector
# on the targeted Linux OCI compute instances via cloud-init user_data.
# NOTE: this snippet is Linux-only (#!/bin/bash) — Windows instances need a
# PowerShell equivalent.
#!/bin/bash
sudo dnf install -y https://github.com/open-telemetry/opentelemetry-collector-releases/releases/latest/download/otelcol-contrib_linux_amd64.rpm
sudo systemctl enable --now otelcol-contrib`,
		},
		{
			Name:    "Enable Database Management on the analytics OCI database",
			GroupID: "demo-oci-databases",
			AffectedResources: []string{
				"ocid1.database.oc1.iad.demo-analytics-db",
			},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Enables OCI Database Management /
# Operations Insights on the analytics database so it surfaces performance
# telemetry.
resource "oci_database_management_managed_database_group" "analytics" {
  compartment_id = var.compartment_ocid
  name           = "analytics-dbmgmt"
  # ...attach the analytics database; enable Operations Insights...
}`,
		},
	}
}
