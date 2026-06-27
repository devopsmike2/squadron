// Package demo provides a built-in, credential-free sample inventory so a
// first-time operator can explore Squadron without connecting a real cloud
// account. The discovery scan + recommendations handlers short-circuit on the
// reserved demo connection (see SentinelAccountID), serving the deterministic
// inventory built here instead of calling any cloud API or the LLM.
//
// Design intent (v0.89.239, first-user onboarding arc): removing the
// "you must have an AWS account configured" barrier is the single highest-
// leverage onboarding improvement. The sample inventory is deliberately
// realistic — a mix of instrumented and uninstrumented resources across
// compute, functions, and databases — so the populated Inventory tab
// demonstrates the actual gaps Squadron is built to surface (instances with
// no OTel agent, Lambdas with no OTel layer, RDS without Performance Insights).
package demo

import (
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// SentinelAccountID is the reserved, AWS-shaped account identifier for the
// built-in demo connection. It is intentionally not a valid 12-digit AWS
// account number so it can never collide with a real connection. The scan +
// recommendations handlers treat this value as "serve sample data."
const SentinelAccountID = "demo-000000000000"

// Region is the single region the demo inventory lives in.
const Region = "us-east-1"

// ScanID is the stable scan identifier for the demo inventory. Recommendation
// IDs derive from it, so they are deterministic across runs.
const ScanID = "demo-scan-0001"

// DisplayName is the operator-facing label for the demo connection.
const DisplayName = "Demo Account (sample data)"

// IsDemo reports whether accountID addresses the built-in demo connection.
func IsDemo(accountID string) bool { return accountID == SentinelAccountID }

// Connection returns the credstore record for the demo connection. The
// Credentials field is intentionally empty: the demo scan path never decrypts
// it, and the enable handler stores the record directly rather than through the
// credentialed save+validate flow.
func Connection() credstore.CloudConnection {
	return credstore.CloudConnection{
		AccountID:      SentinelAccountID,
		Provider:       credstore.ProviderAWS,
		ConnectionType: credstore.ConnectionManualImport,
		DisplayName:    DisplayName,
		Regions:        []string{Region},
		// The credstore requires non-empty credential bytes, but the demo
		// scan path never decrypts them: runAWSScan short-circuits on
		// IsDemo before any credential use. Placeholder bytes satisfy the
		// NOT NULL / len>0 store invariants without holding a real secret.
		Credentials:      []byte("demo"),
		CredentialsNonce: []byte("demo"),
	}
}

// BuildResult returns a deterministic, realistic sample inventory. The shape is
// stable across calls (timestamps excepted) so the UI and tests can rely on it.
// Instrumentation flags are mixed on purpose: roughly half of each surface has
// a gap, which is what makes the demo recommendations (slice 2) non-trivial.
func BuildResult() *scanner.Result {
	now := time.Now().UTC()
	started := now.Add(-2 * time.Second)

	return &scanner.Result{
		ScanID:          ScanID,
		ScanStartedAt:   started,
		ScanCompletedAt: now,
		Provider:        credstore.ProviderAWS,
		AccountID:       SentinelAccountID,
		Regions:         []string{Region},
		Partial:         false,

		// 5 EC2 instances — 2 instrumented, 3 with gaps (one of which is
		// Windows, exercising the OS-aware install-snippet path).
		Compute: []scanner.ComputeInstanceSnapshot{
			{
				ResourceID:   "i-0a1b2c3d4e5f60001",
				InstanceType: "m5.large",
				OSFamily:     "linux",
				Region:       Region,
				HasOTel:      true,
				Tags:         map[string]string{"app": "web", "env": "prod", "otel-collector": "node"},
			},
			{
				ResourceID:   "i-0a1b2c3d4e5f60002",
				InstanceType: "m5.large",
				OSFamily:     "linux",
				Region:       Region,
				HasOTel:      false,
				Tags:         map[string]string{"app": "web", "env": "prod"},
			},
			{
				ResourceID:   "i-0a1b2c3d4e5f60003",
				InstanceType: "c6i.xlarge",
				OSFamily:     "linux",
				Region:       Region,
				HasOTel:      false,
				Tags:         map[string]string{"app": "api", "env": "prod"},
			},
			{
				ResourceID:   "i-0a1b2c3d4e5f60004",
				InstanceType: "m5.2xlarge",
				OSFamily:     "linux",
				Region:       Region,
				HasOTel:      true,
				Tags:         map[string]string{"app": "batch", "env": "staging", "otel-collector": "node"},
			},
			{
				ResourceID:   "i-0a1b2c3d4e5f60005",
				InstanceType: "m5.large",
				OSFamily:     "windows",
				Region:       Region,
				HasOTel:      false,
				Tags:         map[string]string{"app": "iis", "env": "prod"},
			},
		},

		// 3 Lambda functions — 1 instrumented, 2 with gaps. Runtimes vary so
		// the proposer's runtime-keyed guidance has something to chew on.
		Functions: []scanner.FunctionRuntimeSnapshot{
			{
				ResourceID:   "arn:aws:lambda:us-east-1:000000000000:function:orders-processor",
				Name:         "orders-processor",
				Runtime:      "python3.12",
				Region:       Region,
				HasOTelLayer: false,
			},
			{
				ResourceID:   "arn:aws:lambda:us-east-1:000000000000:function:image-thumbnailer",
				Name:         "image-thumbnailer",
				Runtime:      "nodejs20.x",
				Region:       Region,
				HasOTelLayer: true,
			},
			{
				ResourceID:   "arn:aws:lambda:us-east-1:000000000000:function:billing-cron",
				Name:         "billing-cron",
				Runtime:      "python3.11",
				Region:       Region,
				HasOTelLayer: false,
			},
		},

		// 2 RDS instances — 1 fully observable, 1 with both levers off.
		Databases: []scanner.DatabaseInstanceSnapshot{
			{
				ResourceID:                 "arn:aws:rds:us-east-1:000000000000:db:orders-db",
				Engine:                     "postgres",
				EngineVersion:              "15.4",
				InstanceClass:              "db.r6g.large",
				Region:                     Region,
				PerformanceInsightsEnabled: true,
				EnhancedMonitoringEnabled:  false,
				Tags:                       map[string]string{"app": "orders", "env": "prod"},
			},
			{
				ResourceID:                 "arn:aws:rds:us-east-1:000000000000:db:analytics-db",
				Engine:                     "mysql",
				EngineVersion:              "8.0.35",
				InstanceClass:              "db.m5.large",
				Region:                     Region,
				PerformanceInsightsEnabled: false,
				EnhancedMonitoringEnabled:  false,
				Tags:                       map[string]string{"app": "analytics", "env": "staging"},
			},
		},
	}
}
