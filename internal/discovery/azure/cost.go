// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Cost-correlation substrate slice 6 chunk 4 (v0.89.186, #828 Stream
// 225) — the Azure Cost Management QueryCost body + Service Bus cost
// enrichment. Second per-cloud cost reader (after AWS chunk 2/3).
//
// READ-ONLY: issues exactly one Azure API — Cost Management /query, a
// read. No mutating/provisioning call.
//
// FREE PER CALL: Azure Cost Management Query is not per-call-priced
// (subject to ARM throttling, not billing), so the per-call cost is 0
// and the governor authorizes it unconditionally. The governor is
// still REQUIRED — it is the opt-in "cost correlation enabled" signal,
// so Azure cost queries (and the extra ARM calls they make) never run
// by default, only when explicitly wired. Like the rest of the
// substrate, nothing wires it in production by default.

// AzureCostManagementPerCallMicroUSD is the per-call cost of the Cost
// Management /query API: 0 (free; throttle-limited, not billed). The
// governor authorizes a 0-cost call unconditionally — see
// scanner.CostBudgetGovernor.Authorize.
const AzureCostManagementPerCallMicroUSD scanner.MicroUSD = 0

// AzureCostManagementAPIVersion pins the Cost Management query API
// version.
const AzureCostManagementAPIVersion = "2023-03-01"

// azureCostMetricName is the aggregation column name requested + read
// back from the Cost Management response.
const azureCostMetricName = "Cost"

// AzureServiceBusCostServiceName is the Cost Management ServiceName
// dimension value for Azure Service Bus. Cost is attributed at the
// service level (account-wide Service Bus spend) — the same honest
// service-level scope the AWS chunk uses.
const AzureServiceBusCostServiceName = "Service Bus"

// AzureCostCorrelationWindowHours is the trailing window the cost
// figure covers (30 days), reported as a monthly figure.
const AzureCostCorrelationWindowHours = 30 * 24

// azureCostQueryBody is the Cost Management /query request body.
type azureCostQueryBody struct {
	Type       string                   `json:"type"`
	Timeframe  string                   `json:"timeframe"`
	TimePeriod azureCostQueryTimePeriod `json:"timePeriod"`
	Dataset    azureCostQueryDataset    `json:"dataset"`
}

type azureCostQueryTimePeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type azureCostQueryDataset struct {
	Granularity string                               `json:"granularity"`
	Aggregation map[string]azureCostQueryAggregation `json:"aggregation"`
	Filter      *azureCostQueryFilter                `json:"filter,omitempty"`
}

type azureCostQueryAggregation struct {
	Name     string `json:"name"`
	Function string `json:"function"`
}

type azureCostQueryFilter struct {
	Dimensions *azureCostQueryDimensions `json:"dimensions,omitempty"`
}

type azureCostQueryDimensions struct {
	Name     string   `json:"name"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

// azureCostQueryResponse is the Cost Management /query response. The
// rows are positional and described by columns — the substrate finds
// the Cost + Currency columns by name rather than assuming order.
type azureCostQueryResponse struct {
	Properties azureCostQueryProperties `json:"properties"`
}

type azureCostQueryProperties struct {
	Columns []azureCostQueryColumn `json:"columns"`
	Rows    [][]json.RawMessage    `json:"rows"`
}

type azureCostQueryColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// QueryCost implements scanner.CostQuerier for Azure via Cost
// Management /query. Reads the summed Cost attributed to the supplied
// ServiceName dimension over the window.
//
// dimension is the Cost Management ServiceName value to filter on
// (e.g. "Service Bus"). resourceID echoes onto the result.
//
// GATE: requires both an access token AND a governor (the opt-in
// signal). Returns scanner.ErrCostNotImplemented when either is
// missing. The governor authorizes the $0 per-call cost (free
// surface) — it never blocks an Azure cost call, but its presence is
// what enables cost correlation at all.
//
// Empty-result semantics: no cost rows → Covered=false / Amount=0 /
// no error.
func (s *Scanner) QueryCost(
	ctx context.Context,
	resourceID string,
	dimension string,
	window time.Duration,
) (scanner.CostResult, error) {
	base := scanner.CostResult{
		ResourceID:  resourceID,
		Dimension:   dimension,
		Granularity: scanner.CostGranularityMonthly,
		Window:      window,
	}
	if s.accessToken == "" || s.costGovernor == nil {
		return base, scanner.ErrCostNotImplemented
	}
	// Free surface — Authorize(0) always succeeds, but keeps the
	// call-site uniform with the per-call-priced clouds.
	if err := s.costGovernor.Authorize(AzureCostManagementPerCallMicroUSD); err != nil {
		return base, err
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-window)
	body := azureCostQueryBody{
		Type:      "ActualCost",
		Timeframe: "Custom",
		TimePeriod: azureCostQueryTimePeriod{
			From: startTime.Format(time.RFC3339),
			To:   endTime.Format(time.RFC3339),
		},
		Dataset: azureCostQueryDataset{
			Granularity: "None",
			Aggregation: map[string]azureCostQueryAggregation{
				"totalCost": {Name: azureCostMetricName, Function: "Sum"},
			},
			Filter: &azureCostQueryFilter{
				Dimensions: &azureCostQueryDimensions{
					Name:     "ServiceName",
					Operator: "In",
					Values:   []string{dimension},
				},
			},
		},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return base, fmt.Errorf("cost management: marshal body: %w", err)
	}

	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	u := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.CostManagement/query?api-version=%s",
		strings.TrimRight(endpoint, "/"), s.SubscriptionID, AzureCostManagementAPIVersion,
	)

	respBody, callErr := s.doARMPostJSON(ctx, s.accessToken, u, bodyBytes)
	if callErr != nil {
		return base, fmt.Errorf("cost management query: %w", callErr)
	}

	var resp azureCostQueryResponse
	if jerr := json.Unmarshal(respBody, &resp); jerr != nil {
		return base, fmt.Errorf("cost management: parse response: %w", jerr)
	}

	costIdx, currIdx := -1, -1
	for i, col := range resp.Properties.Columns {
		switch col.Name {
		case azureCostMetricName, "PreTaxCost", "CostUSD":
			if costIdx == -1 {
				costIdx = i
			}
		case "Currency":
			currIdx = i
		}
	}
	if costIdx == -1 {
		// No cost column — treat as not measured rather than error.
		base.ObservedAt = endTime
		return base, nil
	}

	var total scanner.MicroUSD
	currency := ""
	covered := false
	for _, row := range resp.Properties.Rows {
		if costIdx >= len(row) {
			continue
		}
		micro, perr := parseAzureCostToMicroUSD(string(row[costIdx]))
		if perr != nil {
			continue
		}
		covered = true
		total += micro
		if currency == "" && currIdx >= 0 && currIdx < len(row) {
			currency = strings.Trim(string(row[currIdx]), `"`)
		}
	}

	base.Covered = covered
	base.AmountMicroUSD = total
	base.Currency = currency
	base.ObservedAt = endTime
	return base, nil
}

// parseAzureCostToMicroUSD parses a Cost Management numeric cost cell
// (a JSON number token, e.g. "12.3456") into integer micro-USD without
// float. Truncates beyond 6 decimal places. Mirrors the AWS
// parseUSDToMicroUSD contract.
func parseAzureCostToMicroUSD(s string) (scanner.MicroUSD, error) {
	// Delegates to the canonical shared parser (slice 6 chunk 5,
	// v0.89.187). Kept as a named wrapper for the call site.
	return scanner.ParseDecimalToMicroUSD(s)
}

// doARMPostJSON issues a bearer-authenticated POST with a JSON body
// and returns the response body on 2xx. Mirrors doARMGet but for the
// Cost Management query POST (the existing doARMPost posts an empty
// body, so the cost path needs its own JSON-body variant).
func (s *Scanner) doARMPostJSON(ctx context.Context, accessToken, fullURL string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, &armCallError{Wrapped: err}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, &armCallError{Wrapped: err, IsNetwork: true}
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var aerr armErrorResponse
		_ = json.Unmarshal(respBody, &aerr)
		return nil, &armCallError{
			StatusCode: resp.StatusCode,
			Code:       aerr.Error.Code,
			Message:    aerr.Error.Message,
			BodyHint:   truncate(string(respBody), 200),
			RetryAfter: resp.Header.Get("Retry-After"),
		}
	}
	return respBody, nil
}

// enrichServiceBusCost attaches the Azure Service Bus service cost to
// every Service Bus namespace snapshot. Cost-correlation slice 6
// chunk 4.
//
// GATE / opt-in: no-op unless an access token is available AND a
// governor is wired (the cost-enabled signal). No production code
// wires the governor by default, so Azure cost queries never run
// unless explicitly enabled. One Cost Management query per scan.
//
// Attaches service_cost_monthly_micro_usd + service_cost_currency +
// service_cost_scope="service" (honest service-level label) on a
// Covered reading; no keys otherwise (cold-start parity on the
// unwired path). The token is wired onto the Scanner from the
// in-scope dispatcher token (same pattern as enrichServiceBusPoisonRate).
//
// See docs/proposals/cost-correlation-substrate-slice6.md §6.
func (s *Scanner) enrichServiceBusCost(ctx context.Context, snaps []scanner.EventSourceInstanceSnapshot, accessToken string) {
	if accessToken == "" || s.costGovernor == nil {
		return
	}
	if s.accessToken == "" {
		s.accessToken = accessToken
	}
	if len(snaps) == 0 {
		return
	}

	res, err := s.QueryCost(
		ctx, AzureServiceBusCostServiceName, AzureServiceBusCostServiceName,
		time.Duration(AzureCostCorrelationWindowHours)*time.Hour,
	)
	if err != nil || !res.Covered {
		return
	}
	for i := range snaps {
		if snaps[i].Detail == nil {
			continue
		}
		snaps[i].Detail["service_cost_monthly_micro_usd"] = res.AmountMicroUSD
		snaps[i].Detail["service_cost_currency"] = res.Currency
		snaps[i].Detail["service_cost_scope"] = "service"
	}
}

// WithCostBudgetGovernor wires the cost governor (the opt-in signal)
// onto the Scanner. Nil disables cost correlation. Returns the Scanner
// so the constructor chain composes.
func (s *Scanner) WithCostBudgetGovernor(g *scanner.CostBudgetGovernor) *Scanner {
	s.costGovernor = g
	return s
}
