// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"strings"
)

// Commercial-tier detector activation (#152 productization). The
// cold-start + error-rate regression detectors are dormant in OSS: their
// observation stores are never wired and the InitDuration query targets a
// namespace that returns nothing. The scan orchestrator activates them —
// only when config.CommercialDetectors.Enabled is true — by wiring the
// observation stores + the commercial gate (EnableCommercialDetectors) and
// letting the run*ForServerless walkers build a per-region CloudWatch client
// on demand.

// cloudWatchBuilder is the concrete-factory capability the commercial
// detectors type-assert off the assume-role ClientFactory to construct a
// per-region CloudWatch metrics client — the analog of costExplorerBuilder
// for the cost path. Keeping it a capability interface (rather than adding
// CloudWatch to ClientFactory) avoids forcing every test stub factory to
// implement it.
type cloudWatchBuilder interface {
	CloudWatch(ctx context.Context, region string) (CloudWatchClient, error)
}

// EnableCommercialDetectors turns the add-on-dependent regression detectors
// on and wires the observation stores the detection branch persists to. The
// per-function CloudWatch client is built lazily, per region, during the
// scan walk (Lambda Insights metrics are region-scoped). Mirrors
// EnableCostCorrelation; called by the scan orchestrator only when
// config.CommercialDetectors.Enabled is true.
func (s *Scanner) EnableCommercialDetectors(coldStartStore ColdStartStore, errorRateStore ErrorRateStore) *Scanner {
	s.commercialDetectors = true
	s.coldStartStore = coldStartStore
	s.errorRateStore = errorRateStore
	return s
}

// EnableServerlessMetricDetection turns on the NATIVE-metric serverless
// detectors without the commercial add-on gate. It wires the error-rate
// observation store and flips serverlessMetricDetection, so
// runErrorRateDetectionForServerless builds a per-region CloudWatch client on
// demand and runs against the native AWS/Lambda Errors + Invocations metrics.
// Lambda cold-start is intentionally NOT enabled here (it needs the paid
// Lambda Insights add-on, which stays under EnableCommercialDetectors). Called
// by the scan orchestrator only when config.ServerlessMetricDetection.Enabled
// is true. Composable with EnableCommercialDetectors (both may be on).
func (s *Scanner) EnableServerlessMetricDetection(errorRateStore ErrorRateStore) *Scanner {
	s.serverlessMetricDetection = true
	s.errorRateStore = errorRateStore
	return s
}

// cloudWatchForRegion returns a CloudWatch client bound to the supplied
// region, building it from the assume-role factory and caching it per region
// so a multi-region scan reuses one client per region. Used only on the
// commercial path; the OSS/test path injects a client directly via
// WithCloudWatchClient.
func (s *Scanner) cloudWatchForRegion(ctx context.Context, region string) (CloudWatchClient, error) {
	if region == "" {
		region = "us-east-1"
	}
	if s.cwClientByRegion == nil {
		s.cwClientByRegion = make(map[string]CloudWatchClient)
	}
	if c, ok := s.cwClientByRegion[region]; ok {
		return c, nil
	}
	factory, err := s.ensureFactory(ctx, region)
	if err != nil {
		return nil, err
	}
	builder, ok := factory.(cloudWatchBuilder)
	if !ok {
		return nil, errors.New("aws: factory does not support CloudWatch client construction")
	}
	c, err := builder.CloudWatch(ctx, region)
	if err != nil {
		return nil, err
	}
	s.cwClientByRegion[region] = c
	return c, nil
}

// regionFromARN extracts the region segment from a standard ARN
// (arn:aws:<svc>:<region>:<account>:...). Returns "" when the ARN is
// malformed or region-less (e.g. IAM/global ARNs), which callers treat as
// "fall back to the default region".
func regionFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) >= 4 {
		return parts[3]
	}
	return ""
}
