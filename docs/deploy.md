# Deploy integration — GitHub Actions (v0.34)

Squadron can dispatch your existing GitHub Actions workflows that
deploy OpenTelemetry collectors. The flow:

1. You register a **target** — the workflow Squadron is allowed to
   trigger, plus the encrypted PAT and the default inputs.
2. You click **Run deployment** in the Squadron UI (or POST to
   `/api/v1/deploy/runs`). Squadron lints the pinned config first;
   if errors exist, the trigger is **hard-blocked** and the lint
   findings render inline.
3. Squadron dispatches the workflow via
   `POST /repos/{owner}/{repo}/actions/workflows/{file}/dispatches`.
4. Status polling (every 60s) refreshes the run lifecycle through
   queued → in_progress → completed.
5. On success, Squadron auto-registers your **expected hosts** into
   the v0.32 inventory table — so the dashboard flags any host the
   workflow claimed to deploy but never checks in via OpAMP.

## Set the secret key

The deploy feature requires a 32-byte secretbox key for encrypting
PATs at rest. Generate one:

```bash
head -c 32 /dev/urandom | base64
```

Set `SQUADRON_DEPLOY_KEY` to the resulting base64 string. In OpenShift
you'd add it to the Squadron `Secret`:

```bash
oc set data secret/squadron-secrets \
  SQUADRON_DEPLOY_KEY="$(head -c 32 /dev/urandom | base64)"
oc rollout restart deployment/squadron
```

Without this variable, Squadron logs "Deploy integration disabled"
on startup and `/api/v1/deploy/*` returns 503. The UI's Deploy page
renders a setup help screen instead.

**Important:** the key is the master credential. Lose it and every
target's stored PAT becomes unrecoverable — you'll need to delete
and recreate each target with a fresh PAT.

## Mint the GitHub PAT

The PAT needs two scopes on the target repo:

- `actions:write` — for `workflow_dispatch`
- `contents:read` — so GitHub can resolve `ref` to a commit

Use a **fine-grained PAT** scoped to just the deploy repo (Settings →
Developer settings → Personal access tokens → Fine-grained tokens).
This keeps blast radius minimal vs. a classic PAT with org-wide scope.

For an audit-friendlier setup, see the "GitHub App" section in the
roadmap below; v0.34 ships PAT support, v0.35 adds App support.

## Register a target

Squadron UI → **Deploy → New target**. Fields:

- **Name** — operator-friendly label, e.g. "Prod OTel deploy".
- **Owner / Repo** — GitHub coordinates.
- **Workflow file** — the filename under `.github/workflows/`, not
  the workflow name. E.g. `deploy-otel.yml`.
- **Branch** — the ref to dispatch on. Defaults to `main`.
- **GitHub PAT** — pasted once, encrypted, never echoed back.
- **Pinned config ID** (optional) — a Squadron config that gets
  lint-checked before every dispatch. If errors, the deploy is
  refused; you fix the config and retry.
- **Default inputs** — JSON object of `string → string` that
  Squadron passes as the workflow's `inputs`. Merged with per-run
  overrides at trigger time.

## Trigger a deploy

UI: pick a target, click **Run deployment**. The modal lets you:

- Override inputs (JSON-edited; defaults pre-loaded).
- Provide an expected-hosts list (comma- or whitespace-separated).
  These get registered into `expected_agents` on success.
- Add a free-form note (shows up in the run history table).

API equivalent:

```bash
curl -X POST https://squadron.example.com/api/v1/deploy/runs \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "target_id": "abc-123",
    "inputs": { "region": "us-west-2" },
    "expected_hosts": ["host01", "host02", "host03"],
    "notes": "canary batch 3"
  }'
```

Token needs `deploy:trigger` scope.

## Workflow-side requirements

Your GitHub Actions workflow needs `on: workflow_dispatch` with the
inputs declared. Example:

```yaml
name: Deploy OTel collectors
on:
  workflow_dispatch:
    inputs:
      region:
        description: AWS region
        type: choice
        options: [us-east-1, us-west-2, eu-west-1]
      environment:
        description: Target environment
        type: string

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Deploy
        run: ./scripts/deploy-otel.sh "${{ inputs.region }}" "${{ inputs.environment }}"
```

If you POST inputs that aren't declared in the workflow file,
GitHub returns 422 — Squadron surfaces that as a clear error.

## Closing the inventory loop

When a deploy succeeds, Squadron registers each expected host with
source `squadron-deploy:<target-id>`. The v0.32 reconciliation
service then watches for those hosts to check in via OpAMP. If any
silent past the 10-minute threshold, the v0.33 silent-agent webhook
fires.

End-to-end: you click deploy in Squadron → GitHub Actions runs your
playbook → 5 minutes later, if `host03` hasn't shown up in
Squadron, your PagerDuty receiver gets paged.

## API reference

| Method | Path                                | Scope            | Notes |
| ------ | ----------------------------------- | ---------------- | ----- |
| GET    | `/api/v1/deploy/targets`            | `deploy:read`    | List targets. |
| GET    | `/api/v1/deploy/targets/:id`        | `deploy:read`    | One target. |
| POST   | `/api/v1/deploy/targets`            | `deploy:trigger` | Create. PAT in body. |
| PUT    | `/api/v1/deploy/targets/:id`        | `deploy:trigger` | Update; empty PAT preserves the existing one. |
| DELETE | `/api/v1/deploy/targets/:id`        | `deploy:trigger` | Drop. |
| POST   | `/api/v1/deploy/targets/:id/lint`   | `deploy:read`    | Preview the lint result before deploying. |
| GET    | `/api/v1/deploy/runs`               | `deploy:read`    | History. `?target_id=` filters. |
| GET    | `/api/v1/deploy/runs/:id`           | `deploy:read`    | Inline syncs from GitHub before returning. |
| POST   | `/api/v1/deploy/runs`               | `deploy:trigger` | Hot path. 422 if lint blocks. |

## Roadmap

- **v0.35** — GitHub App support (org-scoped credentials, better
  audit trails than PATs).
- **v0.36** — Webhook receiver at `/api/v1/deploy/github-webhook`
  for instant status updates (today's 60s polling becomes the
  fallback).
- **v0.37** — Jenkins + GitLab providers behind the same Provider
  interface.

## Security notes

- PATs are encrypted with NaCl secretbox using a fresh 24-byte
  nonce per record. Format on disk: `nonce || ciphertext`.
- Plaintext PATs never appear in API responses; the UI sees only
  `has_credential: true/false`.
- `deploy:trigger` is a separate scope from `deploy:read` — a
  read-only token can browse history but can't fire workflows.
- Every dispatch is audit-logged (actor, target, inputs, expected
  hosts). The `deploy_runs` table is append-only on the wire.
- A successful dispatch logs at `info` level with run ID and host
  count, never the PAT or input values.

See also: [`docs/inventory.md`](inventory.md) for the post-deploy
reconciliation surface, [`docs/silent-agents.md`](silent-agents.md)
for the webhook that fires when expected hosts go quiet.
