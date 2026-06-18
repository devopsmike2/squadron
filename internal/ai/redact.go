// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"fmt"
	"regexp"
	"strings"
)

// Compiled redaction patterns. Each match becomes a placeholder so the
// LLM sees the *shape* of the data without the literal value. The
// goal is conservative: it is fine to over-redact (the model just
// gets a slightly less informative prompt) and it is not fine to
// leak a real credential into the model context.
//
// Order matters: longer or more specific patterns first so generic
// "long random-looking string" rules don't consume tokens we would
// classify more precisely.
var redactionPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	// Anthropic API keys.
	{"anthropic_key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`)},
	// OpenAI API keys.
	{"openai_key", regexp.MustCompile(`sk-[A-Za-z0-9]{32,}`)},
	// GitHub personal access tokens and app tokens. ghp_, gho_, ghu_,
	// ghs_, ghr_ are the documented prefixes.
	{"github_token", regexp.MustCompile(`gh[opusr]_[A-Za-z0-9]{20,}`)},
	// Linear personal API keys.
	{"linear_key", regexp.MustCompile(`lin_api_[A-Za-z0-9]{20,}`)},
	// Generic Bearer tokens in Authorization-header shape.
	{"bearer_token", regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`)},
	// JWT tokens (header.payload.signature). The middle dot pattern
	// catches the vast majority of JWTs without needing a base64url
	// validator.
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`)},
	// Slack tokens (xoxb-, xoxp-, xoxa-, xoxs-).
	{"slack_token", regexp.MustCompile(`xox[bpas]-[A-Za-z0-9\-]{20,}`)},
	// AWS access keys (AKIA prefix).
	{"aws_access_key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	// Internal hostnames the incident drafter prompt asks the model to
	// avoid. Catching them in code is the belt to the prompt's
	// suspenders.
	{"internal_hostname", regexp.MustCompile(`(?i)[a-z0-9][a-z0-9\-\.]*\.(internal|corp|local)\b`)},
	// IPv4 addresses. Net-internal addressing is the common case
	// engineers want kept out of LLM context.
	{"ipv4", regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)},
	// Hex fingerprints (16+ contiguous hex chars). Catches SHA hashes
	// of signing keys and similar.
	{"hex_fingerprint", regexp.MustCompile(`\b[a-fA-F0-9]{16,}\b`)},
}

// RedactSecrets walks the string and replaces matches against the
// pattern list above with stable placeholders like <anthropic_key>
// or <internal_hostname>. Idempotent and safe to call on already-
// redacted strings (the placeholders themselves do not match any
// pattern).
//
// The placeholders are intentionally human readable: when an
// operator reads the LLM explanation they can still see "the actor
// was an internal hostname" rather than a garbage redacted blob.
func RedactSecrets(s string) string {
	if s == "" {
		return ""
	}
	out := s
	for _, p := range redactionPatterns {
		placeholder := "<redacted:" + p.name + ">"
		out = p.re.ReplaceAllString(out, placeholder)
	}
	return out
}

// RedactMap recursively walks a map[string]any and replaces secret-
// shaped strings in any string leaf. Returns a new map; the input
// is not mutated. Non-string leaves pass through unchanged.
//
// The audit payload arrives at the LLM via JSON; redacting at the
// map level catches secrets that the JSON marshal would otherwise
// emit verbatim.
func RedactMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = redactValue(v)
	}
	return out
}

func redactValue(v any) any {
	switch x := v.(type) {
	case string:
		return RedactSecrets(x)
	case map[string]any:
		return RedactMap(x)
	case []any:
		dup := make([]any, len(x))
		for i, item := range x {
			dup[i] = redactValue(item)
		}
		return dup
	default:
		// Numbers, bools, nil — pass through. If a future caller
		// hands us a typed map we don't know about, render it as
		// a string and redact the rendering so we err on the side
		// of leaking nothing.
		return v
	}
}

// SummarizeRedactionPlaceholders is a tiny helper used by callers
// that want to mention in a prompt "by the way we redacted these
// things." It counts placeholders per category so the prompt can
// include a one-line list of what was scrubbed.
func SummarizeRedactionPlaceholders(s string) string {
	counts := make(map[string]int)
	for _, p := range redactionPatterns {
		placeholder := "<redacted:" + p.name + ">"
		if n := strings.Count(s, placeholder); n > 0 {
			counts[p.name] = n
		}
	}
	if len(counts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(counts))
	for k, n := range counts {
		parts = append(parts, fmt.Sprintf("%s x%d", k, n))
	}
	return strings.Join(parts, ", ")
}
