// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package siem is the public extension boundary between the open
// core's audit service and the Compliance Pack's SIEM fan-out
// implementation.
//
// Squadron OSS records every state change to a local audit log
// (audit_events table) and exposes it via /api/v1/audit/events.
// That covers operational visibility and short-term review. What
// the OSS build does NOT do is forward events to an external SIEM
// destination (Splunk HEC, signed webhook receiver) for long-term
// retention and centralized review. That fan-out is the Compliance
// Pack's responsibility because the typical use case (3 to 7 year
// retention for NERC CIP / SOC 2 / HIPAA evidence) is a regulated
// industry concern.
//
// The boundary is this package. The audit service holds a
// Dispatcher and calls Dispatch on every recording. The OSS binary
// wires NoOpDispatcher, which silently swallows events; local
// persistence still happens. The Compliance Pack binary wires a
// real dispatcher that signs events with HMAC-SHA256 and posts to
// every configured destination.
package siem

import "time"

// Event is the contract shape Dispatcher receives. Mirrors what
// the audit service constructs from an AuditEntry, without taking
// a build-time dependency on internal/ packages.
type Event struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`
	Actor      string         `json:"actor"`
	EventType  string         `json:"event_type"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id"`
	Action     string         `json:"action"`
	Payload    map[string]any `json:"payload,omitempty"`
}

// Dispatcher is the boundary for fan-out of audit events to
// external SIEM destinations. The audit service holds a
// Dispatcher and calls it after persisting each event locally.
//
// nil Dispatcher is a valid runtime state: the audit service
// treats it identically to NoOpDispatcher. Wiring a dispatcher is
// the operator's opt-in to external fan-out.
//
// Implementations must be safe for concurrent use; the audit
// service calls Dispatch from any goroutine that records an
// event. Implementations must NOT block on network I/O on the
// caller's goroutine — buffered, asynchronous fan-out is the
// only acceptable pattern. A slow SIEM endpoint cannot be allowed
// to backpressure operational state-change paths.
type Dispatcher interface {
	Dispatch(ev Event)
}

// NoOpDispatcher silently discards every event. It is the OSS
// default; the audit log still persists locally, but nothing
// leaves the box. Operators who need centralized retention or
// signed export run the Compliance Pack build.
//
// The zero value is usable. The audit service treats nil
// Dispatcher and NoOpDispatcher{} identically.
type NoOpDispatcher struct{}

// Dispatch implements Dispatcher and silently swallows the event.
// See package doc for why.
func (NoOpDispatcher) Dispatch(_ Event) {}
