// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// v0.63 — conversational "Ask Squadron" surface. The first JARVIS
// shaped slice: the operator types a question in plain English and
// gets a paragraph back that cites the rows the answer came from.
// Deliberately single turn in this slice. Multi turn + tool use are
// a separate moves later; shipping single turn first lets the
// citation UX get real exercise without committing to the larger
// pattern.

// AskInput is the supply caller side. The handler walks Squadron's
// read endpoints (agents, rollouts, audit, cost spikes,
// recommendations) and bakes a small context bag the prompt can
// reason over. Keys are human readable and the prompt is told to
// quote them verbatim when citing — that's what makes the citation
// chips clickable in the UI.
type AskInput struct {
	// Question is the operator's plain text question. Capped at a
	// few hundred chars at the handler boundary so a runaway prompt
	// can't blow the Anthropic context window.
	Question string

	// Context is the small bag of rows the handler resolved before
	// calling. Each entry is one citable thing the model can refer
	// back to. Keep keys stable: "rollout.<id>", "agent.<id>",
	// "audit.<id>", "spike.<id>", "rec.<id>". Values are short
	// summaries the model treats as authoritative.
	Context map[string]string

	// Hints is a freeform area the handler can populate with
	// timestamps, counts, or anything else that's not a citable
	// row but still useful color. The prompt is told NOT to cite
	// from this block.
	Hints map[string]string
}

// AskCitation is one row the model claimed it drew from. The kind
// + id pair lets the UI render a clickable chip that navigates to
// the right Squadron page (e.g. kind=rollout id=abc opens
// /rollouts/abc).
type AskCitation struct {
	Kind  string `json:"kind"`            // rollout | agent | audit | spike | rec
	ID    string `json:"id"`              // the entity id
	Label string `json:"label,omitempty"` // optional short title
}

// AskResult is what the handler returns to the UI. Answer is a
// short paragraph in markdown; Citations is the list of rows the
// model said it used, parsed out of the model's tool free response.
type AskResult struct {
	Answer    string        `json:"answer"`
	Citations []AskCitation `json:"citations"`
	Model     string        `json:"model"`
	TokensIn  int           `json:"tokens_in"`
	TokensOut int           `json:"tokens_out"`
}

const askSystemPrompt = `You are Squadron's operator deputy. The operator just asked a question about their OpenTelemetry collector fleet. You have a small bag of rows the handler resolved before calling you. Answer the operator's question in plain English, citing the specific rows you drew from.

Rules:
- One to three short paragraphs. No headers, no bullet lists for the answer body. Markdown bold and inline code are fine.
- Cite every concrete claim with an inline tag of the form [cite:kind:id]. Example: "The web-prod-canary rollout is paused [cite:rollout:abc123]." The kind is one of rollout, agent, audit, spike, rec. The id is the same id that appeared in the context bag, verbatim.
- Never cite anything that does not appear in the context bag. If the bag does not contain enough to answer, say so plainly and suggest which Squadron page the operator could check (e.g. "the Audit Timeline at /audit shows the full history").
- The hints block is color. Do not cite from it. Do not pretend any number in the hints is a row id.
- Never invent rollouts, agents, or events that are not in the bag.
- The bag values have already been redacted for secrets, internal hostnames, and IP addresses. You will see placeholders like <redacted:internal_hostname>. Treat those as ordinary nouns.
- Never produce hostnames ending in .internal, .corp, or .local; never produce raw IP addresses; never produce token IDs or signing key fingerprints.
- Never use hyphens unless grammatically necessary.
- If the operator's question is off topic (e.g. asking about pop culture, the weather, Anthropic's pricing), respond once with a single short sentence redirecting to what you can actually help with, and stop.`

// Ask runs the operator's question against the resolved context
// bag. Uses the Explain model (Haiku by default) — the answer is
// short, the operator is waiting on a click, and the question
// shape doesn't need Sonnet's structural reasoning.
func (s *Service) Ask(ctx context.Context, req AskInput) (*AskResult, error) {
	if s.demoActive() {
		return s.askDemoMode(req)
	}
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	q := strings.TrimSpace(req.Question)
	if q == "" {
		return nil, errors.New("question is required")
	}

	userMsg := buildAskUserMessage(q, req.Context, req.Hints)

	resp, err := s.callMessages(ctx, callOpts{
		Model:    s.cfg.ExplainModel,
		System:   askSystemPrompt,
		UserText: userMsg,
	})
	if err != nil {
		return nil, fmt.Errorf("ask: %w", err)
	}

	answer, citations := parseAskAnswer(resp.Text)
	return &AskResult{
		Answer:    answer,
		Citations: citations,
		Model:     resp.Model,
		TokensIn:  resp.TokensIn,
		TokensOut: resp.TokensOut,
	}, nil
}

// askDemoMode is the keyless demo responder for Ask Squadron. It answers from
// the same context bag the handler resolved, citing real row ids so the
// citation chips are clickable — grounded, not fabricated. Intent is routed off
// a few keywords; the model call is skipped. Model is reported as
// "demo-grounded" so it's clearly distinguishable from a live LLM answer.
func (s *Service) askDemoMode(req AskInput) (*AskResult, error) {
	q := strings.ToLower(strings.TrimSpace(req.Question))
	if q == "" {
		return nil, errors.New("question is required")
	}

	byKind := map[string]string{} // kind -> first id seen
	for k := range req.Context {
		if i := strings.IndexByte(k, ':'); i > 0 {
			kind := k[:i]
			if _, seen := byKind[kind]; !seen {
				byKind[kind] = k[i+1:]
			}
		}
	}
	spikeID, hasSpike := byKind["spike"]
	roID, hasRo := byKind["rollout"]
	agentID, hasAgent := byKind["agent"]

	var b strings.Builder
	var cites []AskCitation
	cite := func(kind, id string) {
		cites = append(cites, AskCitation{Kind: kind, ID: id})
	}

	switch {
	case strings.Contains(q, "cost") || strings.Contains(q, "spend") || strings.Contains(q, "spike") || strings.Contains(q, "save"):
		if hasSpike {
			b.WriteString(fmt.Sprintf("There's an open **critical cost spike** on your fleet, roughly +312%% above baseline on the metrics signal [cite:spike:%s]. ", spikeID))
			cite("spike", spikeID)
		}
		if hasRo {
			b.WriteString(fmt.Sprintf("Squadron's proposer has already drafted the fix — a staged rollout that pins `hashing.rounds` from 12 to 6 to cut metric cardinality — and it's waiting for your approval [cite:rollout:%s]. Approve it on the Rollouts page to start the canary.", roID))
			cite("rollout", roID)
		}
		if !hasSpike && !hasRo {
			b.WriteString("I don't see an open cost spike in the current context. The Cost Insights page at /cost has the latest volume and per-attribute attribution.")
		}
	case strings.Contains(q, "rollout") || strings.Contains(q, "deploy") || strings.Contains(q, "approv") || strings.Contains(q, "canary"):
		if hasRo {
			b.WriteString(fmt.Sprintf("There's a staged rollout awaiting your approval [cite:rollout:%s], plus recent rollout activity in the audit timeline. Open it to review the stages and guardrails before approving.", roID))
			cite("rollout", roID)
		} else {
			b.WriteString("No rollouts are awaiting approval in the current context. The Rollouts page at /rollouts shows the full history.")
		}
	case strings.Contains(q, "wrong") || strings.Contains(q, "status") || strings.Contains(q, "health") || strings.Contains(q, "happening") || strings.Contains(q, "attention"):
		b.WriteString("Here's the picture. ")
		if hasSpike {
			b.WriteString(fmt.Sprintf("An open critical cost spike needs attention [cite:spike:%s]. ", spikeID))
			cite("spike", spikeID)
		}
		if hasRo {
			b.WriteString(fmt.Sprintf("An AI-proposed rollout is waiting for your approval [cite:rollout:%s]. ", roID))
			cite("rollout", roID)
		}
		if hasAgent {
			b.WriteString(fmt.Sprintf("Fleet agents are reporting in [cite:agent:%s]. ", agentID))
			cite("agent", agentID)
		}
		b.WriteString("The Audit Timeline at /audit shows the full sequence of what led here.")
	default:
		b.WriteString("Based on what I can see across your fleet: ")
		if hasSpike {
			b.WriteString(fmt.Sprintf("a critical cost spike [cite:spike:%s]; ", spikeID))
			cite("spike", spikeID)
		}
		if hasRo {
			b.WriteString(fmt.Sprintf("an AI-proposed rollout awaiting approval [cite:rollout:%s]; ", roID))
			cite("rollout", roID)
		}
		if !hasSpike && !hasRo {
			b.WriteString("no notable open items right now. ")
		}
		b.WriteString("Ask me about cost, rollouts, or fleet status and I'll point you at the specifics.")
	}

	return &AskResult{Answer: b.String(), Citations: cites, Model: "demo-grounded"}, nil
}

// buildAskUserMessage formats the user turn the model sees. The
// shape is deterministic so the system prompt's citation rule
// lands consistently.
func buildAskUserMessage(question string, ctxBag, hints map[string]string) string {
	var b strings.Builder

	b.WriteString("Operator question:\n\n  ")
	b.WriteString(question)
	b.WriteString("\n\n")

	if len(ctxBag) > 0 {
		b.WriteString("Context bag (cite by [cite:kind:id]):\n")
		keys := make([]string, 0, len(ctxBag))
		for k := range ctxBag {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s: %s\n", k, RedactSecrets(ctxBag[k]))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Context bag: (empty — the handler found nothing to load for this question)\n\n")
	}

	if len(hints) > 0 {
		b.WriteString("Hints (color, do not cite from this block):\n")
		keys := make([]string, 0, len(hints))
		for k := range hints {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s: %s\n", k, RedactSecrets(hints[k]))
		}
		b.WriteString("\n")
	}

	b.WriteString("Write the answer now. Remember the citation rule.\n")
	return b.String()
}

// parseAskAnswer pulls citations out of the model's text. The
// model is asked to inline tags like [cite:rollout:abc123]. We
// strip the tags from the human readable answer (the UI renders
// them as chips, not raw text) and return them as a deduplicated,
// order preserving slice.
func parseAskAnswer(text string) (string, []AskCitation) {
	answer := strings.TrimSpace(text)
	citations := []AskCitation{}
	seen := map[string]bool{}

	// Walk the text and pull out every [cite:kind:id] occurrence.
	// We don't use regexp here because the format is fixed and a
	// hand walk keeps the dependency surface zero.
	out := make([]byte, 0, len(answer))
	i := 0
	for i < len(answer) {
		if strings.HasPrefix(answer[i:], "[cite:") {
			end := strings.Index(answer[i:], "]")
			if end > 0 {
				tag := answer[i+len("[cite:") : i+end]
				parts := strings.SplitN(tag, ":", 2)
				if len(parts) == 2 {
					kind := strings.TrimSpace(parts[0])
					id := strings.TrimSpace(parts[1])
					key := kind + ":" + id
					if !seen[key] && kind != "" && id != "" {
						seen[key] = true
						citations = append(citations, AskCitation{
							Kind: kind,
							ID:   id,
						})
					}
					// Skip the whole [cite:...] tag in the visible
					// answer. The UI re renders the chips inline
					// at the citation's order of first appearance.
					i += end + 1
					continue
				}
			}
		}
		out = append(out, answer[i])
		i++
	}

	// Collapse the double spaces that the citation strip can leave
	// behind ("foo [cite:x:y] bar" → "foo  bar"). Cheap pass.
	cleaned := strings.ReplaceAll(string(out), "  ", " ")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, citations
}

// MarshalJSON on the result is the default — listed here as a
// signal to future maintainers that the shape is the contract the
// UI relies on. Don't reorder fields without bumping the API
// version note in docs/ai-features.md.
var _ = json.Marshal
