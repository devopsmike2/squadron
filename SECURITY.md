# Security Policy

Squadron is a control plane that connects to your cloud accounts (read-only),
your IaC repositories, and your OTel fleet. We take security reports seriously
and appreciate responsible disclosure.

## Reporting a Vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately using GitHub's private vulnerability reporting:

1. Go to the [Security tab](https://github.com/devopsmike2/squadron/security)
   of the repository.
2. Click **Report a vulnerability** (or use this
   [direct link](https://github.com/devopsmike2/squadron/security/advisories/new)).
3. Include: a description, affected version/commit, reproduction steps, and the
   impact you observed.

We aim to acknowledge a report within 5 business days and to provide a remediation
plan or timeline within 10 business days. We will coordinate a disclosure date
with you and credit you in the advisory unless you prefer to remain anonymous.

> Maintainers: enable **Settings → Code security → Private vulnerability
> reporting** so the link above is active.

## Supported Versions

Squadron is pre-1.0 and ships from `main`. Security fixes land on `main` and in
the next tagged release; we do not backport to older tags. Run a recent release
(or `ghcr.io/devopsmike2/squadron:latest`, which tracks `main`).

| Version          | Supported          |
| ---------------- | ------------------ |
| latest / `main`  | :white_check_mark: |
| older tags       | :x:                |

## Scope and Hardening Notes

Squadron is designed to orchestrate, not execute: it holds **read-only** cloud
credentials and opens pull requests against your IaC repo — it never runs
`terraform apply` and never holds cloud write credentials. Before exposing an
instance beyond localhost, review the self-hosting hardening guide:

- Turn authentication **on** (Bearer tokens + scopes) before exposing any port —
  see [`docs/security-self-hosting.md`](docs/security-self-hosting.md) and
  [`docs/auth.md`](docs/auth.md).
- The only data that leaves the box is the LLM call, and only if you set
  `ANTHROPIC_API_KEY`.

If you find a misconfiguration default that is unsafe out of the box, that is in
scope for a report.
