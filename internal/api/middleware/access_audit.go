// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/devopsmike2/squadron/internal/services"
)

// APIAccessAudit returns Gin middleware that emits an api.request audit
// event for every mutating request (POST, PUT, PATCH, DELETE) the API
// successfully accepts. v0.51 added this to close the gap between the
// NIST CSF PR.AA-04 / CIP-007-6 R4.1.2 mapping ("every privileged API
// call is logged") and the actual code, which previously only recorded
// audit events at the service layer for a subset of state changes.
//
// What we capture:
//   - actor (token id + label) from the auth middleware,
//   - HTTP method + path,
//   - status code returned to the caller,
//   - latency (ms),
//   - remote IP.
//
// What we deliberately do not capture:
//   - request body (would routinely contain operator-supplied secrets
//     like SIEM HEC tokens; payload-level state changes are still
//     captured by the service-layer events that own the data),
//   - response body (same risk).
//
// GET / HEAD / OPTIONS are skipped by default. Reads are high-volume
// and rarely interesting for compliance evidence; an operator who
// needs them can flip includeReads to true at construction.
func APIAccessAudit(audit services.AuditService, includeReads bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if audit == nil {
			return
		}
		method := c.Request.Method
		if !includeReads {
			switch method {
			case "GET", "HEAD", "OPTIONS":
				return
			}
		}
		// Don't bother auditing failed-auth requests. RequireBearer
		// already returns 401 for those and we don't want a torrent
		// of "someone hit the API without a token" events in the
		// audit log. 4xx that came from a valid token (403, 404,
		// 422) are still recorded because they represent an
		// authorized actor doing something the system rejected.
		actor := ActorFromGin(c)
		if actor.IsZero() {
			return
		}
		_ = audit.Record(c.Request.Context(), services.AuditEntry{
			EventType:  "api.request",
			TargetType: "api",
			TargetID:   c.Request.URL.Path,
			Action:     method,
			Payload: map[string]any{
				"status":     c.Writer.Status(),
				"latency_ms": time.Since(start).Milliseconds(),
				"remote_ip":  c.ClientIP(),
			},
		})
	}
}
