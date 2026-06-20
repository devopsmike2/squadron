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
        "require_approval": true,
        "stages": [{"mode":"percent","percentage":100,"dwell_seconds":0}],
        "abort_criteria": {"max_drifted_agents":5,"max_error_logs_per_minute":50,"min_dwell_seconds_before_abort":120}
      },
      {
        "name": "AI plan step 1: instrument 1 EC2 instance with ADOT collector",
        "group_id": "123456789012",
        "inline_config_snippet": "resource \"aws_ssm_association\" \"adot_install\" {\n  name = \"AWS-RunShellScript\"\n  targets {\n    key = \"InstanceIds\"\n    values = [\"i-aaa\"]\n  }\n}\n",
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

	step1 := res.Plan.Steps[1]
	assert.Equal(t, "123456789012", step1.GroupID)
	assert.Contains(t, step1.InlineConfigSnippet, "aws_ssm_association", "step 1 Terraform should reference the SSM document")

	assert.Contains(t, res.Reasoning, "Lambdas")
	require.Len(t, res.Evidence, 1)
	assert.Equal(t, "audit_event", res.Evidence[0].Kind)
	assert.Equal(t, "scan-abc123", res.Evidence[0].ID)

	// Metering should round-trip from the fake server's usage block.
	assert.Equal(t, 123, res.TokensIn)
	assert.Equal(t, 456, res.TokensOut)
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

// TestProposeFromDiscoveryScan_RequestsProposerMaxTokens pins the same
// invariant the cost-spike test pins (#550): the proposer call must
// carry the per-call MaxTokens override (ProposerMaxTokens = 4096), not
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
	assert.Equal(t, float64(4096), gotMaxTokens,
		"ProposerMaxTokens itself should stay at 4096 unless we also extend docs/ai-features.md")
}
