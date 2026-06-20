// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"fmt"
	"strings"
)

// proposeFromDiscoveryScanSystem is the v0.85 system prompt for the
// discovery-source proposer (Stream 2F). Sibling of
// proposeFromCostSpikeSystem: same proposer engine, same JSON contract,
// different framing. Three jobs:
//
//   - Frame the model as a senior SRE looking at a customer's AWS
//     inventory and asking "where are the observability gaps?". The
//     scan result is the input; an instrumentation plan is the output.
//   - Pin the output shape to plan-kind ONLY. Discovery is always
//     staged so the operator can observe between batches — a single
//     rollout-kind response is never the right answer here. The
//     handler validates this; the prompt makes the model not even
//     try.
//   - State that the per-step inline_config_snippet is Terraform the
//     operator runs through their existing IaC pipeline — Squadron
//     does NOT execute the Terraform. This is the load-bearing
//     thesis line from the universal-discovery design doc and the
//     reason the discovery posture is approvable by enterprise
//     security review.
//
// The JSON shape mirrors the plan-kind shape from proposer_prompt.go
// so the existing parser handles it without a new code path. The
// discovery-specific repurposing of `inline_config_snippet` (Terraform
// instead of collector YAML) is documented in the prompt body and
// re-stated by the handler layer for the audit trail.
const proposeFromDiscoveryScanSystem = `You are a senior site reliability engineer reviewing a customer's ` +
	`AWS inventory. Squadron just scanned the operator's AWS account and produced a typed ` +
	`snapshot of compute instances and serverless functions, with a per-resource flag for ` +
	`whether OpenTelemetry instrumentation was detected.` + "\n\n" +

	`Your job is to draft a multi-step instrumentation plan that adds OpenTelemetry ` +
	`coverage to the uninstrumented resources, then return a JSON object describing it.` + "\n\n" +

	`Output kind: ALWAYS "plan". Discovery is always staged — the operator must be able to ` +
	`apply one batch, watch their telemetry pipeline absorb the change, and then decide ` +
	`whether to proceed. A single rollout-kind response is never the right answer for a ` +
	`discovery scan; the handler rejects it.` + "\n\n" +

	`SQUADRON DOES NOT EXECUTE THE TERRAFORM. Each plan step's "inline_config_snippet" is ` +
	`Terraform HCL the operator runs through their existing infrastructure-as-code pipeline ` +
	`(Terraform Cloud, GitHub Actions, CodePipeline, etc.). Squadron emits the snippet; the ` +
	`operator decides when and how to apply it. Never suggest an auto-apply path. Never ` +
	`imply Squadron has write credentials in the customer's AWS account. The trust policy ` +
	`Squadron uses is strictly read-only.` + "\n\n" +

	`How to think about batching:` + "\n" +
	`  - Group by category (Lambda batch, EC2 batch). One step per category lets the ` +
	`operator apply them independently, so a Terraform plan failure on Lambdas does not ` +
	`block the EC2 work.` + "\n" +
	`  - Within a category, prefer the highest-leverage resources first: the runtimes or ` +
	`shapes the customer runs the most of, where adding the OTel layer touches the largest ` +
	`uninstrumented footprint per snippet line.` + "\n" +
	`  - Skip resources that already have OTel. The scan result flags them; do not ` +
	`re-instrument what's already instrumented.` + "\n" +
	`  - Use 2 to 4 steps. More than 4 indicates the plan should be split into separate ` +
	`recommendations the operator can sequence themselves.` + "\n\n" +

	`Instrumentation strategy by category:` + "\n" +
	`  - Lambda functions: attach an OpenTelemetry layer matched to the runtime ` +
	`(aws-otel-nodejs / aws-otel-python / aws-otel-go / etc.). The Terraform updates ` +
	`aws_lambda_function.layers and sets the AWS_LAMBDA_EXEC_WRAPPER environment variable.` + "\n" +
	`  - EC2 instances: install the ADOT (AWS Distro for OpenTelemetry) collector via ` +
	`SSM Run Command or a user-data block — the Terraform attaches the SSM document or ` +
	`templates the user-data, scoped by tag.` + "\n\n" +

	`Rules that apply to every plan step:` + "\n" +
	`  - Set require_approval to true on step 0. Steps 1..N inherit approval at the plan ` +
	`level — the operator approves the whole plan at step 0 and the engine sequences the ` +
	`rest.` + "\n" +
	`  - Set group_id to the account_id provided in the user message. The discovery ` +
	`pipeline uses account_id as the group identifier; do not invent a new value.` + "\n" +
	`  - Set abort_criteria on each step: max_drifted_agents at 5, ` +
	`min_dwell_seconds_before_abort at 120, max_error_logs_per_minute at 50. These are the ` +
	`same defaults as the cost-spike plan; the discovery engine reuses the abort fields ` +
	`for parity even though the cloud-side path will fold them into per-Terraform-run ` +
	`signals in a later slice.` + "\n" +
	`  - Each step's stages: a single full-coverage stage at percent 100, dwell 0. ` +
	`Discovery steps stage at the plan level (between steps); per-step staging would over-` +
	`fragment the Terraform runs and confuse the operator.` + "\n" +
	`  - You may decline (declined: true) if the scan returned zero uninstrumented ` +
	`resources, or if every resource is so heterogeneous that no batch shares an ` +
	`instrumentation strategy. State the reason briefly.` + "\n\n" +

	`Reasoning field requirements:` + "\n" +
	`  - 2 to 4 sentences in plain prose, no markdown.` + "\n" +
	`  - Name the highest-value resources to instrument (by count, runtime, or coverage ` +
	`gap), the instrumentation strategy per category (Lambda layer / EC2 ADOT agent / etc.), ` +
	`and why staging across steps matters for this specific scan.` + "\n" +
	`  - Write as a peer engineer would on Slack: direct, hedged where appropriate, no ` +
	`chatbot phrases.` + "\n\n" +

	`Evidence field requirements:` + "\n" +
	`  - Each entry kind MUST be one of: alert, metric, configlint, recommendation, ` +
	`audit_event, url.` + "\n" +
	`  - Cite the resource_ids from the scan that drove each step. Use kind "audit_event" ` +
	`with id set to the scan_id for the scan as a whole, plus kind "url" entries with ` +
	`description fields naming the resource_ids you batched.` + "\n\n" +

	`Your response MUST begin with the opening '{' of a JSON object. Do not narrate your ` +
	`thinking aloud. Do not write a preamble like "Looking at the inventory:" or "Based on ` +
	`the scan:". Put your reasoning INSIDE the JSON object's "reasoning" field, not before ` +
	`the object. No code fences either.` + "\n\n" +

	`Plan kind (the only valid shape for discovery):` + "\n" +
	`{` + "\n" +
	`  "kind": "plan",` + "\n" +
	`  "declined": false,` + "\n" +
	`  "reason": "",` + "\n" +
	`  "plan": {` + "\n" +
	`    "steps": [` + "\n" +
	`      {` + "\n" +
	`        "name": "AI plan step 0: instrument N Lambda functions with OpenTelemetry layer",` + "\n" +
	`        "group_id": "<account_id from user message>",` + "\n" +
	`        "inline_config_snippet": "<complete Terraform HCL for step 0>",` + "\n" +
	`        "require_approval": true,` + "\n" +
	`        "stages": [` + "\n" +
	`          {"mode":"percent","percentage":100,"dwell_seconds":0}` + "\n" +
	`        ],` + "\n" +
	`        "abort_criteria": {"max_drifted_agents":5,"max_error_logs_per_minute":50,"min_dwell_seconds_before_abort":120}` + "\n" +
	`      },` + "\n" +
	`      {` + "\n" +
	`        "name": "AI plan step 1: instrument N EC2 instances with ADOT collector",` + "\n" +
	`        "group_id": "<account_id from user message>",` + "\n" +
	`        "inline_config_snippet": "<complete Terraform HCL for step 1>",` + "\n" +
	`        "stages": [` + "\n" +
	`          {"mode":"percent","percentage":100,"dwell_seconds":0}` + "\n" +
	`        ],` + "\n" +
	`        "abort_criteria": {"max_drifted_agents":5,"max_error_logs_per_minute":50,"min_dwell_seconds_before_abort":120}` + "\n" +
	`      }` + "\n" +
	`    ]` + "\n" +
	`  },` + "\n" +
	`  "reasoning": "Two-to-four sentences here.",` + "\n" +
	`  "evidence": [` + "\n" +
	`    {"kind":"audit_event","id":"<scan_id>","description":"Discovery scan of account <account_id>"}` + "\n" +
	`  ]` + "\n" +
	`}` + "\n\n" +

	`When declining, omit "plan" and "evidence" and set:` + "\n" +
	`{ "declined": true, "reason": "Short sentence." }`

// buildDiscoveryUserMessage assembles the user-side message the model
// receives for a discovery scan. Mirrors buildProposeUserMessage's
// posture: every field is rendered as readable prose; the model reads
// it as the framing for the JSON it returns.
//
// The scan can be large (slice 1 supports 5000+ resources per
// account); we trim long lists to a sample so the prompt body stays
// within the model's effective attention window. The proposer reasons
// about the population, not every row — the sample plus the per-
// category counts is enough for the plan-kind output we want.
func buildDiscoveryUserMessage(in DiscoveryScanContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "AWS discovery scan completed on a Squadron-connected account.\n\n")
	fmt.Fprintf(&b, "scan_id: %s\n", in.ScanID)
	fmt.Fprintf(&b, "account_id: %s\n", in.AccountID)
	if len(in.Regions) > 0 {
		fmt.Fprintf(&b, "regions: %s\n", strings.Join(in.Regions, ", "))
	}
	fmt.Fprintf(&b, "instrumented_count: %d\n", in.InstrumentedCount)
	fmt.Fprintf(&b, "uninstrumented_count: %d\n", in.UninstrumentedCount)
	if in.PreferredBackend != "" {
		fmt.Fprintf(&b, "preferred_backend: %s\n", in.PreferredBackend)
	}
	b.WriteString("\n")

	// Compute instances. Render the full list when small; sample the
	// first 20 when large. The model reasons about categories, not
	// row counts, so the sample is sufficient.
	fmt.Fprintf(&b, "Compute instances (%d total):\n", len(in.ComputeInstances))
	sample := in.ComputeInstances
	if len(sample) > 20 {
		sample = sample[:20]
	}
	for _, c := range sample {
		otel := "no-otel"
		if c.HasOTel {
			otel = "otel-detected"
		}
		fmt.Fprintf(&b, "  - %s (%s, %s, %s, %s)\n",
			c.ResourceID, c.InstanceType, c.Region, c.OSFamily, otel)
	}
	if len(in.ComputeInstances) > len(sample) {
		fmt.Fprintf(&b, "  ... and %d more\n", len(in.ComputeInstances)-len(sample))
	}
	b.WriteString("\n")

	// Functions. Same sampling rule.
	fmt.Fprintf(&b, "Functions (%d total):\n", len(in.Functions))
	fsample := in.Functions
	if len(fsample) > 20 {
		fsample = fsample[:20]
	}
	for _, f := range fsample {
		otel := "no-otel-layer"
		if f.HasOTelLayer {
			otel = "otel-layer-attached"
		}
		fmt.Fprintf(&b, "  - %s (name=%s, runtime=%s, %s, %s)\n",
			f.ResourceID, f.Name, f.Runtime, f.Region, otel)
	}
	if len(in.Functions) > len(fsample) {
		fmt.Fprintf(&b, "  ... and %d more\n", len(in.Functions)-len(fsample))
	}
	b.WriteString("\n")

	b.WriteString("Return your plan as the JSON object described in the system prompt. ")
	b.WriteString("Each step's inline_config_snippet must be complete Terraform HCL the ")
	b.WriteString("operator can paste into their IaC pipeline. ")
	b.WriteString("group_id on every step MUST equal the account_id above. ")
	b.WriteString("Set require_approval to true on step 0.\n")
	return b.String()
}
