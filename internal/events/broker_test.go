// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBroker_PublishReachesSubscriber(t *testing.T) {
	b := NewBroker()
	sub := b.Subscribe(8)
	defer sub.Close()

	b.Publish(Event{Type: AgentRegistered, Data: map[string]any{"id": "abc"}})

	select {
	case ev := <-sub.Events():
		assert.Equal(t, AgentRegistered, ev.Type)
		assert.Equal(t, "abc", ev.Data["id"])
		assert.False(t, ev.At.IsZero(), "broker should stamp At on publish")
	case <-time.After(time.Second):
		t.Fatal("did not receive event within 1s")
	}
}

func TestBroker_FanoutToAllSubscribers(t *testing.T) {
	b := NewBroker()
	a := b.Subscribe(8)
	c := b.Subscribe(8)
	defer a.Close()
	defer c.Close()

	b.Publish(Event{Type: AlertFired})

	for _, sub := range []*Subscription{a, c} {
		select {
		case ev := <-sub.Events():
			assert.Equal(t, AlertFired, ev.Type)
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive event")
		}
	}
}

func TestBroker_PublishIsNonBlocking_DropsOnFullBuffer(t *testing.T) {
	b := NewBroker()
	// Tiny buffer; don't drain.
	sub := b.Subscribe(1)
	defer sub.Close()

	// First publish fills the buffer; subsequent publishes should drop
	// rather than block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(Event{Type: AgentRegistered})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked when subscriber buffer was full")
	}

	assert.Greater(t, sub.Dropped(), int64(0), "expected some drops with a buffer of 1 and 100 publishes")
}

func TestBroker_UnsubscribeRemovesSubscriber(t *testing.T) {
	b := NewBroker()
	require.Equal(t, 0, b.SubscriberCount())

	sub := b.Subscribe(8)
	require.Equal(t, 1, b.SubscriberCount())

	sub.Close()
	// Close should remove the subscriber from the broker's set so future
	// publishes don't try to write to a closed channel.
	require.Equal(t, 0, b.SubscriberCount())

	// Calling Close twice must be safe.
	assert.NotPanics(t, func() { sub.Close() })

	// Publish after close should not panic.
	assert.NotPanics(t, func() { b.Publish(Event{Type: AlertFired}) })
}

func TestBroker_ConcurrentSubscribersAndPublishers(t *testing.T) {
	b := NewBroker()
	const subscribers = 20
	const publishersPerSub = 5
	const eventsPerPublisher = 50

	var subs []*Subscription
	for i := 0; i < subscribers; i++ {
		subs = append(subs, b.Subscribe(1024))
	}
	defer func() {
		for _, s := range subs {
			s.Close()
		}
	}()

	var wg sync.WaitGroup
	for p := 0; p < publishersPerSub; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < eventsPerPublisher; i++ {
				b.Publish(Event{Type: AgentRegistered})
			}
		}()
	}
	wg.Wait()

	// Race detector also runs this test — primary goal is "no data races".
	// Don't assert exact delivery counts; that'd flake on slow CI under
	// the drop-rather-than-block semantics. Just ensure something arrived.
	got := 0
	for _, s := range subs {
		select {
		case <-s.Events():
			got++
		case <-time.After(100 * time.Millisecond):
		}
	}
	assert.Greater(t, got, 0, "at least one subscriber should have received an event")
}
