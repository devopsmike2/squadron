// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

// DriftMetrics tracks fleet-wide configuration drift state. These are the
// signals operators alert on: "more than 5% of my fleet is drifted",
// "drift_transitions_to_drifted is climbing", etc.
type DriftMetrics struct {
	// Fleet size gauge — total agents the control plane knows about. Useful as
	// the denominator for any percentage-of-fleet alert.
	FleetAgentsTotal Gauge `metric:"fleet_agents_total" tags:"component=drift" help:"Total number of agents currently tracked by the control plane"`

	// Per-status gauges. The Prometheus convention here is one gauge per status
	// rather than a single gauge with a status label, because the metrics
	// package binds tags to fields at registration time. Sum of all five
	// should equal FleetAgentsTotal.
	FleetSynced      Gauge `metric:"fleet_drift_status_synced"       tags:"component=drift,status=synced"       help:"Agents whose effective config matches the intended config"`
	FleetDrifted     Gauge `metric:"fleet_drift_status_drifted"      tags:"component=drift,status=drifted"      help:"Agents whose effective config differs from the intended config"`
	FleetNoIntent    Gauge `metric:"fleet_drift_status_no_intent"    tags:"component=drift,status=no_intent"    help:"Agents that have no intended config yet"`
	FleetNoEffective Gauge `metric:"fleet_drift_status_no_effective" tags:"component=drift,status=no_effective" help:"Agents that haven't reported an effective config yet"`
	FleetUnknown     Gauge `metric:"fleet_drift_status_unknown"      tags:"component=drift,status=unknown"      help:"Agents whose drift status couldn't be evaluated"`

	// Transition counters. These are what alerting cares about — a rising rate
	// of TransitionsToDrifted means something just broke.
	TransitionsToDrifted Counter `metric:"drift_transitions_to_drifted_total" tags:"component=drift,to=drifted" help:"Agent transitions into the drifted state"`
	TransitionsToSynced  Counter `metric:"drift_transitions_to_synced_total"  tags:"component=drift,to=synced"  help:"Agent transitions into the synced state (drift resolved)"`
}

// NewDriftMetrics creates and initializes drift metrics.
func NewDriftMetrics(factory Factory) *DriftMetrics {
	m := &DriftMetrics{}
	MustInit(m, factory, nil)
	return m
}
