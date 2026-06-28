// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"errors"
	"fmt"
	"net/http"
)

// classifyOCITierError maps a signed-request walk failure into the
// operator-visible PartialReason for a coverage-parity tier (object
// storage / load balancer), parameterized by service id + label. Same
// shape as the per-tier classifiers in scanner_db.go etc. Returns the
// empty string for a mid-walk 404 (compartment vanished) so the caller
// can skip that compartment without recording a failure.
func classifyOCITierError(serviceID, label string, err error) string {
	if err == nil {
		return ""
	}
	var oce *ociCallError
	if errors.As(err, &oce) {
		if oce.IsNetwork {
			wrapped := ""
			if oce.Wrapped != nil {
				wrapped = oce.Wrapped.Error()
			}
			return fmt.Sprintf("%s: network error: %s", serviceID, truncate(wrapped, 200))
		}
		if oce.StatusCode == http.StatusTooManyRequests || oce.RetryAfter != "" {
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", serviceID)
		}
		switch oce.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Sprintf("%s: credentials invalid (re-check fingerprint, user_ocid, and private key)", serviceID)
		case http.StatusForbidden:
			return fmt.Sprintf("%s: permission denied (verify the user has access to the compartments)", serviceID)
		case http.StatusNotFound:
			// Mid-walk compartment-not-found is non-fatal; caller skips.
			return ""
		default:
			msg := oce.Message
			if msg == "" {
				msg = oce.BodyHint
			}
			return fmt.Sprintf("%s: %s walk failed (HTTP %d): %s", serviceID, label, oce.StatusCode, truncate(msg, 200))
		}
	}
	return fmt.Sprintf("%s: %s", serviceID, truncate(err.Error(), 200))
}
