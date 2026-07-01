// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build enterprise

// wire_detectors_enterprise.go is the enterprise-edition wiring for
// the commercial-tier serverless regression detectors. Built when the
// `enterprise` build tag IS set.
//
// This stub exists in the open core only as a placeholder so the build
// tag is documented and so a developer who checks out the open repo and
// tries `go build -tags enterprise` without the private repo present
// gets a clear error. The actual provider — which honours the runtime
// switch config.CommercialDetectors.Enabled and re-points the detector
// queries at the Lambda Insights / Application Insights namespaces where
// the cold-start + error-rate signals live, wiring the observation
// stores the detection branch persists to — lives in the private
// enterprise repo and is dropped into this directory at build time.
//
// Build the full enterprise binary with:
//
//	make build-enterprise   # copies the real wire files, then builds -tags "enterprise compliance"
//
// See docs/build.md for the edition build model.

package main

import "github.com/devopsmike2/squadron/extension/detectors"

// commercialDetectorProvider is the enterprise-edition version. Symbol
// identical to the OSS file so main.go has a single call site. The real
// provider is in the private repo; this open-core stub panics so an
// enterprise build assembled without the private wire file fails loudly
// instead of silently falling back to OSS behaviour.
func commercialDetectorProvider() detectors.Provider {
	panic("squadron: built with -tags enterprise but the enterprise commercial-detector wire file was not installed; see docs/build.md")
}
