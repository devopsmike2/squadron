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
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/rds"
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

	// TODO(v0.87.4+): PartialReason and FailedServices currently
	// overwrite/clobber on multiple service failures rather than
	// accumulate — the last failed service wins. Single-service-failure
	// is the slice 1+2 common case; slice 3 multi-service scans (S3 +
	// ALB + EKS) will elevate the accumulator question. Separate
	// follow-up; do not fix here.
	factory, err := s.ensureFactory(ctx, regions[0])
	if err != nil {
		result.Partial = true
		result.PartialReason = fmt.Sprintf("assume-role failed: %s", err.Error())
		// Sentinel "assume_role" distinguishes credentials-layer
		// failures from per-service walk failures for audit consumers
		// pattern-matching against FailedServices.
		result.FailedServices = append(result.FailedServices, "assume_role")
		return result, nil
	}

	for _, region := range regions {
		if err := s.scanRegionEC2(ctx, factory, region, result); err != nil {
			result.Partial = true
			result.PartialReason = fmt.Sprintf("ec2 scan failed in %s: %s", region, err.Error())
			result.FailedServices = append(result.FailedServices, "ec2")
		}
		if err := s.scanRegionLambda(ctx, factory, region, result); err != nil {
			result.Partial = true
			result.PartialReason = fmt.Sprintf("lambda scan failed in %s: %s", region, err.Error())
			result.FailedServices = append(result.FailedServices, "lambda")
		}
		if err := s.scanRegionRDS(ctx, factory, region, result); err != nil {
			result.Partial = true
			result.PartialReason = fmt.Sprintf("rds scan failed in %s: %s", region, err.Error())
			result.FailedServices = append(result.FailedServices, "rds")
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

	return result, nil
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
