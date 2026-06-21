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
