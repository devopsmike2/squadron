// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"strings"
	"testing"
)

// TestPick_AWS_EC2_InstallsADOTNotCloudWatch guards the #111 fix: the EC2
// trace-emission pattern must install the ADOT Collector via the AWS-managed
// SSM Distributor package AWSDistroOTel-Collector — NOT AmazonCloudWatchAgent,
// which is a different agent that does not emit OpenTelemetry traces. The
// managed package also auto-selects arm64/amd64, so the snippet stays correct
// on Graviton instances.
func TestPick_AWS_EC2_InstallsADOTNotCloudWatch(t *testing.T) {
	p := Pick(RecommendationContext{Provider: "aws", Tier: "compute"}, "")

	if !strings.Contains(p.PrimaryTerraform, `name   = "AWSDistroOTel-Collector"`) {
		t.Errorf("EC2 snippet must install AWSDistroOTel-Collector; got:\n%s", p.PrimaryTerraform)
	}
	// The CloudWatch Agent package must not appear — it is not the ADOT
	// Collector and does not emit OTel traces.
	if strings.Contains(p.PrimaryTerraform, "AmazonCloudWatchAgent") {
		t.Errorf("EC2 snippet must NOT install AmazonCloudWatchAgent (conflates CW agent with ADOT); got:\n%s", p.PrimaryTerraform)
	}
	// The reasoning must not falsely equate the CloudWatch Agent with ADOT.
	if strings.Contains(p.Reasoning, "CloudWatch Agent (ADOT collector binary)") {
		t.Errorf("reasoning must not call the CloudWatch Agent the ADOT collector; got: %q", p.Reasoning)
	}
	if !strings.Contains(p.Reasoning, "ADOT Collector") {
		t.Errorf("reasoning should name the ADOT Collector; got: %q", p.Reasoning)
	}
}

// TestMalformedTraceparent_AWS_LambdaARN_HonestFraming guards the #109 fix:
// the deterministic AWS Lambda layer-ARN snippet is version-pinned and goes
// stale with every ADOT release, so it must carry the same honest-framing the
// LLM prompt mandates — a VERIFY annotation plus the authoritative source link
// — rather than presenting a frozen ARN as authoritative.
func TestMalformedTraceparent_AWS_LambdaARN_HonestFraming(t *testing.T) {
	p := PickMalformedTraceparentPattern(RecommendationContext{Provider: "aws", Tier: "compute"})
	tf := p.PrimaryTerraform

	if !strings.Contains(tf, "VERIFY") {
		t.Errorf("AWS Lambda ARN snippet must carry a VERIFY staleness annotation; got:\n%s", tf)
	}
	if !strings.Contains(tf, "https://aws-otel.github.io/docs/getting-started/lambda") {
		t.Errorf("AWS Lambda ARN snippet must link the authoritative ADOT layer source; got:\n%s", tf)
	}
	if !strings.Contains(tf, "STALE") {
		t.Errorf("AWS Lambda ARN snippet must flag the pinned value as likely stale; got:\n%s", tf)
	}
	// The ADOT publisher account segment should still be present (the snippet
	// is a real best-known starting point, just clearly marked non-authoritative).
	if !strings.Contains(tf, ":901920570463:layer:aws-otel-") {
		t.Errorf("AWS Lambda ARN snippet should still emit a best-known ADOT layer ARN; got:\n%s", tf)
	}
}

// TestPick_Azure_Compute_FlagsWindowsAgentVariant guards the #114 fix: the
// Azure Monitor Agent extension is OS-specific (AzureMonitorLinuxAgent vs
// AzureMonitorWindowsAgent), and installing the Linux agent on a Windows VM
// fails to provision. The deterministic snippet can't see the VM's OSFamily,
// so it must at least surface the Windows variant rather than silently
// hardcoding Linux.
func TestPick_Azure_Compute_FlagsWindowsAgentVariant(t *testing.T) {
	p := Pick(RecommendationContext{Provider: "azure", Tier: "compute", ResourceTFName: "prod"}, "")

	if !strings.Contains(p.PrimaryTerraform, "AzureMonitorWindowsAgent") {
		t.Errorf("Azure compute snippet must surface the Windows agent variant; got:\n%s", p.PrimaryTerraform)
	}
	if !strings.Contains(p.PrimaryTerraform, "azurerm_windows_virtual_machine") {
		t.Errorf("Azure compute snippet must mention azurerm_windows_virtual_machine for Windows VMs; got:\n%s", p.PrimaryTerraform)
	}
	if !strings.Contains(p.Reasoning, "AzureMonitorWindowsAgent") {
		t.Errorf("reasoning must flag the Windows agent swap; got: %q", p.Reasoning)
	}
}
