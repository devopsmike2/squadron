// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"strings"
	"testing"
)

// TestSamplingRate_SetsSamplerNotJustArg guards the audit fix: setting
// OTEL_TRACES_SAMPLER_ARG without OTEL_TRACES_SAMPLER is a silent no-op — the
// OTel SDK's default sampler (parentbased_always_on) ignores the arg, so the
// function keeps sampling at 100%. Every surface must set a ratio sampler too.
func TestSamplingRate_SetsSamplerNotJustArg(t *testing.T) {
	cases := []struct{ provider, surface string }{
		{"aws", ""},
		{"gcp", "cloudrun"},
		{"gcp", "cloudfunctions"},
		{"azure", ""},
		{"oci", ""},
	}
	for _, c := range cases {
		tf := PickSamplingRateTerraform(c.provider, c.surface, "fn")
		if !strings.Contains(tf, "OTEL_TRACES_SAMPLER_ARG") {
			t.Errorf("%s/%s: expected OTEL_TRACES_SAMPLER_ARG; got:\n%s", c.provider, c.surface, tf)
		}
		if !strings.Contains(tf, "parentbased_traceidratio") {
			t.Errorf("%s/%s: OTEL_TRACES_SAMPLER_ARG without a ratio OTEL_TRACES_SAMPLER is a no-op; got:\n%s", c.provider, c.surface, tf)
		}
	}
}

// TestAzureK8s_AnnotationsAllowedIsString guards the audit fix: azurerm's
// monitor_metrics.annotations_allowed is a comma-separated STRING; a list
// value fails terraform plan with a type error.
func TestAzureK8s_AnnotationsAllowedIsString(t *testing.T) {
	p := Pick(RecommendationContext{Provider: "azure", Tier: "k8s", ResourceTFName: "prod"}, "")
	if !strings.Contains(p.PrimaryTerraform, `annotations_allowed = "service.name,service.instance.id"`) {
		t.Errorf("monitor_metrics.annotations_allowed must be a comma-separated string; got:\n%s", p.PrimaryTerraform)
	}
	if strings.Contains(p.PrimaryTerraform, `["service.name"`) {
		t.Errorf("annotations_allowed must not be a list (type error in azurerm); got:\n%s", p.PrimaryTerraform)
	}
}

// TestOCICompute_NoFabricatedInstallerURL guards the audit fix: the OCI APM
// agent has no public curl|bash installer; the prior snippet pointed at a
// fabricated endpoint whose apply would fail. The snippet must reflect the
// real Java-agent provisioning flow instead.
func TestOCICompute_NoFabricatedInstallerURL(t *testing.T) {
	p := Pick(RecommendationContext{Provider: "oci", Tier: "compute", ResourceTFName: "vm"}, "")
	if strings.Contains(p.PrimaryTerraform, "apm-agent-installer.oraclecloud.com") {
		t.Errorf("snippet must not reference the fabricated installer URL; got:\n%s", p.PrimaryTerraform)
	}
	if !strings.Contains(p.PrimaryTerraform, "provision-agent") {
		t.Errorf("snippet should reflect the real APM Java-agent provisioning flow; got:\n%s", p.PrimaryTerraform)
	}
	if !strings.Contains(p.Reasoning, "no public curl|bash URL exists") {
		t.Errorf("reasoning should flag that no public installer URL exists; got: %q", p.Reasoning)
	}
}

// TestCloudRunColdStart_MinScaleOnTemplate guards the audit fix: the
// autoscaling.knative.dev/minScale annotation is revision-scoped and must sit
// on template.metadata.annotations — on the service-level metadata Cloud Run
// ignores it, so the warm-floor recommendation silently does nothing.
func TestCloudRunColdStart_MinScaleOnTemplate(t *testing.T) {
	p := PickCloudRunColdStartPattern(RecommendationContext{ResourceTFName: "svc"})
	tf := p.PrimaryTerraform
	idxTemplate := strings.Index(tf, "template {")
	idxMinScale := strings.Index(tf, "autoscaling.knative.dev/minScale")
	if idxTemplate == -1 || idxMinScale == -1 || idxMinScale < idxTemplate {
		t.Errorf("minScale must appear inside the template block; got:\n%s", tf)
	}
}

// TestEventGridDiagnostics_ValidLogCategories guards the audit fix: Event Grid
// topics only support DeliveryFailures / PublishFailures / DataPlaneRequests.
// PublishSuccess and DeliverySuccess are not valid categories and fail apply.
func TestEventGridDiagnostics_ValidLogCategories(t *testing.T) {
	tf, _ := PickEventGridDiagnosticsPattern(RecommendationContext{ResourceTFName: "topic"})
	for _, bad := range []string{`category = "PublishSuccess"`, `category = "DeliverySuccess"`} {
		if strings.Contains(tf, bad) {
			t.Errorf("Event Grid snippet must not use invalid category %q; got:\n%s", bad, tf)
		}
	}
	for _, good := range []string{"DeliveryFailures", "PublishFailures"} {
		if !strings.Contains(tf, good) {
			t.Errorf("Event Grid snippet should include valid category %q; got:\n%s", good, tf)
		}
	}
}
