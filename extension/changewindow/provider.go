// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package changewindow

import (
	"context"
	"time"
)

// Provider is the boundary between the open-core rollout engine
// and the Compliance Pack's blackout enforcement. The engine
// consults a Provider on every advancement tick to ask "is the
// target group in an active blackout right now?" When the answer
// is non-nil, the engine refuses to advance the rollout, persists
// the blackout reason on the rollout record, and emits a
// rollout.blackout_blocked audit event (debounced per window).
//
// nil Provider is a valid runtime state: the engine treats it
// identically to NoOpProvider. Wiring a provider is the operator's
// opt-in to enforcement. The OSS binary wires NoOpProvider, which
// returns nil for every group, so groups can still carry windows
// as metadata but the engine never blocks. The Compliance Pack
// binary wires its own implementation backed by an
// ApplicationStore.
type Provider interface {
	// ActiveWindow returns the currently active blackout window for
	// the named group, or nil when no window is active. The
	// returned Window is read-only; the engine reads Name and the
	// other fields to populate audit and tracer payloads.
	//
	// now is supplied by the engine so providers and tests can be
	// deterministic without reading wall clock time inside the
	// provider.
	//
	// Implementations must be safe for concurrent use. The engine
	// calls this once per rollout per tick.
	ActiveWindow(ctx context.Context, groupID string, now time.Time) *Window
}

// NoOpProvider is the open-core default. ActiveWindow always
// returns nil, meaning the engine never blocks on a blackout.
// Groups can still carry windows as metadata (the OSS storage
// schema preserves them); the OSS engine just doesn't enforce
// them. Operators who need enforcement run the Compliance Pack
// build.
//
// The zero value is usable. Engines treat nil Provider and
// NoOpProvider{} identically.
type NoOpProvider struct{}

// ActiveWindow implements Provider and always returns nil. See
// package doc for why.
func (NoOpProvider) ActiveWindow(_ context.Context, _ string, _ time.Time) *Window {
	return nil
}
