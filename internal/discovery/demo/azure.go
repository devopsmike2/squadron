package demo

import (
	"time"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Azure demo connection. Like GCP, Azure connections live in their own store
// with generated UUIDs, so the demo is identified by a sentinel SubscriptionID.
const (
	AzureSubscriptionID = "demo-subscription"
	AzureLocation       = "eastus"
	AzureDisplayName    = "Demo Subscription (sample data)"
	AzureScanID         = "demo-azure-scan-0001"
)

// IsAzureDemoSubscription reports whether subscriptionID addresses the demo
// Azure connection.
func IsAzureDemoSubscription(subscriptionID string) bool {
	return subscriptionID == AzureSubscriptionID
}

// AzureResult returns the deterministic Azure sample inventory: 3 VMs (1
// instrumented, 2 gaps including one Windows VM) and 2 Azure SQL databases
// (1 with SQL Insights diagnostics off). The Azure scan response surfaces
// compute + databases.
func AzureResult() *scanner.Result {
	now := time.Now().UTC()
	return &scanner.Result{
		ScanID:          AzureScanID,
		ScanStartedAt:   now.Add(-2 * time.Second),
		ScanCompletedAt: now,
		Provider:        credstore.ProviderAzure,
		AccountID:       AzureSubscriptionID,
		Regions:         []string{AzureLocation},
		Partial:         false,
		Compute: []scanner.ComputeInstanceSnapshot{
			{
				ResourceID:   "/subscriptions/demo/resourceGroups/prod/providers/Microsoft.Compute/virtualMachines/vm-web-prod-1",
				InstanceType: "Standard_D2s_v3",
				OSFamily:     "linux",
				Region:       AzureLocation,
				HasOTel:      true,
				Tags:         map[string]string{"app": "web", "env": "prod", "otel": "ama"},
			},
			{
				ResourceID:   "/subscriptions/demo/resourceGroups/prod/providers/Microsoft.Compute/virtualMachines/vm-web-prod-2",
				InstanceType: "Standard_D2s_v3",
				OSFamily:     "linux",
				Region:       AzureLocation,
				HasOTel:      false,
				Tags:         map[string]string{"app": "web", "env": "prod"},
			},
			{
				ResourceID:   "/subscriptions/demo/resourceGroups/prod/providers/Microsoft.Compute/virtualMachines/vm-iis-prod-1",
				InstanceType: "Standard_D2s_v3",
				OSFamily:     "windows",
				Region:       AzureLocation,
				HasOTel:      false,
				Tags:         map[string]string{"app": "iis", "env": "prod"},
			},
		},
		Databases: []scanner.DatabaseInstanceSnapshot{
			{
				ResourceID:             "/subscriptions/demo/resourceGroups/prod/providers/Microsoft.Sql/servers/demo-sql/databases/orders-db",
				Engine:                 "sqlserver",
				EngineVersion:          "12.0",
				InstanceClass:          "S3",
				Region:                 AzureLocation,
				Provider:               "azure",
				SQLInsightsDiagEnabled: true,
				Tags:                   map[string]string{"app": "orders", "env": "prod"},
			},
			{
				ResourceID:             "/subscriptions/demo/resourceGroups/prod/providers/Microsoft.Sql/servers/demo-sql/databases/analytics-db",
				Engine:                 "sqlserver",
				EngineVersion:          "12.0",
				InstanceClass:          "S1",
				Region:                 AzureLocation,
				Provider:               "azure",
				SQLInsightsDiagEnabled: false,
				Tags:                   map[string]string{"app": "analytics", "env": "staging"},
			},
		},
	}
}

// AzureRecommendationSteps backs the demo Azure recommendations. Snippets are
// illustrative samples that target the real gaps in AzureResult and are
// OS-aware: the Linux and Windows VMs get separate Azure Monitor Agent steps
// (the AMA extension name differs by OS — the conflation #114 guards against).
func AzureRecommendationSteps() []ai.PlanStepCandidate {
	return []ai.PlanStepCandidate{
		{
			Name:    "Install the Azure Monitor Agent on the uninstrumented Linux VM",
			GroupID: "demo-azure-compute-linux",
			AffectedResources: []string{
				"/subscriptions/demo/resourceGroups/prod/providers/Microsoft.Compute/virtualMachines/vm-web-prod-2",
			},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Installs the Azure Monitor Agent on
# the Linux VM. NOTE: Linux uses the AzureMonitorLinuxAgent extension —
# Windows VMs need AzureMonitorWindowsAgent instead (see the next step).
resource "azurerm_virtual_machine_extension" "ama_linux" {
  name                       = "AzureMonitorLinuxAgent"
  virtual_machine_id         = azurerm_linux_virtual_machine.vm_web_prod_2.id
  publisher                  = "Microsoft.Azure.Monitor"
  type                       = "AzureMonitorLinuxAgent"
  type_handler_version       = "1.29"
  auto_upgrade_minor_version = true
}`,
		},
		{
			Name:    "Install the Azure Monitor Agent on the uninstrumented Windows VM",
			GroupID: "demo-azure-compute-windows",
			AffectedResources: []string{
				"/subscriptions/demo/resourceGroups/prod/providers/Microsoft.Compute/virtualMachines/vm-iis-prod-1",
			},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Installs the Azure Monitor Agent on
# the Windows VM — the extension type differs from Linux.
resource "azurerm_virtual_machine_extension" "ama_windows" {
  name                       = "AzureMonitorWindowsAgent"
  virtual_machine_id         = azurerm_windows_virtual_machine.vm_iis_prod_1.id
  publisher                  = "Microsoft.Azure.Monitor"
  type                       = "AzureMonitorWindowsAgent"
  type_handler_version       = "1.21"
  auto_upgrade_minor_version = true
}`,
		},
		{
			Name:    "Enable SQL Insights diagnostics on the analytics Azure SQL database",
			GroupID: "demo-azure-databases",
			AffectedResources: []string{
				"/subscriptions/demo/resourceGroups/prod/providers/Microsoft.Sql/servers/demo-sql/databases/analytics-db",
			},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Routes the SQLInsights log category
# to a Log Analytics workspace via a diagnostic setting so the analytics
# database surfaces query-level telemetry.
resource "azurerm_monitor_diagnostic_setting" "analytics_sqlinsights" {
  name                       = "sqlinsights"
  target_resource_id         = azurerm_mssql_database.analytics_db.id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.obs.id
  enabled_log { category = "SQLInsights" }
}`,
		},
	}
}
