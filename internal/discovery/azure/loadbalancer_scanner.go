// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

// Load-balancer tier — Azure Load Balancers. Coverage-parity arc slice
// 3. Mirrors the AWS ALB surface: a load balancer counts as
// instrumented when diagnostic logging is configured (any enabled log
// category), matching the ALB AccessLogsEnabled axis. Scheme is derived
// from whether any frontend has a public IP (internet-facing) or not
// (internal).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

type armLoadBalancer struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Location string            `json:"location"`
	Tags     map[string]string `json:"tags"`
	Sku      struct {
		Name string `json:"name"`
	} `json:"sku"`
	Properties struct {
		FrontendIPConfigurations []struct {
			Properties struct {
				PublicIPAddress *struct {
					ID string `json:"id"`
				} `json:"publicIPAddress"`
			} `json:"properties"`
		} `json:"frontendIPConfigurations"`
	} `json:"properties"`
}

type armLoadBalancerListResponse struct {
	Value    []armLoadBalancer `json:"value"`
	NextLink string            `json:"nextLink"`
}

func (s *Scanner) scanAzureLoadBalancers(ctx context.Context, accessToken string, result *scanner.Result) {
	lbs, listErr := s.listLoadBalancers(ctx, accessToken)
	if listErr != nil {
		recordPartialFailure(result, ServiceIDAzureLB, classifyAzureLBError(listErr))
		return
	}
	for _, lb := range lbs {
		logging, diagErr := s.probeLBDiagLogging(ctx, accessToken, lb.ID)
		if diagErr != nil {
			recordPartialFailure(result, ServiceIDAzureLB, classifyAzureLBError(diagErr))
			result.LoadBalancers = append(result.LoadBalancers, projectAzureLB(lb, false))
			continue
		}
		result.LoadBalancers = append(result.LoadBalancers, projectAzureLB(lb, logging))
	}
}

func (s *Scanner) listLoadBalancers(ctx context.Context, accessToken string) ([]armLoadBalancer, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.Network/loadBalancers?api-version=%s",
		strings.TrimRight(endpoint, "/"), s.SubscriptionID, armNetworkAPIVersion,
	)
	var out []armLoadBalancer
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armLoadBalancerListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("load balancer list parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, nil
}

func (s *Scanner) probeLBDiagLogging(ctx context.Context, accessToken, lbARMID string) (bool, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	resourceID := strings.TrimPrefix(lbARMID, "/")
	diagURL := fmt.Sprintf(
		"%s/%s/providers/microsoft.insights/diagnosticSettings?api-version=%s",
		strings.TrimRight(endpoint, "/"), resourceID, armDiagSettingsAPIVersion,
	)
	body, callErr := s.doARMGet(ctx, accessToken, diagURL)
	if callErr != nil {
		var ace *armCallError
		if errors.As(callErr, &ace) && ace.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, callErr
	}
	var resp armDiagnosticSettingsResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return false, &armCallError{Wrapped: fmt.Errorf("diagnostic settings parse: %w", jerr)}
	}
	for _, ds := range resp.Value {
		for _, log := range ds.Properties.Logs {
			if log.Enabled {
				return true, nil
			}
		}
	}
	return false, nil
}

func projectAzureLB(lb armLoadBalancer, logging bool) scanner.LoadBalancerSnapshot {
	scheme := "internal"
	for _, f := range lb.Properties.FrontendIPConfigurations {
		if f.Properties.PublicIPAddress != nil && f.Properties.PublicIPAddress.ID != "" {
			scheme = "internet-facing"
			break
		}
	}
	return scanner.LoadBalancerSnapshot{
		ResourceID:        lb.Name,
		Name:              lb.Name,
		Type:              lb.Sku.Name,
		Scheme:            scheme,
		AccessLogsEnabled: logging,
		Region:            lb.Location,
		Tags:              copyTags(lb.Tags),
	}
}

func classifyAzureLBError(err error) string {
	return classifyAzureTierError(ServiceIDAzureLB, "load balancer", err)
}
