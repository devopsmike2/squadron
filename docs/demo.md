# Demo scenario

This walkthrough seeds a fresh Squadron install with a realistic
engineer copilot scenario, then shows the full cost spike → AI
proposal → approval → action → incident ticket loop in roughly
ninety seconds.

The demo runs entirely against synthetic data. No real OTel fleet
is required, no external service is contacted (other than the
Anthropic API for the proposer and drafter).

## Prerequisites

  Go 1.25 plus Node 20 to build the OSS binary.

  An Anthropic API key with credit. Set it before starting
  Squadron:

      export ANTHROPIC_API_KEY=sk-ant-...

  No production deploy is involved. The action runner half of the
  loop is wired but stays inert because no live runner is
  registered against the demo group. See docs/action-runner-design.md
  if you want to run the full action runner against a VM with
  nginx for the in person demo.

## Steps

  1. Build and start Squadron in one terminal:

         make build
         ./bin/squadron --config config/squadron.yaml

     The startup log should show "AI assist enabled" and
     "AI proposer bridge started ai_enabled=true". The incidents
     drafter bridge also starts. Both are no ops when AI is
     disabled.

  2. In a second terminal, seed the demo:

         make demo-seed

     This drops one demo group, one baseline collector config, one
     synthetic agent, and one cost spike event into the
     application store. The proposer ticks every thirty seconds;
     within a minute a new rollout appears.

  3. Open the UI at http://localhost:8080.

  4. Navigate to Cost Insights or Savings. The seeded cost spike
     shows under "Recent spikes". The attribution names the demo
     agent and lists container.id and k8s.pod.uid as top
     attributes.

  5. Navigate to Rollouts. Within a minute you should see one
     pending_approval entry titled something like "AI: pin
     hashing.rounds=6 for demo-web-prod canary". Click in.

     The detail drawer shows the AI's reasoning, the diff against
     the baseline config, and the evidence refs the model cited.

  6. Click Approve. The rollout engine advances; the diff lands
     in the demo group's effective config.

  7. (If you have a live action runner registered against the
     demo group and a nginx unit it can touch) the runner picks
     up the dispatch, restarts the service, and posts a result.

  8. Within a minute the Incidents page shows a new draft titled
     something like "Restart and verify nginx on demo web canary,
     success". Click in. The body covers what happened, the
     timeline, the resolution applied, and follow ups.

     Edit if you want. Copy the markdown. Click Publish with
     provider clipboard for the demo, or pick Linear / Jira /
     GitHub if you intend to publish for real later.

  9. Open Audit. Every step above lands in the audit timeline as
     a separate event: cost_spike.detected, proposal.created,
     rollout.approved, rollout.stage_advanced, action.dispatched,
     action.executed, incident.drafted, incident.published. With
     a SIEM destination configured under Settings these all fan
     out automatically.

## Resetting

The seed is idempotent. Running make demo-seed twice will skip
the group and config if they already exist. To inject a fresh
cost spike for re demoing:

    ./bin/squadron-demo-seed --db ./data/squadron.db --force

To wipe the demo state entirely, stop Squadron and delete
./data/squadron.db (or whatever path your config uses). The next
start initializes a fresh database.

## Adapting

The seed binary is short by design (see cmd/squadron-demo-seed).
Forking it to seed a different scenario, a different group, or a
different attribution shape is a half hour of work.
