// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package traceindex maintains an in-process, write-through cache
// of which resources have recently emitted spans to Squadron's OTLP
// receiver. The receiver's hot path notifies the index of each
// ResourceSpan via Observe; a background flush job persists the
// accumulated rows to the application store every 30 seconds.
//
// Slice 1 ships VISIBILITY only — operators can SEE which of their
// primitive-enabled resources have recently emitted, but slice 1
// does not yet draft remediation. Slice 2 introduces the
// trace-emission-* recommendation kinds that consume this index.
//
// The §3 architectural decision in the slice-1 design doc names six
// fallback tiers the receiver uses to derive resource_key from the
// span's resource attributes:
//
//  1. cloud.resource_id (strong)
//  2. host.id + cloud.account.id (strong)
//  3. k8s.cluster.name + cloud.account.id (strong)
//  4. db.system + db.name + cloud.account.id (strong)
//  5. host.name alone (weak)
//  6. service.name alone (weak)
//
// Spans with NO usable identifier are dropped silently — slice 2 may
// surface them as an "orphan trace volume" indicator (design doc §13
// Q4) but slice 1 prefers to keep the index lean.
//
// See docs/proposals/trace-integration-slice1.md.
package traceindex
