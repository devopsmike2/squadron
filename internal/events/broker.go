// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package events provides a lightweight in-process pub-sub broker that
// fan-outs domain events (agent registered, alert fired, etc.) to any number
// of subscribers — primarily the SSE endpoint that pushes them to the UI.
//
// Design notes:
//   - Publish is non-blocking. If a subscriber's buffer is full, the event
//     is dropped for that subscriber and a counter is incremented. We prefer
//     dropping a real-time update over backing up a publisher.
//   - The broker has no persistence and no replay. Subscribers see only
//     events that happen after they Subscribe. A page refresh re-fetches
//     state via the REST APIs and then resumes listening.
//   - There is no auth on the broker itself; the SSE handler does its own
//     check before subscribing.
package events

import (
	"sync"
	"sync/atomic"
	"time"
)

// Type categorizes an event. Stable string so the UI can switch on it.
type Type string

const (
	AgentRegistered    Type = "agent_registered"
	AgentDriftChanged  Type = "agent_drift_changed"
	AgentStatusChanged Type = "agent_status_changed"
	AlertFired         Type = "alert_fired"
	AlertResolved      Type = "alert_resolved"
)

// Event is one published domain event. The Data shape is type-specific and
// is left as an arbitrary map so the broker doesn't have to evolve when
// publishers add new fields.
type Event struct {
	Type Type           `json:"type"`
	At   time.Time      `json:"at"`
	Data map[string]any `json:"data,omitempty"`
}

// Subscription holds a subscriber's channel and bookkeeping.
type Subscription struct {
	ch       chan Event
	dropped  atomic.Int64
	closed   atomic.Bool
	unsubFn  func()
	bufSize  int
}

// Events returns the receive-only channel. Callers should drain it
// promptly; slow subscribers will drop events rather than block publishers.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Dropped is the count of events the broker had to drop because this
// subscriber's buffer was full. Useful for diagnostics and for surfacing a
// "you missed N updates" hint in the UI on reconnect.
func (s *Subscription) Dropped() int64 { return s.dropped.Load() }

// Close releases the subscription. Safe to call more than once.
func (s *Subscription) Close() {
	if s.closed.Swap(true) {
		return
	}
	if s.unsubFn != nil {
		s.unsubFn()
	}
	close(s.ch)
}

// Broker fans events out to all live subscribers.
type Broker struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewBroker creates a new event broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[*Subscription]struct{})}
}

// Subscribe registers a new subscriber with the given per-subscriber buffer
// size. Returns the subscription; caller must Close() when done.
func (b *Broker) Subscribe(bufSize int) *Subscription {
	if bufSize <= 0 {
		bufSize = 64
	}
	sub := &Subscription{
		ch:      make(chan Event, bufSize),
		bufSize: bufSize,
	}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	sub.unsubFn = func() {
		b.mu.Lock()
		delete(b.subs, sub)
		b.mu.Unlock()
	}
	return sub
}

// Publish delivers an event to every current subscriber. Non-blocking: if a
// subscriber's buffer is full the event is dropped for that subscriber and
// their dropped counter is incremented. The publisher always returns
// immediately.
func (b *Broker) Publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for sub := range b.subs {
		select {
		case sub.ch <- e:
		default:
			sub.dropped.Add(1)
		}
	}
}

// SubscriberCount returns the current number of active subscribers. Useful
// for tests and for a debug metric.
func (b *Broker) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
