# Self-hosting security posture

A short, honest checklist for running Squadron safely. Squadron is built to be
self-hosted; you own the deployment and its exposure.

## 1. Turn auth on before you expose it

Auth is **off by default** so first-run evaluation is friction-free — when
disabled, Squadron logs a loud `API auth is disabled — every endpoint is open`
warning on startup. **Before putting Squadron on any network beyond your
laptop / a trusted private subnet, enable Bearer-token auth.** See
[`docs/auth.md`](./auth.md) for turning it on, the bootstrap token, scopes, and
token lifecycle.

- OSS auth = Bearer tokens + scopes (read/write per surface).
- `/health` and `/metrics` stay public so scrapers / load balancers work.
- Tokens are stored as sha256 digests; the plaintext is shown once.
- SSO/SAML/OIDC, SCIM, and full RBAC are commercial-tier features — OSS is
  single-tier tokens.

## 2. What data leaves the box

Squadron is self-contained. The only outbound calls it makes are the ones you
explicitly enable:

- **AI features** (recommendations, Explain, Merge, incident drafting) call the
  **Anthropic API** — and **only if you set `ANTHROPIC_API_KEY`**. With no key,
  no AI calls are made and no telemetry/config leaves the box. What gets sent is
  documented in [`docs/ai-assist.md`](./ai-assist.md). If you need an
  air-gapped / bring-your-own-model setup, that's a commercial-tier option.
- **Cloud discovery** calls the cloud provider APIs you connect (read-only
  credentials you supply) — see the per-cloud
  `docs/discovery-*-first-time-setup.md` guides.
- **IaC PRs** call the GitHub API using the PAT you connect; the merge-learning
  webhook listener is one you expose and secure with a webhook secret.

Your telemetry (collector samples, traces, metrics, logs) stays in Squadron's
local store. It is not sent anywhere.

## 3. Credentials & secrets

- Set `SQUADRON_SECRETS_KEY` so connector credentials (cloud keys, GitHub PATs,
  webhook secrets) are sealed at rest rather than stored in plaintext.
- Cloud discovery should use **read-only** credentials. The per-cloud setup
  guides list the minimal read scopes.
- Squadron is an **orchestrator, not an executor**: it opens PRs and stages
  rollouts; it never runs `terraform apply`, never pushes to your branches
  unattended, and never executes cloud writes. Your review + CI + branch
  protection remain the gate.

## 4. Network & deployment

- Ports: `8080` (UI + API), `4317`/`4318` (OTLP in), `4320` (OpAMP). Expose only
  what you need; put the UI/API behind your own TLS-terminating proxy.
- Run behind your normal ingress controls (VPN / private subnet / SSO proxy)
  until auth is on and you've reviewed the above.
- See [`docs/deployment.md`](./deployment.md) and [`docs/operating.md`](./operating.md)
  for the production checklist, backups, and upgrades.
