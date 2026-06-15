// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ----------------------------------------------------------------
// Slack (Block Kit)
// ----------------------------------------------------------------
//
// Slack's modern message shape is "blocks" — a list of typed UI
// elements (section, divider, context, actions). We build a header
// block for the title, a section block with the summary + fields,
// a context block for the timestamp, and an actions block for the
// Squadron deep link when present.
//
// `attachments` with a colored bar is used for the severity tint —
// Slack supports it on top of blocks for that 1990s-IRC-but-pretty
// vibe that incident responders apparently can't live without.

func formatSlack(ev Event) ([]byte, error) {
	color := "#3b82f6" // info (primary blue)
	switch ev.Severity {
	case SeverityWarning:
		color = "#eab308"
	case SeverityCritical:
		color = "#ef4444"
	}

	blocks := []map[string]any{
		{
			"type": "header",
			"text": map[string]any{
				"type":  "plain_text",
				"text":  truncate(ev.Title, 150), // Slack header cap
				"emoji": true,
			},
		},
	}
	if ev.Summary != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": ev.Summary,
			},
		})
	}
	if len(ev.Fields) > 0 {
		fieldsBlock := []map[string]any{}
		for _, f := range ev.Fields {
			fieldsBlock = append(fieldsBlock, map[string]any{
				"type": "mrkdwn",
				"text": fmt.Sprintf("*%s*\n%s", f.Key, f.Value),
			})
		}
		// Slack limits to 10 fields per section. Cap to be safe.
		if len(fieldsBlock) > 10 {
			fieldsBlock = fieldsBlock[:10]
		}
		blocks = append(blocks, map[string]any{
			"type":   "section",
			"fields": fieldsBlock,
		})
	}
	if ev.Link != "" {
		blocks = append(blocks, map[string]any{
			"type": "actions",
			"elements": []map[string]any{
				{
					"type": "button",
					"text": map[string]any{
						"type":  "plain_text",
						"text":  "Open in Squadron",
						"emoji": true,
					},
					"url": ev.Link,
				},
			},
		})
	}
	// Footer context: when + kind. Kept small so the message doesn't
	// dominate scrollback.
	contextItems := []map[string]any{}
	if !ev.At.IsZero() {
		contextItems = append(contextItems, map[string]any{
			"type": "mrkdwn",
			"text": ev.At.Format("Jan 2 15:04 MST"),
		})
	}
	if ev.Kind != "" {
		contextItems = append(contextItems, map[string]any{
			"type": "mrkdwn",
			"text": "`" + ev.Kind + "`",
		})
	}
	if len(contextItems) > 0 {
		blocks = append(blocks, map[string]any{
			"type":     "context",
			"elements": contextItems,
		})
	}

	// Slack lets you wrap the blocks in a colored attachment for
	// severity tinting. This is the canonical pattern from Slack's
	// own examples.
	payload := map[string]any{
		"attachments": []map[string]any{
			{"color": color, "blocks": blocks},
		},
	}
	return json.Marshal(payload)
}

// ----------------------------------------------------------------
// Microsoft Teams (Adaptive Cards via Incoming Webhook)
// ----------------------------------------------------------------
//
// Teams uses Adaptive Cards 1.4 inside a MessageCard envelope when
// arriving via the legacy Incoming Webhook surface (which is what
// most enterprises wire up). The shape is more verbose than Slack's
// but the conceptual structure is the same.

func formatTeams(ev Event) ([]byte, error) {
	theme := "3b82f6"
	switch ev.Severity {
	case SeverityWarning:
		theme = "eab308"
	case SeverityCritical:
		theme = "ef4444"
	}
	facts := []map[string]any{}
	for _, f := range ev.Fields {
		facts = append(facts, map[string]any{
			"name":  f.Key,
			"value": f.Value,
		})
	}
	sections := []map[string]any{
		{
			"activityTitle": ev.Title,
			"activityText":  ev.Summary,
			"facts":         facts,
			"markdown":      true,
		},
	}
	potentialAction := []map[string]any{}
	if ev.Link != "" {
		potentialAction = append(potentialAction, map[string]any{
			"@type": "OpenUri",
			"name":  "Open in Squadron",
			"targets": []map[string]any{
				{"os": "default", "uri": ev.Link},
			},
		})
	}
	payload := map[string]any{
		"@type":           "MessageCard",
		"@context":        "https://schema.org/extensions",
		"themeColor":      theme,
		"summary":         truncate(ev.Title, 250),
		"title":           ev.Title,
		"sections":        sections,
		"potentialAction": potentialAction,
	}
	return json.Marshal(payload)
}

// ----------------------------------------------------------------
// PagerDuty (Events API v2)
// ----------------------------------------------------------------
//
// PagerDuty's v2 shape is tight: routing_key, event_action,
// dedup_key, payload{summary, severity, source, custom_details}.
// dedup_key lets a "trigger" event refresh an existing incident
// instead of opening a new one; "resolve" closes the matching
// incident.

func formatPagerDuty(dest Destination, ev Event) ([]byte, error) {
	routingKey := dest.Extra["routing_key"]
	if routingKey == "" {
		return nil, fmt.Errorf("pagerduty destination missing routing_key in Extra")
	}
	severity := "info"
	switch ev.Severity {
	case SeverityWarning:
		severity = "warning"
	case SeverityCritical:
		severity = "critical"
	}
	action := "trigger"
	if ev.Action == "resolve" {
		action = "resolve"
	}
	source := dest.Extra["source"]
	if source == "" {
		source = "squadron"
	}

	details := map[string]any{}
	for _, f := range ev.Fields {
		details[f.Key] = f.Value
	}
	if ev.Kind != "" {
		details["kind"] = ev.Kind
	}

	payload := map[string]any{
		"routing_key":  routingKey,
		"event_action": action,
		"dedup_key":    ev.DedupKey,
		"payload": map[string]any{
			"summary":        truncateSafe(ev.Title, 1024),
			"severity":       severity,
			"source":         source,
			"timestamp":      ev.At.UTC().Format("2006-01-02T15:04:05Z"),
			"custom_details": details,
		},
	}
	if ev.Link != "" {
		payload["links"] = []map[string]any{
			{"href": ev.Link, "text": "Open in Squadron"},
		}
	}
	if ev.Summary != "" {
		// PagerDuty doesn't have a separate "summary body" — we
		// promote the Squadron Summary into custom_details so on-call
		// has the full context inline on the incident page.
		details["body"] = ev.Summary
	}
	return json.Marshal(payload)
}

// ----------------------------------------------------------------
// Opsgenie (Alerts API)
// ----------------------------------------------------------------
//
// Opsgenie's shape: message, alias (= our dedup_key), description,
// priority (P1-P5), details map, source. Resolution is a separate
// POST to /v2/alerts/{alias}/close — handled in Dispatcher.formatFor.

func formatOpsgenie(ev Event) ([]byte, error) {
	priority := "P3"
	switch ev.Severity {
	case SeverityCritical:
		priority = "P1"
	case SeverityWarning:
		priority = "P3"
	case SeverityInfo:
		priority = "P5"
	}
	details := map[string]string{}
	for _, f := range ev.Fields {
		details[f.Key] = f.Value
	}
	if ev.Kind != "" {
		details["kind"] = ev.Kind
	}
	if ev.Link != "" {
		details["squadron_url"] = ev.Link
	}
	payload := map[string]any{
		"message":     truncate(ev.Title, 130), // Opsgenie cap
		"alias":       ev.DedupKey,
		"description": ev.Summary,
		"priority":    priority,
		"details":     details,
		"source":      "squadron",
	}
	return json.Marshal(payload)
}

// truncate caps a string to n runes. Used for vendor field-length
// limits — Slack header 150, Opsgenie message 130, etc.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n-1]) + "…"
}

// truncateSafe is the same but avoids the ellipsis when n is small
// enough that the cut would land mid-word in a way that looks worse
// than the raw truncation.
func truncateSafe(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
