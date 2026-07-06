// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestBuildPayload_ShapeAndAnonymity(t *testing.T) {
	at := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	body, err := BuildPayload(Snapshot{
		Version:  "0.1.0",
		Edition:  "squadron-oss",
		Agents:   42,
		Rollouts: 3,
	}, at)
	if err != nil {
		t.Fatal(err)
	}

	// The payload must be exactly these keys — a guard so an identifier can't be
	// added to Snapshot without this test failing.
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"squadron_version": true, "edition": true, "agents": true,
		"rollouts": true, "reported_at": true, "schema": true,
	}
	for k := range m {
		if !want[k] {
			t.Fatalf("unexpected key in usage payload: %q (only anonymized aggregates allowed)", k)
		}
	}
	for k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing expected key: %q", k)
		}
	}
	if m["squadron_version"] != "0.1.0" || m["edition"] != "squadron-oss" {
		t.Fatalf("wrong values: %v", m)
	}
	if m["reported_at"] != "2026-07-06T12:00:00Z" || m["schema"] != schemaVersion {
		t.Fatalf("wrong envelope: %v", m)
	}
}

func TestReporter_ReportOnce_PostsSnapshot(t *testing.T) {
	var (
		mu   sync.Mutex
		got  []byte
		hits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits++
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		got = buf
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	collect := func(_ context.Context) (Snapshot, error) {
		return Snapshot{Version: "0.1.0", Edition: "squadron-oss", Agents: 7, Rollouts: 1}, nil
	}
	r := NewReporter(srv.URL, time.Hour, collect, zap.NewNop())
	r.reportOnce()

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("expected 1 POST, got %d", hits)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("posted body not json: %v (%s)", err, got)
	}
	if m["agents"] != float64(7) {
		t.Fatalf("agents count not delivered: %v", m["agents"])
	}
}

func TestReporter_ReportOnce_DropsOnUnreachableEndpoint(t *testing.T) {
	collect := func(_ context.Context) (Snapshot, error) {
		return Snapshot{Version: "x"}, nil
	}
	// Nothing listening here → send fails; reportOnce must return without panic.
	r := NewReporter("http://127.0.0.1:0/nope", time.Hour, collect, zap.NewNop())
	r.reportOnce()
}

func TestReporter_ReportOnce_SkipsOnCollectorError(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	collect := func(_ context.Context) (Snapshot, error) {
		return Snapshot{}, context.DeadlineExceeded
	}
	r := NewReporter(srv.URL, time.Hour, collect, zap.NewNop())
	r.reportOnce()
	if hits != 0 {
		t.Fatalf("must not POST when collection fails; got %d hits", hits)
	}
}

func TestReporter_StartStop_ReportsThenStops(t *testing.T) {
	// This exercises Start/Stop lifecycle; the 30s initial delay means no POST
	// fires during the test — we only assert clean start + stop with no panic
	// and prompt shutdown.
	r := NewReporter("http://127.0.0.1:0", time.Hour, func(_ context.Context) (Snapshot, error) {
		return Snapshot{}, nil
	}, zap.NewNop())
	r.Start()
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return promptly")
	}
}
