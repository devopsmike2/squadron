// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package detectors defines the boundary between the open-core
// discovery scanner and the enterprise edition's commercial-tier
// serverless regression detectors.
//
// Squadron OSS ships the add-on-dependent regression detectors —
// AWS Lambda cold-start / error-rate via Lambda Insights (#152) and
// Azure Functions cold-start / error-rate via Application Insights
// (#153) — as plumbed-but-dormant code. In the open core they never
// run: an OSS scan surfaces the gap by recommending the operator
// enable the paid add-on (lambda-insights-enable /
// azfunc-appinsights-enable) instead of querying it.
//
// Historically the only thing standing between "dormant" and "live"
// was the runtime switch config.CommercialDetectors.Enabled, so a
// cloner could flip a bool and run the commercial-tier detectors. The
// entitlement boundary must be the build EDITION, not a config flag.
// This package is that boundary, mirroring extension/policy,
// extension/changewindow, and extension/siem.
//
// The boundary lives under extension/ (not internal/) so the private
// enterprise repo can import it across module boundaries. The OSS
// binary wires NoOpProvider, which never activates the detectors
// regardless of the runtime switch. The enterprise edition wires its
// own Provider, which honours config.CommercialDetectors.Enabled as a
// per-scan cost/safety toggle (each activated scan issues paid
// Lambda Insights / Application Insights API calls). See
// cmd/all-in-one/wire_detectors_oss.go (default) and
// wire_detectors_enterprise.go (build tag: enterprise), plus
// docs/build.md.
package detectors

// Provider is the boundary between open-core scan orchestration and
// the enterprise edition's commercial-tier detector activation. main.go
// consults it once, at wire time, to resolve whether the add-on-
// dependent detectors should run for this process.
//
// A nil Provider is a valid runtime state: callers treat it identically
// to NoOpProvider (detectors dormant). Wiring a real Provider is the
// enterprise edition's job.
type Provider interface {
	// CommercialDetectorsActive reports whether the add-on-dependent
	// serverless regression detectors should be activated for this
	// build.
	//
	// requested is the operator's runtime switch
	// (config.CommercialDetectors.Enabled) — a cost/safety toggle that
	// only takes effect inside the enterprise edition, where it gates
	// the per-scan Lambda Insights / Application Insights API cost. The
	// OSS NoOpProvider ignores requested and always returns false, so
	// the entitlement is the compiled-in edition, not the flag.
	CommercialDetectorsActive(requested bool) bool
}

// NoOpProvider is the OSS default. The commercial-tier detectors never
// activate: an OSS operator cannot turn them on by flipping
// config.CommercialDetectors.Enabled — the switch is inert outside the
// enterprise edition. The enterprise edition (private repo) supplies a
// Provider that honours the requested switch.
type NoOpProvider struct{}

// CommercialDetectorsActive always reports false: the OSS edition does
// not activate the add-on-dependent detectors regardless of the
// operator's runtime switch.
func (NoOpProvider) CommercialDetectorsActive(bool) bool { return false }

// Active is a nil-safe helper the wire layer uses to resolve activation
// from a (possibly nil) Provider. A nil Provider means "no enterprise
// provider wired" — dormant, the OSS posture.
func Active(p Provider, requested bool) bool {
	if p == nil {
		return false
	}
	return p.CommercialDetectorsActive(requested)
}
