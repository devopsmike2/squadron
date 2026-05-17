# AI Assist

Squadron's v0.26+ AI assist is a thin wrapper around the Anthropic
Messages API. It powers two operator-facing affordances:

- **Explain** on any v0.25 cost recommendation — translates the
  generated YAML snippet into 2-3 sentences of plain English so
  the operator understands what the fix actually does.
- **AI Assist** in the config editor — either summarizes the
  current YAML pipeline-by-pipeline, or merges a snippet (e.g. a
  processor block from a recommendation) into the editor's
  contents for review.

Every call is **user-initiated**. There's no proactive LLM use,
no background analysis, no auto-rollout. The model is one tool in
a chain that already has lint + diff preview + staged rollout +
abort criteria as safety primitives.

## Configuration

AI is **off by default**. To enable, set both the master switch
and an API key:

```yaml
# squadron.yaml
ai:
  enabled: true
  # api_key: "sk-ant-..."          # not recommended; prefer the env var
  # api_key_env: ANTHROPIC_API_KEY # defaults to ANTHROPIC_API_KEY
  # base_url: https://api.anthropic.com
  # explain_model: claude-haiku-4-5-20251001
  # merge_model:   claude-sonnet-4-6
  # max_tokens:    1024
```

Plus the env var:

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
docker compose restart squadron   # or restart however you run squadron
```

At startup the all-in-one binary logs whether AI is wired:

```
{"level":"info","msg":"AI assist enabled","explain_model":"claude-haiku-4-5-20251001","merge_model":"claude-sonnet-4-6"}
```

or, if not configured:

```
{"level":"info","msg":"AI assist not configured (set ANTHROPIC_API_KEY + ai.enabled=true to enable)"}
```

The UI probes `GET /api/v1/ai/status` on app load and hides every
AI affordance when `enabled=false`. There's never a button that
appears and immediately fails.

## What gets sent to Anthropic

Squadron only sends data when an operator explicitly clicks an AI
action. Per action:

| Action | Payload |
|---|---|
| Explain (on a recommendation) | The YAML snippet from the recommendation + the recommendation title + the signal name |
| Explain this config | The entire YAML currently in the editor |
| Merge in a snippet | The base YAML currently in the editor + the snippet the operator typed + optional goal text |

The system prompts are versioned with the Squadron binary; you
can read them in `internal/ai/ai.go`. They don't ask the model to
take any action — every response is text that flows back to the UI
for the operator to read or paste into the editor.

**What's NOT sent:** API tokens, agent labels, telemetry data,
audit log, or anything outside the explicit per-action payload.

## Model defaults

- `explain_model` = **claude-haiku-4-5-20251001** — cheap, fast,
  fine for 2-3 sentence summaries. Used by the recommendation
  Explain button and the config editor's "Explain this config"
  action.
- `merge_model` = **claude-sonnet-4-6** — stronger reasoning,
  used for the "Merge in a snippet" action where the output has
  to be syntactically and structurally correct YAML.

Override either via `ai.explain_model` / `ai.merge_model` in
`squadron.yaml`. Squadron sends whatever you set as the `model`
field on the Messages API request, so any Anthropic model name
your account can use will work.

## Safety harness

The AI is one step in a longer chain. For a merge:

1. Operator clicks "Merge in a snippet" in the config editor.
2. Sonnet produces the merged YAML + a 1-sentence summary.
3. The merged YAML replaces the editor contents.
4. **Squadron Lint runs immediately** — flags structural issues
   (missing exporters, undefined components, references to
   processors that aren't defined, etc.).
5. The operator reviews the diff in the side-by-side editor,
   fixes anything the lint flagged, and saves the new config.
6. Rollout goes through the existing staged rollout flow with
   abort criteria — bad merges that pass lint get caught by
   drop-rate or error-log thresholds during the canary stage.

The LLM is never trusted as the final authority. If a step in
this chain breaks, the rollout aborts and rolls back to the
previous config.

## Cost shape

Anthropic bills per input + output token. For the v0.26 surfaces:

- **Explain (recommendation)**: typically ~300 tokens in, ~80
  tokens out. ≈ $0.0002 per call on Haiku at current pricing.
- **Explain config**: ~500-1500 tokens in (varies with config
  size), ~150 tokens out. ≈ $0.0005 per call on Haiku.
- **Merge snippet**: ~600-2000 tokens in, ~600-2000 tokens out
  (the merged YAML is the bulk). ≈ $0.01 per call on Sonnet.

Costs are bounded per-call by the `ai.max_tokens` config (default
1024 output tokens). There's no per-minute rate limit in Squadron
itself — Anthropic's own rate limits apply.

## Endpoints (for tooling)

All routes behind `ScopeAgentsRead` (the AI surface is read-only
assistive; no mutations). The `/status` route always returns 200
even when AI is disabled, so a capability probe is a single
round-trip.

```
GET  /api/v1/ai/status          → { enabled, explain_model, merge_model }
POST /api/v1/ai/explain         → { explanation, model, tokens_in, tokens_out }
POST /api/v1/ai/merge           → { merged_yaml, summary, model, tokens_in, tokens_out }
POST /api/v1/ai/explain-config  → { summary, pipelines, model, tokens_in, tokens_out }
```

When AI is disabled, the mutation routes return 503 with
`{"enabled": false, "error": "..."}`. The UI checks `/status`
once at app load and hides the affordances accordingly.

## What's NOT in v0.26.0

- Settings UI for the API key (env var only; settings page is
  v0.26.x).
- Streaming responses — every call blocks until the full response
  is back.
- Multi-turn chat — every call is stateless.
- Per-tenant cost dashboards — operators see token counts per
  response but there's no aggregate view yet.
- Automatic snippet generation (the recommendation engine
  generates the snippets; the LLM only explains/merges them).
- Redaction of secrets in configs sent to the model — operators
  should ensure their configs don't contain secrets they're not
  comfortable sending to Anthropic. Use env-var references in
  your collector configs rather than literal credentials.

Tracked for v0.26.x / v0.27.
