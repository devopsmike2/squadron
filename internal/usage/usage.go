// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package usage implements Squadron's OPT-IN, anonymized, aggregate usage
// reporting (a periodic "phone-home"). It is OFF unless an operator enables it
// (config.UsageReportingConfig / SQUADRON_USAGE_ENABLED) AND points it at an
// endpoint. When active it POSTs a Snapshot of low-cardinality COUNTS — Squadron
// version + edition and tallies such as agents/rollouts — on an interval.
//
// It carries NO identifiers: no tenant/host/account ids, no config or resource
// content, no IPs. This is distinct from internal/selftel (which exports THIS
// instance's own operational metrics to the operator's OWN OTLP backend); usage
// reporting is an aggregate product signal, deliberately minimal and privacy-
// preserving. Delivery is best-effort: a hard per-request timeout, and any
// failure is logged and dropped (never blocks or crashes the server).
package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Snapshot is the anonymized aggregate reported on each interval. Every field is
// a count or a low-cardinality label — reviewers should be able to confirm at a
// glance that nothing here identifies a tenant, host, account, or resource.
type Snapshot struct {
	Version  string `json:"squadron_version"`
	Edition  string `json:"edition"` // "squadron-oss" | "squadron-enterprise"
	Agents   int    `json:"agents"`
	Rollouts int    `json:"rollouts"`
}

// report is the wire envelope: the snapshot plus the moment it was taken.
type report struct {
	Snapshot
	ReportedAt string `json:"reported_at"` // RFC3339
	Schema     string `json:"schema"`      // payload schema version
}

const schemaVersion = "usage.v1"

// Collector produces a Snapshot on demand. It is injected (rather than the
// reporter reaching into the app store) so the reporter has no storage
// dependency and stays trivially unit-testable.
type Collector func(ctx context.Context) (Snapshot, error)

// Reporter periodically collects + POSTs an anonymized Snapshot.
type Reporter struct {
	endpoint string
	interval time.Duration
	collect  Collector
	client   *http.Client
	logger   *zap.Logger

	stop   chan struct{}
	stopMu sync.Mutex
	wg     sync.WaitGroup
}

// NewReporter builds a reporter. The caller decides whether reporting is active
// (config.UsageReportingConfig.Target()); this is only constructed when it is.
func NewReporter(endpoint string, interval time.Duration, collect Collector, logger *zap.Logger) *Reporter {
	if logger == nil {
		logger = zap.NewNop()
	}
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &Reporter{
		endpoint: endpoint,
		interval: interval,
		collect:  collect,
		// Hard timeout so a hung endpoint can never wedge the report goroutine
		// (mirrors internal/siem's best-effort exporter).
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
		stop:   make(chan struct{}),
	}
}

// Start launches the background report loop: one report shortly after start,
// then every interval. Safe to call once.
func (r *Reporter) Start() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// A small initial delay so a crash-looping instance doesn't hammer the
		// endpoint on every restart, and startup isn't slowed by the first POST.
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-r.stop:
			return
		case <-timer.C:
		}
		r.reportOnce()

		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				r.reportOnce()
			}
		}
	}()
	r.logger.Info("usage reporting enabled (anonymous aggregate)",
		zap.String("endpoint", r.endpoint), zap.Duration("interval", r.interval))
}

// Stop ends the report loop and waits for the goroutine to exit. Idempotent.
func (r *Reporter) Stop() {
	r.stopMu.Lock()
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	r.stopMu.Unlock()
	r.wg.Wait()
}

// reportOnce collects a snapshot and POSTs it. Best-effort: collection or send
// errors are logged and dropped.
func (r *Reporter) reportOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	snap, err := r.collect(ctx)
	if err != nil {
		r.logger.Debug("usage reporting: snapshot collection failed; skipping", zap.Error(err))
		return
	}
	body, err := BuildPayload(snap, time.Now().UTC())
	if err != nil {
		r.logger.Debug("usage reporting: payload build failed; skipping", zap.Error(err))
		return
	}
	if err := postJSON(ctx, r.client, r.endpoint, body); err != nil {
		r.logger.Debug("usage reporting: send failed; dropping", zap.Error(err))
	}
}

// BuildPayload marshals a Snapshot into the wire envelope (snapshot + RFC3339
// reported_at + schema tag). Exposed for tests + reviewers to inspect the exact
// bytes that leave the instance.
func BuildPayload(s Snapshot, at time.Time) ([]byte, error) {
	return json.Marshal(report{
		Snapshot:   s,
		ReportedAt: at.UTC().Format(time.RFC3339),
		Schema:     schemaVersion,
	})
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("usage endpoint returned %d", resp.StatusCode)
	}
	return nil
}
