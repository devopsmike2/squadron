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
inputs declared. If you POST inputs that aren't declared in the
workflow file, GitHub returns 422 — Squadron surfaces that as a
clear error.

### Pattern A: Hosts live in `inventory.ini` (Ansible)

This is the Southern Company-style pattern. The host list is a
checked-in file; the workflow's only input is whatever knobs the
operator twiddles per run (e.g. `filelog: yes/no`). Squadron reads
the inventory at trigger time and uses the parsed host list to
register expected agents.

The workflow:

```yaml
name: Deploy otelcol to Windows

on:
  workflow_dispatch:
    inputs:
      filelog:
        description: "Collect Filelog [SouthernCo and IIS] data? (yes or no)"
        required: true
        default: "no"

env:
  VAULT_PASS: ${{ secrets.VAULT_PASS }}
  A_ACCOUNT: ${{ secrets.A_ACCOUNT }}
  A_ACCOUNT_PASSWD: ${{ secrets.A_ACCOUNT_PASSWD }}

jobs:
  run-ansible:
    runs-on: build-ghel
    steps:
      - uses: actions/checkout@v4

      - name: Set vault password to temporary file
        run: echo "$VAULT_PASS" > /tmp/.vault_pass

      - name: Run Ansible Playbook
        run: |
          ansible-playbook winOtel/ansible/otel-deploy.yml \
            -i winOtel/ansible/inventory.ini \
            --vault-password-file /tmp/.vault_pass \
            -e "a_account=$A_ACCOUNT a_account_passwd=$A_ACCOUNT_PASSWD collect_filelog=${{ github.event.inputs.filelog }}" \
            -vvv

      - name: Remove temporary file
        run: rm -f /tmp/.vault_pass
```

The Squadron target setup for this workflow:

- **Owner / Repo / Workflow / Branch** — your real values + `main`.
- **Inventory path** — `winOtel/ansible/inventory.ini`. Squadron
  reads this from GitHub via the Contents API at trigger time.
- **Default inputs** — `{"filelog": "no"}` (matches the workflow's
  default).
- **PAT scope** — `actions:write` + `contents:read`.

The trigger sheet renders the parsed host list read-only, so the
operator sees the deploy scope before clicking Run. Hosts that
don't check in within 10 minutes of a successful deploy will
trigger v0.33 silent-agent webhooks via the v0.32 inventory
reconciliation surface.

`inventory.ini` format is standard Ansible INI:

```
[windows]
host01
host02.example.com
# 10.10.40.7  (commented entries are ignored)
GAXGPAP158UA

[windows:vars]
ansible_user=svc-deploy  ; vars sections are ignored for host parsing
```

### Pattern B: Hosts pushed as a workflow input

Useful when there's no inventory file and the workflow generates
its own at runtime from a `target_hosts` input. Leave the target's
inventory path empty and the trigger sheet will show a manual
hosts textarea instead. The operator types hosts → Squadron passes
them in the `expected_hosts` request body → the GHA workflow reads
them from `${{ inputs.target_hosts }}`. (You'd configure your
workflow with a `target_hosts` input and have the first job step
write it to an inventory file.)

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

## v0.35 additions

- **Validate target** — pre-flight checklist that exercises every
  read path without firing a deploy: GitHub auth, workflow exists,
  inventory readable, lint passes. Click "Validate" on a target
  card to confirm setup is correct before your first deploy.
- **Last-deployed badge** — target cards show "Last: succeeded ·
  2h ago" so you can see fleet activity at a glance.
- **Live host status in inventory preview** — when you open the
  trigger sheet, each parsed inventory host has a green/yellow/red
  dot for healthy / silent / never-seen. Lets you spot "host02
  has been quiet for 30 minutes" before clicking Run.
- **In-progress deploy banner** — a pulsing indicator at the top
  of `/deploy` when any deploy is queued or running.
- **Redeploy button** — every completed run has a "Redeploy" link
  that re-fires with the same inputs. Incident-response panic
  button.
- **Completion webhook** — set `deploy.completion_webhook_url` in
  squadron.yaml to receive a JSON POST on every terminal state
  transition (success/failure). Same shape as the v0.33
  silent-agent webhook, key on `kind: "deploy_completed"`.
- **Decommission agent** — Agent drawer gets a "Decommission"
  button for hard-deleting agent records when hosts are retired
  from the fleet. Keeps the inventory view from accumulating
  ghost offline agents.

## Webhook receiver shape

```json
{
  "kind": "deploy_completed",
  "state": "success",
  "run_id": "uuid",
  "target_id": "uuid",
  "target_name": "Deploy otelcol to Windows",
  "requested_by": "miheanacho",
  "github_run_id": 4209876543,
  "github_run_url": "https://github.com/.../runs/4209876543",
  "expected_hosts": ["GAXGPAP158UA"],
  "started_at": "2026-06-13T22:14:33Z",
  "completed_at": "2026-06-13T22:16:07Z",
  "at": "2026-06-13T22:16:07Z"
}
```

## Roadmap

- **v0.36** — GitHub App support (org-scoped credentials, better
  audit trails than PATs).
- **v0.37** — Webhook receiver at `/api/v1/deploy/github-webhook`
  for instant status updates (today's 60s polling becomes the
  fallback).
- **v0.38** — Jenkins + GitLab providers behind the same Provider
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
