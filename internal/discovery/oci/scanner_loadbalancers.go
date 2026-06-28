// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

// Load-balancer tier — OCI Load Balancers. Coverage-parity arc slice 4.
// Mirrors the AWS ALB surface for inventory + scheme (isPrivate →
// internal, else internet-facing). OCI load-balancer access logs are
// delivered through the Logging service (no per-LB inline flag), so
// AccessLogsEnabled is left false and full logging detection is a
// documented future enhancement — the inventory is still surfaced so
// the operator (and proposer) can act on it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// ServiceIDLoadBalancer is the load-balancer-tier service identifier.
const ServiceIDLoadBalancer = "ocilb"

const lbListAPIVersion = "20170115"

type ociLoadBalancer struct {
	ID           string                            `json:"id"`
	DisplayName  string                            `json:"displayName"`
	IsPrivate    bool                              `json:"isPrivate"`
	FreeformTags map[string]string                 `json:"freeformTags"`
	DefinedTags  map[string]map[string]interface{} `json:"definedTags"`
}

func (s *Scanner) lbEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://loadbalancer.%s.oraclecloud.com", s.Region)
}

func (s *Scanner) scanLoadBalancers(ctx context.Context, sk *SigningKey, comps []ociCompartment, result *scanner.Result) {
	for _, comp := range comps {
		lbs, listErr := s.listLoadBalancers(ctx, sk, comp.ID)
		if listErr != nil {
			reason := classifyOCITierError(ServiceIDLoadBalancer, "load balancer", listErr)
			if reason == "" {
				continue
			}
			recordPartialFailure(result, ServiceIDLoadBalancer, reason)
			continue
		}
		for _, lb := range lbs {
			scheme := "internet-facing"
			if lb.IsPrivate {
				scheme = "internal"
			}
			result.LoadBalancers = append(result.LoadBalancers, scanner.LoadBalancerSnapshot{
				ResourceID:        lb.ID,
				Name:              lb.DisplayName,
				Type:              "load-balancer",
				Scheme:            scheme,
				AccessLogsEnabled: false, // detected via the Logging service — deferred
				Region:            s.Region,
				Tags:              flattenTags(lb.FreeformTags, lb.DefinedTags),
			})
		}
	}
}

func (s *Scanner) listLoadBalancers(ctx context.Context, sk *SigningKey, compartmentID string) ([]ociLoadBalancer, error) {
	url := fmt.Sprintf("%s/%s/loadBalancers?compartmentId=%s",
		strings.TrimRight(s.lbEndpoint(), "/"), lbListAPIVersion, compartmentID)
	body, callErr := s.doSignedGET(ctx, sk, url)
	if callErr != nil {
		return nil, callErr
	}
	var out []ociLoadBalancer
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, &ociCallError{Wrapped: fmt.Errorf("load balancer list parse: %w", jerr)}
	}
	return out, nil
}
