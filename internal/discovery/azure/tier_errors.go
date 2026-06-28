// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"errors"
	"fmt"
	"net/http"
)

// classifyAzureTierError maps an ARM walk failure into the operator-
// visible PartialReason string for a coverage-parity tier (storage /
// load balancer). Same shape as classifyAzureSQLError, parameterized by
// the service id + a human label for the generic-tail message.
func classifyAzureTierError(serviceID, label string, err error) string {
	if err == nil {
		return ""
	}
	var ace *armCallError
	if errors.As(err, &ace) {
		if ace.IsNetwork {
			wrapped := ""
			if ace.Wrapped != nil {
				wrapped = ace.Wrapped.Error()
			}
			return fmt.Sprintf("%s: network error: %s", serviceID, truncate(wrapped, 200))
		}
		if ace.StatusCode == http.StatusTooManyRequests || ace.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", serviceID)
		}
		switch ace.StatusCode {
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the service principal has Reader role on the subscription)", serviceID)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: subscription not found (verify subscription_id is correct)", serviceID)
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check tenant_id, client_id, client_secret)", serviceID)
		default:
			msg := ace.Message
			if msg == "" {
				msg = ace.BodyHint
			}
			return fmt.Sprintf("%s: %s walk failed (HTTP %d): %s", serviceID, label, ace.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", serviceID, truncate(err.Error(), 200))
}
