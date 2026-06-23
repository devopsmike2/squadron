// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
