// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"encoding/json"
	"net/http"
	"strings"
)

// routeEmptyInfraTier handles the object-store (Microsoft.Storage/
// storageAccounts) and load-balancer (Microsoft.Network/loadBalancers)
// list endpoints with an empty-inventory response, returning true when
// it handled the request.
//
// Every full-Scan() test fake must route these two endpoints or the
// object-store + load-balancer walkers — which always run after the
// SQL / AKS / serverless walks — hit the fake's unhandled-path 404 and
// record spurious "azurestorage" / "azurelb" partial failures (the
// regression this helper centralizes the fix for). It mirrors the
// per-fake Microsoft.Web/sites + managedClusters empty-list routes; a
// single helper keeps every fake in lock-step as new infra tiers land.
func routeEmptyInfraTier(w http.ResponseWriter, path string) bool {
	switch {
	case strings.HasSuffix(path, "/providers/Microsoft.Storage/storageAccounts"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(armStorageAccountListResponse{Value: nil})
		return true
	case strings.HasSuffix(path, "/providers/Microsoft.Network/loadBalancers"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(armLoadBalancerListResponse{Value: nil})
		return true
	}
	return false
}
