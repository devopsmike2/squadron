// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"context"
	"fmt"

	"github.com/devopsmike2/squadron/internal/services"
)

// EventTypeConnectionRead is the unprefixed action suffix used for
// every substrate read. The emitted EventType is composed as
// "discovery.<provider>.connection_read" — e.g.
// "discovery.aws.connection_read" — so a multi-cloud deployment can
// filter audit events per provider without parsing the payload.
//
// The provider value is dynamic in the emission code; adding GCP or
// Azure later requires no audit-infra changes. The constant here is
// the suffix only, exported so external packages writing fake audit
// recorders can construct the expected event type identically.
const EventTypeConnectionRead = "connection_read"

// FormatConnectionReadEvent returns the provider-prefixed audit event
// type string for a substrate read. Exposed so test doubles can
// construct the expected event type without re-implementing the
// formatting rule.
func FormatConnectionReadEvent(p Provider) string {
	return fmt.Sprintf("discovery.%s.%s", p, EventTypeConnectionRead)
}

// TargetTypeCloudConnection is the audit target_type string for any
// CloudConnection row, regardless of provider. The provider is carried
// in the event type ("discovery.aws.connection_read") and in the
// payload ("provider" key) so the target_type stays uniform — that way
// the audit timeline can group "all substrate reads" with a single
// filter.
const TargetTypeCloudConnection = "cloud_connection"

// AuditRecorder is the minimal audit-emission contract the credential
// substrate needs. The canonical *services.AuditService satisfies this
// interface naturally — the package-local alias keeps tests trivial to
// wire (a struct collecting calls implements it in three lines) and
// avoids dragging the full AuditService surface into this package.
//
// Per the discovery design doc, every substrate read MUST emit a
// discovery.<provider>.connection_read event. Callers wiring a real
// Store MUST provide a non-nil recorder.
type AuditRecorder interface {
	Record(ctx context.Context, entry services.AuditEntry) error
}
