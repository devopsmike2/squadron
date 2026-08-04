// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// health_test.go — liveness/readiness split (GET /livez, GET /readyz).
// Pairs with health.go. Covers: /livez is dependency-free (200 even when
// the store is broken), /readyz is 200 on a healthy store Ping and 503
// when the Ping errors, /readyz is 503 when no store is wired, and — the
// regression that mattered in the OpenShift pilot — /readyz resolves its
// store LAZILY, so a store wired AFTER the handler is constructed is seen.

// fakePinger is a minimal StorePinger for the readiness tests.
type fakePinger struct {
	err   error
	calls int
}

func (f *fakePinger) Ping(_ context.Context) error {
	f.calls++
	return f.err
}

// provider is a tiny helper: a readyStore func that always returns s.
func provider(s StorePinger) func() StorePinger {
	return func() StorePinger { return s }
}

func doHealthRequest(h *HealthHandlers, method, path string, register func(*gin.Engine, *HealthHandlers)) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	register(r, h)
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleLive_AlwaysOK_NoStoreDependency(t *testing.T) {
	// Liveness must never touch a dependency: even with a store whose Ping
	// always errors, /livez returns 200 and does NOT call Ping.
	fp := &fakePinger{err: errors.New("store down")}
	h := NewHealthHandlers(nil, nil, provider(fp), nil)

	w := doHealthRequest(h, http.MethodGet, "/livez", func(r *gin.Engine, h *HealthHandlers) {
		r.GET("/livez", h.HandleLive)
	})

	if w.Code != http.StatusOK {
		t.Fatalf("livez: want 200, got %d (body %s)", w.Code, w.Body.String())
	}
	if fp.calls != 0 {
		t.Fatalf("livez must not touch the store, but Ping was called %d times", fp.calls)
	}
	if got := w.Body.String(); got != `{"status":"ok"}` {
		t.Fatalf("livez body: want {\"status\":\"ok\"}, got %s", got)
	}
}

func TestHandleReady_HealthyStore_200(t *testing.T) {
	fp := &fakePinger{err: nil}
	h := NewHealthHandlers(nil, nil, provider(fp), nil)

	w := doHealthRequest(h, http.MethodGet, "/readyz", func(r *gin.Engine, h *HealthHandlers) {
		r.GET("/readyz", h.HandleReady)
	})

	if w.Code != http.StatusOK {
		t.Fatalf("readyz(healthy): want 200, got %d (body %s)", w.Code, w.Body.String())
	}
	if fp.calls != 1 {
		t.Fatalf("readyz should Ping the store exactly once, got %d", fp.calls)
	}
}

func TestHandleReady_PingError_503(t *testing.T) {
	fp := &fakePinger{err: errors.New("select 1 failed")}
	h := NewHealthHandlers(nil, nil, provider(fp), nil)

	w := doHealthRequest(h, http.MethodGet, "/readyz", func(r *gin.Engine, h *HealthHandlers) {
		r.GET("/readyz", h.HandleReady)
	})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz(ping error): want 503, got %d (body %s)", w.Code, w.Body.String())
	}
}

func TestHandleReady_NoProviderWired_503(t *testing.T) {
	// nil provider: /readyz must report unready, not panic.
	h := NewHealthHandlers(nil, nil, nil, nil)

	w := doHealthRequest(h, http.MethodGet, "/readyz", func(r *gin.Engine, h *HealthHandlers) {
		r.GET("/readyz", h.HandleReady)
	})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz(no provider): want 503, got %d (body %s)", w.Code, w.Body.String())
	}
}

func TestHandleReady_ProviderReturnsNil_503(t *testing.T) {
	// Provider present but store still nil (e.g. probed before wiring):
	// /readyz must report unready, not panic.
	h := NewHealthHandlers(nil, nil, provider(nil), nil)

	w := doHealthRequest(h, http.MethodGet, "/readyz", func(r *gin.Engine, h *HealthHandlers) {
		r.GET("/readyz", h.HandleReady)
	})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz(provider->nil): want 503, got %d (body %s)", w.Code, w.Body.String())
	}
}

// TestHandleReady_LazyStore_WiredAfterConstruction reproduces the OpenShift
// pilot bug: registerRoutes builds the health handler while appStore is still
// nil, and the store is wired later by SetActionStoreAndSigner. A handler that
// captured the store eagerly would 503 forever. Because HandleReady resolves
// h.readyStore() per request, the store wired AFTER construction is seen and
// /readyz flips 503 -> 200. This test FAILS against the old eager-capture code.
func TestHandleReady_LazyStore_WiredAfterConstruction(t *testing.T) {
	// live models s.appStore: nil at construction, set post-construction.
	var live StorePinger
	h := NewHealthHandlers(nil, nil, func() StorePinger { return live }, nil)

	reg := func(r *gin.Engine, h *HealthHandlers) { r.GET("/readyz", h.HandleReady) }

	// Before wiring: unready.
	if w := doHealthRequest(h, http.MethodGet, "/readyz", reg); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before wiring: want 503, got %d (body %s)", w.Code, w.Body.String())
	}

	// Wire the store post-construction (as main.go does after NewServer).
	fp := &fakePinger{err: nil}
	live = fp

	// After wiring: ready, and the freshly wired store is the one pinged.
	if w := doHealthRequest(h, http.MethodGet, "/readyz", reg); w.Code != http.StatusOK {
		t.Fatalf("readyz after wiring: want 200, got %d (body %s)", w.Code, w.Body.String())
	}
	if fp.calls != 1 {
		t.Fatalf("readyz should ping the post-construction store exactly once, got %d", fp.calls)
	}
}
