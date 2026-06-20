// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithy "github.com/aws/smithy-go"
	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Scanner implements scanner.Scanner for the AWS provider. Slice-1
// scope: EC2 + Lambda inventory, single-region per call, read-only.
// Slice 2 (v0.87) adds RDS as the third service walked — same posture,
// strictly read-only (rds:DescribeDBInstances). RDS recommendations
// are emitted as plan steps the operator runs via their own
// ModifyDBInstance tooling; Squadron does NOT issue the modify call.
//
// Slice 3a (v0.88.0) adds S3 (Server Access Logging detection,
// single-axis instrumented rule) and ALB / NLB / Gateway LB (Access
// Logs detection, single-axis rule with an operator-visible
// cross-reference to the scan's S3 inventory). Same strictly-read-only
// posture: Squadron NEVER executes s3:PutBucketLogging or
// elasticloadbalancing:ModifyLoadBalancerAttributes.
//
// Slice 3b (v0.89.0) adds EKS as the 6th service category. Unlike
// the single-pass services, EKS requires a two-pass walk: ListClusters
// returns names, then per-cluster DescribeCluster + ListAddons +
// DescribeAddon (per addon) + ListNodegroups + ListFargateProfiles
// surface the COMPOSITE instrumented rule (control plane logging on
// AND observability addon present). Same strictly-read-only posture:
// Squadron NEVER executes eks:UpdateCluster or eks:CreateAddon.
//
// Construct via NewScannerForValidation when serving the connector
// wizard's validate endpoint (the connection has not yet been
// persisted), or via NewScannerFromConnection when scanning a stored
// connection (decrypts via the credstore key). Both paths route
// through a shared ClientFactory so the call shape is identical.
type Scanner struct {
	creds     credstore.AWSCredentials
	accountID string

	// factory hands out per-region service clients backed by the
	// assumed-role session. In production this is built lazily on
	// the first Scan/Validate call; tests substitute a stub
	// factory so no real AWS call is made.
	factory ClientFactory

	// factoryBuilder constructs the factory on demand. Indirected so
	// tests can inject a stub factory without touching the network.
	factoryBuilder func(ctx context.Context, creds credstore.AWSCredentials, region string) (ClientFactory, error)
}

// NewScannerForValidation builds a Scanner suitable for the connector
// wizard's pre-commit validate endpoint. The credentials are NOT
// persisted; the caller has just received them from the operator's
// browser. The accountID is the AWS account number the trust policy
// is supposed to give Squadron access to — used only as the Result's
// AccountID field on a successful scan.
func NewScannerForValidation(creds credstore.AWSCredentials, accountID string) *Scanner {
	return &Scanner{
		creds:          creds,
		accountID:      accountID,
		factoryBuilder: defaultFactoryBuilder,
	}
}

// NewScannerFromConnection builds a Scanner for a stored connection
// — the conn's Credentials are decrypted via UnmarshalAWSCredentials
// with the supplied key, then the same code path as the validate
// flow takes over. Returns an error if the connection is not AWS or
// the ciphertext fails to decrypt.
//
// This is the entry point the (future) scheduled-scan engine will use.
// Slice 1's validate endpoint uses NewScannerForValidation; the
// production-path constructor lives here so the Scanner has a single
// interface surface regardless of how it was constructed.
func NewScannerFromConnection(conn *credstore.CloudConnection, key *credstore.Key) (*Scanner, error) {
	if conn == nil {
		return nil, errors.New("aws: nil CloudConnection")
	}
	if conn.Provider != credstore.ProviderAWS {
		return nil, fmt.Errorf("aws: connection provider is %q, expected %q", conn.Provider, credstore.ProviderAWS)
	}
	creds, err := credstore.UnmarshalAWSCredentials(conn.Credentials, conn.CredentialsNonce, key)
	if err != nil {
		return nil, fmt.Errorf("aws: decrypt connection credentials: %w", err)
	}
	return &Scanner{
		creds:          *creds,
		accountID:      conn.AccountID,
		factoryBuilder: defaultFactoryBuilder,
	}, nil
}

// defaultFactoryBuilder is the production factory builder — it does a
// real sts:AssumeRole. Tests overwrite Scanner.factoryBuilder to
// return a stub factory.
func defaultFactoryBuilder(ctx context.Context, creds credstore.AWSCredentials, region string) (ClientFactory, error) {
	return newSDKClientFactory(ctx, creds, region)
}

// Provider satisfies scanner.Scanner.
func (s *Scanner) Provider() credstore.Provider {
	return credstore.ProviderAWS
}

// ensureFactory lazily builds the assume-role factory and caches it
// on the Scanner. The region argument is the home region used for
// the STS endpoint; per-service clients pick their own region when
// the scanner calls EC2(region) / Lambda(region).
func (s *Scanner) ensureFactory(ctx context.Context, region string) (ClientFactory, error) {
	if s.factory != nil {
		return s.factory, nil
	}
	if s.factoryBuilder == nil {
		s.factoryBuilder = defaultFactoryBuilder
	}
	f, err := s.factoryBuilder(ctx, s.creds, region)
	if err != nil {
		return nil, err
	}
	s.factory = f
	return f, nil
}

// Validate satisfies scanner.Scanner. Runs sts:GetCallerIdentity to
// confirm the role chain works, then runs a single small
// DescribeInstances + ListFunctions + DescribeDBInstances per region
// to confirm read permissions. Creates zero persistent records.
//
// The "single small call" rationale comes from the design doc's
// "Connector workflow design > Validation endpoint" section: this is
// a permissions probe, not an inventory walk. MaxResults / MaxItems
// stays at 5 so a misconfigured role fails fast.
func (s *Scanner) Validate(ctx context.Context, conn *credstore.CloudConnection) (*scanner.ValidationResult, error) {
	regions := s.resolveRegions(conn)
	primaryRegion := regions[0]

	factory, err := s.ensureFactory(ctx, primaryRegion)
	if err != nil {
		return &scanner.ValidationResult{
			AssumeRoleOK:  false,
			AssumeRoleErr: HumanizeError(err),
		}, nil
	}

	stsClient, err := factory.STS(ctx, primaryRegion)
	if err != nil {
		return &scanner.ValidationResult{
			AssumeRoleOK:  false,
			AssumeRoleErr: HumanizeError(err),
		}, nil
	}
	if _, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
		return &scanner.ValidationResult{
			AssumeRoleOK:  false,
			AssumeRoleErr: HumanizeError(err),
		}, nil
	}

	result := &scanner.ValidationResult{
		AssumeRoleOK: true,
	}

	// Run one preflight per (service, primaryRegion) pair. Slice 1
	// always validates against the first region; slice 3 will
	// iterate when scheduled scans land.
	if check := s.preflightEC2(ctx, factory, primaryRegion); check != nil {
		result.Preflight = append(result.Preflight, *check)
	}
	if check := s.preflightLambda(ctx, factory, primaryRegion); check != nil {
		result.Preflight = append(result.Preflight, *check)
	}
	if check := s.preflightRDS(ctx, factory, primaryRegion); check != nil {
		result.Preflight = append(result.Preflight, *check)
	}
	// Slice 3a (v0.88.0) — S3 and ALB join the preflight battery.
	// Each runs a single low-cost API call (ListBuckets is a single
	// call for the whole account; DescribeLoadBalancers with
	// PageSize=1 keeps the probe within the design doc's
	// "permissions probe, not inventory walk" contract).
	if check := s.preflightS3(ctx, factory, primaryRegion); check != nil {
		result.Preflight = append(result.Preflight, *check)
	}
	if check := s.preflightALB(ctx, factory, primaryRegion); check != nil {
		result.Preflight = append(result.Preflight, *check)
	}
	// Slice 3b (v0.89.0) — EKS joins the preflight battery via a
	// single low-cost ListClusters call with MaxResults=1.
	if check := s.preflightEKS(ctx, factory, primaryRegion); check != nil {
		result.Preflight = append(result.Preflight, *check)
	}

	return result, nil
}

// preflightEC2 runs a single DescribeInstances with MaxResults=5
// against the supplied region. Returns a PreflightCheck describing
// what happened — the caller appends it to the ValidationResult.
//
// Returns nil only when the factory itself fails to produce an EC2
// client (an unexpected internal error). All AWS-side failures become
// PreflightCheck rows with OK=false so the wizard can render them.
func (s *Scanner) preflightEC2(ctx context.Context, factory ClientFactory, region string) *scanner.PreflightCheck {
	client, err := factory.EC2(ctx, region)
	if err != nil {
		return &scanner.PreflightCheck{Service: "ec2", OK: false, Err: HumanizeError(err)}
	}
	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		MaxResults: awssdk.Int32(5),
	})
	if err != nil {
		return &scanner.PreflightCheck{Service: "ec2", OK: false, Err: HumanizeError(err)}
	}
	sample := 0
	for _, r := range out.Reservations {
		sample += len(r.Instances)
	}
	if sample > 5 {
		sample = 5
	}
	return &scanner.PreflightCheck{Service: "ec2", OK: true, SampleCount: sample}
}

// preflightLambda runs a single ListFunctions with MaxItems=5 against
// the supplied region. Mirrors preflightEC2.
func (s *Scanner) preflightLambda(ctx context.Context, factory ClientFactory, region string) *scanner.PreflightCheck {
	client, err := factory.Lambda(ctx, region)
	if err != nil {
		return &scanner.PreflightCheck{Service: "lambda", OK: false, Err: HumanizeError(err)}
	}
	out, err := client.ListFunctions(ctx, &lambda.ListFunctionsInput{
		MaxItems: awssdk.Int32(5),
	})
	if err != nil {
		return &scanner.PreflightCheck{Service: "lambda", OK: false, Err: HumanizeError(err)}
	}
	sample := len(out.Functions)
	if sample > 5 {
		sample = 5
	}
	return &scanner.PreflightCheck{Service: "lambda", OK: true, SampleCount: sample}
}

// preflightRDS runs a single DescribeDBInstances with MaxRecords=20
// (RDS's minimum allowed value — the API rejects anything below 20)
// against the supplied region. Mirrors preflightEC2. SampleCount is
// still capped at 5 in the returned PreflightCheck so the wire shape
// stays consistent with the EC2 + Lambda probes.
//
// Slice 2's only required RDS permission is rds:DescribeDBInstances.
// The proposer surfaces enablement recommendations as plan steps; the
// modify call is executed by the operator's own IaC tooling.
func (s *Scanner) preflightRDS(ctx context.Context, factory ClientFactory, region string) *scanner.PreflightCheck {
	client, err := factory.RDS(ctx, region)
	if err != nil {
		return &scanner.PreflightCheck{Service: "rds", OK: false, Err: HumanizeError(err)}
	}
	out, err := client.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		// 20 is the RDS API's minimum MaxRecords value. SDK validation
		// rejects MaxRecords < 20; the SampleCount cap below keeps the
		// wire response consistent with the EC2/Lambda probes regardless.
		MaxRecords: awssdk.Int32(20),
	})
	if err != nil {
		return &scanner.PreflightCheck{Service: "rds", OK: false, Err: HumanizeError(err)}
	}
	sample := len(out.DBInstances)
	if sample > 5 {
		sample = 5
	}
	return &scanner.PreflightCheck{Service: "rds", OK: true, SampleCount: sample}
}

// preflightS3 runs a single ListBuckets call against the supplied
// region. S3's ListBuckets is global — the region argument only
// pins the SDK client's signing region — but the preflight semantics
// are the same as the per-region probes: a single API round-trip
// that proves the trust policy granted s3:ListAllMyBuckets.
//
// Slice 3a (v0.88.0) — the rest of the S3 instrumentation read path
// (GetBucketLocation + GetBucketLogging + GetBucketTagging) only
// fires per-bucket at scan time, so the preflight intentionally
// stops at ListBuckets: the cheap signal is enough to deep-link the
// operator back to the trust-policy step on AccessDenied.
func (s *Scanner) preflightS3(ctx context.Context, factory ClientFactory, region string) *scanner.PreflightCheck {
	client, err := factory.S3(ctx, region)
	if err != nil {
		return &scanner.PreflightCheck{Service: "s3", OK: false, Err: HumanizeError(err)}
	}
	out, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return &scanner.PreflightCheck{Service: "s3", OK: false, Err: HumanizeError(err)}
	}
	sample := len(out.Buckets)
	if sample > 5 {
		sample = 5
	}
	return &scanner.PreflightCheck{Service: "s3", OK: true, SampleCount: sample}
}

// preflightALB runs a single DescribeLoadBalancers call with
// PageSize=1 against the supplied region. PageSize keeps the probe
// within the design doc's "permissions probe, not inventory walk"
// contract — a misconfigured role fails fast, and a properly
// configured one returns at most one row.
//
// Slice 3a (v0.88.0) — the per-LB attribute reads
// (DescribeLoadBalancerAttributes + DescribeTags) only fire at scan
// time. The preflight only proves
// elasticloadbalancing:DescribeLoadBalancers is granted; the other
// two permissions surface at scan time and emit "alb" to
// Result.FailedServices on the failure path.
func (s *Scanner) preflightALB(ctx context.Context, factory ClientFactory, region string) *scanner.PreflightCheck {
	client, err := factory.ELBv2(ctx, region)
	if err != nil {
		return &scanner.PreflightCheck{Service: "alb", OK: false, Err: HumanizeError(err)}
	}
	out, err := client.DescribeLoadBalancers(ctx, &elasticloadbalancingv2.DescribeLoadBalancersInput{
		PageSize: awssdk.Int32(1),
	})
	if err != nil {
		return &scanner.PreflightCheck{Service: "alb", OK: false, Err: HumanizeError(err)}
	}
	sample := len(out.LoadBalancers)
	if sample > 5 {
		sample = 5
	}
	return &scanner.PreflightCheck{Service: "alb", OK: true, SampleCount: sample}
}

// preflightEKS runs a single ListClusters call with MaxResults=1
// against the supplied region. Mirrors preflightALB — a misconfigured
// role fails fast, and a properly configured one returns at most one
// row. The per-cluster fan-out (DescribeCluster + ListAddons +
// ListNodegroups + ListFargateProfiles) only fires at scan time, so
// the preflight intentionally stops at ListClusters: the cheap signal
// is enough to deep-link the operator back to the trust-policy step
// on AccessDenied.
//
// Slice 3b (v0.89.0) — the rest of the EKS instrumentation read path
// (DescribeCluster, ListAddons, DescribeAddon, ListNodegroups,
// ListFargateProfiles) only fires at scan time, so the preflight
// proves eks:ListClusters is granted; the other four permissions
// surface at scan time and emit "eks" to Result.FailedServices on
// the failure path.
func (s *Scanner) preflightEKS(ctx context.Context, factory ClientFactory, region string) *scanner.PreflightCheck {
	client, err := factory.EKS(ctx, region)
	if err != nil {
		return &scanner.PreflightCheck{Service: "eks", OK: false, Err: HumanizeError(err)}
	}
	out, err := client.ListClusters(ctx, &eks.ListClustersInput{
		MaxResults: awssdk.Int32(1),
	})
	if err != nil {
		return &scanner.PreflightCheck{Service: "eks", OK: false, Err: HumanizeError(err)}
	}
	sample := len(out.Clusters)
	if sample > 5 {
		sample = 5
	}
	return &scanner.PreflightCheck{Service: "eks", OK: true, SampleCount: sample}
}

// Scan satisfies scanner.Scanner. Walks each region in turn,
// paginating DescribeInstances, ListFunctions, and DescribeDBInstances
// with exponential backoff on throttling. On unrecoverable errors
// (anything not throttling), the scan returns Partial=true with the
// failing region's error humanized into PartialReason.
//
// regions argument overrides the connection's Regions list — slice 1
// passes a single-entry slice; slice 3 will iterate. Empty slice
// falls back to the connection's Regions field.
func (s *Scanner) Scan(ctx context.Context, conn *credstore.CloudConnection, regions []string) (*scanner.Result, error) {
	if len(regions) == 0 {
		regions = s.resolveRegions(conn)
	}
	scanID := uuid.NewString()
	result := &scanner.Result{
		ScanID:        scanID,
		ScanStartedAt: time.Now().UTC(),
		Provider:      credstore.ProviderAWS,
		AccountID:     s.accountID,
		Regions:       append([]string(nil), regions...),
	}
	defer func() {
		result.ScanCompletedAt = time.Now().UTC()
	}()

	// v0.88.3: closes #586. Prior to this, each failure emission site
	// OVERWROTE result.PartialReason — so when two services failed in
	// the same scan, only the last one's diagnostic survived. The
	// FailedServices slice was always correctly accumulating (it's an
	// append), so audit consumers had the structured list right, but
	// the human-readable string lost the earlier failure. Now both
	// fields accumulate. recordPartialFailure is the single emission
	// site; new services append to the existing chain instead of
	// clobbering. Operationally visible during Track A live-deploy
	// (#586 — when both rds:DescribeDBInstances and the (subsequent)
	// alb walk failed, only the alb message showed in PartialReason).
	factory, err := s.ensureFactory(ctx, regions[0])
	if err != nil {
		// Sentinel "assume_role" distinguishes credentials-layer
		// failures from per-service walk failures for audit consumers
		// pattern-matching against FailedServices.
		recordPartialFailure(result, "assume_role", fmt.Sprintf("assume-role failed: %s", err.Error()))
		return result, nil
	}

	for _, region := range regions {
		if err := s.scanRegionEC2(ctx, factory, region, result); err != nil {
			recordPartialFailure(result, "ec2", fmt.Sprintf("ec2 scan failed in %s: %s", region, err.Error()))
		}
		if err := s.scanRegionLambda(ctx, factory, region, result); err != nil {
			recordPartialFailure(result, "lambda", fmt.Sprintf("lambda scan failed in %s: %s", region, err.Error()))
		}
		if err := s.scanRegionRDS(ctx, factory, region, result); err != nil {
			recordPartialFailure(result, "rds", fmt.Sprintf("rds scan failed in %s: %s", region, err.Error()))
		}
		// Slice 3a (v0.88.0) — S3 + ALB join the per-region walk.
		if err := s.scanRegionS3(ctx, factory, region, result); err != nil {
			recordPartialFailure(result, "s3", fmt.Sprintf("s3 scan failed in %s: %s", region, err.Error()))
		}
		if err := s.scanRegionALB(ctx, factory, region, result); err != nil {
			recordPartialFailure(result, "alb", fmt.Sprintf("alb scan failed in %s: %s", region, err.Error()))
		}
		// Slice 3b (v0.89.0) — EKS joins the per-region walk.
		if err := s.scanRegionEKS(ctx, factory, region, result); err != nil {
			recordPartialFailure(result, "eks", fmt.Sprintf("eks scan failed in %s: %s", region, err.Error()))
		}
	}

	for _, c := range result.Compute {
		if c.HasOTel {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}
	for _, f := range result.Functions {
		if f.HasOTelLayer {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}
	// RDS counts as instrumented when BOTH Performance Insights AND
	// Enhanced Monitoring are enabled — the two-part rule documented
	// on scanner.DatabaseInstanceSnapshot. The proposer prompt teaches
	// the same rule, so the operator-visible Inventory tab and the
	// AI's reasoning use the same denominator.
	for _, d := range result.Databases {
		if d.PerformanceInsightsEnabled && d.EnhancedMonitoringEnabled {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}
	// Slice 3a (v0.88.0) — single-axis instrumented rules.
	//   - ObjectStores: ServerAccessLoggingEnabled (RequestMetrics
	//     is informational only — see scanner.ObjectStoreSnapshot).
	//   - LoadBalancers: AccessLogsEnabled (AccessLogsS3Bucket is
	//     the operator-chosen target, informational on the row).
	for _, o := range result.ObjectStores {
		if o.ServerAccessLoggingEnabled {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}
	for _, l := range result.LoadBalancers {
		if l.AccessLogsEnabled {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}
	// Slice 3b (v0.89.0) — EKS clusters count as instrumented only
	// when BOTH the control plane logging axis (api + audit present)
	// AND the observability addon axis (an ACTIVE adot or
	// amazon-cloudwatch-observability addon) hold. The composite rule
	// is documented on scanner.ClusterSnapshot; clusterIsInstrumented
	// is the shared predicate the proposer prompt and the Inventory
	// tally use.
	for _, c := range result.Clusters {
		if clusterIsInstrumented(c) {
			result.InstrumentedCount++
		} else {
			result.UninstrumentedCount++
		}
	}

	return result, nil
}

// clusterIsInstrumented implements the slice 3b composite
// instrumented rule. The function is exported-shaped (capitalized
// in code) only conceptually — it lives in the aws package and is
// package-private; the proposer-side check uses a parallel
// implementation against ClusterCandidate (which carries the same
// two axes flattened from the snapshot).
func clusterIsInstrumented(c scanner.ClusterSnapshot) bool {
	// Axis 1: control plane logging includes BOTH "api" AND "audit".
	hasAPI, hasAudit := false, false
	for _, t := range c.ControlPlaneLogging {
		switch strings.ToLower(t) {
		case "api":
			hasAPI = true
		case "audit":
			hasAudit = true
		}
	}
	if !hasAPI || !hasAudit {
		return false
	}
	// Axis 2: at least one ACTIVE observability addon. Names checked
	// case-insensitively; the EKS API canonicalizes to lowercase but
	// defense-in-depth is cheap here.
	for _, a := range c.Addons {
		if !strings.EqualFold(a.Status, "ACTIVE") {
			continue
		}
		name := strings.ToLower(a.Name)
		if name == "adot" || name == "amazon-cloudwatch-observability" {
			return true
		}
	}
	return false
}

// resolveRegions picks the regions slice the caller's request implied.
// Empty connection.Regions falls back to a single default (us-east-1)
// — slice 1's UI always populates the field, but the default keeps
// the validate endpoint usable from a curl client that didn't.
func (s *Scanner) resolveRegions(conn *credstore.CloudConnection) []string {
	if conn != nil && len(conn.Regions) > 0 {
		out := make([]string, len(conn.Regions))
		copy(out, conn.Regions)
		return out
	}
	return []string{"us-east-1"}
}

// scanRegionEC2 paginates DescribeInstances and appends mapped
// snapshots to result.Compute. Uses a simple retry-with-backoff
// wrapper for transient throttling — see retryWithBackoff below.
func (s *Scanner) scanRegionEC2(ctx context.Context, factory ClientFactory, region string, result *scanner.Result) error {
	client, err := factory.EC2(ctx, region)
	if err != nil {
		return err
	}
	var nextToken *string
	for {
		input := &ec2.DescribeInstancesInput{}
		if nextToken != nil {
			input.NextToken = nextToken
		}
		var out *ec2.DescribeInstancesOutput
		err := retryWithBackoff(ctx, func() error {
			var callErr error
			out, callErr = client.DescribeInstances(ctx, input)
			return callErr
		})
		if err != nil {
			return err
		}
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				result.Compute = append(result.Compute, mapEC2Instance(inst, region))
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		nextToken = out.NextToken
	}
	return nil
}

// scanRegionLambda paginates ListFunctions and appends mapped
// snapshots to result.Functions. Each FunctionConfiguration arrives
// with its Layers already populated by ListFunctions, so no
// per-function GetFunctionConfiguration call is needed at this scope.
func (s *Scanner) scanRegionLambda(ctx context.Context, factory ClientFactory, region string, result *scanner.Result) error {
	client, err := factory.Lambda(ctx, region)
	if err != nil {
		return err
	}
	var marker *string
	for {
		input := &lambda.ListFunctionsInput{}
		if marker != nil {
			input.Marker = marker
		}
		var out *lambda.ListFunctionsOutput
		err := retryWithBackoff(ctx, func() error {
			var callErr error
			out, callErr = client.ListFunctions(ctx, input)
			return callErr
		})
		if err != nil {
			return err
		}
		for _, fn := range out.Functions {
			result.Functions = append(result.Functions, mapLambdaFunction(fn, region))
		}
		if out.NextMarker == nil || *out.NextMarker == "" {
			break
		}
		marker = out.NextMarker
	}
	return nil
}

// scanRegionRDS paginates DescribeDBInstances and appends mapped
// snapshots to result.Databases. Each DBInstance arrives with its
// PerformanceInsightsEnabled flag and Enhanced Monitoring interval
// already populated — the proposer's two RDS levers — so no
// per-instance follow-up call is needed at this scope.
//
// IAM permission required: rds:DescribeDBInstances. The trust policy
// snippet in docs/universal-discovery-design.md's "Permissions policy"
// section is updated to add this one action when slice 2 ships.
// Squadron does NOT execute rds:ModifyDBInstance — discovery is
// strictly read-only; the operator runs the modify call through their
// own IaC tooling.
func (s *Scanner) scanRegionRDS(ctx context.Context, factory ClientFactory, region string, result *scanner.Result) error {
	client, err := factory.RDS(ctx, region)
	if err != nil {
		return err
	}
	var marker *string
	for {
		input := &rds.DescribeDBInstancesInput{}
		if marker != nil {
			input.Marker = marker
		}
		var out *rds.DescribeDBInstancesOutput
		err := retryWithBackoff(ctx, func() error {
			var callErr error
			out, callErr = client.DescribeDBInstances(ctx, input)
			return callErr
		})
		if err != nil {
			return err
		}
		for _, db := range out.DBInstances {
			result.Databases = append(result.Databases, mapRDSInstance(db, region))
		}
		if out.Marker == nil || *out.Marker == "" {
			break
		}
		marker = out.Marker
	}
	return nil
}

// scanRegionS3 walks the account's S3 buckets and appends mapped
// snapshots to result.ObjectStores. The walk has three phases:
//
//  1. ListBuckets returns every bucket in the account. S3 is global
//     for listing, so the call happens once per scan rather than
//     per region — but the scanner still invokes scanRegionS3 once
//     per region in the connection's region list. The first region
//     does the real work; subsequent regions short-circuit (the
//     scanner tracks which buckets it has already mapped).
//
//  2. For each bucket, GetBucketLocation returns the bucket's home
//     region. The scanner filters to the connection's region list
//     before doing the more expensive per-bucket reads.
//
//  3. GetBucketLogging + GetBucketTagging produce the per-bucket
//     observability state + tags that fill the
//     ObjectStoreSnapshot.
//
// IAM permissions required: s3:ListAllMyBuckets +
// s3:GetBucketLocation + s3:GetBucketLogging + s3:GetBucketTagging.
// The trust policy snippet in docs/universal-discovery-design.md's
// "Permissions policy" section adds all four when slice 3a ships.
// Squadron does NOT execute s3:PutBucketLogging — discovery is
// strictly read-only.
//
// On the first invocation of this function within a Scan call,
// scanRegionS3 lists every bucket. On subsequent invocations
// (multi-region connections), it returns nil immediately because
// the buckets have already been collected. The
// alreadyWalkedObjectStores flag on the result struct's region
// list (encoded via len(result.ObjectStores) > 0 — buckets are
// only added on first walk) keeps the wire shape consistent with
// the EC2 / Lambda / RDS per-region pattern.
func (s *Scanner) scanRegionS3(ctx context.Context, factory ClientFactory, region string, result *scanner.Result) error {
	// Multi-region short-circuit: S3 ListBuckets is global, so only
	// the first region's invocation does the work. We use the
	// region-list head as the "first invocation" sentinel — the
	// scan's region loop calls scanRegionS3 in iteration order
	// matching result.Regions, so the head is the first to fire.
	if len(result.Regions) > 1 && region != result.Regions[0] {
		return nil
	}
	client, err := factory.S3(ctx, region)
	if err != nil {
		return err
	}
	var out *s3.ListBucketsOutput
	err = retryWithBackoff(ctx, func() error {
		var callErr error
		out, callErr = client.ListBuckets(ctx, &s3.ListBucketsInput{})
		return callErr
	})
	if err != nil {
		return err
	}
	// Build a quick membership set for the connection's region
	// filter. An empty filter (no regions configured) lets every
	// bucket through — defense in depth; the connection layer
	// already guarantees a non-empty slice.
	allowed := map[string]bool{}
	for _, r := range result.Regions {
		allowed[r] = true
	}
	for _, b := range out.Buckets {
		if b.Name == nil {
			continue
		}
		bucketName := *b.Name
		// Resolve the bucket's home region. GetBucketLocation
		// returns an empty LocationConstraint for us-east-1
		// (legacy AWS quirk); the mapper normalizes.
		loc, locErr := client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{
			Bucket: awssdk.String(bucketName),
		})
		bucketRegion := "us-east-1"
		if locErr == nil && loc != nil && loc.LocationConstraint != "" {
			bucketRegion = string(loc.LocationConstraint)
		}
		// Filter to the connection's region list. A bucket
		// outside the configured regions stays out of the scan
		// result entirely — operators who want broader visibility
		// add more regions to the connection.
		if len(allowed) > 0 && !allowed[bucketRegion] {
			continue
		}
		snap, err := s.collectBucketDetails(ctx, client, bucketName, bucketRegion)
		if err != nil {
			// Per-bucket GetBucketLogging / GetBucketTagging
			// failures inside the walk fail the whole S3 scan —
			// the proposer needs a complete view of bucket
			// instrumentation state to reason correctly. Return
			// the error so the caller emits the s3 FailedServices
			// entry.
			return err
		}
		result.ObjectStores = append(result.ObjectStores, snap)
	}
	return nil
}

// collectBucketDetails runs GetBucketLogging + GetBucketTagging
// against a single bucket and returns the mapped snapshot. Extracted
// for readability; the per-bucket fan-out is the most complex part
// of the S3 walk.
//
// GetBucketTagging returns NoSuchTagSet on untagged buckets; that's
// a successful read with no tags, not an error. The mapper handles
// it by leaving snap.Tags nil.
func (s *Scanner) collectBucketDetails(ctx context.Context, client S3Client, bucketName, bucketRegion string) (scanner.ObjectStoreSnapshot, error) {
	snap := scanner.ObjectStoreSnapshot{
		ResourceID: bucketName,
		Region:     bucketRegion,
	}
	logging, err := client.GetBucketLogging(ctx, &s3.GetBucketLoggingInput{
		Bucket: awssdk.String(bucketName),
	})
	if err != nil {
		return snap, err
	}
	if logging != nil && logging.LoggingEnabled != nil &&
		logging.LoggingEnabled.TargetBucket != nil &&
		*logging.LoggingEnabled.TargetBucket != "" {
		snap.ServerAccessLoggingEnabled = true
	}
	tagging, err := client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{
		Bucket: awssdk.String(bucketName),
	})
	if err != nil {
		// NoSuchTagSet is the documented "bucket has no tags"
		// signal; treat it as a successful read with empty tags.
		// Any other error fails the walk.
		if !isNoSuchTagSet(err) {
			return snap, err
		}
	}
	if tagging != nil && len(tagging.TagSet) > 0 {
		snap.Tags = make(map[string]string, len(tagging.TagSet))
		for _, t := range tagging.TagSet {
			if t.Key == nil {
				continue
			}
			key := *t.Key
			val := ""
			if t.Value != nil {
				val = *t.Value
			}
			snap.Tags[key] = val
		}
	}
	return snap, nil
}

// isNoSuchTagSet pattern-matches the smithy.APIError shape against
// the S3 NoSuchTagSet code. Used by collectBucketDetails to
// distinguish "bucket has no tags" (successful read, empty result)
// from a genuine permissions or transport failure.
func isNoSuchTagSet(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode() == "NoSuchTagSet"
}

// scanRegionALB paginates DescribeLoadBalancers, then for each load
// balancer fans out to DescribeLoadBalancerAttributes +
// DescribeTags. Mapped snapshots land in result.LoadBalancers.
//
// IAM permissions required: elasticloadbalancing:DescribeLoadBalancers
// + elasticloadbalancing:DescribeLoadBalancerAttributes +
// elasticloadbalancing:DescribeTags. The permissions policy snippet
// in docs/universal-discovery-design.md adds all three when slice 3a
// ships. Squadron does NOT execute
// elasticloadbalancing:ModifyLoadBalancerAttributes — discovery is
// strictly read-only.
//
// DescribeTags accepts up to 20 ARNs per call; the scanner batches
// the per-LB tag reads accordingly so a 50-ALB account spends 3
// DescribeTags calls instead of 50.
func (s *Scanner) scanRegionALB(ctx context.Context, factory ClientFactory, region string, result *scanner.Result) error {
	client, err := factory.ELBv2(ctx, region)
	if err != nil {
		return err
	}
	var marker *string
	for {
		input := &elasticloadbalancingv2.DescribeLoadBalancersInput{}
		if marker != nil {
			input.Marker = marker
		}
		var out *elasticloadbalancingv2.DescribeLoadBalancersOutput
		err := retryWithBackoff(ctx, func() error {
			var callErr error
			out, callErr = client.DescribeLoadBalancers(ctx, input)
			return callErr
		})
		if err != nil {
			return err
		}
		// Per-LB attribute reads. DescribeLoadBalancerAttributes
		// is per-LB; DescribeTags is batched (up to 20 ARNs).
		arns := make([]string, 0, len(out.LoadBalancers))
		snapsByARN := make(map[string]*scanner.LoadBalancerSnapshot, len(out.LoadBalancers))
		for i := range out.LoadBalancers {
			lb := out.LoadBalancers[i]
			snap := mapALBBase(lb, region)
			// Fetch access-logs attribute per-LB. A failure here
			// (e.g. AccessDenied on DescribeLoadBalancerAttributes
			// when the policy granted only DescribeLoadBalancers)
			// fails the whole ALB walk so the FailedServices
			// emission is consistent.
			if lb.LoadBalancerArn != nil {
				attrs, attrErr := client.DescribeLoadBalancerAttributes(ctx,
					&elasticloadbalancingv2.DescribeLoadBalancerAttributesInput{
						LoadBalancerArn: lb.LoadBalancerArn,
					})
				if attrErr != nil {
					return attrErr
				}
				applyALBAttributes(&snap, attrs)
				arns = append(arns, *lb.LoadBalancerArn)
				snapsByARN[*lb.LoadBalancerArn] = &snap
			}
			result.LoadBalancers = append(result.LoadBalancers, snap)
		}
		// Batch tag fetch — up to 20 ARNs per DescribeTags call.
		// The ELB v2 API returns one TagDescription per ARN.
		for i := 0; i < len(arns); i += 20 {
			end := i + 20
			if end > len(arns) {
				end = len(arns)
			}
			tagsOut, tagsErr := client.DescribeTags(ctx,
				&elasticloadbalancingv2.DescribeTagsInput{
					ResourceArns: arns[i:end],
				})
			if tagsErr != nil {
				return tagsErr
			}
			for _, desc := range tagsOut.TagDescriptions {
				if desc.ResourceArn == nil {
					continue
				}
				snap := snapsByARN[*desc.ResourceArn]
				if snap == nil {
					continue
				}
				if len(desc.Tags) > 0 {
					snap.Tags = make(map[string]string, len(desc.Tags))
					for _, t := range desc.Tags {
						if t.Key == nil {
							continue
						}
						key := *t.Key
						val := ""
						if t.Value != nil {
							val = *t.Value
						}
						snap.Tags[key] = val
					}
				}
			}
		}
		// Tag-fetch updated the snaps stored via pointer in
		// snapsByARN. The slice entries we appended above are
		// value copies — push the tags back so the final
		// result.LoadBalancers slice carries them.
		for i := range result.LoadBalancers {
			if result.LoadBalancers[i].ResourceID == "" {
				continue
			}
			updated := snapsByARN[result.LoadBalancers[i].ResourceID]
			if updated != nil && updated.Tags != nil {
				result.LoadBalancers[i].Tags = updated.Tags
			}
		}
		if out.NextMarker == nil || *out.NextMarker == "" {
			break
		}
		marker = out.NextMarker
	}
	return nil
}

// scanRegionEKS walks the region's EKS clusters in two passes and
// appends mapped snapshots to result.Clusters. Unlike the single-pass
// services (EC2 / Lambda / RDS / S3 / ALB) the EKS API surface
// requires a per-cluster fan-out: ListClusters returns only the
// cluster name list, and the observability state (control plane
// logging config + add-ons) lives behind DescribeCluster + ListAddons
// + DescribeAddon. Nodegroup + Fargate-profile counts are
// informational and come from ListNodegroups + ListFargateProfiles.
//
// IAM permissions required: eks:ListClusters + eks:DescribeCluster +
// eks:ListAddons + eks:DescribeAddon + eks:ListNodegroups. The
// permissions policy snippet in docs/universal-discovery-design.md
// adds all five when slice 3b ships. ListFargateProfiles reuses
// eks:ListClusters scope — no separate permission needed (the AWS
// docs list it under the cluster's IAM action set).
//
// Squadron does NOT execute eks:UpdateCluster or eks:CreateAddon —
// discovery is strictly read-only.
//
// Per-cluster fan-out runs SEQUENTIALLY in v0.89.0. Real operators
// at 50+ clusters per region will likely hit a wall here; deferring
// concurrency to a follow-up slice keeps this ship small. The
// retryWithBackoff helper protects each per-cluster call against
// throttling.
//
// On any per-cluster API failure the function returns the error so
// the caller records "eks" on FailedServices via
// recordPartialFailure. Partial per-cluster failures (one cluster's
// DescribeCluster fails, others succeed) currently fail the whole
// EKS walk in the region — same posture as scanRegionS3's
// per-bucket failure handling.
func (s *Scanner) scanRegionEKS(ctx context.Context, factory ClientFactory, region string, result *scanner.Result) error {
	client, err := factory.EKS(ctx, region)
	if err != nil {
		return err
	}
	// Pass 1: ListClusters, paginated via NextToken.
	var names []string
	var nextToken *string
	for {
		in := &eks.ListClustersInput{}
		if nextToken != nil {
			in.NextToken = nextToken
		}
		var out *eks.ListClustersOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			out, e = client.ListClusters(ctx, in)
			return e
		})
		if callErr != nil {
			return callErr
		}
		names = append(names, out.Clusters...)
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		nextToken = out.NextToken
	}
	// Pass 2: per-cluster expand.
	for _, name := range names {
		snap, err := s.expandEKSCluster(ctx, client, name, region)
		if err != nil {
			return err
		}
		result.Clusters = append(result.Clusters, snap)
	}
	return nil
}

// expandEKSCluster runs the per-cluster fan-out: DescribeCluster +
// ListAddons (+ DescribeAddon per add-on) + ListNodegroups +
// ListFargateProfiles. Returns the populated ClusterSnapshot or the
// first error encountered. Extracted for readability; the per-cluster
// fan-out is the most complex part of the EKS walk.
func (s *Scanner) expandEKSCluster(ctx context.Context, client EKSClient, name, region string) (scanner.ClusterSnapshot, error) {
	snap := scanner.ClusterSnapshot{
		Name:   name,
		Region: region,
	}
	// DescribeCluster — control plane logging + version + status +
	// ARN + tags.
	var descOut *eks.DescribeClusterOutput
	err := retryWithBackoff(ctx, func() error {
		var e error
		descOut, e = client.DescribeCluster(ctx, &eks.DescribeClusterInput{
			Name: awssdk.String(name),
		})
		return e
	})
	if err != nil {
		return snap, err
	}
	if descOut != nil && descOut.Cluster != nil {
		applyEKSClusterDescription(&snap, descOut.Cluster)
	}
	// ListAddons — paginated; then per-addon DescribeAddon.
	var addonNames []string
	var addonToken *string
	for {
		in := &eks.ListAddonsInput{ClusterName: awssdk.String(name)}
		if addonToken != nil {
			in.NextToken = addonToken
		}
		var out *eks.ListAddonsOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			out, e = client.ListAddons(ctx, in)
			return e
		})
		if callErr != nil {
			return snap, callErr
		}
		addonNames = append(addonNames, out.Addons...)
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		addonToken = out.NextToken
	}
	for _, an := range addonNames {
		var out *eks.DescribeAddonOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			out, e = client.DescribeAddon(ctx, &eks.DescribeAddonInput{
				ClusterName: awssdk.String(name),
				AddonName:   awssdk.String(an),
			})
			return e
		})
		if callErr != nil {
			return snap, callErr
		}
		if out == nil || out.Addon == nil {
			continue
		}
		snap.Addons = append(snap.Addons, mapEKSAddon(*out.Addon))
	}
	// ListNodegroups — count only; the proposer reasons at the
	// cluster level, not per-nodegroup.
	var ngToken *string
	for {
		in := &eks.ListNodegroupsInput{ClusterName: awssdk.String(name)}
		if ngToken != nil {
			in.NextToken = ngToken
		}
		var out *eks.ListNodegroupsOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			out, e = client.ListNodegroups(ctx, in)
			return e
		})
		if callErr != nil {
			return snap, callErr
		}
		snap.NodegroupCount += len(out.Nodegroups)
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		ngToken = out.NextToken
	}
	// ListFargateProfiles — count only. Same posture as nodegroups.
	var fpToken *string
	for {
		in := &eks.ListFargateProfilesInput{ClusterName: awssdk.String(name)}
		if fpToken != nil {
			in.NextToken = fpToken
		}
		var out *eks.ListFargateProfilesOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			out, e = client.ListFargateProfiles(ctx, in)
			return e
		})
		if callErr != nil {
			return snap, callErr
		}
		snap.FargateProfileCount += len(out.FargateProfileNames)
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		fpToken = out.NextToken
	}
	return snap, nil
}

// applyEKSClusterDescription pulls the control plane logging config
// + version + status + ARN + tags off an SDK Cluster value and
// populates the snapshot. Extracted so the snapshot construction
// stays readable.
//
// Control plane logging: EKS exposes the config as a list of
// LogSetup entries; each entry is { Types: [...], Enabled: bool }.
// Squadron's snapshot only carries the ENABLED log types
// (disabled-with-types entries are filtered out at the mapper).
func applyEKSClusterDescription(snap *scanner.ClusterSnapshot, c *ekstypes.Cluster) {
	if c.Arn != nil {
		snap.ResourceID = *c.Arn
	}
	if c.Version != nil {
		snap.KubernetesVersion = *c.Version
	}
	snap.Status = string(c.Status)
	if c.Logging != nil {
		for _, setup := range c.Logging.ClusterLogging {
			if setup.Enabled == nil || !*setup.Enabled {
				continue
			}
			for _, t := range setup.Types {
				snap.ControlPlaneLogging = append(snap.ControlPlaneLogging, string(t))
			}
		}
	}
	if len(c.Tags) > 0 {
		snap.Tags = make(map[string]string, len(c.Tags))
		for k, v := range c.Tags {
			snap.Tags[k] = v
		}
	}
}

// mapEKSAddon turns an SDK Addon into the snapshot's ClusterAddon
// shape. Status enums (ACTIVE / DEGRADED / etc.) come straight off
// the SDK string-typed value.
func mapEKSAddon(a eksAddon) scanner.ClusterAddon {
	out := scanner.ClusterAddon{}
	if a.AddonName != nil {
		out.Name = *a.AddonName
	}
	if a.AddonVersion != nil {
		out.Version = *a.AddonVersion
	}
	out.Status = string(a.Status)
	return out
}

// mapALBBase turns an SDK LoadBalancer into the base snapshot — name,
// type, scheme, region, ARN — without the per-LB attributes that
// require a follow-up call. applyALBAttributes fills in the
// AccessLogs* fields once DescribeLoadBalancerAttributes returns.
func mapALBBase(lb elbv2LoadBalancer, region string) scanner.LoadBalancerSnapshot {
	snap := scanner.LoadBalancerSnapshot{
		Region: region,
	}
	if lb.LoadBalancerArn != nil {
		snap.ResourceID = *lb.LoadBalancerArn
	}
	if lb.LoadBalancerName != nil {
		snap.Name = *lb.LoadBalancerName
	}
	// Type and Scheme are enum-typed in the SDK but render as
	// plain strings on the snapshot — the proposer reasons about
	// the lowercase string (application / network / gateway,
	// internet-facing / internal).
	snap.Type = string(lb.Type)
	snap.Scheme = string(lb.Scheme)
	return snap
}

// applyALBAttributes maps the access_logs.s3.enabled +
// access_logs.s3.bucket attributes from
// DescribeLoadBalancerAttributes onto the snapshot. The attributes
// arrive as a flat key/value list with stringly-typed values; the
// "true" / "false" string is the boolean encoding the API uses.
func applyALBAttributes(snap *scanner.LoadBalancerSnapshot, out *elasticloadbalancingv2.DescribeLoadBalancerAttributesOutput) {
	if out == nil {
		return
	}
	for _, attr := range out.Attributes {
		if attr.Key == nil || attr.Value == nil {
			continue
		}
		switch *attr.Key {
		case "access_logs.s3.enabled":
			if *attr.Value == "true" {
				snap.AccessLogsEnabled = true
			}
		case "access_logs.s3.bucket":
			snap.AccessLogsS3Bucket = *attr.Value
		}
	}
}

// Compile-time references to keep the s3types + elbv2types imports
// live even if a future refactor inlines the mappers. The mappers
// use the SDK types directly (s3types.Bucket / elbv2types.LoadBalancer
// flow through the type aliases in types.go); the references below
// are belt-and-braces.
var (
	_ = s3types.Bucket{}
	_ = elbv2types.LoadBalancer{}
)

// mapRDSInstance turns an SDK DBInstance into the category-typed
// snapshot. The two observability lever flags come straight off the
// DescribeDBInstances response:
//   - PerformanceInsightsEnabled is the boolean the SDK exposes
//     verbatim (PI is a per-instance toggle).
//   - Enhanced Monitoring is signaled by a non-zero MonitoringInterval
//     (the SDK reports the interval in seconds; 0 means disabled, any
//     positive value — typically 1, 5, 10, 15, 30, or 60 — means
//     enabled).
//
// Tags come from the TagList field RDS returns alongside the instance
// description; the flatten mirrors the EC2 tag normalization.
func mapRDSInstance(db rdsDBInstance, region string) scanner.DatabaseInstanceSnapshot {
	snap := scanner.DatabaseInstanceSnapshot{
		Region: region,
	}
	if db.DBInstanceArn != nil {
		snap.ResourceID = *db.DBInstanceArn
	}
	if db.Engine != nil {
		snap.Engine = *db.Engine
	}
	if db.EngineVersion != nil {
		snap.EngineVersion = *db.EngineVersion
	}
	if db.DBInstanceClass != nil {
		snap.InstanceClass = *db.DBInstanceClass
	}
	if db.PerformanceInsightsEnabled != nil {
		snap.PerformanceInsightsEnabled = *db.PerformanceInsightsEnabled
	}
	// Enhanced Monitoring: any non-zero MonitoringInterval means the
	// per-second OS-metrics stream is being delivered to CloudWatch.
	// The SDK uses *int32; nil is the "field absent" shape RDS uses
	// for instances created before EM existed.
	if db.MonitoringInterval != nil && *db.MonitoringInterval > 0 {
		snap.EnhancedMonitoringEnabled = true
	}
	if len(db.TagList) > 0 {
		snap.Tags = make(map[string]string, len(db.TagList))
		for _, t := range db.TagList {
			if t.Key == nil {
				continue
			}
			key := *t.Key
			val := ""
			if t.Value != nil {
				val = *t.Value
			}
			snap.Tags[key] = val
		}
	}
	return snap
}

// mapEC2Instance turns an SDK Instance into the category-typed
// snapshot the proposer reasons about. The OTel detection is the
// slice-1 tag heuristic — any tag key starting with otel
// (case-insensitive) flips HasOTel to true.
func mapEC2Instance(inst ec2Instance, region string) scanner.ComputeInstanceSnapshot {
	snap := scanner.ComputeInstanceSnapshot{
		Region:   region,
		OSFamily: detectOSFamily(inst),
	}
	if inst.InstanceId != nil {
		snap.ResourceID = *inst.InstanceId
	}
	if inst.InstanceType != "" {
		snap.InstanceType = string(inst.InstanceType)
	}
	if len(inst.Tags) > 0 {
		snap.Tags = make(map[string]string, len(inst.Tags))
		for _, t := range inst.Tags {
			if t.Key == nil {
				continue
			}
			key := *t.Key
			val := ""
			if t.Value != nil {
				val = *t.Value
			}
			snap.Tags[key] = val
			if !snap.HasOTel && strings.HasPrefix(strings.ToLower(key), "otel") {
				snap.HasOTel = true
			}
		}
	}
	return snap
}

// detectOSFamily reads inst.Platform / PlatformDetails to classify
// the OS. AWS reports Platform=windows for Windows instances; empty
// Platform with a non-empty PlatformDetails that mentions "linux"
// signals Linux. Anything else stays "unknown" so the proposer
// emits a hedged recommendation.
func detectOSFamily(inst ec2Instance) string {
	if string(inst.Platform) == "windows" {
		return "windows"
	}
	if inst.PlatformDetails != nil {
		details := strings.ToLower(*inst.PlatformDetails)
		if strings.Contains(details, "linux") {
			return "linux"
		}
		if strings.Contains(details, "windows") {
			return "windows"
		}
	}
	// Empty Platform with no PlatformDetails on a running EC2
	// instance almost always means Linux (Windows always populates
	// Platform), but the design's OTel-detection layer is more
	// conservative — defaulting to linux when AWS hasn't told us is
	// the right operator-visible signal.
	if inst.Platform == "" && inst.PlatformDetails == nil {
		return "linux"
	}
	return "unknown"
}

// mapLambdaFunction turns an SDK FunctionConfiguration into the
// category-typed snapshot. OTel detection runs on the layer ARNs —
// any layer whose ARN contains otel or opentelemetry (case-
// insensitive) flips HasOTelLayer to true.
func mapLambdaFunction(fn lambdaFunction, region string) scanner.FunctionRuntimeSnapshot {
	snap := scanner.FunctionRuntimeSnapshot{
		Region: region,
	}
	if fn.FunctionArn != nil {
		snap.ResourceID = *fn.FunctionArn
	}
	if fn.FunctionName != nil {
		snap.Name = *fn.FunctionName
	}
	if fn.Runtime != "" {
		snap.Runtime = string(fn.Runtime)
	}
	for _, l := range fn.Layers {
		if l.Arn == nil {
			continue
		}
		lower := strings.ToLower(*l.Arn)
		if strings.Contains(lower, "otel") || strings.Contains(lower, "opentelemetry") {
			snap.HasOTelLayer = true
			break
		}
	}
	return snap
}

// retryWithBackoff runs fn up to maxRetries times, doubling the sleep
// between attempts when fn returns a throttling-shaped AWS error. Non-
// throttling errors short-circuit immediately. The base / max counts
// are intentionally conservative — slice 1 prioritizes finishing
// scans over fighting a degraded AWS, so a hard cap of ~3.5s of
// cumulative wait keeps the wizard responsive.
func retryWithBackoff(ctx context.Context, fn func() error) error {
	const (
		maxAttempts = 3
		baseWait    = 500 * time.Millisecond
	)
	var lastErr error
	wait := baseWait
	for attempt := 0; attempt < maxAttempts; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if !isThrottlingError(lastErr) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
	return lastErr
}

// isThrottlingError pattern-matches the smithy.APIError shape against
// the throttling codes AWS surfaces. Used by retryWithBackoff to
// decide whether a retry is worth the wait.
func isThrottlingError(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "Throttling", "ThrottlingException", "RequestLimitExceeded", "TooManyRequestsException":
		return true
	}
	return false
}

// recordPartialFailure marks the scan partial and appends both a
// service identifier to FailedServices AND a human-readable reason to
// PartialReason. v0.88.3 fix for #586: prior versions OVERWROTE
// PartialReason at each failure site, so when two services failed in
// the same scan only the last one's diagnostic survived (operationally
// observed during Track A — when both rds and alb failed in different
// runs, the alb message would replace rds). FailedServices was always
// accumulating correctly (it's an append), so audit consumers had the
// structured list right; the human-readable string lost the earlier
// failure. Now both fields accumulate, joined by "; " in
// PartialReason. Single-failure scans are unaffected — the join only
// kicks in on the second-and-subsequent failures.
//
// Service identifiers shipping today: "assume_role" (sentinel for
// credentials-layer failures), "ec2", "lambda", "rds", "s3", "alb",
// "eks" (slice 3b — v0.89.0).
func recordPartialFailure(result *scanner.Result, service, reason string) {
	result.Partial = true
	if result.PartialReason == "" {
		result.PartialReason = reason
	} else {
		result.PartialReason = result.PartialReason + "; " + reason
	}
	result.FailedServices = append(result.FailedServices, service)
}
