// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !enterprise

// wire_detectors_oss.go is the default open-core wiring for the
// commercial-tier serverless regression detectors (AWS Lambda
// Insights, Azure Application Insights). Built when the `enterprise`
// build tag is NOT set.
//
// It returns the no-op provider, so the add-on-dependent detectors
// never activate: config.CommercialDetectors.Enabled becomes inert in
// the OSS build (the entitlement is the compiled-in edition, not the
// runtime flag). The detectors stay dormant exactly as they do today
// with the flag off, and OSS keeps surfacing the lambda-insights-enable
// / azfunc-appinsights-enable recommendations that advise enabling the
// paid add-on.
//
// The enterprise edition ships a parallel wire_detectors_enterprise.go
// (build tag: enterprise) that returns a real provider honouring the
// runtime switch. Both files expose the same commercialDetectorProvider
// symbol so main.go has a single call site. See docs/build.md.

package main

import "github.com/devopsmike2/squadron/extension/detectors"

// commercialDetectorProvider returns the edition's commercial-detector
// provider. The OSS build returns the no-op provider (detectors never
// activate). Mirrors wire_oss.go's no-op posture for the Compliance
// Pack extension points.
func commercialDetectorProvider() detectors.Provider {
	return detectors.NoOpProvider{}
}
