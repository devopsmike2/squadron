// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sampleEvent() Event {
	return Event{
		Title:    "Silent agent: host-01",
		Summary:  "Hasn't reported for 12m.",
		Severity: SeverityWarning,
		Kind:     "silent_agent.firing",
		DedupKey: "agent-uuid-001",
		Action:   "trigger",
		Link:     "https://squadron.example/agents/agent-uuid-001",
		Fields: []Field{
			{Key: "Hostname", Value: "host-01"},
			{Key: "Last seen", Value: "Jun 14 22:01 UTC"},
		},
		At: time.Date(2026, 6, 14, 22, 13, 0, 0, time.UTC),
	}
}

// captureServer is a tiny test HTTP server that records the body and
// content-type of the inbound request. The Dispatcher writes to it
// and we assert on the saved values.
type captureServer struct {
	*httptest.Server
	body []byte
	ctype string
	path  string
	auth  string
}

func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cs.body = b
		cs.ctype = r.Header.Get("Content-Type")
		cs.path = r.URL.Path
		cs.auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func TestDispatch_SlackFormatsBlockKit(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDispatcher()
	dest := Destination{URL: cs.URL, Type: TypeSlack}
	if err := d.Dispatch(context.Background(), dest, sampleEvent()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(cs.body, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, cs.body)
	}
	// Slack format wraps in attachments[0].blocks with color.
	atts, ok := got["attachments"].([]any)
	if !ok || len(atts) == 0 {
		t.Fatalf("expected attachments, got: %v", got)
	}
	first := atts[0].(map[string]any)
	if first["color"] != "#eab308" {
		t.Errorf("warning color: got %v want #eab308", first["color"])
	}
	blocks, _ := first["blocks"].([]any)
	if len(blocks) == 0 {
		t.Fatalf("expected blocks, got 0")
	}
}

func TestDispatch_TeamsAdaptiveCard(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDispatcher()
	dest := Destination{URL: cs.URL, Type: TypeTeams}
	if err := d.Dispatch(context.Background(), dest, sampleEvent()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(cs.body, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, cs.body)
	}
	if got["@type"] != "MessageCard" {
		t.Errorf("@type: got %v want MessageCard", got["@type"])
	}
	if got["themeColor"] != "eab308" {
		t.Errorf("themeColor: got %v want eab308", got["themeColor"])
	}
}

// v0.62 — Discord uses incoming webhooks that accept an "embeds"
// array. The formatter emits one embed per event with severity
// tinted color, fields rendered as inline name/value pairs, and the
// Squadron link attached to the embed title.
func TestDispatch_DiscordFormatsEmbed(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDispatcher()
	dest := Destination{URL: cs.URL, Type: TypeDiscord}
	if err := d.Dispatch(context.Background(), dest, sampleEvent()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(cs.body, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, cs.body)
	}
	embeds, ok := got["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("expected one embed, got: %v", got)
	}
	embed := embeds[0].(map[string]any)
	if embed["title"] != "Silent agent: host-01" {
		t.Errorf("title: got %v", embed["title"])
	}
	// Warning severity → amber decimal color.
	if c, ok := embed["color"].(float64); !ok || c != 15375362 {
		t.Errorf("color: got %v want 15375362", embed["color"])
	}
	// Fields are present and tagged inline.
	fields, ok := embed["fields"].([]any)
	if !ok || len(fields) == 0 {
		t.Fatalf("expected fields on the embed, got: %v", embed)
	}
	first := fields[0].(map[string]any)
	if first["inline"] != true {
		t.Errorf("fields should be inline, got: %v", first)
	}
}

func TestDispatch_PagerDutyRequiresRoutingKey(t *testing.T) {
	d := NewDispatcher()
	// No routing_key in Extra → should error out before any HTTP call.
	err := d.Dispatch(context.Background(),
		Destination{URL: "ignored", Type: TypePagerDuty},
		sampleEvent())
	if err == nil || !strings.Contains(err.Error(), "routing_key") {
		t.Fatalf("expected routing_key error, got: %v", err)
	}
}

func TestDispatch_GenericPreservesRawJSON(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDispatcher()
	dest := Destination{URL: cs.URL, Type: TypeGeneric}
	if err := d.Dispatch(context.Background(), dest, sampleEvent()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Generic mode just JSON-marshals the Event.
	var got Event
	if err := json.Unmarshal(cs.body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Title != "Silent agent: host-01" {
		t.Errorf("title: got %q", got.Title)
	}
}

func TestDispatch_OpsgenieResolveHitsClosePath(t *testing.T) {
	// Override the production Opsgenie URL by pointing the
	// dispatcher's http.Client at a custom RoundTripper. Easier path:
	// just verify the URL composition through the formatFor branch
	// without making an HTTP call.
	d := NewDispatcher()
	ev := sampleEvent()
	ev.Action = "resolve"
	_, _, url, err := d.formatFor(
		Destination{Type: TypeOpsgenie, Extra: map[string]string{"api_key": "k"}},
		ev,
	)
	if err != nil {
		t.Fatalf("formatFor: %v", err)
	}
	if !strings.Contains(url, "/v2/alerts/"+ev.DedupKey+"/close") {
		t.Errorf("resolve URL: got %s", url)
	}
}

func TestDispatch_EmptyURLIsNoOp(t *testing.T) {
	// Dispatcher with no destination URL should swallow silently.
	d := NewDispatcher()
	if err := d.Dispatch(context.Background(),
		Destination{Type: TypeGeneric}, sampleEvent()); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}
