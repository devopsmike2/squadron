// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// connectionRegistry is the HA S3b ownership seam (ADR 0035) the OpAMP server
// and the S3a reconcile loop write through. It records which Squadron instance
// owns each agent's OpAMP WebSocket:
//
//   - the OpAMP server upserts on connect (this instance now owns the socket)
//     and deletes on a clean disconnect (this instance relinquishes it);
//   - the reconcile loop upserts each tick as a heartbeat that advances
//     last_heartbeat_at for every agent connected to THIS instance.
//
// Satisfied by types.ApplicationStore in production (its UpsertConnectionOwner /
// DeleteConnectionOwner methods); faked in tests. Kept minimal — just the two
// writes these call sites use — so a fake need not implement the whole store,
// mirroring the configPusher seam in reconciler.go.
//
// All writes are SYSTEM-SCOPED: the registry is instance-identity, orthogonal
// to tenant, so the call sites stamp identity.WithSystemContext before writing.
type connectionRegistry interface {
	UpsertConnectionOwner(ctx context.Context, agentID uuid.UUID, instanceID string, now time.Time) error
	DeleteConnectionOwner(ctx context.Context, agentID uuid.UUID) error
}
