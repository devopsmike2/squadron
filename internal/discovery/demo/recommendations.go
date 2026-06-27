package demo

import "github.com/devopsmike2/squadron/internal/ai"

// RecommendationSteps returns the canned plan steps that back the demo
// connection's recommendations. They are fed through the SAME
// buildDiscoveryRecommendations walk the real proposer output uses, so the
// resulting recommendation envelopes are shape-identical to live ones — only
// the source (canned vs LLM) differs. Each step targets a real gap in the demo
// inventory (see BuildResult): two uninstrumented Linux EC2 instances, one
// Windows EC2 instance, two uninstrumented Python Lambdas, and one RDS instance
// with both observability levers off.
//
// The snippets are illustrative samples (each carries a "demo inventory"
// header) but are deliberately honest: the Python Lambda wrapper is
// /opt/otel-instrument (not the Node /opt/otel-handler), the layer ARN carries
// the standard "replace with the current version for your region" caveat, and
// the Windows step notes the agent-config path differs from Linux.
func RecommendationSteps() []ai.PlanStepCandidate {
	return []ai.PlanStepCandidate{
		{
			Name:    "Install the ADOT Collector on 2 uninstrumented Linux EC2 instances",
			GroupID: "demo-compute-linux",
			AffectedResources: []string{
				"i-0a1b2c3d4e5f60002",
				"i-0a1b2c3d4e5f60003",
			},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Installs the AWS Distro for
# OpenTelemetry (ADOT) Collector on the targeted Linux EC2 instances via SSM.
resource "aws_ssm_association" "adot_collector_linux" {
  name = "AWS-ConfigureAWSPackage"
  targets {
    key    = "InstanceIds"
    values = ["i-0a1b2c3d4e5f60002", "i-0a1b2c3d4e5f60003"]
  }
  parameters = {
    action = "Install"
    name   = "AWSDistroOTel-Collector"
  }
}`,
		},
		{
			Name:    "Install the ADOT Collector on the Windows EC2 instance",
			GroupID: "demo-compute-windows",
			AffectedResources: []string{
				"i-0a1b2c3d4e5f60005",
			},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Installs the ADOT Collector on the
# Windows instance via the same SSM package. NOTE: on Windows the collector
# config lives under C:\ProgramData\ — the agent-config path differs from
# the Linux /opt/aws/... layout.
resource "aws_ssm_association" "adot_collector_windows" {
  name = "AWS-ConfigureAWSPackage"
  targets {
    key    = "InstanceIds"
    values = ["i-0a1b2c3d4e5f60005"]
  }
  parameters = {
    action = "Install"
    name   = "AWSDistroOTel-Collector"
  }
}`,
		},
		{
			Name:    "Attach the ADOT Lambda layer to 2 uninstrumented Python functions",
			GroupID: "demo-functions",
			AffectedResources: []string{
				"arn:aws:lambda:us-east-1:000000000000:function:orders-processor",
				"arn:aws:lambda:us-east-1:000000000000:function:billing-cron",
			},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Attaches the ADOT Lambda layer +
# auto-instrumentation wrapper to the uninstrumented Python functions.
# Replace the layer ARN with the CURRENT version for your region from the
# AWS ADOT Lambda layer list — layer versions are region-specific and change.
resource "aws_lambda_function" "orders_processor" {
  # ...existing function config...
  layers = ["arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-python-amd64-ver-1-29-0:1"]
  environment {
    variables = {
      # Python uses /opt/otel-instrument (Node uses /opt/otel-handler).
      AWS_LAMBDA_EXEC_WRAPPER = "/opt/otel-instrument"
    }
  }
}`,
		},
		{
			Name:    "Enable Performance Insights + Enhanced Monitoring on the analytics RDS instance",
			GroupID: "demo-databases",
			AffectedResources: []string{
				"arn:aws:rds:us-east-1:000000000000:db:analytics-db",
			},
			InlineConfigSnippet: `# Sample remediation (demo inventory). Turns on both RDS observability
# levers on the analytics database. Enhanced Monitoring needs an IAM role
# with the AmazonRDSEnhancedMonitoringRole policy attached.
resource "aws_db_instance" "analytics_db" {
  # ...existing instance config...
  performance_insights_enabled = true
  monitoring_interval          = 60
  monitoring_role_arn          = aws_iam_role.rds_monitoring.arn
}`,
		},
	}
}
