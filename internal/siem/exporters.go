// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package siem

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Exporter ships a single Event to a SIEM destination. Implementations
// are responsible for protocol-specific framing (Splunk HEC envelope,
// HMAC-signed webhook body) and bounded I/O timeouts.
//
// Send returns nil on 2xx; any error (4xx, 5xx, timeout, DNS, etc.)
// triggers the dispatcher's retry logic.
type Exporter interface {
	Send(ctx context.Context, ev Event) error
}

// httpClient is what both exporters use. Extracted so tests can swap
// in a httptest server. 10s timeout is generous enough for slow SIEMs
// but tight enough that a stuck remote doesn't pile up backlog.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// SplunkHECExporter posts to Splunk HEC's /services/collector/event
// endpoint. Body is { "event": <payload>, "sourcetype": "squadron",
// "source": "squadron", "time": <epoch> }. Splunk indexes the
// "event" sub-object's fields automatically.
type SplunkHECExporter struct {
	URL   string
	Token string
}

// Send POSTs an event to Splunk HEC.
func (e *SplunkHECExporter) Send(ctx context.Context, ev Event) error {
	body := map[string]any{
		"time":       ev.Timestamp.Unix(),
		"source":     "squadron",
		"sourcetype": "squadron:audit",
		"event":      ev,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	// Splunk HEC's auth scheme: Authorization: Splunk <token>.
	// Distinct from Bearer; documented in Splunk's HEC reference.
	req.Header.Set("Authorization", "Splunk "+e.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Drain a chunk of the body so the operator can debug
		// — Splunk returns JSON with an "code" / "text" pair on
		// errors. Cap at 256 bytes so a misconfigured endpoint
		// returning a giant HTML page doesn't bloat our logs.
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("splunk hec %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

// WebhookExporter POSTs signed JSON to a generic HTTPS endpoint.
// The body is the Event as JSON; signature is HMAC-SHA256 of the
// body with the secret, hex-encoded, sent in X-Squadron-Signature.
//
// Receivers verify by recomputing HMAC over the raw body and
// comparing — this proves the payload came from Squadron with
// access to the secret, even over plain HTTP (though HTTPS is still
// strongly recommended).
type WebhookExporter struct {
	URL    string
	Secret []byte
}

// Send POSTs a signed event.
func (e *WebhookExporter) Send(ctx context.Context, ev Event) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	mac := hmac.New(sha256.New, e.Secret)
	mac.Write(raw)
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Squadron-Signature", "sha256="+sig)
	req.Header.Set("X-Squadron-Event-Type", ev.EventType)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("webhook %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

// BuildExporter returns the right Exporter for a destination given
// its decrypted secret. Pure function — no I/O. Returns nil + error
// for unknown types.
func BuildExporter(dest *Destination, secret []byte) (Exporter, error) {
	switch dest.Type {
	case SplunkHEC:
		return &SplunkHECExporter{URL: dest.URL, Token: string(secret)}, nil
	case GenericWebhook:
		return &WebhookExporter{URL: dest.URL, Secret: secret}, nil
	default:
		return nil, fmt.Errorf("unknown destination type %q", dest.Type)
	}
}
