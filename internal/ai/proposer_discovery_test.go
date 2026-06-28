// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// discoveryContextForTest returns a DiscoveryScanContext with a small
// but realistic AWS-shaped inventory. Used across the happy-path and
// validation-error tests so the prompt body is roughly the same shape
// in every test and any model-side reasoning isn't hidden by a
// trivially-small input.
func discoveryContextForTest() *DiscoveryScanContext {
	return &DiscoveryScanContext{
		ScanID:    "scan-abc123",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		ComputeInstances: []ComputeResourceCandidate{
			{ResourceID: "i-aaa", InstanceType: "t3.micro", Region: "us-east-1", OSFamily: "linux", HasOTel: false},
			{ResourceID: "i-bbb", InstanceType: "m5.large", Region: "us-east-1", OSFamily: "linux", HasOTel: true},
		},
		Functions: []FunctionResourceCandidate{
			{ResourceID: "arn:aws:lambda:us-east-1:123:function:hello", Name: "hello", Runtime: "python3.11", Region: "us-east-1", HasOTelLayer: false},
			{ResourceID: "arn:aws:lambda:us-east-1:123:function:goodbye", Name: "goodbye", Runtime: "nodejs20.x", Region: "us-east-1", HasOTelLayer: false},
		},
		InstrumentedCount:   1,
		UninstrumentedCount: 3,
	}
}

// TestProposeFromDiscoveryScan_HappyPath: the fake server returns a
// well-formed plan with 2 steps with non-empty Terraform snippets.
// Verify the result rides through the parser without validation
// complaining and that the Plan candidate carries the steps the model
// emitted.
//
// v0.89.4 (#611) — also pins the proposer's per-step
// AffectedResources field round-trip. The prompt teaches the model to
// emit a JSON array of resource identifiers per step; the parser
// must thread it onto each PlanStepCandidate so the handler layer
// can copy it onto the recommendation envelope and the Open-PR
// backend can build an accurate PR title + body.
func TestProposeFromDiscoveryScan_HappyPath(t *testing.T) {
	reply := anthropicReply(`{
  "kind": "plan",
  "declined": false,
  "reason": "",
  "plan": {
    "steps": [
      {
        "name": "AI plan step 0: instrument 2 Lambda functions with OpenTelemetry layer",
        "group_id": "123456789012",
        "inline_config_snippet": "resource \"aws_lambda_function\" \"hello\" {\n  function_name = \"hello\"\n  layers = [\"arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-python-amd64-ver-1-21-0:1\"]\n}\n",
        "affected_resources": ["arn:aws:lambda:us-east-1:123:function:hello","arn:aws:lambda:us-east-1:123:function:goodbye"],
        "require_approval": true,
        "stages": [{"mode":"percent","percentage":100,"dwell_seconds":0}],
        "abort_criteria": {"max_drifted_agents":5,"max_error_logs_per_minute":50,"min_dwell_seconds_before_abort":120}
      },
      {
        "name": "AI plan step 1: instrument 1 EC2 instance with ADOT collector",
        "group_id": "123456789012",
        "inline_config_snippet": "resource \"aws_ssm_association\" \"adot_install\" {\n  name = \"AWS-RunShellScript\"\n  targets {\n    key = \"InstanceIds\"\n    values = [\"i-aaa\"]\n  }\n}\n",
        "affected_resources": ["i-aaa"],
        "stages": [{"mode":"percent","percentage":100,"dwell_seconds":0}],
        "abort_criteria": {"max_drifted_agents":5,"max_error_logs_per_minute":50,"min_dwell_seconds_before_abort":120}
      }
    ]
  },
  "reasoning": "Two Lambdas plus one EC2 instance lack OTel. Step 0 attaches the layer to both Lambdas in one Terraform run; step 1 covers the EC2 via SSM so the operator can observe the Lambdas first before touching VMs.",
  "evidence": [
    {"kind":"audit_event","id":"scan-abc123","description":"Discovery scan of account 123456789012"}
  ]
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	res, err := svc.ProposeFromDiscoveryScan(context.Background(), discoveryContextForTest())
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.False(t, res.Declined, "well-formed plan should not be declined")
	assert.Equal(t, ProposalKindPlan, res.Kind)
	require.Len(t, res.Plan.Steps, 2)

	step0 := res.Plan.Steps[0]
	assert.Equal(t, "123456789012", step0.GroupID, "step group_id should match context account_id")
	assert.Contains(t, step0.InlineConfigSnippet, "aws_lambda_function", "step 0 Terraform should reference the Lambda resource")
	assert.True(t, step0.RequireApproval, "discovery plans must require approval at step 0")
	// v0.89.4 (#611) — AffectedResources rides through the parser.
	assert.Equal(t,
		[]string{
			"arn:aws:lambda:us-east-1:123:function:hello",
			"arn:aws:lambda:us-east-1:123:function:goodbye",
		},
		step0.AffectedResources,
		"step 0 affected_resources should round-trip from the model output",
	)

	step1 := res.Plan.Steps[1]
	assert.Equal(t, "123456789012", step1.GroupID)
	assert.Contains(t, step1.InlineConfigSnippet, "aws_ssm_association", "step 1 Terraform should reference the SSM document")
	// v0.89.4 (#611) — EC2 uses the canonical instance id rather
	// than an ARN (no Lambda-style ARN exists for raw EC2); the
	// proposer prompt explicitly allows the canonical id fallback.
	assert.Equal(t, []string{"i-aaa"}, step1.AffectedResources,
		"step 1 affected_resources should be the EC2 instance id")

	assert.Contains(t, res.Reasoning, "Lambdas")
	require.Len(t, res.Evidence, 1)
	assert.Equal(t, "audit_event", res.Evidence[0].Kind)
	assert.Equal(t, "scan-abc123", res.Evidence[0].ID)

	// Metering should round-trip from the fake server's usage block.
	assert.Equal(t, 123, res.TokensIn)
	assert.Equal(t, 456, res.TokensOut)
}

// TestProposeFromDiscoveryScan_OmitsAffectedResources_StillParses
// pins the backward-compat path: a cold-start model that emits a
// plan step WITHOUT the affected_resources field is not an error.
// The PlanStepCandidate's AffectedResources stays nil, the handler
// layer copies that through to the recommendation envelope, the UI
// sends an empty array on Open PR, and the PR title falls back to
// "for 0 resources" rather than failing the request. v0.89.4 (#611).
func TestProposeFromDiscoveryScan_OmitsAffectedResources_StillParses(t *testing.T) {
	reply := anthropicReply(`{
  "kind": "plan",
  "declined": false,
  "plan": {
    "steps": [
      {
        "name": "AI plan step 0: instrument 2 Lambdas",
        "group_id": "123456789012",
        "inline_config_snippet": "resource \"aws_lambda_function\" \"x\" {\n  layers = [\"x\"]\n}\n",
        "require_approval": true,
        "stages": [{"mode":"percent","percentage":100,"dwell_seconds":0}],
        "abort_criteria": {"max_drifted_agents":5,"max_error_logs_per_minute":50,"min_dwell_seconds_before_abort":120}
      }
    ]
  },
  "reasoning": "..."
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	res, err := svc.ProposeFromDiscoveryScan(context.Background(), discoveryContextForTest())
	require.NoError(t, err, "missing affected_resources is backward-compat, not an error")
	require.NotNil(t, res)
	require.Len(t, res.Plan.Steps, 1)
	assert.Empty(t, res.Plan.Steps[0].AffectedResources,
		"AffectedResources is empty when the model didn't emit the field")
}

// TestProposeFromDiscoveryScan_Declined: the model said no productive
// plan exists. ProposalResult.Declined is true; no plan body; no
// error.
func TestProposeFromDiscoveryScan_Declined(t *testing.T) {
	reply := anthropicReply(`{
  "kind": "plan",
  "declined": true,
  "reason": "Every scanned resource already has OTel coverage."
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	res, err := svc.ProposeFromDiscoveryScan(context.Background(), discoveryContextForTest())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Declined)
	assert.Contains(t, res.Reason, "already has OTel coverage")
	assert.Empty(t, res.Plan.Steps, "no plan when declined")
}

// TestProposeFromDiscoveryScan_RejectsRolloutKind: the model returned a
// rollout-kind response. Discovery is plan-only; surface the
// validation error so the handler can refuse to convert it into
// recommendations.
func TestProposeFromDiscoveryScan_RejectsRolloutKind(t *testing.T) {
	reply := anthropicReply(`{
  "kind": "rollout",
  "declined": false,
  "proposal": {
    "name": "AI: instrument something",
    "group_id": "123456789012",
    "target_config_id": "cfg-abc",
    "require_approval": true,
    "stages": [{"mode":"percentage","percentage":10,"dwell_seconds":600}],
    "abort_criteria": {}
  },
  "reasoning": "Should not happen."
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	_, err := svc.ProposeFromDiscoveryScan(context.Background(), discoveryContextForTest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plan-only")
}

// TestProposeFromDiscoveryScan_RejectsEmptyTerraform: the model
// returned a plan with an empty inline_config_snippet. The validator
// rejects it so the handler doesn't surface a recommendation with no
// IaC source.
func TestProposeFromDiscoveryScan_RejectsEmptyTerraform(t *testing.T) {
	reply := anthropicReply(`{
  "kind": "plan",
  "declined": false,
  "plan": {
    "steps": [
      {
        "name": "AI plan step 0: empty",
        "group_id": "123456789012",
        "inline_config_snippet": "   ",
        "stages": [{"mode":"percent","percentage":100,"dwell_seconds":0}],
        "abort_criteria": {}
      }
    ]
  },
  "reasoning": "..."
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	_, err := svc.ProposeFromDiscoveryScan(context.Background(), discoveryContextForTest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inline_config_snippet")
}

// TestProposeFromDiscoveryScan_RejectsGroupMismatch: the model
// returned a plan whose step group_id doesn't equal the context's
// account_id. Discovery uses account_id as the group identifier; a
// mismatch means the model invented a value.
func TestProposeFromDiscoveryScan_RejectsGroupMismatch(t *testing.T) {
	reply := anthropicReply(`{
  "kind": "plan",
  "declined": false,
  "plan": {
    "steps": [
      {
        "name": "AI plan step 0",
        "group_id": "WRONG-ACCOUNT",
        "inline_config_snippet": "resource \"x\" \"y\" {}\n",
        "stages": [{"mode":"percent","percentage":100,"dwell_seconds":0}],
        "abort_criteria": {}
      }
    ]
  },
  "reasoning": "..."
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	_, err := svc.ProposeFromDiscoveryScan(context.Background(), discoveryContextForTest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "group_id")
}

// TestProposeFromDiscoveryScan_DatabaseValidation pins the slice 2
// pre-call validator on DatabaseResourceCandidate rows. A row missing
// resource_id or engine is a converter bug; the proposer call fails
// loudly rather than threading the half-populated row into the prompt
// body, where the model's reasoning would surface "the database with
// empty ARN" — operator-unactionable.
func TestProposeFromDiscoveryScan_DatabaseValidation(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	t.Run("missing resource_id is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.Databases = []DatabaseResourceCandidate{{Engine: "postgres"}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resource_id")
	})

	t.Run("missing engine is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.Databases = []DatabaseResourceCandidate{{ResourceID: "arn:aws:rds:us-east-1:123:db:x"}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "engine")
	})
}

// TestBuildDiscoveryUserMessage_Databases verifies the slice 2 user
// prompt builder threads each Database field into the message body
// and renders the four coverage-state shorthand strings the system
// prompt teaches: covered / pi-only / em-only / uncovered.
func TestBuildDiscoveryUserMessage_Databases(t *testing.T) {
	ctx := discoveryContextForTest()
	ctx.Databases = []DatabaseResourceCandidate{
		{ResourceID: "arn:aws:rds:us-east-1:123:db:db-covered", Engine: "postgres", EngineVersion: "15.4", InstanceClass: "db.r6g.large", PerformanceInsightsEnabled: true, EnhancedMonitoringEnabled: true, Region: "us-east-1"},
		{ResourceID: "arn:aws:rds:us-east-1:123:db:db-pi-only", Engine: "postgres", EngineVersion: "15.4", InstanceClass: "db.r6g.large", PerformanceInsightsEnabled: true, EnhancedMonitoringEnabled: false, Region: "us-east-1"},
		{ResourceID: "arn:aws:rds:us-east-1:123:db:db-em-only", Engine: "mysql", EngineVersion: "8.0", InstanceClass: "db.t3.medium", PerformanceInsightsEnabled: false, EnhancedMonitoringEnabled: true, Region: "us-east-1"},
		{ResourceID: "arn:aws:rds:us-east-1:123:db:db-uncovered", Engine: "aurora-postgresql", EngineVersion: "14.7", InstanceClass: "db.r6g.large", PerformanceInsightsEnabled: false, EnhancedMonitoringEnabled: false, Region: "us-east-1"},
	}
	msg := buildDiscoveryUserMessage(*ctx)

	for _, want := range []string{
		"Databases (4 total)",
		"db-covered", "covered",
		"db-pi-only", "pi-only",
		"db-em-only", "em-only",
		"db-uncovered", "uncovered",
		"postgres", "aurora-postgresql", "mysql",
		"db.r6g.large",
	} {
		assert.Contains(t, msg, want, "prompt should include %q", want)
	}
}

// TestProposeFromDiscoveryScan_ObjectStoreValidation pins the slice
// 3a (v0.88.0) pre-call validator on ObjectStoreCandidate rows. A
// row missing resource_id or region is a converter bug; the
// proposer call fails loudly rather than threading the half-populated
// row into the prompt body. Same posture as the v0.87 database
// validator.
func TestProposeFromDiscoveryScan_ObjectStoreValidation(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	t.Run("missing resource_id is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.ObjectStores = []ObjectStoreCandidate{{Region: "us-east-1"}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resource_id")
	})

	t.Run("missing region is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.ObjectStores = []ObjectStoreCandidate{{ResourceID: "prod-bucket"}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "region")
	})
}

// TestProposeFromDiscoveryScan_LoadBalancerValidation pins the slice
// 3a (v0.88.0) pre-call validator on LoadBalancerCandidate rows. A
// row missing resource_id, name, or type is a converter bug.
func TestProposeFromDiscoveryScan_LoadBalancerValidation(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	t.Run("missing resource_id is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.LoadBalancers = []LoadBalancerCandidate{{Name: "alb", Type: "application"}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resource_id")
	})
	t.Run("missing name is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.LoadBalancers = []LoadBalancerCandidate{{
			ResourceID: "arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/x/y",
			Type:       "application",
		}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})
	t.Run("missing type is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.LoadBalancers = []LoadBalancerCandidate{{
			ResourceID: "arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/x/y",
			Name:       "x",
		}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "type")
	})
}

// TestBuildDiscoveryUserMessage_ObjectStores_LoadBalancers verifies
// the slice 3a user prompt builder threads each ObjectStore + LB
// field into the message body and renders the coverage shorthand
// strings the system prompt teaches (covered / uncovered) for both
// categories. Pins the ALB→S3 cross-reference rendering: when an
// ALB has access_logs_s3_bucket populated, the bucket name appears
// in the rendered row so the model can match it against the
// inventory.
func TestBuildDiscoveryUserMessage_ObjectStores_LoadBalancers(t *testing.T) {
	ctx := discoveryContextForTest()
	ctx.ObjectStores = []ObjectStoreCandidate{
		{ResourceID: "prod-data", Region: "us-east-1", ServerAccessLoggingEnabled: true},
		{ResourceID: "staging-data", Region: "us-east-1", ServerAccessLoggingEnabled: false},
	}
	ctx.LoadBalancers = []LoadBalancerCandidate{
		{
			ResourceID: "arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/api-prod/abc",
			Name:       "api-prod", Type: "application", Scheme: "internet-facing",
			AccessLogsEnabled: true, AccessLogsS3Bucket: "prod-logs", Region: "us-east-1",
		},
		{
			ResourceID: "arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/api-staging/def",
			Name:       "api-staging", Type: "application", Scheme: "internal",
			AccessLogsEnabled: false, Region: "us-east-1",
		},
	}
	msg := buildDiscoveryUserMessage(*ctx)

	for _, want := range []string{
		"Object stores (2 total)",
		"prod-data", "staging-data",
		"covered", "uncovered",
		"Load balancers (2 total)",
		"api-prod", "api-staging",
		"application", "internet-facing", "internal",
		// Cross-reference rendering: the ALB's currently-configured
		// access-logs target bucket appears in the rendered row so
		// the model can decide whether to re-recommend.
		"logs-to=prod-logs",
	} {
		assert.Contains(t, msg, want, "prompt should include %q", want)
	}
}

// TestProposeFromDiscoveryScan_Disabled documents the gate: a service
// constructed without an API key short-circuits to ErrDisabled so
// callers don't have to nil-check the service.
func TestProposeFromDiscoveryScan_Disabled(t *testing.T) {
	svc := NewService(Config{Enabled: false}, zap.NewNop())
	_, err := svc.ProposeFromDiscoveryScan(context.Background(), discoveryContextForTest())
	require.ErrorIs(t, err, ErrDisabled)
}

// TestProposeFromDiscoveryScan_MissingRequiredFields documents the
// pre-call validation: scan_id and account_id are required.
func TestProposeFromDiscoveryScan_MissingRequiredFields(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	_, err := svc.ProposeFromDiscoveryScan(context.Background(), &DiscoveryScanContext{AccountID: "a"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scan_id")

	_, err = svc.ProposeFromDiscoveryScan(context.Background(), &DiscoveryScanContext{ScanID: "s"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account_id")

	_, err = svc.ProposeFromDiscoveryScan(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required")
}

// TestBuildDiscoveryUserMessage verifies the prompt builder threads
// every supplied scan field into the message body. The handler relies
// on these markers being present when constructing the prompt; the
// model in turn cites these values back via the `evidence` array.
func TestBuildDiscoveryUserMessage(t *testing.T) {
	ctx := discoveryContextForTest()
	ctx.PreferredBackend = "honeycomb"
	msg := buildDiscoveryUserMessage(*ctx)

	for _, want := range []string{
		"scan-abc123",
		"123456789012",
		"us-east-1",
		"instrumented_count: 1",
		"uninstrumented_count: 3",
		"preferred_backend: honeycomb",
		"i-aaa", "i-bbb",
		"arn:aws:lambda:us-east-1:123:function:hello",
		"python3.11",
		"nodejs20.x",
		"MUST equal",
	} {
		assert.Contains(t, msg, want, "prompt should include %q", want)
	}
}

// TestProposeFromDiscoveryScanSystemPrompt_TeachesAffectedResources
// pins the v0.89.4 (#611) addition to the prompt: the system message
// must explicitly tell the model to emit a per-step
// affected_resources array, both as a rule line and in the JSON
// example skeleton. A regression that drops the prompt mention is
// silently a "for 0 resources" PR title in production.
func TestProposeFromDiscoveryScanSystemPrompt_TeachesAffectedResources(t *testing.T) {
	for _, want := range []string{
		// Rule-line mention so the model knows the field is required.
		`Set "affected_resources" on every step`,
		// JSON-shape example so the model knows where to put it.
		`"affected_resources":`,
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt should teach affected_resources: %q", want)
	}
}

// TestProposeFromDiscoveryScan_RequestsProposerMaxTokens pins the same
// invariant the cost-spike test pins (#550): the proposer call must
// carry the per-call MaxTokens override (ProposerMaxTokens — was 4096
// in v0.82, bumped to 8192 in v0.88.2 for slice 3a discovery output
// per #597), not
// the global s.cfg.MaxTokens default (1024). Discovery plans emit
// Terraform per step — at least as token-heavy as a collector YAML —
// so the override matters here for the same reason it matters for
// cost spikes.
func TestProposeFromDiscoveryScan_RequestsProposerMaxTokens(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicReply(`{"kind":"plan","declined":true,"reason":"test"}`))
	}))
	defer srv.Close()

	// Configure with the small global default (1024) so we can tell
	// the per-call override is what's landing on the wire and not the
	// global setting bleeding in.
	svc := NewService(Config{
		Enabled:    true,
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		MergeModel: "claude-sonnet-4-6",
		MaxTokens:  1024,
	}, zap.NewNop())

	_, err := svc.ProposeFromDiscoveryScan(context.Background(), discoveryContextForTest())
	require.NoError(t, err)

	gotMaxTokens, ok := captured["max_tokens"].(float64)
	require.True(t, ok, "request should carry max_tokens; got %v", captured)
	assert.Equal(t, float64(ProposerMaxTokens), gotMaxTokens,
		"discovery proposer must use ProposerMaxTokens (%d), not the global s.cfg.MaxTokens (1024)",
		ProposerMaxTokens)
	assert.Equal(t, float64(8192), gotMaxTokens,
		"ProposerMaxTokens itself should stay at 8192 unless we also extend docs/ai-features.md")
}

// TestProposeFromDiscoveryScan_ClusterValidation pins the slice 3b
// (v0.89.0) pre-call validator on ClusterCandidate rows. A row
// missing resource_id or name is a converter bug — the proposer
// cites them back via the evidence array, and an empty value would
// surface in the model's reasoning as an unactionable row.
// ControlPlaneLogging and AddonNames are NOT enforced because the
// "uncovered" signal IS an empty-or-missing axis; the validator
// only enforces identifier fields.
func TestProposeFromDiscoveryScan_ClusterValidation(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	t.Run("missing resource_id is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.Clusters = []ClusterCandidate{{Name: "x"}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resource_id")
	})
	t.Run("missing name is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.Clusters = []ClusterCandidate{{
			ResourceID: "arn:aws:eks:us-east-1:123:cluster/x",
		}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})
}

// TestProposeFromDiscoveryScan_DynamoDBValidation pins the slice 4
// (v0.89.6) pre-call validator on DynamoDBTableCandidate rows. A
// row missing resource_id or name is a converter bug; the proposer
// cites them back via the evidence array.
// ContributorInsightsStatus is NOT enforced because the
// "uncovered" signal IS an empty-or-non-ENABLED status; the
// validator only enforces identifier fields. Mirrors the v0.89.0
// EKS cluster validator.
func TestProposeFromDiscoveryScan_DynamoDBValidation(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	t.Run("missing resource_id is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.DynamoDBTables = []DynamoDBTableCandidate{{Name: "x"}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resource_id")
	})
	t.Run("missing name is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.DynamoDBTables = []DynamoDBTableCandidate{{
			ResourceID: "arn:aws:dynamodb:us-east-1:123:table/x",
		}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})
}

// TestBuildDiscoveryUserMessage_DynamoDBTables verifies the slice 4
// user-prompt builder threads each DynamoDBTableCandidate field
// into the rendered message body and emits the three-state
// coverage shorthand the prompt body teaches (covered /
// uncovered / unknown — UNKNOWN for the scanner's AccessDenied
// fallback). Pins the rule rendering so a future prompt edit
// can't silently regress the framing.
func TestBuildDiscoveryUserMessage_DynamoDBTables(t *testing.T) {
	ctx := discoveryContextForTest()
	ctx.DynamoDBTables = []DynamoDBTableCandidate{
		{
			ResourceID:                "arn:aws:dynamodb:us-east-1:123:table/orders",
			Name:                      "orders",
			BillingMode:               "PAY_PER_REQUEST",
			ContributorInsightsStatus: "ENABLED",
			Region:                    "us-east-1",
		},
		{
			ResourceID:                "arn:aws:dynamodb:us-east-1:123:table/events",
			Name:                      "events",
			BillingMode:               "PROVISIONED",
			ContributorInsightsStatus: "DISABLED",
			Region:                    "us-east-1",
		},
		{
			ResourceID:                "arn:aws:dynamodb:us-east-1:123:table/legacy",
			Name:                      "legacy",
			ContributorInsightsStatus: "UNKNOWN",
			Region:                    "us-east-1",
		},
	}
	got := buildDiscoveryUserMessage(*ctx)
	assert.Contains(t, got, "DynamoDB tables (3 total):")
	assert.Contains(t, got, "orders")
	assert.Contains(t, got, "events")
	assert.Contains(t, got, "legacy")
	assert.Contains(t, got, "PAY_PER_REQUEST")
	assert.Contains(t, got, "PROVISIONED")
	assert.Contains(t, got, "ci=ENABLED")
	assert.Contains(t, got, "ci=DISABLED")
	assert.Contains(t, got, "ci=UNKNOWN")
	assert.Contains(t, got, "covered")
	assert.Contains(t, got, "uncovered")
	assert.Contains(t, got, "unknown")
}

// TestProposeFromDiscoveryScanSystemPrompt_TeachesDynamoDBRule pins
// the slice 4 (v0.89.6) prompt extension: the system message must
// teach the model the single-axis rule on Contributor Insights AND
// the resource_kind name AND the canonical Terraform shape AND the
// SDK-side limitation. A regression that drops any of these is
// silently a "DynamoDB recommendations don't ship" failure mode in
// production.
func TestProposeFromDiscoveryScanSystemPrompt_TeachesDynamoDBRule(t *testing.T) {
	for _, want := range []string{
		"CONTRIBUTOR INSIGHTS",
		"contributor_insights_status",
		`"ENABLED"`,
		"aws_dynamodb_contributor_insights",
		// SDK-side limitation must appear verbatim-ish in the
		// prompt so the model can hedge in its reasoning.
		"SDK-side OpenTelemetry",
		"cloud-API-only scanning",
		// The four-action IAM list is named so the model knows
		// what's read-only.
		"dynamodb:ListTables",
		"dynamodb:DescribeContributorInsights",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt should teach DynamoDB rule: %q", want)
	}
}

// TestProposeFromDiscoveryScan_ECSValidation pins the slice 5
// (v0.89.10) pre-call validator on ECSClusterCandidate rows. A row
// missing arn or name is a converter bug; the proposer cites them
// back via the evidence array. ContainerInsightsStatus is NOT
// enforced because the "uncovered" signal IS an
// empty-or-non-"enabled" status; the validator only enforces
// identifier fields. Mirrors the slice 4 DynamoDB validator.
func TestProposeFromDiscoveryScan_ECSValidation(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	t.Run("missing arn is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.ECSClusters = []ECSClusterCandidate{{Name: "x"}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "arn")
	})
	t.Run("missing name is rejected", func(t *testing.T) {
		ctx := discoveryContextForTest()
		ctx.ECSClusters = []ECSClusterCandidate{{
			ARN: "arn:aws:ecs:us-east-1:123:cluster/x",
		}}
		_, err := svc.ProposeFromDiscoveryScan(context.Background(), ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})
}

// TestBuildDiscoveryUserMessage_ECSClusters verifies the slice 5
// user-prompt builder threads each ECSClusterCandidate field into
// the rendered message body and emits the three-state coverage
// shorthand the prompt body teaches (covered / uncovered / unknown
// — UNKNOWN for the scanner's fallback). Pins the rule rendering so
// a future prompt edit can't silently regress the framing.
func TestBuildDiscoveryUserMessage_ECSClusters(t *testing.T) {
	ctx := discoveryContextForTest()
	ctx.ECSClusters = []ECSClusterCandidate{
		{
			ARN:                     "arn:aws:ecs:us-east-1:123:cluster/prod",
			Name:                    "prod",
			Status:                  "ACTIVE",
			ContainerInsightsStatus: "enabled",
			RunningTasksCount:       42,
			ActiveServicesCount:     7,
			Region:                  "us-east-1",
		},
		{
			ARN:                     "arn:aws:ecs:us-east-1:123:cluster/staging",
			Name:                    "staging",
			Status:                  "ACTIVE",
			ContainerInsightsStatus: "disabled",
			RunningTasksCount:       4,
			ActiveServicesCount:     1,
			Region:                  "us-east-1",
		},
		{
			ARN:                     "arn:aws:ecs:us-east-1:123:cluster/legacy",
			Name:                    "legacy",
			Status:                  "ACTIVE",
			ContainerInsightsStatus: "UNKNOWN",
			Region:                  "us-east-1",
		},
	}
	got := buildDiscoveryUserMessage(*ctx)
	assert.Contains(t, got, "ECS clusters (3 total):")
	assert.Contains(t, got, "prod")
	assert.Contains(t, got, "staging")
	assert.Contains(t, got, "legacy")
	assert.Contains(t, got, "ci=enabled")
	assert.Contains(t, got, "ci=disabled")
	assert.Contains(t, got, "ci=UNKNOWN")
	assert.Contains(t, got, "covered")
	assert.Contains(t, got, "uncovered")
	assert.Contains(t, got, "unknown")
}

// TestProposeFromDiscoveryScanSystemPrompt_TeachesECSRule pins the
// slice 5 (v0.89.10) prompt extension: the system message must
// teach the model the single-axis rule on cluster-level Container
// Insights AND the resource_kind name AND the canonical Terraform
// shape AND the task-definition-level limitation. A regression
// that drops any of these is silently an "ECS recommendations
// don't ship" failure mode in production.
func TestProposeFromDiscoveryScanSystemPrompt_TeachesECSRule(t *testing.T) {
	for _, want := range []string{
		"CLUSTER-LEVEL CONTAINER",
		"container_insights_status",
		`"enabled"`,
		"aws_ecs_cluster",
		"containerInsights",
		// Task-definition-level limitation must appear so the
		// model can hedge in its reasoning.
		"task-definition-level",
		"X-Ray daemon",
		"FireLens",
		"cluster-level scanning",
		// Both launch types covered by the same per-cluster rule.
		"Fargate and EC2 launch types",
		// The three-action IAM list is named so the model knows
		// what's read-only.
		"ecs:ListClusters",
		"ecs:DescribeClusters",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt should teach ECS rule: %q", want)
	}
}

// TestBuildDiscoveryUserMessage_Clusters verifies the slice 3b
// user-prompt builder threads each ClusterCandidate field into the
// rendered message body and emits the four-corner coverage
// shorthand strings the system prompt teaches (covered /
// logs-only / addon-only / uncovered). Pins the rule rendering
// so a future prompt edit can't silently regress the framing.
func TestBuildDiscoveryUserMessage_Clusters(t *testing.T) {
	ctx := discoveryContextForTest()
	ctx.Clusters = []ClusterCandidate{
		{
			ResourceID: "arn:aws:eks:us-east-1:123:cluster/covered",
			Name:       "covered", KubernetesVersion: "1.29",
			ControlPlaneLogging: []string{"api", "audit"},
			AddonNames:          []string{"adot"},
			Region:              "us-east-1",
		},
		{
			ResourceID: "arn:aws:eks:us-east-1:123:cluster/logs-only",
			Name:       "logs-only", KubernetesVersion: "1.29",
			ControlPlaneLogging: []string{"api", "audit"},
			Region:              "us-east-1",
		},
		{
			ResourceID: "arn:aws:eks:us-east-1:123:cluster/addon-only",
			Name:       "addon-only", KubernetesVersion: "1.29",
			AddonNames: []string{"amazon-cloudwatch-observability"},
			Region:     "us-east-1",
		},
		{
			ResourceID: "arn:aws:eks:us-east-1:123:cluster/uncovered",
			Name:       "uncovered", KubernetesVersion: "1.29",
			Region: "us-east-1",
		},
	}
	got := buildDiscoveryUserMessage(*ctx)
	assert.Contains(t, got, "Clusters (4 total):")
	assert.Contains(t, got, "covered")
	assert.Contains(t, got, "logs-only")
	assert.Contains(t, got, "addon-only")
	assert.Contains(t, got, "uncovered")
	assert.Contains(t, got, "adot")
	assert.Contains(t, got, "amazon-cloudwatch-observability")
	assert.Contains(t, got, "k8s=1.29")
}

// --- v0.89.48 (#671 Stream 69) GCP discovery slice 1 chunk 5 tests ---

// TestDiscoveryProposer_GCPProvider_PromptIncludesGCEOtelLabel — when
// Provider="gcp" and ProjectID is set, the user message renders the
// GCP scope description (provider=gcp + project_id) and the system
// prompt teaches the gce-otel-label kind. Pins the §9 contract from
// docs/proposals/gcp-discovery-slice1.md.
func TestDiscoveryProposer_GCPProvider_PromptIncludesGCEOtelLabel(t *testing.T) {
	ctx := DiscoveryScanContext{
		ScanID:    "scan-gcp-001",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	}
	msg := buildDiscoveryUserMessage(ctx)
	// User message describes the scope as GCP project, not AWS account.
	assert.Contains(t, msg, "provider: gcp")
	assert.Contains(t, msg, "project_id: my-sandbox-project")
	assert.NotContains(t, msg, "account_id: my-sandbox-project")
	assert.Contains(t, msg, "GCP discovery scan")
	assert.Contains(t, msg, "group_id on every step MUST equal the project_id above")

	// System prompt teaches the gce-otel-label kind and the
	// google_compute_instance Terraform resource per §10 contract
	// item 10.
	for _, want := range []string{
		"gce-otel-label",
		"google_compute_instance",
		"GCE instances",
		"OTel LABEL",
		"compute.viewer",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt should teach GCP rule: %q", want)
	}
}

// TestDiscoveryProposer_AWSProvider_PromptUnchanged — when Provider
// is unset (the v0.89.47 default) or set to "aws", the user message
// renders the exact same scope description shape as before chunk 5.
// Cold-start parity preservation per §9 of the design doc — the
// existing slice 1 v0.89.28 prompt golden tests stay green.
func TestDiscoveryProposer_AWSProvider_PromptUnchanged(t *testing.T) {
	// Empty provider — the backward-compat default.
	ctxAWSDefault := DiscoveryScanContext{
		ScanID:    "scan-aws-001",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	}
	// Explicit "aws" provider.
	ctxAWSExplicit := ctxAWSDefault
	ctxAWSExplicit.Provider = "aws"

	msgDefault := buildDiscoveryUserMessage(ctxAWSDefault)
	msgExplicit := buildDiscoveryUserMessage(ctxAWSExplicit)

	// Byte-identity: empty provider and explicit "aws" produce the
	// same message. This is the chunk 5 acceptance test 7 cold-start
	// parity invariant — explicit-aws callers see no regression in
	// the prompt body shape.
	if msgDefault != msgExplicit {
		t.Fatalf("AWS prompt parity broken between provider='' and provider='aws'\n--- default ---\n%s\n--- explicit ---\n%s",
			msgDefault, msgExplicit)
	}

	// AWS framing preserved.
	assert.Contains(t, msgDefault, "AWS discovery scan")
	assert.Contains(t, msgDefault, "account_id: 123456789012")
	assert.NotContains(t, msgDefault, "provider: gcp")
	assert.NotContains(t, msgDefault, "project_id:")
	assert.Contains(t, msgDefault, "group_id on every step MUST equal the account_id above")
}

// TestDiscoveryProposer_CrossProviderMix_InstructionPresentForBoth —
// the system prompt is shared across providers; both AWS kinds and
// the GCP gce-otel-label kind appear in the same system message
// (the user message discriminates which scope was scanned). Design
// choice documented inline so a future provider-scoped-prompt
// refactor surfaces the intent.
//
// Design choice: a single shared system prompt simplifies the
// proposer engine — one System constant, one parser, one validator.
// Cross-provider proposals are invalid at the contract layer, not
// at the prompt layer; the validateDiscoveryPlan group_id check
// rejects any plan whose group_id doesn't match the context's
// ScopeID() so a model that mis-emitted a gce-otel-label step
// against an AWS scope would fail the post-call validator. Slice 2+
// can revisit if the prompt becomes too long.
func TestDiscoveryProposer_CrossProviderMix_InstructionPresentForBoth(t *testing.T) {
	// System prompt carries both vocabularies.
	for _, awsKind := range []string{
		"ec2-otel-layer", "lambda-otel-layer", "rds-pi-em",
		"s3-access-logging", "alb-access-logs",
		"eks-cluster-logging", "eks-observability-addon",
		"dynamodb-contributor-insights", "ecs-container-insights",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, awsKind,
			"shared system prompt should still teach the AWS kind %q", awsKind)
	}
	for _, gcpKind := range []string{
		"gce-otel-label",
		"google_compute_instance",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, gcpKind,
			"shared system prompt should teach the GCP kind %q", gcpKind)
	}
}

// TestDiscoveryScanContext_ScopeID_Provider — pins the ScopeID()
// helper's provider-aware routing: ProjectID for GCP, AccountID for
// AWS (including the empty-provider default).
func TestDiscoveryScanContext_ScopeID_Provider(t *testing.T) {
	awsCtx := DiscoveryScanContext{AccountID: "123456789012"}
	if got := awsCtx.ScopeID(); got != "123456789012" {
		t.Errorf("AWS empty-provider ScopeID = %q, want 123456789012", got)
	}
	awsCtxExplicit := DiscoveryScanContext{Provider: "aws", AccountID: "999999999999"}
	if got := awsCtxExplicit.ScopeID(); got != "999999999999" {
		t.Errorf("AWS explicit-provider ScopeID = %q, want 999999999999", got)
	}
	gcpCtx := DiscoveryScanContext{Provider: "gcp", ProjectID: "my-project"}
	if got := gcpCtx.ScopeID(); got != "my-project" {
		t.Errorf("GCP ScopeID = %q, want my-project", got)
	}
	// Nil receiver is empty-safe.
	var nilCtx *DiscoveryScanContext
	if got := nilCtx.ScopeID(); got != "" {
		t.Errorf("nil ScopeID = %q, want empty", got)
	}
}

// TestProposeFromDiscoveryScan_GCPRequiresProjectID — provider="gcp"
// with empty ProjectID is rejected at the pre-call validator. AWS
// rules unchanged.
func TestProposeFromDiscoveryScan_GCPRequiresProjectID(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	_, err := svc.ProposeFromDiscoveryScan(context.Background(), &DiscoveryScanContext{
		ScanID:   "scan-x",
		Provider: "gcp",
		// ProjectID intentionally empty.
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project_id is required when provider=gcp")
}

// --- v0.89.53 (#678 Stream 76) Azure discovery slice 1 chunk 5 tests ---

// TestDiscoveryProposer_AzureProvider_PromptIncludesVMOtelTag — when
// Provider="azure" and SubscriptionID is set, the user message
// renders the Azure scope description (provider=azure +
// subscription_id) and the system prompt teaches the vm-otel-tag
// kind. Pins the §10 contract from
// docs/proposals/azure-discovery-slice1.md.
func TestDiscoveryProposer_AzureProvider_PromptIncludesVMOtelTag(t *testing.T) {
	ctx := DiscoveryScanContext{
		ScanID:         "scan-azure-001",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	}
	msg := buildDiscoveryUserMessage(ctx)
	// User message describes the scope as Azure subscription, not AWS
	// account or GCP project.
	assert.Contains(t, msg, "provider: azure")
	assert.Contains(t, msg, "subscription_id: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	assert.Contains(t, msg, "tenant_id: 11111111-2222-3333-4444-555555555555")
	assert.NotContains(t, msg, "account_id: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	assert.NotContains(t, msg, "project_id: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	assert.Contains(t, msg, "Azure discovery scan")
	assert.Contains(t, msg, "group_id on every step MUST equal the subscription_id above")

	// System prompt teaches the vm-otel-tag kind and the
	// azurerm_*_virtual_machine Terraform resources per §10 contract.
	for _, want := range []string{
		"vm-otel-tag",
		"azurerm_linux_virtual_machine",
		"azurerm_windows_virtual_machine",
		"Azure Virtual Machines",
		"OTel TAG",
		"Reader role",
		"Microsoft.Compute/virtualMachines/read",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt should teach Azure rule: %q", want)
	}
}

// TestDiscoveryProposer_AWSProvider_PromptUnchanged_PostAzure — chunk
// 5 cold-start parity: an AWS user message (provider="" and
// provider="aws") is byte-for-byte identical to the AWS user message
// produced by the chunk 4 baseline. The acceptance test §15.12
// invariant — adding the Azure path doesn't perturb AWS prompt
// generation.
func TestDiscoveryProposer_AWSProvider_PromptUnchanged_PostAzure(t *testing.T) {
	// Empty provider — the backward-compat default.
	ctxAWSDefault := DiscoveryScanContext{
		ScanID:    "scan-aws-001",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	}
	// Explicit "aws" provider.
	ctxAWSExplicit := ctxAWSDefault
	ctxAWSExplicit.Provider = "aws"

	msgDefault := buildDiscoveryUserMessage(ctxAWSDefault)
	msgExplicit := buildDiscoveryUserMessage(ctxAWSExplicit)

	// Byte-identity: empty provider and explicit "aws" produce the
	// same message after the chunk 5 Azure switch refactor.
	if msgDefault != msgExplicit {
		t.Fatalf("AWS prompt parity broken between provider='' and provider='aws' after chunk 5\n--- default ---\n%s\n--- explicit ---\n%s",
			msgDefault, msgExplicit)
	}

	// AWS framing preserved.
	assert.Contains(t, msgDefault, "AWS discovery scan")
	assert.Contains(t, msgDefault, "account_id: 123456789012")
	assert.NotContains(t, msgDefault, "provider: gcp")
	assert.NotContains(t, msgDefault, "provider: azure")
	assert.NotContains(t, msgDefault, "project_id:")
	assert.NotContains(t, msgDefault, "subscription_id:")
	assert.Contains(t, msgDefault, "group_id on every step MUST equal the account_id above")
}

// TestDiscoveryProposer_GCPProvider_PromptUnchanged_PostAzure — chunk
// 5 cold-start parity: the GCP user message produced under
// Provider="gcp" is byte-for-byte identical to the GCP user message
// from v0.89.48 (the original GCP slice 1 chunk 5 baseline).
// Acceptance test §15.12 invariant — adding the Azure path doesn't
// perturb GCP prompt generation.
func TestDiscoveryProposer_GCPProvider_PromptUnchanged_PostAzure(t *testing.T) {
	ctx := DiscoveryScanContext{
		ScanID:    "scan-gcp-001",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	}
	msg := buildDiscoveryUserMessage(ctx)

	// GCP framing preserved byte-for-byte. The GCP-specific markers
	// from TestDiscoveryProposer_GCPProvider_PromptIncludesGCEOtelLabel
	// all still appear unchanged.
	assert.Contains(t, msg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.Contains(t, msg, "provider: gcp")
	assert.Contains(t, msg, "project_id: my-sandbox-project")
	assert.NotContains(t, msg, "provider: azure")
	assert.NotContains(t, msg, "provider: aws")
	assert.NotContains(t, msg, "subscription_id:")
	assert.NotContains(t, msg, "tenant_id:")
	assert.NotContains(t, msg, "account_id:")
	assert.Contains(t, msg, "group_id on every step MUST equal the project_id above")
}

// TestDiscoveryScanContext_ScopeID_AzureProvider — pins the ScopeID()
// helper's provider-aware routing for the Azure path:
// SubscriptionID for Provider="azure", AccountID for AWS, ProjectID
// for GCP. Mirrors TestDiscoveryScanContext_ScopeID_Provider with
// the Azure case added.
func TestDiscoveryScanContext_ScopeID_AzureProvider(t *testing.T) {
	azureCtx := DiscoveryScanContext{Provider: "azure", SubscriptionID: "abc"}
	if got := azureCtx.ScopeID(); got != "abc" {
		t.Errorf("Azure ScopeID = %q, want abc", got)
	}
	// Azure ignores AccountID + ProjectID if both are populated.
	azureCtxAll := DiscoveryScanContext{
		Provider:       "azure",
		SubscriptionID: "sub-001",
		AccountID:      "aws-account-bleed",
		ProjectID:      "gcp-project-bleed",
	}
	if got := azureCtxAll.ScopeID(); got != "sub-001" {
		t.Errorf("Azure ScopeID with bleed-through fields = %q, want sub-001", got)
	}
	// Cross-check: AWS path still works after Azure was added.
	awsCtx := DiscoveryScanContext{AccountID: "123456789012"}
	if got := awsCtx.ScopeID(); got != "123456789012" {
		t.Errorf("AWS ScopeID after Azure addition = %q, want 123456789012", got)
	}
	// Cross-check: GCP path still works after Azure was added.
	gcpCtx := DiscoveryScanContext{Provider: "gcp", ProjectID: "my-project"}
	if got := gcpCtx.ScopeID(); got != "my-project" {
		t.Errorf("GCP ScopeID after Azure addition = %q, want my-project", got)
	}
}

// TestProposeFromDiscoveryScan_AzureRequiresSubscriptionID —
// Provider="azure" with empty SubscriptionID is rejected at the
// pre-call validator. AWS + GCP rules unchanged.
func TestProposeFromDiscoveryScan_AzureRequiresSubscriptionID(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	_, err := svc.ProposeFromDiscoveryScan(context.Background(), &DiscoveryScanContext{
		ScanID:   "scan-x",
		Provider: "azure",
		// SubscriptionID intentionally empty.
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subscription_id is required when provider=azure")
}

// --- v0.89.58 (#685 Stream 83) OCI discovery slice 1 chunk 5 tests ---

// TestDiscoveryProposer_OCIProvider_PromptIncludesComputeOtelTag — when
// Provider="oci" and TenancyOCID is set, the user message renders the
// OCI scope description (provider=oci + tenancy_ocid) and the system
// prompt teaches the compute-otel-tag kind. Pins the §10 contract
// from docs/proposals/oci-discovery-slice1.md.
func TestDiscoveryProposer_OCIProvider_PromptIncludesComputeOtelTag(t *testing.T) {
	ctx := DiscoveryScanContext{
		ScanID:      "scan-oci-001",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		UserOCID:    "ocid1.user.oc1..bbbbbbbb",
		Regions:     []string{"us-phoenix-1"},
	}
	msg := buildDiscoveryUserMessage(ctx)
	// User message describes the scope as OCI tenancy, not AWS / GCP /
	// Azure.
	assert.Contains(t, msg, "provider: oci")
	assert.Contains(t, msg, "tenancy_ocid: ocid1.tenancy.oc1..aaaaaaaa")
	assert.Contains(t, msg, "user_ocid: ocid1.user.oc1..bbbbbbbb")
	assert.NotContains(t, msg, "account_id: ocid1.tenancy.oc1..aaaaaaaa")
	assert.NotContains(t, msg, "project_id: ocid1.tenancy.oc1..aaaaaaaa")
	assert.NotContains(t, msg, "subscription_id: ocid1.tenancy.oc1..aaaaaaaa")
	assert.Contains(t, msg, "OCI discovery scan")
	assert.Contains(t, msg, "group_id on every step MUST equal the tenancy_ocid above")

	// System prompt teaches the compute-otel-tag kind and the
	// oci_core_instance Terraform resource per §10 contract.
	for _, want := range []string{
		"compute-otel-tag",
		"oci_core_instance",
		"OCI Compute instances",
		"OTel TAG",
		"freeform_tags",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt should teach OCI rule: %q", want)
	}
}

// TestDiscoveryProposer_AWSProvider_PromptUnchanged_PostOCI — chunk 5
// cold-start parity: an AWS user message (provider="" and
// provider="aws") is byte-for-byte identical to the AWS user message
// produced by the chunk 4 baseline. The acceptance test §15.13
// invariant — adding the OCI path doesn't perturb AWS prompt
// generation.
func TestDiscoveryProposer_AWSProvider_PromptUnchanged_PostOCI(t *testing.T) {
	ctxAWSDefault := DiscoveryScanContext{
		ScanID:    "scan-aws-001",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	}
	ctxAWSExplicit := ctxAWSDefault
	ctxAWSExplicit.Provider = "aws"

	msgDefault := buildDiscoveryUserMessage(ctxAWSDefault)
	msgExplicit := buildDiscoveryUserMessage(ctxAWSExplicit)

	if msgDefault != msgExplicit {
		t.Fatalf("AWS prompt parity broken between provider='' and provider='aws' after chunk 5 OCI addition\n--- default ---\n%s\n--- explicit ---\n%s",
			msgDefault, msgExplicit)
	}

	assert.Contains(t, msgDefault, "AWS discovery scan")
	assert.Contains(t, msgDefault, "account_id: 123456789012")
	assert.NotContains(t, msgDefault, "provider: gcp")
	assert.NotContains(t, msgDefault, "provider: azure")
	assert.NotContains(t, msgDefault, "provider: oci")
	assert.NotContains(t, msgDefault, "project_id:")
	assert.NotContains(t, msgDefault, "subscription_id:")
	assert.NotContains(t, msgDefault, "tenancy_ocid:")
	assert.Contains(t, msgDefault, "group_id on every step MUST equal the account_id above")
}

// TestDiscoveryProposer_GCPProvider_PromptUnchanged_PostOCI — chunk 5
// cold-start parity: the GCP user message produced under
// Provider="gcp" is byte-for-byte identical to the GCP user message
// from v0.89.48. Acceptance test §15.13 invariant.
func TestDiscoveryProposer_GCPProvider_PromptUnchanged_PostOCI(t *testing.T) {
	ctx := DiscoveryScanContext{
		ScanID:    "scan-gcp-001",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	}
	msg := buildDiscoveryUserMessage(ctx)

	assert.Contains(t, msg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.Contains(t, msg, "provider: gcp")
	assert.Contains(t, msg, "project_id: my-sandbox-project")
	assert.NotContains(t, msg, "provider: azure")
	assert.NotContains(t, msg, "provider: aws")
	assert.NotContains(t, msg, "provider: oci")
	assert.NotContains(t, msg, "subscription_id:")
	assert.NotContains(t, msg, "tenant_id:")
	assert.NotContains(t, msg, "tenancy_ocid:")
	assert.NotContains(t, msg, "account_id:")
	assert.Contains(t, msg, "group_id on every step MUST equal the project_id above")
}

// TestDiscoveryProposer_AzureProvider_PromptUnchanged_PostOCI — chunk
// 5 cold-start parity: the Azure user message produced under
// Provider="azure" is byte-for-byte identical to the Azure user
// message from v0.89.53. Acceptance test §15.13 invariant — adding
// the OCI path doesn't perturb Azure prompt generation.
func TestDiscoveryProposer_AzureProvider_PromptUnchanged_PostOCI(t *testing.T) {
	ctx := DiscoveryScanContext{
		ScanID:         "scan-azure-001",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	}
	msg := buildDiscoveryUserMessage(ctx)

	assert.Contains(t, msg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.Contains(t, msg, "provider: azure")
	assert.Contains(t, msg, "subscription_id: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	assert.Contains(t, msg, "tenant_id: 11111111-2222-3333-4444-555555555555")
	assert.NotContains(t, msg, "provider: gcp")
	assert.NotContains(t, msg, "provider: aws")
	assert.NotContains(t, msg, "provider: oci")
	assert.NotContains(t, msg, "project_id:")
	assert.NotContains(t, msg, "tenancy_ocid:")
	assert.NotContains(t, msg, "account_id:")
	assert.Contains(t, msg, "group_id on every step MUST equal the subscription_id above")
}

// TestDiscoveryScanContext_ScopeID_OCIProvider — pins the ScopeID()
// helper's provider-aware routing for the OCI path: TenancyOCID for
// Provider="oci", SubscriptionID for Azure, ProjectID for GCP,
// AccountID for AWS.
func TestDiscoveryScanContext_ScopeID_OCIProvider(t *testing.T) {
	ociCtx := DiscoveryScanContext{Provider: "oci", TenancyOCID: "ocid1.tenancy.oc1..xyz"}
	if got := ociCtx.ScopeID(); got != "ocid1.tenancy.oc1..xyz" {
		t.Errorf("OCI ScopeID = %q, want ocid1.tenancy.oc1..xyz", got)
	}
	// OCI ignores AccountID + ProjectID + SubscriptionID if all are populated.
	ociCtxAll := DiscoveryScanContext{
		Provider:       "oci",
		TenancyOCID:    "ocid1.tenancy.oc1..main",
		AccountID:      "aws-account-bleed",
		ProjectID:      "gcp-project-bleed",
		SubscriptionID: "azure-sub-bleed",
	}
	if got := ociCtxAll.ScopeID(); got != "ocid1.tenancy.oc1..main" {
		t.Errorf("OCI ScopeID with bleed-through fields = %q, want ocid1.tenancy.oc1..main", got)
	}
	// Cross-check: AWS path still works after OCI was added.
	awsCtx := DiscoveryScanContext{AccountID: "123456789012"}
	if got := awsCtx.ScopeID(); got != "123456789012" {
		t.Errorf("AWS ScopeID after OCI addition = %q, want 123456789012", got)
	}
	// Cross-check: GCP path still works after OCI was added.
	gcpCtx := DiscoveryScanContext{Provider: "gcp", ProjectID: "my-project"}
	if got := gcpCtx.ScopeID(); got != "my-project" {
		t.Errorf("GCP ScopeID after OCI addition = %q, want my-project", got)
	}
	// Cross-check: Azure path still works after OCI was added.
	azureCtx := DiscoveryScanContext{Provider: "azure", SubscriptionID: "sub-abc"}
	if got := azureCtx.ScopeID(); got != "sub-abc" {
		t.Errorf("Azure ScopeID after OCI addition = %q, want sub-abc", got)
	}
}

// TestProposeFromDiscoveryScan_OCIRequiresTenancyOCID —
// Provider="oci" with empty TenancyOCID is rejected at the pre-call
// validator. AWS + GCP + Azure rules unchanged.
func TestProposeFromDiscoveryScan_OCIRequiresTenancyOCID(t *testing.T) {
	svc := proposerServiceForTest("http://unused.example")
	svc.cfg.APIKey = "test-key"
	svc.cfg.Enabled = true

	_, err := svc.ProposeFromDiscoveryScan(context.Background(), &DiscoveryScanContext{
		ScanID:   "scan-x",
		Provider: "oci",
		// TenancyOCID intentionally empty.
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tenancy_ocid is required when provider=oci")
}

// TestDiscoveryProposer_CrossProviderMix_OCIInstructionPresent — the
// system prompt is shared across providers; the OCI compute-otel-tag
// kind appears in the same system message that already carries the
// AWS, GCP, and Azure kinds.
func TestDiscoveryProposer_CrossProviderMix_OCIInstructionPresent(t *testing.T) {
	for _, ociKind := range []string{
		"compute-otel-tag",
		"oci_core_instance",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, ociKind,
			"shared system prompt should teach the OCI kind %q", ociKind)
	}
	// AWS + GCP + Azure kinds still present after OCI addition.
	for _, awsKind := range []string{
		"ec2-otel-layer", "lambda-otel-layer", "rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, awsKind,
			"shared system prompt should still teach the AWS kind %q after OCI addition", awsKind)
	}
	for _, gcpKind := range []string{
		"gce-otel-label", "google_compute_instance",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, gcpKind,
			"shared system prompt should still teach the GCP kind %q after OCI addition", gcpKind)
	}
	for _, azureKind := range []string{
		"vm-otel-tag", "azurerm_linux_virtual_machine",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, azureKind,
			"shared system prompt should still teach the Azure kind %q after OCI addition", azureKind)
	}
}

// TestDiscoveryProposer_CrossProviderMix_AzureInstructionPresent —
// the system prompt is shared across providers; the Azure
// vm-otel-tag kind appears in the same system message that already
// carries the AWS kinds and the GCP gce-otel-label kind. Same design
// choice as the v0.89.48 GCP cross-provider mix test — a single
// shared system prompt simplifies the proposer engine.
func TestDiscoveryProposer_CrossProviderMix_AzureInstructionPresent(t *testing.T) {
	for _, azureKind := range []string{
		"vm-otel-tag",
		"azurerm_linux_virtual_machine",
		"azurerm_windows_virtual_machine",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, azureKind,
			"shared system prompt should teach the Azure kind %q", azureKind)
	}
	// AWS + GCP kinds still present after Azure addition.
	for _, awsKind := range []string{
		"ec2-otel-layer", "lambda-otel-layer", "rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, awsKind,
			"shared system prompt should still teach the AWS kind %q after Azure addition", awsKind)
	}
	for _, gcpKind := range []string{
		"gce-otel-label", "google_compute_instance",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, gcpKind,
			"shared system prompt should still teach the GCP kind %q after Azure addition", gcpKind)
	}
}

// TestDiscoveryProposer_DatabaseTierKindsInSystemPrompt — database
// tier slice 2 chunk 5 (v0.89.66, #695 Stream 93). The three new
// per-cloud database recommendation kinds (cloudsql-pi-enable for
// GCP, azsql-diag-enable for Azure, ocidb-perfhub-enable for OCI)
// must appear in the shared system prompt so the model can route
// findings to the right kind when the scan inventory carries
// database rows. The slice 1 compute kinds (gce-otel-label,
// vm-otel-tag, compute-otel-tag) must remain present after the
// extension — same shared-system-prompt invariant the prior chunk
// 5 tests pin.
func TestDiscoveryProposer_DatabaseTierKindsInSystemPrompt(t *testing.T) {
	for _, dbKind := range []string{
		"cloudsql-pi-enable",
		"azsql-diag-enable",
		"ocidb-perfhub-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, dbKind,
			"shared system prompt should teach the database tier slice 2 kind %q", dbKind)
	}
	// Slice 1 compute kinds still present after the database tier
	// addition — defends against an accidental rewrite of the
	// compute-kind paragraphs.
	for _, computeKind := range []string{
		"gce-otel-label", "vm-otel-tag", "compute-otel-tag",
		"ec2-otel-layer", "lambda-otel-layer", "rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, computeKind,
			"shared system prompt should still teach the slice 1 kind %q after database tier slice 2", computeKind)
	}
	// Terraform shape hints make it into the prompt body so the
	// model's Terraform snippet emits the right resource type.
	for _, marker := range []string{
		"google_sql_database_instance",
		"azurerm_monitor_diagnostic_setting",
		"oci_database_db_systems_management",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, marker,
			"shared system prompt should mention Terraform resource %q for the database tier", marker)
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostDBTier —
// database tier slice 2 chunk 5 (v0.89.66, #695 Stream 93)
// cold-start parity invariant: across all four providers, the
// compute-only user message produced by buildDiscoveryUserMessage
// must remain byte-identical to v0.89.65 when the scan context
// carries no database rows. The acceptance test §11.7 invariant —
// adding the database tier kinds must not perturb compute-only
// prompt generation for any provider.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostDBTier(t *testing.T) {
	// AWS cold start.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.Contains(t, awsMsg, "account_id: 123456789012")
	assert.Contains(t, awsMsg, "Databases (0 total):")
	assert.NotContains(t, awsMsg, "cloudsql-pi-enable")
	assert.NotContains(t, awsMsg, "azsql-diag-enable")
	assert.NotContains(t, awsMsg, "ocidb-perfhub-enable")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.Contains(t, gcpMsg, "project_id: my-sandbox-project")
	assert.Contains(t, gcpMsg, "Databases (0 total):")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.Contains(t, azureMsg, "subscription_id: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	assert.Contains(t, azureMsg, "Databases (0 total):")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.Contains(t, ociMsg, "tenancy_ocid: ocid1.tenancy.oc1..aaaaaaaa")
	assert.Contains(t, ociMsg, "Databases (0 total):")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// TestDiscoveryProposer_DatabasesInScanResult_AppendedToUserMessage —
// database tier slice 2 chunk 5 (v0.89.66, #695 Stream 93). When
// the scan context carries database rows with the new per-cloud
// axis flags populated, the user message renders each row with the
// correct coverage shorthand:
//   - GCP rows with QueryInsightsEnabled=true → "covered"; else "uncovered"
//   - Azure rows with SQLInsightsDiagEnabled=true → "covered"; else "uncovered"
//   - OCI rows with DatabaseManagementEnabled=true → "covered"; else "uncovered"
//   - AWS rows continue to render the v0.89.65 covered / pi-only /
//     em-only / uncovered shorthand based on PI + EM (cold-start
//     parity).
func TestDiscoveryProposer_DatabasesInScanResult_AppendedToUserMessage(t *testing.T) {
	// GCP — a covered + uncovered Cloud SQL row.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-db",
		Provider:  "gcp",
		ProjectID: "my-prod-project",
		Regions:   []string{"us-central1"},
		Databases: []DatabaseResourceCandidate{
			{
				ResourceID:           "projects/p/instances/db-covered",
				Engine:               "postgres",
				EngineVersion:        "15",
				InstanceClass:        "db-custom-2-7680",
				Region:               "us-central1",
				Provider:             "gcp",
				QueryInsightsEnabled: true,
			},
			{
				ResourceID:           "projects/p/instances/db-uncovered",
				Engine:               "mysql",
				EngineVersion:        "8.0",
				InstanceClass:        "db-n1-standard-1",
				Region:               "us-central1",
				Provider:             "gcp",
				QueryInsightsEnabled: false,
			},
		},
	})
	assert.Contains(t, gcpMsg, "Databases (2 total):")
	assert.Contains(t, gcpMsg, "projects/p/instances/db-covered (engine=postgres 15, class=db-custom-2-7680, us-central1, covered)")
	assert.Contains(t, gcpMsg, "projects/p/instances/db-uncovered (engine=mysql 8.0, class=db-n1-standard-1, us-central1, uncovered)")

	// Azure — a covered + uncovered Azure SQL row.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-db",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
		Databases: []DatabaseResourceCandidate{
			{
				ResourceID:             "/subscriptions/s/.../databases/db-covered",
				Engine:                 "sqlserver",
				EngineVersion:          "12.0",
				InstanceClass:          "GP_S_Gen5_2",
				Region:                 "eastus",
				Provider:               "azure",
				SQLInsightsDiagEnabled: true,
			},
			{
				ResourceID:             "/subscriptions/s/.../databases/db-uncovered",
				Engine:                 "sqlserver",
				EngineVersion:          "12.0",
				InstanceClass:          "GP_S_Gen5_1",
				Region:                 "eastus",
				Provider:               "azure",
				SQLInsightsDiagEnabled: false,
			},
		},
	})
	assert.Contains(t, azureMsg, "Databases (2 total):")
	assert.Contains(t, azureMsg, "/subscriptions/s/.../databases/db-covered (engine=sqlserver 12.0, class=GP_S_Gen5_2, eastus, covered)")
	assert.Contains(t, azureMsg, "/subscriptions/s/.../databases/db-uncovered (engine=sqlserver 12.0, class=GP_S_Gen5_1, eastus, uncovered)")

	// OCI — a covered + uncovered OCI Database row.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-db",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
		Databases: []DatabaseResourceCandidate{
			{
				ResourceID:                "ocid1.dbsystem.oc1.phx.covered",
				Engine:                    "oracle",
				EngineVersion:             "19c",
				InstanceClass:             "VM.Standard.E4.Flex",
				Region:                    "us-phoenix-1",
				Provider:                  "oci",
				DatabaseManagementEnabled: true,
			},
			{
				ResourceID:                "ocid1.autonomousdatabase.oc1.phx.uncovered",
				Engine:                    "oracle",
				EngineVersion:             "19c",
				InstanceClass:             "OCPU=2",
				Region:                    "us-phoenix-1",
				Provider:                  "oci",
				DatabaseManagementEnabled: false,
			},
		},
	})
	assert.Contains(t, ociMsg, "Databases (2 total):")
	assert.Contains(t, ociMsg, "ocid1.dbsystem.oc1.phx.covered (engine=oracle 19c, class=VM.Standard.E4.Flex, us-phoenix-1, covered)")
	assert.Contains(t, ociMsg, "ocid1.autonomousdatabase.oc1.phx.uncovered (engine=oracle 19c, class=OCPU=2, us-phoenix-1, uncovered)")

	// AWS — the v0.89.65 pi/em coverage shorthand must survive the
	// new switch so the cold-start invariant holds for inventories
	// that DO carry database rows.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-db",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		Databases: []DatabaseResourceCandidate{
			{
				ResourceID:                 "arn:aws:rds:us-east-1:123:db:covered",
				Engine:                     "postgres",
				EngineVersion:              "15",
				InstanceClass:              "db.r6g.large",
				Region:                     "us-east-1",
				PerformanceInsightsEnabled: true,
				EnhancedMonitoringEnabled:  true,
			},
			{
				ResourceID:                 "arn:aws:rds:us-east-1:123:db:pi-only",
				Engine:                     "mysql",
				EngineVersion:              "8.0",
				InstanceClass:              "db.t3.medium",
				Region:                     "us-east-1",
				PerformanceInsightsEnabled: true,
			},
			{
				ResourceID:                "arn:aws:rds:us-east-1:123:db:em-only",
				Engine:                    "mysql",
				EngineVersion:             "8.0",
				InstanceClass:             "db.t3.medium",
				Region:                    "us-east-1",
				EnhancedMonitoringEnabled: true,
			},
			{
				ResourceID:    "arn:aws:rds:us-east-1:123:db:uncovered",
				Engine:        "postgres",
				EngineVersion: "15",
				InstanceClass: "db.r6g.large",
				Region:        "us-east-1",
			},
		},
	})
	assert.Contains(t, awsMsg, "Databases (4 total):")
	assert.Contains(t, awsMsg, "arn:aws:rds:us-east-1:123:db:covered (engine=postgres 15, class=db.r6g.large, us-east-1, covered)")
	assert.Contains(t, awsMsg, "arn:aws:rds:us-east-1:123:db:pi-only (engine=mysql 8.0, class=db.t3.medium, us-east-1, pi-only)")
	assert.Contains(t, awsMsg, "arn:aws:rds:us-east-1:123:db:em-only (engine=mysql 8.0, class=db.t3.medium, us-east-1, em-only)")
	assert.Contains(t, awsMsg, "arn:aws:rds:us-east-1:123:db:uncovered (engine=postgres 15, class=db.r6g.large, us-east-1, uncovered)")
}

// TestDiscoveryProposer_K8sTierKindsInSystemPrompt — Kubernetes tier
// slice 2 chunk 5 (v0.89.71, #702 Stream 100). The three new
// per-cloud Kubernetes recommendation kinds (gke-mp-enable for GCP,
// aks-monitor-enable for Azure, oke-ops-insights-enable for OCI)
// must appear in the shared system prompt so the model can route
// findings to the right kind when the scan inventory carries
// cluster rows. Slice 1 compute kinds + database tier slice 2 kinds
// must remain present after the K8s extension — the same shared-
// system-prompt invariant the prior chunk 5 tests pin.
func TestDiscoveryProposer_K8sTierKindsInSystemPrompt(t *testing.T) {
	for _, k8sKind := range []string{
		"gke-mp-enable",
		"aks-monitor-enable",
		"oke-ops-insights-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, k8sKind,
			"shared system prompt should teach the Kubernetes tier slice 2 kind %q", k8sKind)
	}
	// Slice 1 compute kinds still present after the K8s tier
	// addition — defends against an accidental rewrite of the
	// compute-kind paragraphs.
	for _, computeKind := range []string{
		"gce-otel-label", "vm-otel-tag", "compute-otel-tag",
		"ec2-otel-layer", "lambda-otel-layer", "rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, computeKind,
			"shared system prompt should still teach the slice 1 kind %q after Kubernetes tier slice 2", computeKind)
	}
	// Database tier slice 2 kinds still present.
	for _, dbKind := range []string{
		"cloudsql-pi-enable", "azsql-diag-enable", "ocidb-perfhub-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, dbKind,
			"shared system prompt should still teach the database tier slice 2 kind %q after Kubernetes tier slice 2", dbKind)
	}
	// Terraform shape hints make it into the prompt body so the
	// model's Terraform snippet emits the right resource type.
	for _, marker := range []string{
		"google_container_cluster.monitoring_config",
		"azurerm_kubernetes_cluster.monitor_metrics",
		"oci_containerengine_cluster.freeform_tags",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, marker,
			"shared system prompt should mention Terraform resource %q for the Kubernetes tier", marker)
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostK8sTier —
// Kubernetes tier slice 2 chunk 5 (v0.89.71, #702 Stream 100)
// cold-start parity invariant: across all four providers, the
// compute-only user message produced by buildDiscoveryUserMessage
// must remain byte-identical to v0.89.70 when the scan context
// carries no cluster rows. Acceptance test §11.9 invariant —
// adding the Kubernetes tier kinds must not perturb compute-only
// prompt generation for any provider.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostK8sTier(t *testing.T) {
	// AWS cold start.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.Contains(t, awsMsg, "account_id: 123456789012")
	assert.Contains(t, awsMsg, "Clusters (0 total):")
	assert.NotContains(t, awsMsg, "gke-mp-enable")
	assert.NotContains(t, awsMsg, "aks-monitor-enable")
	assert.NotContains(t, awsMsg, "oke-ops-insights-enable")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.Contains(t, gcpMsg, "project_id: my-sandbox-project")
	assert.Contains(t, gcpMsg, "Clusters (0 total):")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.Contains(t, azureMsg, "subscription_id: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	assert.Contains(t, azureMsg, "Clusters (0 total):")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.Contains(t, ociMsg, "tenancy_ocid: ocid1.tenancy.oc1..aaaaaaaa")
	assert.Contains(t, ociMsg, "Clusters (0 total):")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// TestDiscoveryProposer_ClustersInScanResult_AppendedToUserMessage —
// Kubernetes tier slice 2 chunk 5 (v0.89.71, #702 Stream 100). When
// the scan context carries cluster rows with the new per-cloud
// axis flags populated, the user message renders each row with the
// correct coverage shorthand:
//   - GCP rows with ManagedPrometheusEnabled=true → "covered"; else "uncovered"
//   - Azure rows with AzureMonitorEnabled=true → "covered"; else "uncovered"
//   - OCI rows with OperationsInsightsEnabled=true → "covered"; else "uncovered"
//   - AWS rows continue to render the v0.89.70 covered / logs-only /
//     addon-only / uncovered shorthand based on ControlPlaneLogging +
//     AddonNames (cold-start parity).
func TestDiscoveryProposer_ClustersInScanResult_AppendedToUserMessage(t *testing.T) {
	// GCP — a covered + uncovered GKE row.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-k8s",
		Provider:  "gcp",
		ProjectID: "my-prod-project",
		Regions:   []string{"us-central1"},
		Clusters: []ClusterCandidate{
			{
				ResourceID:               "projects/p/locations/us-central1/clusters/gke-covered",
				Name:                     "gke-covered",
				KubernetesVersion:        "1.29",
				Region:                   "us-central1",
				Provider:                 "gcp",
				ManagedPrometheusEnabled: true,
			},
			{
				ResourceID:               "projects/p/locations/us-central1/clusters/gke-uncovered",
				Name:                     "gke-uncovered",
				KubernetesVersion:        "1.29",
				Region:                   "us-central1",
				Provider:                 "gcp",
				ManagedPrometheusEnabled: false,
			},
		},
	})
	assert.Contains(t, gcpMsg, "Clusters (2 total):")
	assert.Contains(t, gcpMsg, "gke-covered (name=gke-covered, k8s=1.29, region=us-central1, logging=none, addons=none, covered)")
	assert.Contains(t, gcpMsg, "gke-uncovered (name=gke-uncovered, k8s=1.29, region=us-central1, logging=none, addons=none, uncovered)")

	// Azure — a covered + uncovered AKS row.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-k8s",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
		Clusters: []ClusterCandidate{
			{
				ResourceID:          "/subscriptions/s/.../managedClusters/aks-covered",
				Name:                "aks-covered",
				KubernetesVersion:   "1.29",
				Region:              "eastus",
				Provider:            "azure",
				AzureMonitorEnabled: true,
			},
			{
				ResourceID:          "/subscriptions/s/.../managedClusters/aks-uncovered",
				Name:                "aks-uncovered",
				KubernetesVersion:   "1.29",
				Region:              "eastus",
				Provider:            "azure",
				AzureMonitorEnabled: false,
			},
		},
	})
	assert.Contains(t, azureMsg, "Clusters (2 total):")
	assert.Contains(t, azureMsg, "aks-covered, k8s=1.29, region=eastus, logging=none, addons=none, covered)")
	assert.Contains(t, azureMsg, "aks-uncovered, k8s=1.29, region=eastus, logging=none, addons=none, uncovered)")

	// OCI — a covered + uncovered OKE row.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-k8s",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
		Clusters: []ClusterCandidate{
			{
				ResourceID:                "ocid1.cluster.oc1.phx.covered",
				Name:                      "oke-covered",
				KubernetesVersion:         "1.29",
				Region:                    "us-phoenix-1",
				Provider:                  "oci",
				OperationsInsightsEnabled: true,
			},
			{
				ResourceID:                "ocid1.cluster.oc1.phx.uncovered",
				Name:                      "oke-uncovered",
				KubernetesVersion:         "1.29",
				Region:                    "us-phoenix-1",
				Provider:                  "oci",
				OperationsInsightsEnabled: false,
			},
		},
	})
	assert.Contains(t, ociMsg, "Clusters (2 total):")
	assert.Contains(t, ociMsg, "oke-covered, k8s=1.29, region=us-phoenix-1, logging=none, addons=none, covered)")
	assert.Contains(t, ociMsg, "oke-uncovered, k8s=1.29, region=us-phoenix-1, logging=none, addons=none, uncovered)")

	// AWS — the v0.89.70 composite coverage shorthand must survive
	// the new switch so the cold-start invariant holds for
	// inventories that DO carry EKS cluster rows.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-k8s",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		Clusters: []ClusterCandidate{
			{
				ResourceID:          "arn:aws:eks:us-east-1:123:cluster/eks-covered",
				Name:                "eks-covered",
				KubernetesVersion:   "1.29",
				ControlPlaneLogging: []string{"api", "audit"},
				AddonNames:          []string{"adot"},
				Region:              "us-east-1",
			},
			{
				ResourceID:          "arn:aws:eks:us-east-1:123:cluster/eks-logs-only",
				Name:                "eks-logs-only",
				KubernetesVersion:   "1.29",
				ControlPlaneLogging: []string{"api", "audit"},
				Region:              "us-east-1",
			},
			{
				ResourceID:        "arn:aws:eks:us-east-1:123:cluster/eks-addon-only",
				Name:              "eks-addon-only",
				KubernetesVersion: "1.29",
				AddonNames:        []string{"amazon-cloudwatch-observability"},
				Region:            "us-east-1",
			},
			{
				ResourceID:        "arn:aws:eks:us-east-1:123:cluster/eks-uncovered",
				Name:              "eks-uncovered",
				KubernetesVersion: "1.29",
				Region:            "us-east-1",
			},
		},
	})
	assert.Contains(t, awsMsg, "Clusters (4 total):")
	assert.Contains(t, awsMsg, "eks-covered, k8s=1.29, region=us-east-1, logging=api,audit, addons=adot, covered)")
	assert.Contains(t, awsMsg, "eks-logs-only, k8s=1.29, region=us-east-1, logging=api,audit, addons=none, logs-only)")
	assert.Contains(t, awsMsg, "eks-addon-only, k8s=1.29, region=us-east-1, logging=none, addons=amazon-cloudwatch-observability, addon-only)")
	assert.Contains(t, awsMsg, "eks-uncovered, k8s=1.29, region=us-east-1, logging=none, addons=none, uncovered)")
}

// --- Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123) ----
//
// TestDiscoveryProposer_ServerlessTierKindsInSystemPrompt — the 11
// new per-cloud serverless recommendation kinds must appear in the
// shared system prompt so the model can route findings to the right
// kind when the scan inventory carries serverless rows. Slice 1
// compute + database tier + Kubernetes tier kinds must remain present
// after the serverless extension — the same shared-system-prompt
// invariant the prior chunk 5 tests pin.
func TestDiscoveryProposer_ServerlessTierKindsInSystemPrompt(t *testing.T) {
	for _, srvKind := range []string{
		// AWS Lambda.
		"lambda-xray-active", "lambda-otel-layer", "lambda-otel-wrapper",
		// GCP Cloud Run.
		"cloudrun-trace-enable", "cloudrun-otel-sidecar", "cloudrun-otel-export-endpoint",
		// GCP Cloud Functions.
		"cloudfunc-trace-enable", "cloudfunc-otel-layer",
		// Azure Functions.
		"azfunc-appinsights-enable", "azfunc-otel-distro",
		// OCI Functions.
		"ocifunc-apm-enable", "ocifunc-otel-distro",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, srvKind,
			"shared system prompt should teach the serverless tier slice 1 kind %q", srvKind)
	}
	// Prior-tier compute / db / k8s kinds still present.
	for _, priorKind := range []string{
		"gce-otel-label", "vm-otel-tag", "compute-otel-tag",
		"ec2-otel-layer", "rds-pi-em",
		"cloudsql-pi-enable", "azsql-diag-enable", "ocidb-perfhub-enable",
		"gke-mp-enable", "aks-monitor-enable", "oke-ops-insights-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorKind,
			"shared system prompt should still teach the prior-tier kind %q after the serverless tier", priorKind)
	}
	// The reasoning template (verbatim from the spec) is present.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"This [surface] has [axis] disabled.",
		"shared system prompt should carry the serverless tier reasoning template")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"~5 minutes of the first",
		"shared system prompt should carry the serverless tier reasoning template's traceindex follow-up")
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostServerlessTier
// — serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123)
// cold-start parity invariant: across all four providers, the
// compute-only user message produced by buildDiscoveryUserMessage
// must remain byte-identical to v0.89.88 when the scan context
// carries no serverless rows. Acceptance test §11.18 invariant —
// adding the serverless tier kinds must not perturb compute-only
// prompt generation for any provider. The new kinds live ONLY in the
// system prompt; the user message has no serverless section, so a
// cold-start scan (no serverless rows in the inventory) renders the
// same body the prior chunk 5 tier extensions pinned.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostServerlessTier(t *testing.T) {
	// AWS cold start.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.Contains(t, awsMsg, "account_id: 123456789012")
	assert.NotContains(t, awsMsg, "lambda-xray-active")
	assert.NotContains(t, awsMsg, "cloudrun-otel-sidecar")
	assert.NotContains(t, awsMsg, "azfunc-appinsights-enable")
	assert.NotContains(t, awsMsg, "ocifunc-apm-enable")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.Contains(t, gcpMsg, "project_id: my-sandbox-project")
	assert.NotContains(t, gcpMsg, "cloudrun-otel-sidecar")
	assert.NotContains(t, gcpMsg, "cloudfunc-trace-enable")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.Contains(t, azureMsg, "subscription_id: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	assert.NotContains(t, azureMsg, "azfunc-appinsights-enable")
	assert.NotContains(t, azureMsg, "azfunc-otel-distro")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.Contains(t, ociMsg, "tenancy_ocid: ocid1.tenancy.oc1..aaaaaaaa")
	assert.NotContains(t, ociMsg, "ocifunc-apm-enable")
	assert.NotContains(t, ociMsg, "ocifunc-otel-distro")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// TestDiscoveryProposer_OrchestrationKindsInSystemPrompt — orchestration
// tier slice 1 chunk 4 (v0.89.97, #731 Stream 129). The 6 new
// per-cloud orchestration recommendation kinds must appear in the
// shared system prompt so the model can route findings to the right
// kind when the scan inventory carries orchestration rows. Slice 1
// compute + database + Kubernetes + serverless tier kinds must remain
// present after the orchestration extension — same shared-system-prompt
// invariant the prior chunk 5 tests pin.
func TestDiscoveryProposer_OrchestrationKindsInSystemPrompt(t *testing.T) {
	for _, orchKind := range []string{
		// AWS Step Functions.
		"stepfunc-xray-active", "stepfunc-logging-enable",
		// GCP Workflows.
		"workflows-trace-enable", "workflows-logging-enable",
		// Azure Logic Apps.
		"logicapps-appinsights-enable", "logicapps-diagnostics-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, orchKind,
			"shared system prompt should teach the orchestration tier slice 1 kind %q", orchKind)
	}
	// Prior-tier kinds still present.
	for _, priorKind := range []string{
		"lambda-xray-active", "cloudrun-otel-sidecar",
		"azfunc-appinsights-enable", "ocifunc-apm-enable",
		"gce-otel-label", "vm-otel-tag", "compute-otel-tag",
		"ec2-otel-layer", "rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorKind,
			"shared system prompt should still teach the prior-tier kind %q after the orchestration tier", priorKind)
	}
	// Reasoning template tokens.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"Orchestration workflows",
		"shared system prompt should carry the orchestration tier reasoning template")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"OCI orchestration is",
		"shared system prompt should call out the OCI deferral note")
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostOrchestrationSlice1
// — orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129)
// cold-start parity invariant: across all four providers, the
// compute-only user message produced by buildDiscoveryUserMessage
// must remain byte-identical to v0.89.93 / v0.89.88 when the scan
// context carries no orchestration rows. Acceptance test §11
// invariant — adding orchestration tier kinds must not perturb
// compute-only prompt generation for any provider. The new kinds
// live ONLY in the system prompt; the user message has no
// orchestration section, so a cold-start scan renders the same body
// the prior chunk 5 tier extensions pinned.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostOrchestrationSlice1(t *testing.T) {
	// AWS cold start.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.NotContains(t, awsMsg, "stepfunc-xray-active")
	assert.NotContains(t, awsMsg, "stepfunc-logging-enable")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.NotContains(t, gcpMsg, "workflows-trace-enable")
	assert.NotContains(t, gcpMsg, "workflows-logging-enable")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.NotContains(t, azureMsg, "logicapps-appinsights-enable")
	assert.NotContains(t, azureMsg, "logicapps-diagnostics-enable")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	// OCI is explicitly deferred to slice 2; the cold-start prompt
	// carries no orchestration tokens because the user-message
	// renderer has no orchestration section. The system-prompt
	// section names the kinds but does not embed them into the user
	// message.
	assert.NotContains(t, ociMsg, "stepfunc-")
	assert.NotContains(t, ociMsg, "workflows-trace-enable")
	assert.NotContains(t, ociMsg, "logicapps-appinsights-enable")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// --- Event source tier slice 1 chunk 5 (v0.89.102, #738 Stream 136) -

// TestDiscoveryProposer_EventSourceKindsInSystemPrompt — event source
// tier slice 1 chunk 5. The 7 new per-cloud event source recommendation
// kinds must appear in the shared system prompt so the model can route
// findings to the right kind when the scan inventory carries event
// source rows. Prior-tier kinds must remain present after the
// extension — same shared-system-prompt invariant the prior chunk 5
// tests pin.
func TestDiscoveryProposer_EventSourceKindsInSystemPrompt(t *testing.T) {
	for _, evtKind := range []string{
		// AWS EventBridge.
		"eventbridge-xray-enable",
		"eventbridge-schemas-discover",
		"eventbridge-logging-enable",
		// GCP Pub/Sub.
		"pubsub-trace-enable",
		"pubsub-schema-attach",
		// Azure Service Bus.
		"servicebus-diagnostics-enable",
		// OCI Streaming.
		"streaming-logging-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, evtKind,
			"shared system prompt should teach the event source tier slice 1 kind %q", evtKind)
	}
	// Prior-tier kinds still present after the event source extension.
	for _, priorKind := range []string{
		"stepfunc-xray-active",
		"workflows-trace-enable",
		"logicapps-appinsights-enable",
		"lambda-xray-active",
		"cloudrun-otel-sidecar",
		"ocifunc-apm-enable",
		"gce-otel-label",
		"vm-otel-tag",
		"compute-otel-tag",
		"ec2-otel-layer",
		"rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorKind,
			"shared system prompt should still teach the prior-tier kind %q after the event source tier", priorKind)
	}
	// Reasoning template tokens.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"Event sources are the",
		"shared system prompt should carry the event source tier reasoning template")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"root of trace continuity",
		"shared system prompt should call out the event source as root of trace continuity")
}

// TestDiscoveryProposer_PropagationKindsInSystemPrompt — event source
// tier slice 2 chunk 5 (v0.89.107, #745 Stream 143). The 5 new
// per-message propagation recommendation kinds must appear in the
// shared system prompt so the model can route propagation findings to
// the right kind when the scan inventory carries event source rows
// with has_propagation_config=false. Prior-tier kinds (including the
// slice 1 event source kinds) must remain present after the
// extension — same shared-system-prompt invariant the prior chunk 5
// tests pin.
func TestDiscoveryProposer_PropagationKindsInSystemPrompt(t *testing.T) {
	for _, propKind := range []string{
		"eventbridge-rule-preserves-trace",
		"pubsub-schema-includes-traceparent",
		"pubsub-subscription-preserves-attrs",
		"servicebus-policy-preserves-traceparent",
		"streaming-config-preserves-headers",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, propKind,
			"shared system prompt should teach the event source tier slice 2 propagation kind %q", propKind)
	}
	// Slice 1 event source kinds still present after the slice 2
	// extension.
	for _, slice1Kind := range []string{
		"eventbridge-xray-enable",
		"eventbridge-schemas-discover",
		"eventbridge-logging-enable",
		"pubsub-trace-enable",
		"pubsub-schema-attach",
		"servicebus-diagnostics-enable",
		"streaming-logging-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, slice1Kind,
			"shared system prompt should still teach the slice 1 event source kind %q after the slice 2 extension", slice1Kind)
	}
	// Prior-tier kinds still present.
	for _, priorKind := range []string{
		"stepfunc-xray-active",
		"workflows-trace-enable",
		"logicapps-appinsights-enable",
		"lambda-xray-active",
		"cloudrun-otel-sidecar",
		"ocifunc-apm-enable",
		"rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorKind,
			"shared system prompt should still teach the prior-tier kind %q after the slice 2 event source extension", priorKind)
	}
	// Reasoning template tokens for the slice 2 propagation section.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"EVENT SOURCE TIER PROPAGATION KINDS (slice 2)",
		"shared system prompt should include the slice 2 propagation section header")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"propagation config would drop trace context",
		"shared system prompt should carry the propagation reasoning template")
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice1
// — event source tier slice 1 chunk 5 cold-start parity invariant:
// across all four providers, the compute-only user message produced by
// buildDiscoveryUserMessage must remain byte-identical to v0.89.98 when
// the scan context carries no event source rows. The new kinds live
// ONLY in the system prompt; the user message has no event source
// section, so a cold-start scan renders the same body the prior chunk
// 5 tier extensions pinned.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice1(t *testing.T) {
	// AWS cold start.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.NotContains(t, awsMsg, "eventbridge-xray-enable")
	assert.NotContains(t, awsMsg, "eventbridge-schemas-discover")
	assert.NotContains(t, awsMsg, "eventbridge-logging-enable")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.NotContains(t, gcpMsg, "pubsub-trace-enable")
	assert.NotContains(t, gcpMsg, "pubsub-schema-attach")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.NotContains(t, azureMsg, "servicebus-diagnostics-enable")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start. Unlike orchestration, OCI gets an event source
	// surface in slice 1 — but the user-message renderer is unchanged
	// because the new kinds live only in the system prompt.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.NotContains(t, ociMsg, "streaming-logging-enable")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice2
// — event source tier slice 2 chunk 5 (v0.89.107, #745 Stream 143)
// cold-start parity invariant: across all four providers, the user
// message produced by buildDiscoveryUserMessage must remain
// byte-identical to v0.89.103 when the scan context carries no event
// source rows (and therefore no propagation rows). The 5 new
// propagation kinds live ONLY in the system prompt; the user message
// has no propagation section, so a cold-start scan renders the same
// body v0.89.103 pinned. This pins design doc §11 acceptance test 16.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice2(t *testing.T) {
	// AWS cold start. The slice 2 propagation kind for EventBridge
	// must NOT leak into the user message — it belongs to the system
	// prompt only.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.NotContains(t, awsMsg, "eventbridge-rule-preserves-trace")
	assert.NotContains(t, awsMsg, "EVENT SOURCE TIER PROPAGATION KINDS")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.NotContains(t, gcpMsg, "pubsub-schema-includes-traceparent")
	assert.NotContains(t, gcpMsg, "pubsub-subscription-preserves-attrs")
	assert.NotContains(t, gcpMsg, "EVENT SOURCE TIER PROPAGATION KINDS")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.NotContains(t, azureMsg, "servicebus-policy-preserves-traceparent")
	assert.NotContains(t, azureMsg, "EVENT SOURCE TIER PROPAGATION KINDS")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.NotContains(t, ociMsg, "streaming-config-preserves-headers")
	assert.NotContains(t, ociMsg, "EVENT SOURCE TIER PROPAGATION KINDS")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// --- Event source tier slice 3 chunk 2 (v0.89.139, #779 Stream 177) -

// TestDiscoveryProposer_SNSKindsInSystemPrompt — event source tier
// slice 3 chunk 2. The 2 new AWS SNS recommendation kinds must
// appear in the shared system prompt so the model can route findings
// to the right kind when the scan inventory carries SNS topic rows.
// Prior-tier kinds (including slice 1 + slice 2 event source kinds)
// must remain present after the extension — same shared-system-prompt
// invariant the prior chunk 5 / chunk 2 tests pin.
func TestDiscoveryProposer_SNSKindsInSystemPrompt(t *testing.T) {
	for _, snsKind := range []string{
		"sns-subscriptions-attach",
		"sns-delivery-logging-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, snsKind,
			"shared system prompt should teach the event source tier slice 3 SNS kind %q", snsKind)
	}
	// Slice 1 + slice 2 event source kinds still present after the
	// slice 3 extension.
	for _, priorEvtKind := range []string{
		"eventbridge-xray-enable",
		"eventbridge-schemas-discover",
		"eventbridge-logging-enable",
		"pubsub-trace-enable",
		"pubsub-schema-attach",
		"servicebus-diagnostics-enable",
		"streaming-logging-enable",
		"eventbridge-rule-preserves-trace",
		"pubsub-schema-includes-traceparent",
		"pubsub-subscription-preserves-attrs",
		"servicebus-policy-preserves-traceparent",
		"streaming-config-preserves-headers",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorEvtKind,
			"shared system prompt should still teach the prior event source kind %q after the slice 3 extension", priorEvtKind)
	}
	// Prior-tier kinds still present.
	for _, priorKind := range []string{
		"stepfunc-xray-active",
		"workflows-trace-enable",
		"logicapps-appinsights-enable",
		"resmgr-logging-enable",
		"lambda-xray-active",
		"cloudrun-otel-sidecar",
		"ocifunc-apm-enable",
		"rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorKind,
			"shared system prompt should still teach the prior-tier kind %q after the slice 3 SNS extension", priorKind)
	}
	// Reasoning template tokens for the slice 3 SNS section.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"EVENT SOURCE TIER SLICE 3 — AWS SNS",
		"shared system prompt should include the slice 3 SNS section header")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"per-protocol delivery feedback role",
		"shared system prompt should describe the canonical SNS delivery feedback signal")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"AUDIT-ONLY recommendation",
		"shared system prompt should mark sns-subscriptions-attach as audit-only")
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice3
// — event source tier slice 3 chunk 2 cold-start parity invariant:
// across all four providers, the compute-only user message produced
// by buildDiscoveryUserMessage must remain byte-identical to v0.89.136
// when the scan context carries no SNS rows. The new kinds live ONLY
// in the system prompt; the user message has no SNS section, so a
// cold-start scan renders the same body the prior chunk extensions
// pinned. Pins design doc §11 acceptance test 13.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice3(t *testing.T) {
	// AWS cold start. The slice 3 SNS kinds must NOT leak into the
	// user message — they belong to the system prompt only.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.NotContains(t, awsMsg, "sns-subscriptions-attach")
	assert.NotContains(t, awsMsg, "sns-delivery-logging-enable")
	assert.NotContains(t, awsMsg, "EVENT SOURCE TIER SLICE 3")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.NotContains(t, gcpMsg, "sns-subscriptions-attach")
	assert.NotContains(t, gcpMsg, "sns-delivery-logging-enable")
	assert.NotContains(t, gcpMsg, "EVENT SOURCE TIER SLICE 3")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.NotContains(t, azureMsg, "sns-subscriptions-attach")
	assert.NotContains(t, azureMsg, "sns-delivery-logging-enable")
	assert.NotContains(t, azureMsg, "EVENT SOURCE TIER SLICE 3")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.NotContains(t, ociMsg, "sns-subscriptions-attach")
	assert.NotContains(t, ociMsg, "sns-delivery-logging-enable")
	assert.NotContains(t, ociMsg, "EVENT SOURCE TIER SLICE 3")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// --- Event source tier slice 4 chunk 2 (v0.89.142, #782 Stream 180) -

// TestDiscoveryProposer_SQSKindsInSystemPrompt — event source tier
// slice 4 chunk 2. The 2 new AWS SQS recommendation kinds must
// appear in the shared system prompt so the model can route findings
// to the right kind when the scan inventory carries SQS queue rows.
// Prior-tier kinds (including slice 1 + slice 2 + slice 3 event
// source kinds) must remain present after the extension — same
// shared-system-prompt invariant the prior chunk tests pin.
func TestDiscoveryProposer_SQSKindsInSystemPrompt(t *testing.T) {
	for _, sqsKind := range []string{
		"sqs-redrive-policy-enable",
		"sqs-deadletter-queue-attach",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, sqsKind,
			"shared system prompt should teach the event source tier slice 4 SQS kind %q", sqsKind)
	}
	// Slice 1 + slice 2 + slice 3 event source kinds still present
	// after the slice 4 extension.
	for _, priorEvtKind := range []string{
		"eventbridge-xray-enable",
		"eventbridge-schemas-discover",
		"eventbridge-logging-enable",
		"pubsub-trace-enable",
		"pubsub-schema-attach",
		"servicebus-diagnostics-enable",
		"streaming-logging-enable",
		"eventbridge-rule-preserves-trace",
		"pubsub-schema-includes-traceparent",
		"pubsub-subscription-preserves-attrs",
		"servicebus-policy-preserves-traceparent",
		"streaming-config-preserves-headers",
		"sns-subscriptions-attach",
		"sns-delivery-logging-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorEvtKind,
			"shared system prompt should still teach the prior event source kind %q after the slice 4 extension", priorEvtKind)
	}
	// Prior-tier kinds still present.
	for _, priorKind := range []string{
		"stepfunc-xray-active",
		"workflows-trace-enable",
		"logicapps-appinsights-enable",
		"resmgr-logging-enable",
		"lambda-xray-active",
		"cloudrun-otel-sidecar",
		"ocifunc-apm-enable",
		"rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorKind,
			"shared system prompt should still teach the prior-tier kind %q after the slice 4 SQS extension", priorKind)
	}
	// Reasoning template tokens for the slice 4 SQS section.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"EVENT SOURCE TIER SLICE 4 — AWS SQS",
		"shared system prompt should include the slice 4 SQS section header")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"redrive policy",
		"shared system prompt should describe the canonical SQS redrive policy signal")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"AUDIT-ONLY recommendation",
		"shared system prompt should mark the audit-only SQS DLQ kind")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"SINGLE MOST COMMON AWS messaging production",
		"shared system prompt should carry the slice 4 framing for sqs-redrive-policy-enable")
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice4
// — event source tier slice 4 chunk 2 cold-start parity invariant:
// across all four providers, the compute-only user message produced
// by buildDiscoveryUserMessage must remain byte-identical to v0.89.139
// when the scan context carries no SQS rows. The new kinds live ONLY
// in the system prompt; the user message has no SQS section, so a
// cold-start scan renders the same body the prior chunk extensions
// pinned. Pins design doc §11 acceptance test 15.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice4(t *testing.T) {
	// AWS cold start. The slice 4 SQS kinds must NOT leak into the
	// user message — they belong to the system prompt only.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.NotContains(t, awsMsg, "sqs-redrive-policy-enable")
	assert.NotContains(t, awsMsg, "sqs-deadletter-queue-attach")
	assert.NotContains(t, awsMsg, "EVENT SOURCE TIER SLICE 4")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.NotContains(t, gcpMsg, "sqs-redrive-policy-enable")
	assert.NotContains(t, gcpMsg, "sqs-deadletter-queue-attach")
	assert.NotContains(t, gcpMsg, "EVENT SOURCE TIER SLICE 4")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.NotContains(t, azureMsg, "sqs-redrive-policy-enable")
	assert.NotContains(t, azureMsg, "sqs-deadletter-queue-attach")
	assert.NotContains(t, azureMsg, "EVENT SOURCE TIER SLICE 4")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.NotContains(t, ociMsg, "sqs-redrive-policy-enable")
	assert.NotContains(t, ociMsg, "sqs-deadletter-queue-attach")
	assert.NotContains(t, ociMsg, "EVENT SOURCE TIER SLICE 4")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// --- Event source tier slice 5 chunk 2 (v0.89.145, #785 Stream 183) -

// TestDiscoveryProposer_CloudTasksKindsInSystemPrompt — event source
// tier slice 5 chunk 2. The 2 new GCP Cloud Tasks recommendation kinds
// must appear in the shared system prompt so the model can route
// findings to the right kind when the scan inventory carries Cloud
// Tasks queue rows. Prior-tier kinds (including slice 1-4 event
// source kinds) must remain present after the extension.
func TestDiscoveryProposer_CloudTasksKindsInSystemPrompt(t *testing.T) {
	for _, ctKind := range []string{
		"cloudtasks-retry-policy-enable",
		"cloudtasks-logging-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, ctKind,
			"shared system prompt should teach the event source tier slice 5 Cloud Tasks kind %q", ctKind)
	}
	// Slice 1 + slice 2 + slice 3 + slice 4 event source kinds still
	// present after the slice 5 extension.
	for _, priorEvtKind := range []string{
		"eventbridge-xray-enable",
		"eventbridge-schemas-discover",
		"eventbridge-logging-enable",
		"pubsub-trace-enable",
		"pubsub-schema-attach",
		"servicebus-diagnostics-enable",
		"streaming-logging-enable",
		"eventbridge-rule-preserves-trace",
		"pubsub-schema-includes-traceparent",
		"pubsub-subscription-preserves-attrs",
		"servicebus-policy-preserves-traceparent",
		"streaming-config-preserves-headers",
		"sns-subscriptions-attach",
		"sns-delivery-logging-enable",
		"sqs-redrive-policy-enable",
		"sqs-deadletter-queue-attach",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorEvtKind,
			"shared system prompt should still teach the prior event source kind %q after the slice 5 extension", priorEvtKind)
	}
	// Reasoning template tokens for the slice 5 Cloud Tasks section.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"EVENT SOURCE TIER SLICE 5 — GCP CLOUD TASKS",
		"shared system prompt should include the slice 5 Cloud Tasks section header")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"google_cloud_tasks_queue",
		"shared system prompt should reference the Terraform resource name")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"maxAttempts = -1",
		"shared system prompt should call out the unlimited-retry sentinel")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"Pub/Sub → Cloud Tasks → HTTP target",
		"shared system prompt should name the canonical GCP pub/sub-with-retry architecture")
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice5
// — event source tier slice 5 chunk 2 cold-start parity invariant:
// across all four providers, the compute-only user message produced
// by buildDiscoveryUserMessage must remain byte-identical to v0.89.142
// when the scan context carries no Cloud Tasks rows. The new kinds
// live ONLY in the system prompt; the user message has no Cloud Tasks
// section, so a cold-start scan renders the same body the prior chunk
// extensions pinned. Pins design doc §11 acceptance test 17.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice5(t *testing.T) {
	// AWS cold start. The slice 5 Cloud Tasks kinds must NOT leak into
	// the user message — they belong to the system prompt only.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.NotContains(t, awsMsg, "cloudtasks-retry-policy-enable")
	assert.NotContains(t, awsMsg, "cloudtasks-logging-enable")
	assert.NotContains(t, awsMsg, "EVENT SOURCE TIER SLICE 5")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.NotContains(t, gcpMsg, "cloudtasks-retry-policy-enable")
	assert.NotContains(t, gcpMsg, "cloudtasks-logging-enable")
	assert.NotContains(t, gcpMsg, "EVENT SOURCE TIER SLICE 5")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.NotContains(t, azureMsg, "cloudtasks-retry-policy-enable")
	assert.NotContains(t, azureMsg, "cloudtasks-logging-enable")
	assert.NotContains(t, azureMsg, "EVENT SOURCE TIER SLICE 5")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.NotContains(t, ociMsg, "cloudtasks-retry-policy-enable")
	assert.NotContains(t, ociMsg, "cloudtasks-logging-enable")
	assert.NotContains(t, ociMsg, "EVENT SOURCE TIER SLICE 5")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// --- Event source tier slice 6 chunk 2 (v0.89.148, #788 Stream 186) -

// TestDiscoveryProposer_EventGridKindsInSystemPrompt — event source
// tier slice 6 chunk 2. The 2 new Azure Event Grid recommendation
// kinds must appear in the shared system prompt so the model can
// route findings to the right kind when the scan inventory carries
// Event Grid topic rows. Prior-tier kinds (including slice 1-5 event
// source kinds) must remain present after the extension.
func TestDiscoveryProposer_EventGridKindsInSystemPrompt(t *testing.T) {
	for _, egKind := range []string{
		"eventgrid-diagnostics-enable",
		"eventgrid-cloudevent-schema-enforce",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, egKind,
			"shared system prompt should teach the event source tier slice 6 Event Grid kind %q", egKind)
	}
	// Slice 1 + slice 2 + slice 3 + slice 4 + slice 5 event source kinds
	// still present after the slice 6 extension.
	for _, priorEvtKind := range []string{
		"eventbridge-xray-enable",
		"eventbridge-schemas-discover",
		"eventbridge-logging-enable",
		"pubsub-trace-enable",
		"pubsub-schema-attach",
		"servicebus-diagnostics-enable",
		"streaming-logging-enable",
		"eventbridge-rule-preserves-trace",
		"pubsub-schema-includes-traceparent",
		"pubsub-subscription-preserves-attrs",
		"servicebus-policy-preserves-traceparent",
		"streaming-config-preserves-headers",
		"sns-subscriptions-attach",
		"sns-delivery-logging-enable",
		"sqs-redrive-policy-enable",
		"sqs-deadletter-queue-attach",
		"cloudtasks-retry-policy-enable",
		"cloudtasks-logging-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorEvtKind,
			"shared system prompt should still teach the prior event source kind %q after the slice 6 extension", priorEvtKind)
	}
	// Reasoning template tokens for the slice 6 Event Grid section.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"EVENT SOURCE TIER SLICE 6 — AZURE EVENT GRID",
		"shared system prompt should include the slice 6 Event Grid section header")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"azurerm_monitor_diagnostic_setting",
		"shared system prompt should reference the Terraform resource for diagnostics")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"CloudEventSchemaV1_0",
		"shared system prompt should reference the canonical CloudEvents 1.0 schema identifier")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"BREAKING CHANGE",
		"shared system prompt should call out the BREAKING CHANGE warning on schema enforcement")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"Event Grid → Service Bus / Functions / Logic Apps",
		"shared system prompt should name the canonical Azure event distribution architecture")
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice6
// — event source tier slice 6 chunk 2 cold-start parity invariant:
// across all four providers, the compute-only user message produced
// by buildDiscoveryUserMessage must remain byte-identical to v0.89.145
// when the scan context carries no Event Grid rows. The new kinds
// live ONLY in the system prompt; the user message has no Event Grid
// section, so a cold-start scan renders the same body the prior chunk
// extensions pinned. Pins design doc §11 acceptance test 17.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice6(t *testing.T) {
	// AWS cold start. The slice 6 Event Grid kinds must NOT leak into
	// the user message — they belong to the system prompt only.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.NotContains(t, awsMsg, "eventgrid-diagnostics-enable")
	assert.NotContains(t, awsMsg, "eventgrid-cloudevent-schema-enforce")
	assert.NotContains(t, awsMsg, "EVENT SOURCE TIER SLICE 6")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.NotContains(t, gcpMsg, "eventgrid-diagnostics-enable")
	assert.NotContains(t, gcpMsg, "eventgrid-cloudevent-schema-enforce")
	assert.NotContains(t, gcpMsg, "EVENT SOURCE TIER SLICE 6")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.NotContains(t, azureMsg, "eventgrid-diagnostics-enable")
	assert.NotContains(t, azureMsg, "eventgrid-cloudevent-schema-enforce")
	assert.NotContains(t, azureMsg, "EVENT SOURCE TIER SLICE 6")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.NotContains(t, ociMsg, "eventgrid-diagnostics-enable")
	assert.NotContains(t, ociMsg, "eventgrid-cloudevent-schema-enforce")
	assert.NotContains(t, ociMsg, "EVENT SOURCE TIER SLICE 6")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// TestDiscoveryProposer_ColdStartKindInSystemPrompt — Cold-start
// latency analysis slice 1 chunk 3 (v0.89.115, #753 Stream 151). The
// new lambda-cold-start-baseline recommendation kind must appear in
// the shared system prompt so the model can route cold-start findings
// to the right kind when the scan inventory carries Lambda rows that
// crossed the 1.5x ratio + 500ms floor predicates. Prior-tier kinds
// must remain present after the extension.
func TestDiscoveryProposer_ColdStartKindInSystemPrompt(t *testing.T) {
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"lambda-cold-start-baseline",
		"shared system prompt should teach the cold-start slice 1 kind")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"SERVERLESS COLD-START KINDS",
		"shared system prompt should include the cold-start section header")
	// Three-failure-mode framing from §8.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"Init script regression",
		"shared system prompt should carry the cold-start cause 1 framing")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"Cold-start frequency increase",
		"shared system prompt should carry the cold-start cause 2 framing")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"Architecture change",
		"shared system prompt should carry the cold-start cause 3 framing")
	// Terraform shape cue.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"aws_lambda_provisioned_concurrency_config",
		"shared system prompt should name the Terraform resource the picker emits")
	// REASONING TEMPLATE block.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"REASONING TEMPLATE for cold-start recommendations",
		"shared system prompt should include the cold-start reasoning template header")
	// Prior-tier kinds still present after the cold-start extension.
	for _, priorKind := range []string{
		"lambda-xray-active",
		"lambda-otel-layer",
		"lambda-otel-wrapper",
		"eventbridge-rule-preserves-trace",
		"streaming-config-preserves-headers",
		"ec2-otel-layer",
		"rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorKind,
			"shared system prompt should still teach the prior-tier kind %q after the cold-start extension", priorKind)
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostColdStartSlice1
// — Cold-start latency analysis slice 1 chunk 3 (v0.89.115, #753
// Stream 151) cold-start parity invariant: across all four providers,
// the user message produced by buildDiscoveryUserMessage must remain
// byte-identical to v0.89.111 when the scan context carries no
// cold-start observations. The new kind lives ONLY in the system
// prompt; the user message has no cold-start section, so a cold-start
// scan renders the same body the prior chunk-5 tier extensions
// pinned. This pins design doc §11 acceptance test 15.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostColdStartSlice1(t *testing.T) {
	// AWS cold start. The slice 1 chunk 3 cold-start kind must NOT
	// leak into the user message — it belongs to the system prompt
	// only.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.NotContains(t, awsMsg, "lambda-cold-start-baseline")
	assert.NotContains(t, awsMsg, "SERVERLESS COLD-START KINDS")
	assert.NotContains(t, awsMsg, "aws_lambda_provisioned_concurrency_config")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.NotContains(t, gcpMsg, "lambda-cold-start-baseline")
	assert.NotContains(t, gcpMsg, "SERVERLESS COLD-START KINDS")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.NotContains(t, azureMsg, "lambda-cold-start-baseline")
	assert.NotContains(t, azureMsg, "SERVERLESS COLD-START KINDS")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.NotContains(t, ociMsg, "lambda-cold-start-baseline")
	assert.NotContains(t, ociMsg, "SERVERLESS COLD-START KINDS")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// TestDiscoveryProposer_FourCloudColdStartKindsInSystemPrompt —
// Cold-start latency analysis slice 2 chunk 4 (v0.89.119, #759 Stream
// 157). The four new per-cloud cold-start kinds must appear in the
// shared system prompt so the model can route findings to the right
// kind when the scan inventory carries Cloud Run / Cloud Functions /
// Azure Functions / OCI Functions rows that crossed the substrate
// thresholds. The slice 1 lambda kind must remain present alongside
// the new four — the substrate's cross-cloud uniform thresholds
// claim depends on all five kinds being co-located in one section.
func TestDiscoveryProposer_FourCloudColdStartKindsInSystemPrompt(t *testing.T) {
	for _, kind := range []string{
		"lambda-cold-start-baseline",
		"cloudrun-cold-start-baseline",
		"cloudfunc-cold-start-baseline",
		"azfunc-cold-start-baseline",
		"ocifunc-cold-start-baseline",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, kind,
			"shared system prompt should teach the cold-start kind %q", kind)
	}
	// The slice 2 framing extends the section header.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"SERVERLESS COLD-START KINDS (cold-start latency analysis slice 1 + slice 2)",
		"prompt section header should mention both slice 1 and slice 2")
	// 3-failure-mode framing applies to all 4 kinds.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"3-FAILURE-MODE REASONING applies to all 4 kinds",
		"prompt should call out that 3-failure-mode framing applies cross-cloud")
	// Per-cloud caveat summary block.
	for _, want := range []string{
		"warm-path inclusion",
		"IsAfterColdStart",
		"function_duration not cold-start-isolated",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"prompt should call out per-cloud caveat: %q", want)
	}
	// Terraform shape cues per cloud.
	for _, want := range []string{
		"autoscaling.knative.dev/minScale",
		"min_instance_count",
		`sku_name = "EP1"`,
		"WEBSITE_USE_PLACEHOLDER",
		"WARMUP_DELAY",
		"provisioned_concurrent_executions",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"prompt should name the per-cloud Terraform shape: %q", want)
	}
	// Slice 1 framing remains intact.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"aws_lambda_provisioned_concurrency_config",
		"slice 1 AWS Terraform shape should remain after the slice 2 extension")
	// Prior-tier kinds still present.
	for _, priorKind := range []string{
		"lambda-xray-active",
		"lambda-otel-layer",
		"eventbridge-rule-preserves-trace",
		"rds-pi-em",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorKind,
			"prior-tier kind %q should still appear after the slice 2 cold-start extension", priorKind)
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostColdStartSlice2
// — Cold-start parity invariant (slice 2 §11 acceptance test 13).
// Across all four providers, the user message produced by
// buildDiscoveryUserMessage must remain byte-identical to v0.89.116
// when the scan context carries no cold-start observations. The four
// new kinds live ONLY in the system prompt; the user message has no
// cold-start section, so a cold-start scan renders the same body the
// prior tier extensions pinned.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostColdStartSlice2(t *testing.T) {
	// AWS.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold-s2",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	for _, kind := range []string{
		"lambda-cold-start-baseline",
		"cloudrun-cold-start-baseline",
		"cloudfunc-cold-start-baseline",
		"azfunc-cold-start-baseline",
		"ocifunc-cold-start-baseline",
		"SERVERLESS COLD-START KINDS",
		"autoscaling.knative.dev/minScale",
		"WARMUP_DELAY",
	} {
		assert.NotContains(t, awsMsg, kind,
			"AWS user message should NOT include cold-start system-prompt content: %q", kind)
	}

	// GCP.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold-s2",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	for _, kind := range []string{
		"cloudrun-cold-start-baseline",
		"cloudfunc-cold-start-baseline",
		"SERVERLESS COLD-START KINDS",
		"autoscaling.knative.dev/minScale",
	} {
		assert.NotContains(t, gcpMsg, kind,
			"GCP user message should NOT include cold-start system-prompt content: %q", kind)
	}

	// Azure.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold-s2",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	for _, kind := range []string{
		"azfunc-cold-start-baseline",
		"SERVERLESS COLD-START KINDS",
		"WEBSITE_USE_PLACEHOLDER",
	} {
		assert.NotContains(t, azureMsg, kind,
			"Azure user message should NOT include cold-start system-prompt content: %q", kind)
	}

	// OCI.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold-s2",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	for _, kind := range []string{
		"ocifunc-cold-start-baseline",
		"SERVERLESS COLD-START KINDS",
		"WARMUP_DELAY",
	} {
		assert.NotContains(t, ociMsg, kind,
			"OCI user message should NOT include cold-start system-prompt content: %q", kind)
	}
}

// TestDiscoveryProposer_SamplingKindInSystemPrompt — sampling rate
// analysis slice 1 chunk 2 (v0.89.123). The new
// span-quality-sampling-too-aggressive kind + its 3-failure-mode
// reasoning framing must appear in the discovery system prompt
// alongside the existing kind catalog. Pinning the section
// presence prevents accidental removal during future edits.
func TestDiscoveryProposer_SamplingKindInSystemPrompt(t *testing.T) {
	prompt := DiscoverySystemPromptForTest()
	for _, want := range []string{
		"SAMPLING RATE KIND",
		"span-quality-sampling-too-aggressive",
		"OTEL_TRACES_SAMPLER_ARG",
		"Default sampler too aggressive",
		"Adaptive sampling throttling",
		"Tail-sampling collector",
		"observed_span_count",
		"expected_invocation_count",
		// The 0.5 default IS operator-tunable — the prompt MUST
		// document that explicitly so the model surfaces it in
		// the recommendation reasoning text.
		"OPERATOR TUNES",
		// Reuses the span-quality- prefix; the prompt must say so.
		"span-quality- webhook prefix",
	} {
		assert.Contains(t, prompt, want,
			"system prompt missing sampling-rate content: %q", want)
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostSamplingSlice1
// — Cold-start parity invariant per §11 acceptance test 16. Across
// all four providers, the user message produced by
// buildDiscoveryUserMessage must remain byte-identical to v0.89.120
// when the scan context carries no sampling-rate observations. The
// new sampling kind lives ONLY in the system prompt; the user
// message has no sampling section, so a cold-start scan renders
// the same body the prior tier extensions pinned.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostSamplingSlice1(t *testing.T) {
	// AWS.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-sampling-s1",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	for _, kind := range []string{
		"span-quality-sampling-too-aggressive",
		"SAMPLING RATE KIND",
		"OTEL_TRACES_SAMPLER_ARG",
	} {
		assert.NotContains(t, awsMsg, kind,
			"AWS user message should NOT include sampling system-prompt content: %q", kind)
	}

	// GCP.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-sampling-s1",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	for _, kind := range []string{
		"span-quality-sampling-too-aggressive",
		"SAMPLING RATE KIND",
		"OTEL_TRACES_SAMPLER_ARG",
	} {
		assert.NotContains(t, gcpMsg, kind,
			"GCP user message should NOT include sampling system-prompt content: %q", kind)
	}

	// Azure.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-sampling-s1",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	for _, kind := range []string{
		"span-quality-sampling-too-aggressive",
		"SAMPLING RATE KIND",
		"OTEL_TRACES_SAMPLER_ARG",
	} {
		assert.NotContains(t, azureMsg, kind,
			"Azure user message should NOT include sampling system-prompt content: %q", kind)
	}

	// OCI.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-sampling-s1",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	for _, kind := range []string{
		"span-quality-sampling-too-aggressive",
		"SAMPLING RATE KIND",
		"OTEL_TRACES_SAMPLER_ARG",
	} {
		assert.NotContains(t, ociMsg, kind,
			"OCI user message should NOT include sampling system-prompt content: %q", kind)
	}
}

// TestDiscoveryProposer_ErrorRateKindInSystemPrompt — error rate
// correlation slice 1 chunk 2 (v0.89.128). The new
// span-quality-error-rate-spike kind + its 3-failure-mode reasoning
// framing must appear in the discovery system prompt alongside the
// existing kind catalog. Pinning the section presence prevents
// accidental removal during future edits.
func TestDiscoveryProposer_ErrorRateKindInSystemPrompt(t *testing.T) {
	prompt := DiscoverySystemPromptForTest()
	for _, want := range []string{
		"ERROR RATE CORRELATION KIND",
		"span-quality-error-rate-spike",
		"Recent deploy regression",
		"Downstream dependency failure",
		"Resource exhaustion under load",
		"Near-zero baseline guard",
		"MORE COMMON",
		// Per-cloud error metric names — the section MUST enumerate
		// them so the model knows which metric the detection branch
		// reads for each surface.
		"Lambda Errors",
		"request_count{5xx}",
		"execution_count{error}",
		"FunctionErrors",
		"function_invocation_count{error}",
	} {
		assert.Contains(t, prompt, want,
			"system prompt missing error-rate content: %q", want)
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostErrorRateSlice1
// — Cold-start parity invariant per §11 acceptance test 15. Across
// all four providers, the user message produced by
// buildDiscoveryUserMessage must remain byte-identical to v0.89.125
// when the scan context carries no error-rate observations. The new
// error rate kind lives ONLY in the system prompt; the user
// message has no error-rate section, so a cold-start scan renders
// the same body the prior tier extensions pinned.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostErrorRateSlice1(t *testing.T) {
	// AWS.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-errorrate-s1",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	for _, kind := range []string{
		"ERROR RATE CORRELATION KIND",
		"span-quality-error-rate-spike",
		"Resource exhaustion under load",
	} {
		assert.NotContains(t, awsMsg, kind,
			"AWS user message should NOT include error-rate system-prompt content: %q", kind)
	}

	// GCP.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-errorrate-s1",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	for _, kind := range []string{
		"ERROR RATE CORRELATION KIND",
		"span-quality-error-rate-spike",
		"Resource exhaustion under load",
	} {
		assert.NotContains(t, gcpMsg, kind,
			"GCP user message should NOT include error-rate system-prompt content: %q", kind)
	}

	// Azure.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-errorrate-s1",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	for _, kind := range []string{
		"ERROR RATE CORRELATION KIND",
		"span-quality-error-rate-spike",
		"Resource exhaustion under load",
	} {
		assert.NotContains(t, azureMsg, kind,
			"Azure user message should NOT include error-rate system-prompt content: %q", kind)
	}

	// OCI.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-errorrate-s1",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	for _, kind := range []string{
		"ERROR RATE CORRELATION KIND",
		"span-quality-error-rate-spike",
		"Resource exhaustion under load",
	} {
		assert.NotContains(t, ociMsg, kind,
			"OCI user message should NOT include error-rate system-prompt content: %q", kind)
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostWorkloadHealthPanel
// — Cold-start parity invariant per the Workload Health dashboard
// panel slice 1 chunk 1 (v0.89.132, #772 Stream 170) §8 acceptance
// test 15. Across all four providers, the user message produced by
// buildDiscoveryUserMessage must remain byte-identical to v0.89.130
// when the scan context carries no workload-health-flagged rows.
//
// The Workload Health arc is dashboard polish only — no new
// recommendation kinds, no new substrate, no proposer code path
// changes. The four user messages keep the v0.89.130 shape because
// the new endpoint + UI panel only surface EXISTING substrate
// diagnostics at a new dashboard tile; the per-resource cold-start /
// sampling / error-rate prompts the proposer assembles stay
// identical.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostWorkloadHealthPanel(t *testing.T) {
	// AWS.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-workloadhealth-s1",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	for _, kind := range []string{
		"WORKLOAD HEALTH",
		"workload_health",
		"discovery.workload_health.requested",
	} {
		assert.NotContains(t, awsMsg, kind,
			"AWS user message should NOT include workload-health dashboard content: %q", kind)
	}

	// GCP.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-workloadhealth-s1",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	for _, kind := range []string{
		"WORKLOAD HEALTH",
		"workload_health",
		"discovery.workload_health.requested",
	} {
		assert.NotContains(t, gcpMsg, kind,
			"GCP user message should NOT include workload-health dashboard content: %q", kind)
	}

	// Azure.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-workloadhealth-s1",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	for _, kind := range []string{
		"WORKLOAD HEALTH",
		"workload_health",
		"discovery.workload_health.requested",
	} {
		assert.NotContains(t, azureMsg, kind,
			"Azure user message should NOT include workload-health dashboard content: %q", kind)
	}

	// OCI.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-workloadhealth-s1",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	for _, kind := range []string{
		"WORKLOAD HEALTH",
		"workload_health",
		"discovery.workload_health.requested",
	} {
		assert.NotContains(t, ociMsg, kind,
			"OCI user message should NOT include workload-health dashboard content: %q", kind)
	}
}

// --- Orchestration tier slice 2 chunk 2 (v0.89.136, #776 Stream 174) -

// TestDiscoveryProposer_ResmgrKindInSystemPrompt — orchestration tier
// slice 2 chunk 2 (v0.89.136, #776 Stream 174). The new
// resmgr-logging-enable kind must appear in the shared system prompt
// so the model can route findings to the right kind when the OCI
// scanner surfaces Resource Manager Stacks with has_log_axis=false.
// Slice 1 orchestration kinds (stepfunc-/workflows-/logicapps-) must
// remain present after the OCI extension — same shared-system-prompt
// invariant the prior chunk tests pin.
func TestDiscoveryProposer_ResmgrKindInSystemPrompt(t *testing.T) {
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"resmgr-logging-enable",
		"shared system prompt should teach the orchestration tier slice 2 kind")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"ORCHESTRATION TIER OCI EXTENSION (slice 2",
		"shared system prompt should include the slice 2 section header")
	// Honest-framing tokens from the design doc.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"Resource Manager (Stacks + Jobs)",
		"shared system prompt should carry the RM framing")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"deferred to slice 3",
		"shared system prompt should carry the Process Automation deferral")
	// Terraform shape cues the picker emits.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"oci_logging_log_group",
		"shared system prompt should name the Terraform resource the picker emits")
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"\"resourcemanager\"",
		"shared system prompt should name the OCI Logging source service")
	// Decline path framing.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"DECLINE PATH for resmgr-logging-enable",
		"shared system prompt should carry the slice 2 decline-path framing")
	// Compartment-level caveat.
	assert.Contains(t, proposeFromDiscoveryScanSystem,
		"COMPARTMENT-LEVEL",
		"shared system prompt should carry the compartment-level caveat")
	// Slice 1 orchestration kinds still present.
	for _, priorKind := range []string{
		"stepfunc-xray-active",
		"stepfunc-logging-enable",
		"workflows-trace-enable",
		"workflows-logging-enable",
		"logicapps-appinsights-enable",
		"logicapps-diagnostics-enable",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorKind,
			"shared system prompt should still teach the slice 1 orchestration kind %q after the OCI extension", priorKind)
	}
	// Prior-tier kinds still present.
	for _, priorKind := range []string{
		"lambda-xray-active",
		"cloudrun-otel-sidecar",
		"ocifunc-apm-enable",
		"gce-otel-label",
		"vm-otel-tag",
		"compute-otel-tag",
		"streaming-logging-enable",
		"ec2-otel-layer",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, priorKind,
			"shared system prompt should still teach the prior-tier kind %q after the OCI orchestration extension", priorKind)
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostOrchestrationSlice2
// — orchestration tier slice 2 chunk 2 (v0.89.136, #776 Stream 174)
// cold-start parity invariant: across all four providers, the
// compute-only user message produced by buildDiscoveryUserMessage
// must remain byte-identical to v0.89.133 when the scan context
// carries no resmgr rows. The new kind lives ONLY in the system
// prompt; the user message has no orchestration section, so a
// cold-start scan renders the same body the prior chunk extensions
// pinned. Pins design doc §11 acceptance test 9.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostOrchestrationSlice2(t *testing.T) {
	// AWS cold start.
	awsMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-aws-cold",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	})
	assert.Contains(t, awsMsg, "AWS discovery scan completed on a Squadron-connected account.")
	assert.NotContains(t, awsMsg, "resmgr-logging-enable")
	assert.NotContains(t, awsMsg, "ORCHESTRATION TIER OCI EXTENSION")
	assert.Contains(t, awsMsg, "group_id on every step MUST equal the account_id above")

	// GCP cold start.
	gcpMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:    "scan-gcp-cold",
		Provider:  "gcp",
		ProjectID: "my-sandbox-project",
		Regions:   []string{"us-central1"},
	})
	assert.Contains(t, gcpMsg, "GCP discovery scan completed on a Squadron-connected project.")
	assert.NotContains(t, gcpMsg, "resmgr-logging-enable")
	assert.NotContains(t, gcpMsg, "ORCHESTRATION TIER OCI EXTENSION")
	assert.Contains(t, gcpMsg, "group_id on every step MUST equal the project_id above")

	// Azure cold start.
	azureMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:         "scan-azure-cold",
		Provider:       "azure",
		TenantID:       "11111111-2222-3333-4444-555555555555",
		SubscriptionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Regions:        []string{"eastus"},
	})
	assert.Contains(t, azureMsg, "Azure discovery scan completed on a Squadron-connected subscription.")
	assert.NotContains(t, azureMsg, "resmgr-logging-enable")
	assert.NotContains(t, azureMsg, "ORCHESTRATION TIER OCI EXTENSION")
	assert.Contains(t, azureMsg, "group_id on every step MUST equal the subscription_id above")

	// OCI cold start. The slice 2 kind for OCI must NOT leak into
	// the user message — it belongs to the system prompt only.
	ociMsg := buildDiscoveryUserMessage(DiscoveryScanContext{
		ScanID:      "scan-oci-cold",
		Provider:    "oci",
		TenancyOCID: "ocid1.tenancy.oc1..aaaaaaaa",
		Regions:     []string{"us-phoenix-1"},
	})
	assert.Contains(t, ociMsg, "OCI discovery scan completed on a Squadron-connected tenancy.")
	assert.NotContains(t, ociMsg, "resmgr-logging-enable")
	assert.NotContains(t, ociMsg, "ORCHESTRATION TIER OCI EXTENSION")
	assert.Contains(t, ociMsg, "group_id on every step MUST equal the tenancy_ocid above")
}

// TestDiscoveryProposer_EventSourcesRendered — the event-source ->
// proposer bridge (v0.89.189). When EventSources is populated, the
// user message renders an "Event sources" section with the per-queue
// DLQ-axis signals the proposer keys off to emit the dead-letter
// remediation family. When empty, NO section renders (cold-start
// parity — the pre-bridge output byte-for-byte).
func TestDiscoveryProposer_EventSourcesRendered(t *testing.T) {
	ctx := DiscoveryScanContext{
		ScanID:    "scan-es-001",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		EventSources: []EventSourceCandidate{
			{
				Provider: "aws", Surface: "sqs", SourceType: "queue",
				ResourceName: "orders-no-dlq", ResourceARN: "arn:aws:sqs:us-east-1:123456789012:orders-no-dlq",
				Region: "us-east-1", HasTraceAxis: false, HasLogAxis: false,
				HasDLQ: false,
			},
			{
				Provider: "aws", Surface: "sqs", SourceType: "queue",
				ResourceName: "orders-dangling", ResourceARN: "arn:aws:sqs:us-east-1:123456789012:orders-dangling",
				Region: "us-east-1", HasTraceAxis: true, HasLogAxis: false,
				HasDLQ: false, RedrivePolicyTargetARN: "arn:aws:sqs:us-east-1:123456789012:does-not-exist",
			},
			{
				Provider: "aws", Surface: "sqs", SourceType: "queue",
				ResourceName: "orders-bad-retry", ResourceARN: "arn:aws:sqs:us-east-1:123456789012:orders-bad-retry",
				Region: "us-east-1", HasTraceAxis: true, HasLogAxis: true,
				HasDLQ: true, RedrivePolicyTargetARN: "arn:aws:sqs:us-east-1:123456789012:shared-dlq",
				DLQRetryCount: 100, DLQRetryCountInBand: false,
			},
		},
	}
	got := buildDiscoveryUserMessage(ctx)

	// The section + each queue + its DLQ signals must surface so the
	// model has what it needs for sqs-dlq-attach /
	// sqs-deadletter-queue-attach / sqs-dlq-retry-count-bound.
	assert.Contains(t, got, "Event sources (3 total):")
	assert.Contains(t, got, "orders-no-dlq")
	assert.Contains(t, got, "dlq=no-dlq")
	assert.Contains(t, got, "orders-dangling")
	assert.Contains(t, got, "redrive_target=arn:aws:sqs:us-east-1:123456789012:does-not-exist")
	assert.Contains(t, got, "orders-bad-retry")
	assert.Contains(t, got, "dlq=has-dlq")
	assert.Contains(t, got, "retry_count=100")
	assert.Contains(t, got, "retry_in_band=false")
	assert.Contains(t, got, "[aws/sqs/queue]")

	// Cold-start parity: an empty event-source list renders NO section.
	empty := DiscoveryScanContext{ScanID: "scan-es-002", AccountID: "123456789012", Regions: []string{"us-east-1"}}
	emptyMsg := buildDiscoveryUserMessage(empty)
	assert.NotContains(t, emptyMsg, "Event sources", "empty event-source list must render no section (pre-bridge parity)")
}

// TestDiscoveryProposer_EventSourcePropagationRendered pins the slice-2
// propagation axis (v0.89.194) flowing through the bridge: a broken bus
// surfaces propagation_ok=false plus its per-issue note (the reasoning
// template's "[specific note]" slot for eventbridge-rule-preserves-trace),
// while a healthy bus renders propagation_ok=true with no note line.
func TestDiscoveryProposer_EventSourcePropagationRendered(t *testing.T) {
	ctx := DiscoveryScanContext{
		ScanID:    "scan-es-prop-001",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		EventSources: []EventSourceCandidate{
			{
				Provider: "aws", Surface: "eventbridge", SourceType: "bus",
				ResourceName: "orders-bus",
				ResourceARN:  "arn:aws:events:us-east-1:123456789012:event-bus/orders-bus",
				Region:       "us-east-1", HasTraceAxis: true, HasLogAxis: true,
				HasPropagationConfig: false,
				PropagationNotes: []string{
					"rule 'order-events' has InputPath '$.detail' that strips trace header",
				},
			},
			{
				Provider: "aws", Surface: "eventbridge", SourceType: "bus",
				ResourceName: "healthy-bus",
				ResourceARN:  "arn:aws:events:us-east-1:123456789012:event-bus/healthy-bus",
				Region:       "us-east-1", HasTraceAxis: true, HasLogAxis: true,
				HasPropagationConfig: true,
			},
		},
	}
	got := buildDiscoveryUserMessage(ctx)

	// Broken bus: propagation_ok=false + the per-issue note line.
	assert.Contains(t, got, "orders-bus")
	assert.Contains(t, got, "propagation_ok=false")
	assert.Contains(t, got, "propagation_note: rule 'order-events' has InputPath '$.detail' that strips trace header")

	// Healthy bus: propagation_ok=true, and exactly one note line in the
	// whole readout (only the broken bus emits one).
	assert.Contains(t, got, "healthy-bus")
	assert.Contains(t, got, "propagation_ok=true")
	assert.Equal(t, 1, strings.Count(got, "propagation_note:"), "notes render only for the broken bus")
}

// TestProposeFromDiscoveryScanSystemPrompt_RuntimeSpecificExecWrapper pins
// the v0.89.213 fix: AWS_LAMBDA_EXEC_WRAPPER is RUNTIME-SPECIFIC
// (/opt/otel-handler for nodejs|java, /opt/otel-instrument for
// python|dotnet, none for go). The prompt previously hardcoded
// /opt/otel-handler in the locked lambda-otel-layer patch shape, which
// silently misconfigured python/dotnet Lambda instrumentation in every
// generated PR. A regression that drops the mapping reintroduces that.
func TestProposeFromDiscoveryScanSystemPrompt_RuntimeSpecificExecWrapper(t *testing.T) {
	for _, want := range []string{
		"RUNTIME-SPECIFIC",
		"/opt/otel-instrument for python",
		"/opt/otel-handler for nodejs",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt must teach the runtime-specific exec wrapper: %q", want)
	}
}

// TestProposeFromDiscoveryScanSystemPrompt_EKSCertManagerPrereq pins the
// v0.89.214 fix: the adot EKS managed add-on requires cert-manager
// installed first, or it goes CREATE_FAILED/DEGRADED and the cluster
// stays uninstrumented despite a clean Terraform apply. The prompt must
// teach the model to surface this prerequisite in every adot-add-on
// recommendation. A regression that drops it ships syntactically-valid
// PRs that silently fail to instrument EKS clusters.
func TestProposeFromDiscoveryScanSystemPrompt_EKSCertManagerPrereq(t *testing.T) {
	for _, want := range []string{
		"cert-manager",
		"PREREQUISITE",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt must teach the EKS adot cert-manager prerequisite: %q", want)
	}
}

// TestProposeFromDiscoveryScanSystemPrompt_EC2ArchMatch pins the v0.89.215
// fix: the EC2 ADOT collector build must match the instance architecture
// (arm64 for Graviton families, x86_64 otherwise). An x86_64 collector on
// a Graviton instance fails silently at service start, so a merged PR
// leaves the host uninstrumented. A regression that drops the arch
// guidance reintroduces that silent failure for arm64 fleets.
func TestProposeFromDiscoveryScanSystemPrompt_EC2ArchMatch(t *testing.T) {
	for _, want := range []string{
		"ARCHITECTURE-MATCH",
		"Graviton",
		"arm64",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt must teach EC2 collector architecture matching: %q", want)
	}
}

// TestProposeFromDiscoveryScanSystemPrompt_S3LogDeliveryPolicy pins the
// v0.89.216 fix: S3 server access logs are only delivered if the TARGET
// bucket grants logging.s3.amazonaws.com write access. Enabling logging
// on the source without it succeeds but silently delivers nothing, so the
// prompt must require the target-bucket policy. A regression that drops it
// ships PRs that look done but never produce logs.
func TestProposeFromDiscoveryScanSystemPrompt_S3LogDeliveryPolicy(t *testing.T) {
	for _, want := range []string{
		"logging.s3.amazonaws.com",
		"TARGET-BUCKET POLICY",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt must require the S3 log-delivery target-bucket policy: %q", want)
	}
}

// TestProposeFromDiscoveryScanSystemPrompt_ALBLogDeliveryPolicy pins the
// v0.89.217 fix: enabling aws_lb access_logs does a test write at apply
// time, so the target bucket must already grant the ELB log-delivery
// principal write access or the apply fails — and that principal is
// region-dependent (service principal in current regions, regional ELB
// account ID in older ones). A regression that drops this ships PRs that
// fail to apply or hardcode the wrong region's principal.
func TestProposeFromDiscoveryScanSystemPrompt_ALBLogDeliveryPolicy(t *testing.T) {
	for _, want := range []string{
		"logdelivery.elasticloadbalancing.amazonaws.com",
		"REGION-DEPENDENT",
		"Access Denied for bucket",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt must require the ALB log-delivery target-bucket policy: %q", want)
	}
}

// TestProposeFromDiscoveryScanSystemPrompt_ARNVerifyFraming pins the
// v0.89.218 honest-framing fix: the model cannot know the current ADOT
// layer version, so it must NOT present the layer ARN as authoritative —
// it must emit a VERIFY annotation and tell the operator to confirm the
// current ARN. This is the interim until a propose/scan-time ARN resolver
// lands (see docs/proposals/adot-arn-freshness-design.md). A regression
// that drops it lets stale ARNs ship as if current.
func TestProposeFromDiscoveryScanSystemPrompt_ARNVerifyFraming(t *testing.T) {
	for _, want := range []string{
		"VERIFY: ADOT layer version may be stale",
		"CANNOT know the current version",
	} {
		assert.Contains(t, proposeFromDiscoveryScanSystem, want,
			"system prompt must frame the layer ARN as verify-this, not authoritative: %q", want)
	}
}

// TestDiscoverySystemPrompt_AzureMonitorAgent_OSDerived guards #114: the
// Azure VM trace-emission guidance must OS-derive the Azure Monitor Agent
// extension (AzureMonitorWindowsAgent for Windows VMs) rather than hardcoding
// the Linux agent, which fails to provision on a Windows VM.
func TestDiscoverySystemPrompt_AzureMonitorAgent_OSDerived(t *testing.T) {
	p := DiscoverySystemPromptForTest()
	assert.Contains(t, p, "AzureMonitorWindowsAgent",
		"Azure VM agent guidance must cover the Windows agent variant")
	assert.NotContains(t, p, "Terraform: AzureMonitorLinuxAgent extension.",
		"the hardcoded Linux-only agent line must be gone")
}

// TestDiscoverySystemPrompt_SnippetParityFixes guards the prompt against the
// same silent-correctness bugs fixed in the deterministic iacpicker snippets:
// sampling needs OTEL_TRACES_SAMPLER (not just the ARG); Event Grid must not
// reference the invalid success-variant categories; Cloud Run minScale must be
// placed on the revision template.
func TestDiscoverySystemPrompt_SnippetParityFixes(t *testing.T) {
	p := DiscoverySystemPromptForTest()
	assert.Contains(t, p, "OTEL_TRACES_SAMPLER=parentbased_traceidratio",
		"sampling guidance must set the ratio sampler, not just the ARG (else no-op)")
	assert.NotContains(t, p, "DeliverySuccess",
		"Event Grid guidance must not reference the invalid DeliverySuccess category")
	assert.NotContains(t, p, "PublishSuccess,",
		"Event Grid guidance must not reference the invalid PublishSuccess category")
	assert.Contains(t, p, "template.metadata.annotations",
		"Cloud Run minScale guidance must specify the revision template placement")
}

// TestDiscoveryPrompt_ServerlessAddOnEnablement asserts the proposer prompt
// instructs recommending the detection-prerequisite paid add-ons (Lambda
// Insights / Application Insights) with the why + cost framing — the decided
// approach for the serverless cold-start/error data-source gap (#152/#153).
func TestDiscoveryPrompt_ServerlessAddOnEnablement(t *testing.T) {
	p := DiscoverySystemPromptForTest()
	for _, want := range []string{
		"lambda-insights-enable",
		"CloudWatchLambdaInsightsExecutionRolePolicy",
		"PREREQUISITE for cold-start",
		"PAID add-ons",
		"metric-filter", // the cheaper AWS alternative
		"azfunc-appinsights-enable",
		"billed on data\n  ingestion", // App Insights cost framing
	} {
		if !strings.Contains(p, want) {
			t.Errorf("discovery prompt missing expected add-on guidance: %q", want)
		}
	}
}

// TestBuildDiscoveryUserMessage_RepoContext covers the v0.90 slice-2
// repo-context block: empty RepoContext keeps the prompt unchanged
// (cold-start parity), and a non-empty RepoContext is rendered verbatim
// so the model sees the operator's real Terraform addresses.
func TestBuildDiscoveryUserMessage_RepoContext(t *testing.T) {
	// Parity: empty RepoContext must NOT introduce the block.
	base := discoveryContextForTest()
	baseMsg := buildDiscoveryUserMessage(*base)
	if strings.Contains(baseMsg, "EXISTING TERRAFORM CONTEXT") {
		t.Fatalf("empty RepoContext should not render the block; got:\n%s", baseMsg)
	}

	// With context: the block is rendered verbatim.
	withCtx := discoveryContextForTest()
	withCtx.RepoContext = "EXISTING TERRAFORM CONTEXT (the operator's repo):\nFile modules/compute/main.tf:\n  resources: aws_instance.this\n"
	msg := buildDiscoveryUserMessage(*withCtx)
	for _, want := range []string{
		"EXISTING TERRAFORM CONTEXT",
		"modules/compute/main.tf",
		"aws_instance.this",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("RepoContext block missing %q; got:\n%s", want, msg)
		}
	}
	// The block must appear before the closing JSON instruction so the
	// model reads it as scan context, not as an afterthought.
	if idxCtx, idxInstr := strings.Index(msg, "EXISTING TERRAFORM CONTEXT"), strings.Index(msg, "Return your plan as the JSON object"); idxCtx == -1 || idxInstr == -1 || idxCtx > idxInstr {
		t.Errorf("RepoContext should precede the JSON instruction (ctx=%d instr=%d)", idxCtx, idxInstr)
	}
}
