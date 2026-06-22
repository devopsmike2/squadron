# GitHub webhook listener — operator runbook

This is the operator-facing runbook for the v0.89.23 GitHub
webhook listener that closes the PR audit lifecycle. It covers
generating the secret, configuring Squadron and GitHub, verifying
the loop end-to-end, reading the audit signal, and the
troubleshooting matrix when something doesn't fire.

If you haven't yet connected an IaC repo to Squadron, start there
instead:
[discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md).
The webhook listener sits on top of an existing IaC connection;
it tells Squadron when a PR Squadron opened actually merges, and
nothing more.

For a first test against a personal GitHub account with a sandbox
repo, the walkthrough takes about 15 minutes — most of it spent
in GitHub's repo settings UI. For a production setup against an
org-owned repo, budget 30 minutes plus whatever your org's
change-management process requires for adding webhooks.

## What we're building

Three things, in this order:

1. **A 32-byte random webhook secret** that you generate locally
   and share with both Squadron and GitHub. Squadron uses it to
   verify that every inbound request actually came from GitHub.
   GitHub uses it to sign every delivery with HMAC-SHA256. The
   secret is the *only* authentication on this route — there is
   no Bearer token, no IP allow-list, no mTLS. The HMAC
   signature **is** the auth.
2. **The `SQUADRON_GITHUB_WEBHOOK_SECRET` env var** set on the
   Squadron process. The handler reads it once at startup, caches
   the bytes, and uses constant-time HMAC comparison on every
   request. If the env var is empty, the route still mounts but
   responds with 503 + a humanized "secret not configured"
   message — so a misconfigured deployment surfaces clearly in
   GitHub's delivery log rather than silently no-op.
3. **A GitHub repo webhook** pointing at
   `https://your-squadron-host/api/v1/webhooks/github`, configured
   to fire on the `pull_request` event with the same secret
   you set above. When a PR Squadron opened gets merged, GitHub
   POSTs the event; Squadron verifies the signature, parses the
   payload, and emits an audit event.

The result: every Squadron-initiated PR that merges shows up in
the Timeline page as **"Merged PR #N in github.com/<repo> for
<kind>"** alongside the earlier **"Opened PR #N …"** event from
the same arc. The PR lifecycle is now closed-loop in audit:
`recommendation.pr_opened` → operator review + merge →
`recommendation.pr_merged`.

## What this is good for

- You want a queryable audit fact for "did this PR ever land?"
  instead of having to open GitHub and re-check the PR state.
- You want SIEM correlation between a Squadron recommendation
  and its eventual merge (the audit event carries
  `connection_id`, `recommendation_kind`, and the GitHub user
  who merged).
- You want to wire downstream automation (Slack notification,
  metrics counter, MTTR dashboard) off a single audit event
  stream rather than polling the GitHub API.
- You want Squadron's discovery proposer to read merged
  recommendations as positive signal and stop re-proposing
  them on the next scan. The
  [discovery proposer feedback loop](./discovery-proposer-learning.md)
  (v0.89.28, #643 slice 1) consumes
  `recommendation.pr_merged` events directly. Without the
  webhook live, that loop has no input.

## What this is NOT

Read this list carefully. The first three are features, not
limitations.

- **The webhook does not gate, comment on, or modify the PR.**
  Squadron is a listener, not an actor. GitHub Checks API
  integration (post a check run, block merge on lint failure,
  comment on the PR with proposer reasoning) is on the slice-2
  roadmap and explicitly out of slice 1.
- **The receiver only ACTS on PR merges.** Other GitHub event
  types — `push`, `issues`, `ping`, `release`, etc. — return
  200 with `{"ok": true, "ignored": true, "event": "<type>"}`
  so GitHub's redelivery system doesn't fire. PR `closed`
  events that aren't merged (the operator hit "Close pull
  request" without merging) return 200 with
  `{"ok": true, "ignored": true, "reason": "pr_closed_not_merged"}`.
  No audit event in either case.
- **The route is public-by-design.** GitHub doesn't
  authenticate to Squadron's API — the HMAC signature is the
  auth. The handler comment explicitly says "do NOT add auth
  middleware here," and the route is mounted above the
  `RequireBearer` group in `server.go::registerRoutes`. If you
  put Squadron behind a reverse proxy that adds auth headers,
  exempt this route or GitHub will fail the delivery.
- **Slice 1 uses a single GLOBAL secret.** Every IaC connection
  in your Squadron deployment shares the same
  `SQUADRON_GITHUB_WEBHOOK_SECRET`. Per-connection secrets,
  secret rotation, and a wizard step for entering the secret
  are all slice-2 work. For most operators with one or two
  managed repos this is fine; for teams with 10+ repos each
  managed by a different group, slice 2 will matter.
- **There is no replay protection in slice 1.** GitHub sends an
  `X-GitHub-Delivery` UUID with every request; Squadron logs
  it but doesn't dedupe by it. HMAC + GitHub's own delivery
  semantics are sufficient for v1, but if you're operating in
  an environment where a replay attack is a real threat
  (compromised TLS terminator, intermediary proxy), wait for
  slice 2's `X-GitHub-Delivery` dedupe table.
- **No backfill.** Pre-existing merged PRs that closed before
  v0.89.23 stay unrecorded in audit. The `pr_opened` events
  are still in the timeline; only future merges land as
  `pr_merged`.
- **Squadron must be reachable from GitHub.** This is the
  hardest prerequisite. GitHub's webhook delivery is
  outbound-from-GitHub: your Squadron deployment needs a
  public IP and TLS, or a tunnel (Cloudflare Tunnel, ngrok,
  Tailscale Funnel). Squadron will not poll GitHub for merges
  — slice 1 is push-only.

## Prerequisites

- A Squadron deployment on v0.89.23 or later. Earlier versions
  don't have the route registered, and GitHub will see 404 on
  every delivery.
- A reachable URL for your Squadron deployment. For a personal
  sandbox test, ngrok pointed at `localhost:8080` is fine
  (`ngrok http 8080` gives you a public HTTPS URL). For
  production, your Squadron should already have a TLS-
  terminated public address.
- At least one existing IaC connection
  ([discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md))
  so there's a `repo_full_name` for the receiver to correlate
  inbound merges against. PRs from repos Squadron doesn't
  manage still get an audit event, but with an empty
  `connection_id` — the merge is real even if Squadron
  didn't author the branch.
- `openssl` (or equivalent) on the machine where you're
  generating the secret. macOS, every Linux distro, and Git
  for Windows all include it.

## Step 1 — Generate the webhook secret

Pick a strong 32-byte random secret. The shape we recommend:

```sh
openssl rand -hex 32
```

This emits 64 hex characters representing 32 random bytes.
GitHub's webhook UI accepts arbitrary strings up to 4096 bytes;
HMAC-SHA256 doesn't care about the format. Hex is convenient
because it's URL-safe, copy-paste-safe, and doesn't contain
shell metacharacters.

Save this somewhere safe — a password manager, your
deployment's secret store (Vault, AWS Secrets Manager,
Kubernetes Secret), or wherever your team manages
infrastructure credentials. You'll paste it into two places in
the next steps: the Squadron env var and the GitHub repo
webhook config.

Do NOT commit the secret to your IaC repo, even encrypted.
Even with the encryption layer, the blast radius of a
compromised key is the same as a leaked secret. Treat it like
a database password.

## Step 2 — Configure Squadron

Set the secret on the Squadron process via the
`SQUADRON_GITHUB_WEBHOOK_SECRET` env var:

```sh
export SQUADRON_GITHUB_WEBHOOK_SECRET="<paste your 64-hex-char secret here>"
```

How you set this depends on your deployment shape:

- **Single VM (systemd):** add `Environment=` to your service
  unit, or write the secret to `/etc/squadron/secret.env` and
  reference it with `EnvironmentFile=`. Restart squadron after.
- **Docker Compose:** add the env var to the `environment:`
  block of the `squadron` service, or reference an
  `.env` file via `env_file:`. `docker compose up -d` to
  re-roll the container.
- **Kubernetes:** create a Secret with the value, mount it as
  an env var on the squadron pod spec. The standard
  `valueFrom.secretKeyRef` shape works.

The handler reads the env var ONCE at startup and caches the
bytes. If you rotate the secret later, you have to restart the
Squadron process — there's no hot-reload path in slice 1.

Verify Squadron picked it up by hitting the route without a
signature:

```sh
curl -i https://your-squadron-host/api/v1/webhooks/github \
  -X POST -d '{}'
```

You should see `401 Unauthorized` with body
`{"error": "invalid signature"}`. If you see `503` with the
"secret not configured" message, the env var didn't reach the
process — check the deployment shape above. If you see `404`,
you're on a version before v0.89.23.

## Step 3 — Configure the GitHub repo webhook

In the GitHub UI:

1. Open the repo Squadron manages (the one your IaC connection
   points at).
2. Navigate to **Settings → Webhooks → Add webhook**. For
   org-owned repos you may need admin rights or your
   organization's webhook-add permission.
3. Fill in the form:

   - **Payload URL**: `https://your-squadron-host/api/v1/webhooks/github`
     — exactly that path, no trailing slash. The host should
     be the same TLS-terminated host you set above.
   - **Content type**: `application/json` (NOT
     `application/x-www-form-urlencoded` — the handler
     parses JSON only, the form-encoded payload would
     unmarshal to an empty struct).
   - **Secret**: paste the same secret you set on Squadron in
     Step 2. GitHub uses this to compute the
     `X-Hub-Signature-256` header on every delivery.
   - **SSL verification**: leave enabled. The whole point of
     TLS is to verify the receiver's identity.
   - **Which events would you like to trigger this webhook?**:
     pick **"Let me select individual events"**, then check
     ONLY **Pull requests**. Don't enable Pushes, Issues, or
     "Send me everything" — the handler ignores those anyway,
     but every additional event type is extra inbound traffic
     and noise in GitHub's delivery log.
   - **Active**: leave checked.

4. Click **Add webhook**.

GitHub immediately sends a `ping` event to your URL to verify
the receiver is reachable. The handler returns 200 with
`{"ok": true, "ignored": true, "event": "ping"}`. Confirm the
delivery shows a green checkmark in **Recent Deliveries**.

If the ping shows a red X, click into the delivery to see the
response. Common causes are covered in §"Troubleshooting"
below.

## Step 4 — Verify the loop end-to-end

The cleanest test is to drive a recommendation all the way
through:

1. **Open a Squadron PR.** From the Recommendations tab on the
   Discovery page, find a recommendation against the repo you
   wired up, and click **Open PR**. Squadron creates a branch
   under `squadron/rec/`, commits the proposer's snippet, and
   opens the PR. The Timeline page shows
   **"Opened PR #N in github.com/<repo> for <kind>"**.
2. **Merge the PR in GitHub.** Click **Merge pull request**.
   Optionally delete the branch — Squadron doesn't care
   either way.
3. **Wait a few seconds.** GitHub's webhook delivery is
   typically sub-second, but allow up to 30s for the
   handler to process and the timeline page's SSE stream to
   refresh.
4. **Confirm the audit event.** Open the Timeline page. The
   most recent event for this group should be
   **"Merged PR #N in github.com/<repo> for <kind>"** with
   subtitle **"Branch squadron/rec/<kind>/<id>, merged by
   <your-github-login>"**.

If you don't see the event within 30 seconds:

- Check **Recent Deliveries** in the GitHub webhook UI.
  Each delivery shows the status code Squadron returned.
  Green checkmark = audit event emitted. Red X = signature
  failed or handler errored.
- Click into the delivery to see the request body, headers,
  and response. The `X-Hub-Signature-256` header should be
  present; if not, the secret isn't configured on the GitHub
  side. The response body explains what the handler did —
  `"ignored": true` means the handler verified the signature
  but the event didn't match a merge.

## Step 5 — Read the audit signal

The `recommendation.pr_merged` audit event carries this
payload:

```json
{
  "event_type": "recommendation.pr_merged",
  "actor": "github_webhook",
  "target_type": "iac_recommendation",
  "target_id": "<connection_id>",
  "action": "pr_merged",
  "payload": {
    "repo_full_name": "acme/infra",
    "pr_number": 42,
    "pr_url": "https://github.com/acme/infra/pull/42",
    "branch": "squadron/rec/rds-pi-em/abc123",
    "merged_at": "2026-06-22T10:00:00Z",
    "merged_by": "alice",
    "recommendation_kind": "rds-pi-em",
    "connection_id": "conn-abc"
  }
}
```

Field-by-field:

- **`actor`** is always `"github_webhook"` — not the GitHub
  user. The merger's identity is in `merged_by`. This lets
  SIEM consumers filter on `actor=github_webhook` to surface
  every inbound webhook event together, regardless of which
  human pressed Merge.
- **`target_type`** reuses the existing v0.89.3
  `iac_recommendation` constant so the timeline humanizer
  groups pr_opened / pr_open_failed / pr_merged events
  together.
- **`target_id`** is the IaC connection that authored the
  original PR. Empty when the receiver verified a real merge
  but couldn't find a Squadron connection for the repo (the
  merge is recorded honestly even if Squadron doesn't manage
  the repo).
- **`recommendation_kind`** is parsed from the branch name
  (`squadron/rec/<kind>/...`) and matches the same kind set
  the proposer uses everywhere else. Empty when the PR came
  from a non-Squadron-shaped branch.
- **`merged_at`** comes from GitHub's payload, not the
  receiver's clock. The two values differ slightly when
  GitHub's delivery sits in the queue for a few seconds.
- **`merged_by`** is the GitHub login of whoever clicked
  Merge. This is the audit identity for the merge action.
  If your team merges via a bot account, you'll see the
  bot's login here.

The Timeline page humanizer renders this event as
**"Merged PR #N in github.com/<repo> for <kind>"** with
subtitle **"Branch <branch>, merged by <login>"**. PRs that
came from non-Squadron-shaped branches render without the
"for <kind>" suffix.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| GitHub delivery shows 503 with "webhook secret not configured" | `SQUADRON_GITHUB_WEBHOOK_SECRET` env var not set on the Squadron process | Set the env var per §Step 2; restart Squadron |
| GitHub delivery shows 401 with "invalid signature" | Secret mismatch between Squadron and GitHub | Compare the secret pasted into the GitHub repo webhook UI against `SQUADRON_GITHUB_WEBHOOK_SECRET` byte-for-byte; trailing newlines from copy-paste are a common culprit |
| GitHub delivery shows 200 with `"ignored": true, "event": "ping"` | Normal — this is GitHub's initial reachability probe | No action needed; you should see this once per webhook setup |
| GitHub delivery shows 200 with `"ignored": true, "reason": "pr_closed_not_merged"` | Operator closed the PR without merging | No action needed; only merges produce audit events |
| Squadron logs show webhook fired but no audit event | The merge happened from a non-Squadron repo (no `iac_connection` matched) AND your audit storage filtered the event out | The event IS in audit; check the audit log with no target_id filter. The `target_id` is empty when no connection matched |
| GitHub delivery shows 200 with `"ignored": true, "event": "push"` | The webhook is configured for events Squadron doesn't act on | Edit the webhook in GitHub repo settings → uncheck everything except "Pull requests" |
| Curl from your terminal returns 404 | Squadron is on a version older than v0.89.23 | Upgrade |
| Curl from GitHub returns network error / timeout | Squadron isn't reachable from the public internet | Tunnel (ngrok / Cloudflare Tunnel) for testing; for production, fix the network path |

## Slice 2 roadmap

The slice-1 trade-offs most likely to shift in later releases,
in rough priority order:

- **Per-connection webhook secrets.** A `webhook_secret` column
  on `iac_connections` sealed with the same AES-GCM substrate
  as the PAT. Operators in multi-tenant deployments where each
  managed repo is owned by a different team get to scope secret
  rotation per-repo. The handler picks the secret based on the
  inbound `repo_full_name` rather than a single global env var.
- **Wizard UI for entering the secret.** A step on the existing
  Connect IaC repo wizard that generates the secret, shows it
  with a Copy button, and walks the operator through pasting it
  into the GitHub repo settings UI. Closes the "operators have
  to read a runbook" gap.
- **Replay protection via `X-GitHub-Delivery` dedupe.** A small
  table tracking the last N delivery UUIDs per connection.
  Inbound deliveries with a UUID already recorded return 200
  with `"replayed": true` and emit no audit event. Defends
  against the compromised-TLS-terminator and intermediary-proxy
  cases the slice-1 design explicitly leaves on the table.
- **GitHub Checks API back-signal.** Squadron posts a check run
  on the PR with the proposer's reasoning + the disposition
  the handler picked. Lets operators see Squadron's analysis
  alongside the PR diff without leaving GitHub. Slice 2 or
  slice 3 work.
- **Backfill of pre-existing merges.** A `POST
  /api/v1/iac/connections/:id/backfill-merges` endpoint that
  walks the GitHub repo's recent PRs via the REST API, matches
  the squadron/rec/ branch prefix, and emits
  `recommendation.pr_merged` events for each merged one. Useful
  for retroactive analytics; not blocking anyone.

These candidates live in the v0.89.23 commit message and the
slice-1 design discussion. None ship today; everything in this
runbook describes behavior you can rely on as of v0.89.23.

## Cross-references

- [Discovery IaC first-time setup](./discovery-iac-first-time-setup.md) —
  prerequisite for the webhook listener. Walks the IaC
  connection wizard plus the "Open PR" loop. The webhook is
  the lifecycle close on the PR loop documented there.
- [Audit log](./audit-log.md) — full catalog of audit event
  types and target types. The
  `recommendation.pr_opened` / `pr_open_failed` / `pr_merged`
  trio is documented there alongside the rest of the IaC arc.
- [API reference](./api-reference.md) — the
  `POST /api/v1/webhooks/github` endpoint contract (request
  shape, signature header, response codes).
