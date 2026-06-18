# Incident drafter design

Move 3 of the engineer copilot roadmap. The action runner closes the
loop between "Squadron detected an issue" and "Squadron fixed it on
the node." The incident drafter closes the loop between Squadron
fixed it and the engineer has a written record they can hand to
leadership, link in a change calendar, or send to a customer.

The drafter is not a ticketing system. It writes the ticket; the
operator decides where it goes. That distinction matters because
every team uses something different (Jira, Linear, GitHub Issues,
ServiceNow, a markdown file in Confluence) and Squadron should not
build deep integrations with all of them.

## The pitch

An engineer who has Squadron deployed comes in Monday morning and
sees:

  Saturday 02:14  cost spike on web group, +312 percent on
                  hashing.rounds=12 from a new ML attribution
                  workload. Proposer drafted a rollout pinning
                  rounds=6 on the canary tier. You approved at
                  02:21. Runner restarted nginx after the config
                  push. Draft incident ticket attached.

  [open ticket draft]

They click. The drafter has already filled in:

  Title: Container restart and cost mitigation: web.staging,
         hashing.rounds tuning

  Summary: Squadron detected a 312 percent jump in OTLP
           ingest volume from the web group at 02:14 UTC
           Saturday. The attribution traced to a new ML
           feature that emitted spans tagged service.name=
           feature.embedding_score with a hashing.rounds=12
           computation in the receiver. The proposer drafted
           a rollout that pinned rounds=6 for the web group
           canary tier. The operator approved at 02:21.
           Squadron's action runner restarted nginx on the
           canary host after the config landed. Cost returned
           to baseline at 02:38.

  Timeline:
    02:14  cost spike detected
    02:14  AI proposer drafted candidate rollout
    02:16  Slack notification fired to oncall channel
    02:21  operator approved (token: ops-rotation)
    02:24  Squadron signed dispatch to runner
    02:24  runner verified signature, ran dry run
    02:25  runner executed restart-systemd-service for nginx
    02:38  ingest volume returned to baseline

  Resolution applied:
    receivers.otlp.protocols.hashing.rounds: 12 to 6

  Follow up:
    Confirm ML feature owner is aware of the change.
    Decide whether rounds=6 is the new permanent value or
    whether attribution should be moved to a separate
    pipeline that can afford rounds=12 without affecting
    the rest of the web group.

  Audit reference: rollout_id=rl_4b29..., action_id=ar_7c11...

  [copy to Linear]   [copy to Jira]   [copy to clipboard]
  [discard]

The engineer reads it, edits the follow up section, clicks copy to
Linear, and moves on with their morning. That experience is the
product.

## What gets drafted

A draft ticket is one row in `incident_drafts`. The fields are:

  id                  ulid
  action_request_id   foreign key to the action that triggered
                      the draft (nullable when the operator
                      drafts a ticket from an open rollout that
                      did not run an action)
  rollout_id          foreign key to the originating rollout
                      (nullable for ad hoc drafts)
  status              draft | published | dismissed
  title               short summary line
  body_markdown       the rendered ticket body
  draft_content_json  the structured response from the AI, kept
                      around so the UI can re render with edits
                      without losing the original
  provider            when published: linear | jira | github |
                      generic
  external_id         when published: the ticket ID at the
                      provider
  external_url        when published: the URL the user can click
  created_at, updated_at

The AI's structured response is the same shape every time:

```json
{
  "title": "...",
  "summary": "...",
  "timeline": [
    {"at": "2026-06-14T02:14:00Z", "text": "cost spike detected"}
  ],
  "resolution_applied": "...",
  "follow_ups": ["...", "..."],
  "audit_references": {"rollout_id": "...", "action_id": "..."}
}
```

The body markdown is rendered from the structured shape so the UI
can offer field by field editing later without reparsing prose.

## What goes in, what stays out

The drafter pulls from sources Squadron already has:

  the originating cost spike or alert that triggered the proposer
  the proposer's drafted rollout (RolloutInput, including the
    proposal_reasoning and evidence_refs from SQ-1.1)
  the audit events for that rollout (rollout.created,
    rollout.approved, rollout.stage_advanced, ...)
  the action request (parameters, phase, dispatched_at)
  the action result (status, started_at, completed_at, stdout)
  the audit events for that action (action.dispatched,
    action.executed)

These are all internal data and acceptable to include in a draft
the operator reviews before publishing. The drafter does not pull:

  raw OTLP payloads, which can contain customer data
  agent secrets or environment variables
  the unredacted content of any token or signing key
  IP addresses of upstream callers, which the drafter has no
  business correlating across customers

The threat model is "an operator at company A pastes the drafted
ticket into a public bug tracker." Everything in the structured
response should be safe under that assumption. The drafter prompt
explicitly tells the model not to include hostnames that look
internal (web-prod-1.internal.example.com) or IP addresses, and the
draft renderer strips them defensively in case the prompt is
ignored.

## The drafter service

`internal/incidents/drafter.go` exports a `Drafter` struct with one
public method:

```go
func (d *Drafter) DraftFromAction(
    ctx context.Context,
    in DraftInput,
) (*IncidentDraft, error)
```

`DraftInput` carries every reference the system already has by ID.
The drafter is responsible for fetching the supporting context from
the stores it was wired with: rollout service, audit service,
applicationstore (for the action). This keeps the API endpoint thin:
"draft the ticket for action ar_7c11" is the whole request shape.

Errors from the AI client are returned unchanged. The bridge that
calls the drafter (next chunk) treats an error as "we tried, drop
the attempt, do not retry on the next audit tick." This is the same
pattern the proposer bridge uses.

## The audit trigger

The drafter is wired to the audit stream in a small bridge
(`internal/incidents/bridge.go`, next chunk). When an
`action.executed` or `action.failed` event fires, the bridge:

  1. Loads the action request and result
  2. Calls Drafter.DraftFromAction
  3. Persists the IncidentDraft in status=draft

The operator sees the draft in the UI inbox and chooses what to do
with it. The bridge does not publish to any external system.

The bridge dedups on action_request_id so a flapping action does
not produce 40 drafts. The dedup is in memory and resets on restart;
that is fine since the persisted draft also has the action_request_id
and the bridge checks the store before drafting.

## Why not publish automatically

Two reasons. First, every team has a different tracker and a
different expectation of what should land there. Auto publishing
means committing to integrations Squadron has to maintain. Second,
a draft is a probability statement: "this is probably what
happened." The operator should read it before it becomes the
official record. Auto publish bypasses that step and lets an AI
mistake become a customer commitment.

The published path is a separate explicit click. When the operator
publishes through the provider plug in, the bridge updates the
IncidentDraft with the external_id and external_url.

## Provider plug in

`internal/incidents/provider.go` defines a small interface so the
next chunk can drop in providers without re working the drafter:

```go
type Provider interface {
    Name() string
    Publish(ctx context.Context, draft *IncidentDraft) (
        externalID string, externalURL string, err error,
    )
}
```

First provider is "clipboard" which just renders the markdown for
the operator to paste. Second is GitHub Issues (we already have the
GitHub provider plumbing from the deploy work). Linear, Jira,
ServiceNow follow when there is a customer asking.

## Open questions tracked separately

  Per group throttling so a noisy group does not flood the
  inbox. Initial implementation drafts one ticket per
  action.executed and one per action.failed.

  Editing history. The first version persists the original
  draft_content_json. If the operator edits the markdown, the
  edited body lives in body_markdown and the original stays in
  draft_content_json. A future revision can model multiple
  draft versions if a use case emerges.

  Multi action incidents. The first version assumes one action
  produces one draft. A composite "this whole rollout" ticket
  that summarizes every action is a future iteration.
