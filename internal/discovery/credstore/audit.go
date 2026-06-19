// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"context"

	"github.com/devopsmike2/squadron/internal/services"
)

// EventTypeRoleAssumed is the canonical event-type string emitted on
// every read of the credential substrate. The discovery design doc
// guarantees the substrate has no unaudited read path; this constant
// is how the substrate fulfils that contract.
const EventTypeRoleAssumed = "discovery.role_assumed"

// TargetTypeAWSConnection is the audit target_type string for AWS
// account connections. Kept package-local so the audit timeline shows
// "aws_connection" consistently without pulling new constants into
// internal/services.
const TargetTypeAWSConnection = "aws_connection"

// AuditRecorder is the minimal audit-emission contract the credential
// substrate needs. The canonical *services.AuditService satisfies this
// interface naturally — the package-local alias keeps tests trivial to
// wire (a struct collecting calls implements it in three lines) and
// avoids dragging the full AuditService surface into this package.
//
// Per the discovery design doc, every substrate read MUST emit a
// discovery.role_assumed event. Callers wiring a real Store MUST
// provide a non-nil recorder.
type AuditRecorder interface {
	Record(ctx context.Context, entry services.AuditEntry) error
}
