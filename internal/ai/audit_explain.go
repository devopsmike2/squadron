// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ExplainAuditEventInput is the supply caller side. The Event fields
// mirror services.AuditEvent without taking a cross-package import in
// the AI package. The Context bag carries the small "what kind of
// thing is this" lookup the handler did before calling: e.g. for a
// rollout event the handler resolves the rollout's name + group + state
// and drops them under "rollout.*" keys; for an action_request event
// the handler drops "action.*" keys. The prompt does not know what
// keys to expect; it walks them all and incorporates whatever it
// finds. Keep keys human readable for that reason.
type ExplainAuditEventInput struct {
	EventID    string
	Timestamp  time.Time
	Actor      string
	EventType  string
	TargetType string
	TargetID   string
	Action     string
	Payload    map[string]any
	Context    map[string]string
}

// ExplainAuditEventResult is what the handler caches on the row.
type ExplainAuditEventResult struct {
	Explanation string `json:"explanation"`
	Model       string `json:"model"`
	TokensIn    int    `json:"tokens_in"`
	TokensOut   int    `json:"tokens_out"`
	// RedactionSummary names what categories of values were scrubbed
	// before the prompt went out. Empty when nothing matched any
	// redaction pattern.
	RedactionSummary string `json:"redaction_summary,omitempty"`
}

const auditExplainSystemPrompt = `You are a senior platform engineer writing a one-paragraph plain-English explanation of one audit log entry for a Squadron OpenTelemetry control plane.

Rules:
- Two to four sentences. Concrete, not generic. No marketing language.
- Start by stating what happened (not "this event is about..."). Example openers: "Squadron approved a rollout..." or "The action runner reported a failed restart..."
- If the event involved an actor other than "system", name the actor or describe their role (operator, agent, the runner daemon).
- If the context block names the entity (e.g. rollout name, group, agent hostname), use the name. Do not say "the rollout" when you can say "the web-prod-canary rollout".
- If the action failed, say what the failure was. If it succeeded, say so plainly.
- No headers, no bullet lists. One paragraph of normal prose. Markdown bold and inline code are fine and welcome where they help readability.
- Never invent fields that are not in the input. If something is unclear, say "the available context does not indicate ...".
- The input has already been redacted for secrets, internal hostnames, and IP addresses. You will see placeholders like <redacted:internal_hostname>. Treat those as ordinary nouns ("an internal host") and do not flag them as missing data.
- Never produce content that includes hostnames ending in .internal, .corp, or .local; never produce raw IP addresses; never produce token IDs or signing key fingerprints. If a value in the input looks like one of those, omit it from your explanation.
- Never use hyphens unless grammatically necessary.`

// ExplainAuditEvent generates a plain-English narrative of one audit
// log row. Uses the Explain model (Haiku by default) because the
// answer is short and the operator is waiting on a click.
func (s *Service) ExplainAuditEvent(ctx context.Context, req ExplainAuditEventInput) (*ExplainAuditEventResult, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(req.EventID) == "" {
		return nil, errors.New("event id is required")
	}
	if strings.TrimSpace(req.EventType) == "" {
		return nil, errors.New("event type is required")
	}

	userMsg, redactionSummary, err := buildAuditExplainUserMessage(req)
	if err != nil {
		return nil, err
	}

	resp, err := s.callMessages(ctx, callOpts{
		Model:    s.cfg.ExplainModel,
		System:   auditExplainSystemPrompt,
		UserText: userMsg,
	})
	if err != nil {
		return nil, fmt.Errorf("explain audit event: %w", err)
	}
	return &ExplainAuditEventResult{
		Explanation:      strings.TrimSpace(resp.Text),
		Model:            resp.Model,
		TokensIn:         resp.TokensIn,
		TokensOut:        resp.TokensOut,
		RedactionSummary: redactionSummary,
	}, nil
}

// buildAuditExplainUserMessage assembles the user-turn content the
// model receives. The shape is deterministic so the prompt's "no
// marketing, two to four sentences" instruction lands consistently
// across event types.
//
// Returns the formatted message plus a short summary string the
// caller can persist alongside the explanation describing what
// categories of values were redacted (empty when nothing matched).
func buildAuditExplainUserMessage(req ExplainAuditEventInput) (string, string, error) {
	var b strings.Builder

	b.WriteString("Audit log entry to explain:\n\n")
	fmt.Fprintf(&b, "  event id:     %s\n", req.EventID)
	fmt.Fprintf(&b, "  event type:   %s\n", req.EventType)
	fmt.Fprintf(&b, "  action:       %s\n", req.Action)
	fmt.Fprintf(&b, "  actor:        %s\n", RedactSecrets(req.Actor))
	fmt.Fprintf(&b, "  target type:  %s\n", req.TargetType)
	if req.TargetID != "" {
		fmt.Fprintf(&b, "  target id:    %s\n", req.TargetID)
	}
	if !req.Timestamp.IsZero() {
		fmt.Fprintf(&b, "  timestamp:    %s\n", req.Timestamp.UTC().Format(time.RFC3339))
	}

	// Redacted payload. We render the payload as JSON with HTML
	// escaping turned off so the redaction placeholders (which use
	// "<" and ">") survive into the prompt verbatim instead of being
	// escaped to < / >. We redact at the map level before
	// rendering, then run the string redactor again on the rendering
	// to catch any secret that emerged after JSON marshal.
	if len(req.Payload) > 0 {
		redactedPayload := RedactMap(req.Payload)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("  ", "  ")
		if err := enc.Encode(redactedPayload); err != nil {
			return "", "", fmt.Errorf("marshal payload: %w", err)
		}
		// Encoder.Encode appends a trailing newline; trim it so the
		// indented block sits flush with the surrounding text.
		scrubbed := RedactSecrets(strings.TrimRight(buf.String(), "\n"))
		b.WriteString("\n  payload:\n  ")
		b.WriteString(scrubbed)
		b.WriteString("\n")
	}

	// Context block, sorted for prompt stability.
	if len(req.Context) > 0 {
		keys := make([]string, 0, len(req.Context))
		for k := range req.Context {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("\nExtra context the handler resolved for you:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s: %s\n", k, RedactSecrets(req.Context[k]))
		}
	}

	b.WriteString("\nWrite the explanation now.\n")

	final := b.String()
	return final, SummarizeRedactionPlaceholders(final), nil
}
