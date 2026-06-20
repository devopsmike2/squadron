# AI features in Squadron

Squadron is an AI augmented platform engineering control plane. This
page is the operator reference for every AI surface in the open core
build: what each one does, what context the model receives, what is
redacted before the prompt leaves the box, and how to control the
behavior.

The AI service is opt in. Set `ai.enabled: true` plus an Anthropic
API key in `squadron.yaml` (or the `ANTHROPIC_API_KEY` env var) to
turn it on. When the key is absent the routes return 503 with a
clear "AI assist is not configured" message and the UI hides the
buttons; nothing is calling the model in the background.

## Models in use

- **Haiku** (`claude-haiku-4-5`) for short explanations, fleet query
  translation, and audit narratives. Fast and cheap.
- **Sonnet** (`claude-sonnet-4-6`) for structural reasoning over
  YAML configs (merge, remediate) and for proposal generation.

Both model strings are overridable in `squadron.yaml`. Token usage
is metered on every response so an operator can see what was spent
on what.

### Token budget per call

The service-wide `max_tokens` cap in `squadron.yaml` (default
`1024`) governs the upper bound on response length for the short
surfaces — explain, ask, merge — where the model is expected to
emit a couple of paragraphs and stop. The proposer is the
exception: it carries a per-call override (`ai.ProposerMaxTokens`)
because plan-kind responses can include a complete inline
OpenTelemetry Collector YAML per step (the v0.78
inline\_config\_snippet contract). At the 1024 default those
responses would truncate mid-config and the bridge would silently
drop the spike (this is what `#550` was). v0.82 set the override
to 4096 — enough for JARVIS 2-3 step plans. v0.88.2 bumped to 8192
because slice 3a's discovery proposer emits plan steps for FIVE
service categories (compute / functions / databases / object stores
/ load balancers), each carrying its own `inline_config_snippet`;
the 4096 ceiling truncated discovery responses against a 17-resource
live AWS account (#597). The trade-off:
the cap is a ceiling, not a per-call bill — the model only uses
what it needs, so raising the ceiling adds no cost on short
responses. A future release that wants to drop the cap should
convert `inline_config_snippet` from a complete YAML to a diff
encoding, which is a v0.78 contract pivot rather than a hotfix.

## Audit log explain (v0.57)

The audit log is the surface every Squadron operator stares at
during an incident. The explain feature lets the operator click any
row and get a one paragraph plain English narrative of what
actually happened: what was the action, who triggered it, and (for
events with structured context) which entity it touched.

### What the model receives

- The audit row itself: event id, event type, action, actor,
  target type, target id, timestamp, and the freeform payload.
- A small context bag the handler resolved before calling. For
  example a `rollout.advanced` row gets `rollout.name`,
  `rollout.state`, and `rollout.stage_index`; a `action.executed`
  row gets `action.type`, `action.phase`, `action.status`, and
  `action.runner_id`.
- The system prompt above. The prompt instructs the model to write
  two to four sentences, name entities by name, say if the action
  failed and why, and avoid hyphens.

### Redaction

Before the prompt leaves the server every field on the row gets
walked through a regex scrubber that replaces credentials and
internal references with placeholders. Categories scrubbed:

- Anthropic / OpenAI / GitHub / Linear / Slack / AWS API keys
- Bearer tokens in Authorization header shape
- JWTs (header.payload.signature shape)
- Hostnames ending in `.internal`, `.corp`, `.local`
- IPv4 addresses
- 16+ character hex strings (matches SHA fingerprints)

The model never sees the literal value; it sees a placeholder like
`<redacted:internal_hostname>` and is taught to treat that as the
ordinary noun "an internal host" in the narrative.

The response includes a `redaction_summary` field listing what
categories were scrubbed, so an operator who is curious about what
got pulled can see "github_token x1, internal_hostname x3" at the
bottom of the explanation block.

### Caching

The explanation is cached on the audit row itself in three
nullable columns: `ai_explanation`, `ai_explanation_model`, and
`ai_explanation_generated_at`. A second click on the same row
short circuits the LLM and serves the cache. Audit rows are
immutable, so the cached explanation never goes stale in the data
sense.

The operator can force a fresh angle by clicking Regenerate, which
calls the same endpoint with `?regenerate=1`. The new explanation
replaces whatever was cached.

### Endpoint

`POST /api/v1/audit/:id/explain`
Returns `{explanation, model, generated_at, cached, redaction_summary}`.
Scope required: `audit:read`. The cache mutation is treated as
part of the read because the operator can only mutate rows they
could already read.

### Cost spike rows (v0.59)

The audit explain handler resolves `target_type=cost_spike` rows to
the spike's severity, signal, percent above baseline, and dollar
numbers. Combined with the v0.59 `proposal.skipped` event type, an
operator can click any pre-LLM refusal in the audit log and get a
narrative that names the spike's specifics and the structured reason
the bridge skipped it.

### Cost notes

A typical audit row generates 200 to 400 input tokens and 50 to
100 output tokens against Haiku. Cached responses are free. A
fleet that runs a few thousand operator clicks per month should
expect single digit dollars of Anthropic spend from this feature.

## Other AI features

### Config explain and merge (v0.26)

- `ExplainSnippet`: plain English summary of a YAML processor or
  pipeline snippet.
- `MergeIntoConfig`: take an existing config plus a recommendation
  snippet, produce a merged YAML.
- `ExplainConfig`: summarize what each pipeline in a full collector
  config does.

### Fleet query translation (v0.44)

`TranslateFleetQuery` turns natural language ("show me agents in
web-prod with drift") into the structured filter params the
Agents API takes. Powers the Ask AI box on the Agents page.

### Lint remediation (v0.44)

`RemediateLintWarnings` reads a YAML config plus the configlint
findings against it, and produces a fixed YAML the operator can
preview, lint again, and roll out.

### Cost spike proposer (v0.52 Move 1)

`ProposeFromCostSpike` is the AI proposer. When a cost spike fires,
the proposer reads the spike context, the current config for the
group, and recent attribution, and emits a structured rollout
proposal. The proposal lands in the rollouts table with
`proposed_by=ai` and `require_approval=true`. A human approves
before the rollout engine touches a single agent.

### Cost spike proposer — plan output mode (v0.79)

The v0.58–v0.78 proposer emitted exactly one rollout per spike. v0.79
extends the structured output schema with a discriminated union:

```
{ "kind": "rollout" | "plan", ... }
```

The model picks one of two shapes per spike. The prompt provides a
decision framework:

- **`kind: "rollout"`** when a single config change is sufficient
  and a target config already exists in storage to reference by id.
- **`kind: "plan"`** when a single config change might not be
  sufficient and progressive changes with observation windows reduce
  regression risk (multi-attribute drops, sample rate ratchets,
  staged pipeline splits).

The plan branch produces N steps (2–4 in practice), each carrying an
`inline_config_snippet` instead of `target_config_id`. The v0.78
plan create path materializes each snippet as a new `Config` row in
storage before persisting the rollout. The proposer never has to
invent config IDs that don't exist yet.

Bridge dispatch happens at decode time in `internal/proposer/bridge.go`:
the bridge inspects `result.Kind` and routes through either
`services.RolloutService.Create` (rollout branch) or
`services.RolloutService.CreatePlan` (plan branch). Empty `Kind`
defaults to rollout for backwards compatibility with pre-v0.79 model
outputs.

The JARVIS payoff: when a plan lands, the operator approves once at
step 0, the engine sequences the cascade through step N, and if any
step fails the v0.72 backwards rollback walk undoes every succeeded
predecessor. One approval, full sequenced fix, automatic rollback.

What plans **don't** do (yet): every plan step is a rollout — a
config push. Plans cannot include action-runner calls (verify,
notify, page on-call). That's queued as a separate arc — adding
action-runner-as-plan-step requires engine logic to dispatch actions,
wait for completion before advancing, and handle action failures.
See [docs/multi-step-plans-design.md](./multi-step-plans-design.md)
for the v0.80+ roadmap.

### Incident drafter (v0.54 Move 3)

`DraftIncidentFromAction` reads a completed action request plus the
relevant audit chain and writes a postmortem style incident draft.
The draft lands in the incident drafter inbox at `/incidents`; the
operator edits, then publishes to clipboard, GitHub Issues, Linear,
or Jira (see `docs/publishers.md`).

## Demo and content angles

The audit explain feature is the one that screenshots well. Pick an
audit row that is not trivially obvious from the event name alone
(a rollout abort with abort criteria specifics, an action denied
for an out of capability reason, a config push that hit the
configlint rule for high cardinality), expand the row, click
Explain, and the AI writes a one paragraph narrative that names
the entity by name and explains the failure mode.

This is the "click any row, get a plain English story" demo
moment. Pairs well with a LinkedIn post about the audit log being
the surface every operator already stares at during an incident.
