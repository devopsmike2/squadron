// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Commercial-tier detector activation for Azure Functions (#153
// productization, slice 2). The cold-start + error-rate detectors are
// dormant in OSS: the Functions Scan() lifecycle never invokes them, and the
// metrics they need (requests/duration, requests/failed) live on the
// Application Insights COMPONENT resource — not the Function App. This file
// resolves each Function App's linked App Insights component and runs the
// detectors against it when the commercial gate is enabled, annotating the
// scan-response serverless rows (ColdStartP95Ms / ErrorRate fields the UI
// reads) live. No persistence is involved — the result is surfaced on the
// same scan that produced it.
//
// IAM: resolving the component requires Microsoft.Insights/components/read
// (subscription-scope component LIST) on the operator's Service Principal,
// plus the Microsoft.Insights/metrics/read already used by the metric path.
// The built-in Reader / Monitoring Reader roles both grant these; operators
// using a narrow custom role must add Microsoft.Insights/components/read.
// See docs/detection-coverage.md + the v0.89.313 release notes.

// appInsightsComponentsListAPIVersion pins the ARM
// Microsoft.Insights/components LIST call. 2020-02-02 is the current GA
// version and returns InstrumentationKey on every component.
const appInsightsComponentsListAPIVersion = "2020-02-02"

// armAppInsightsComponent is the slim shape Squadron consumes from the
// Microsoft.Insights/components LIST response: the ARM resource id (the
// metric-query target) keyed by its InstrumentationKey (the handle a
// Function App's connection string carries).
type armAppInsightsComponent struct {
	ID         string `json:"id"`
	Properties struct {
		InstrumentationKey string `json:"InstrumentationKey"`
	} `json:"properties"`
}

type armAppInsightsComponentList struct {
	Value    []armAppInsightsComponent `json:"value"`
	NextLink string                    `json:"nextLink"`
}

// listAppInsightsComponentsByIK returns a map of InstrumentationKey →
// component ARM resource id for every Application Insights component in the
// subscription. The InstrumentationKey is the join key: a Function App's
// APPLICATIONINSIGHTS_CONNECTION_STRING carries the IK, but not the ARM id
// the metric query needs.
func (s *Scanner) listAppInsightsComponentsByIK(ctx context.Context, accessToken string) (map[string]string, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.Insights/components?api-version=%s",
		strings.TrimRight(endpoint, "/"),
		s.SubscriptionID,
		appInsightsComponentsListAPIVersion,
	)

	out := make(map[string]string)
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armAppInsightsComponentList
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("app insights components list parse: %w", jerr)}
		}
		for _, c := range page.Value {
			if c.Properties.InstrumentationKey != "" && c.ID != "" {
				out[c.Properties.InstrumentationKey] = c.ID
			}
		}
		pageURL = page.NextLink
	}
	return out, nil
}

// instrumentationKeyFromConnString extracts the InstrumentationKey from an
// App Insights connection string of the form
// "InstrumentationKey=<guid>;IngestionEndpoint=...;ApplicationId=<guid>".
// Returns "" when the key is absent or the string is empty. The IK is a
// telemetry-ingestion handle, not a secret credential, and is never logged
// or surfaced — it is used only as the in-memory join key to the component.
func instrumentationKeyFromConnString(connString string) string {
	for _, part := range strings.Split(connString, ";") {
		part = strings.TrimSpace(part)
		const prefix = "InstrumentationKey="
		if strings.HasPrefix(part, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(part, prefix))
		}
	}
	return ""
}

// captureFunctionInstrumentationKey records a Function App's App Insights
// InstrumentationKey (parsed from its connection-string app setting) keyed by
// the Function App's ARM id, so the post-scan commercial enrichment can
// resolve the linked component. Only called on the commercial path during
// the Functions walk — a no-op when the connection string is absent.
func (s *Scanner) captureFunctionInstrumentationKey(functionARMID, connString string) {
	ik := instrumentationKeyFromConnString(connString)
	if ik == "" {
		return
	}
	if s.ikByFunctionARN == nil {
		s.ikByFunctionARN = make(map[string]string)
	}
	s.ikByFunctionARN[functionARMID] = ik
}

// runAzureServerlessCommercialDetection is the gated post-projection
// enrichment pass (#153 productization). When the commercial gate is on it
// resolves each Function App's linked Application Insights component and runs
// the cold-start + error-rate detectors against it, annotating the
// corresponding serverless row in-place. Best-effort throughout: a function
// whose component can't be resolved (no App Insights linked, unknown IK)
// safe-degrades to no annotation — never a partial-scan failure for the
// inventory walk. A components-list failure records one partial failure and
// skips the whole pass.
func (s *Scanner) runAzureServerlessCommercialDetection(ctx context.Context, accessToken string, result *scanner.Result) {
	if !s.commercialDetectors {
		return
	}
	// The detectors authenticate via the scanner's accessToken field +
	// metricsLimiter; Scan() acquires the token locally, so arm them here.
	if s.accessToken == "" {
		s.accessToken = accessToken
	}
	if s.metricsLimiter == nil {
		s.metricsLimiter = rate.NewLimiter(rate.Limit(float64(AzureMonitorRateLimitRPH)/3600.0), 1)
	}
	if len(s.ikByFunctionARN) == 0 {
		return // no Function App carried an App Insights connection string
	}

	ikToComponent, err := s.listAppInsightsComponentsByIK(ctx, accessToken)
	if err != nil {
		recordPartialFailure(result, ServiceIDAzureFunctions,
			fmt.Sprintf("%s: app insights component resolution failed: %s",
				ServiceIDAzureFunctions, classifyARMError(err)))
		return
	}

	for i := range result.Serverless {
		snap := &result.Serverless[i]
		if snap.Surface != azureFunctionsServerlessSurface || snap.ResourceARN == "" {
			continue
		}
		ik := s.ikByFunctionARN[snap.ResourceARN]
		if ik == "" {
			continue // function not linked to App Insights → nothing to measure
		}
		componentID := ikToComponent[ik]
		if componentID == "" {
			continue // IK didn't resolve to a readable component (cross-sub / RBAC)
		}
		s.annotateServerlessCommercial(ctx, snap, componentID, result)
	}
}

// annotateServerlessCommercial runs both detectors against the resolved
// App Insights component and writes the typed annotation fields the UI reads.
// Per-detector failures are recorded as partial failures but never abort the
// surrounding loop. For transparency the resolved component id is surfaced in
// the row's Detail bag.
func (s *Scanner) annotateServerlessCommercial(
	ctx context.Context, snap *scanner.ServerlessInstanceSnapshot, componentID string, result *scanner.Result,
) {
	if snap.Detail == nil {
		snap.Detail = make(map[string]any)
	}
	snap.Detail["appinsights_component_id"] = componentID

	if cs, err := s.DetectColdStartRegression(ctx, componentID); err != nil {
		recordPartialFailure(result, ServiceIDAzureFunctions,
			fmt.Sprintf("%s: cold-start detection failed for %s: %s",
				ServiceIDAzureFunctions, snap.ResourceARN, err.Error()))
	} else if cs.CurrentSampleCount > 0 || cs.CurrentP95Ms > 0 {
		p95 := cs.CurrentP95Ms
		exceeds := cs.ShouldFireRecommendation()
		snap.ColdStartP95Ms = &p95
		snap.ColdStartExceedsThreshold = &exceeds
	}

	if er, err := s.DetectErrorRate(ctx, componentID); err != nil {
		recordPartialFailure(result, ServiceIDAzureFunctions,
			fmt.Sprintf("%s: error-rate detection failed for %s: %s",
				ServiceIDAzureFunctions, snap.ResourceARN, err.Error()))
	} else if er.CurrentInvocationCount > 0 {
		errRate := er.CurrentErrorRate
		exceeds := errorRateExceeds(er)
		snap.CurrentErrorRate = &errRate
		snap.ErrorRateExceedsThreshold = &exceeds
	}
}

// errorRateExceeds mirrors proposer.ErrorRateDetectionResult.ShouldFireRecommendation
// (and handlers.computeErrorRateExceedsThreshold): the amber predicate fires
// when the current/baseline error-rate ratio clears the floor AND the current
// window carries statistically meaningful volume (enough invocations + enough
// errors). The baseline must itself be above the noise floor for the ratio to
// be meaningful.
func errorRateExceeds(er ErrorRateDetectionResult) bool {
	if er.BaselineErrorRate < ErrorRateBaselineFloor {
		return false
	}
	ratio := er.CurrentErrorRate / er.BaselineErrorRate
	return ratio >= ErrorRateRatioFloor &&
		er.CurrentInvocationCount >= ErrorRateMinInvocationCount &&
		er.CurrentErrorCount >= ErrorRateMinErrorCount
}
