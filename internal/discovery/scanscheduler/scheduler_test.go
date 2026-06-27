// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package scanscheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestRunOnce_ScansAllAccounts_PartialFailureNonFatal(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	s := &Scheduler{
		Logger: zap.NewNop(),
		ListAccounts: func(context.Context) ([]string, error) {
			return []string{"a1", "a2", "bad", "a3"}, nil
		},
		ScanAccount: func(_ context.Context, id string) error {
			mu.Lock()
			seen[id] = true
			mu.Unlock()
			if id == "bad" {
				return errors.New("boom")
			}
			return nil
		},
	}
	scanned, failed := s.RunOnce(context.Background())
	if scanned != 3 || failed != 1 {
		t.Fatalf("got scanned=%d failed=%d, want 3/1", scanned, failed)
	}
	for _, id := range []string{"a1", "a2", "bad", "a3"} {
		if !seen[id] {
			t.Errorf("account %s was not scanned (one failure aborted the sweep)", id)
		}
	}
}

func TestRunOnce_EmptyList_NoOp(t *testing.T) {
	called := false
	s := &Scheduler{
		Logger:       zap.NewNop(),
		ListAccounts: func(context.Context) ([]string, error) { return nil, nil },
		ScanAccount:  func(context.Context, string) error { called = true; return nil },
	}
	if scanned, failed := s.RunOnce(context.Background()); scanned != 0 || failed != 0 {
		t.Fatalf("got %d/%d, want 0/0", scanned, failed)
	}
	if called {
		t.Error("ScanAccount called on empty list")
	}
}

func TestRunOnce_ListError_NoScans(t *testing.T) {
	called := false
	s := &Scheduler{
		Logger:       zap.NewNop(),
		ListAccounts: func(context.Context) ([]string, error) { return nil, errors.New("store down") },
		ScanAccount:  func(context.Context, string) error { called = true; return nil },
	}
	if scanned, failed := s.RunOnce(context.Background()); scanned != 0 || failed != 0 {
		t.Fatalf("got %d/%d, want 0/0 on list error", scanned, failed)
	}
	if called {
		t.Error("ScanAccount called despite list error")
	}
}

func TestRunOnce_RespectsConcurrency(t *testing.T) {
	var inflight, maxInflight int32
	s := &Scheduler{
		Logger:      zap.NewNop(),
		Concurrency: 2,
		ListAccounts: func(context.Context) ([]string, error) {
			return []string{"1", "2", "3", "4", "5", "6"}, nil
		},
		ScanAccount: func(context.Context, string) error {
			n := atomic.AddInt32(&inflight, 1)
			for {
				m := atomic.LoadInt32(&maxInflight)
				if n <= m || atomic.CompareAndSwapInt32(&maxInflight, m, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt32(&inflight, -1)
			return nil
		},
	}
	s.RunOnce(context.Background())
	if maxInflight > 2 {
		t.Errorf("concurrency exceeded: max inflight = %d, want <= 2", maxInflight)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	s := &Scheduler{
		Logger:       zap.NewNop(),
		Interval:     time.Hour, // floored up, never fires in this test
		ListAccounts: func(context.Context) ([]string, error) { return nil, nil },
		ScanAccount:  func(context.Context, string) error { return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after context cancel")
	}
}

func TestEffectiveInterval_Floor(t *testing.T) {
	s := &Scheduler{Logger: zap.NewNop(), Interval: time.Minute}
	if got := s.effectiveInterval(); got != MinInterval {
		t.Errorf("got %v, want floor %v", got, MinInterval)
	}
	s2 := &Scheduler{Logger: zap.NewNop(), Interval: 6 * time.Hour}
	if got := s2.effectiveInterval(); got != 6*time.Hour {
		t.Errorf("got %v, want 6h unchanged", got)
	}
}
