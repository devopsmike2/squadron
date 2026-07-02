// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

// RolloutMetrics tracks the rollout engine's background tick loop.
// Added for the v0.89 scale pass: the engine does per-tick work that
// grows with fleet size and active-rollout count (every in-progress
// rollout lists the whole fleet each tick, and a stage advance pushes
// config to every canary agent synchronously inside the tick), and
// before this there was NO signal for how long a tick actually takes
// against its 5s budget.
type RolloutMetrics struct {
	// TickDuration observes the wall time of every engine tick.
	TickDuration Timer `metric:"rollout_engine_tick_duration_seconds" tags:"component=rollouts" help:"Wall-clock duration of each rollout engine tick"`

	// TicksTotal counts engine ticks — the denominator for slow-tick rate.
	TicksTotal Counter `metric:"rollout_engine_ticks_total" tags:"component=rollouts" help:"Total rollout engine ticks executed"`

	// SlowTicks counts ticks that exceeded the tick interval — the
	// engine is falling behind and stage timing (dwell, abort checks)
	// degrades from "every 5s" toward "whenever the previous tick
	// finishes".
	SlowTicks Counter `metric:"rollout_engine_slow_ticks_total" tags:"component=rollouts" help:"Ticks that took longer than the tick interval"`
}

// NewRolloutMetrics creates and initializes rollout engine metrics.
func NewRolloutMetrics(factory Factory) *RolloutMetrics {
	m := &RolloutMetrics{}
	MustInit(m, factory, nil)
	return m
}
