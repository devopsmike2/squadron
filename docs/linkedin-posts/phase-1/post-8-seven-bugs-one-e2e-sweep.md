# Post 8: We caught and shipped 7 bugs in one E2E sweep

**Pillar:** Squadron
**Tag at publish:** v0.81.4
**Visual evidence:** A screenshot of the `git log --oneline` output
filtered to the four v0.81.x release tags and their commit subjects
— v0.81.1, v0.81.2, v0.81.3, v0.81.4 — showing the four releases
shipped from one sweep. The terminal text is the evidence; no
mockup, no diagram.
**Hashtags:** #OpenTelemetry #PlatformEngineering
**Target word count:** 200-400

## Draft

One end-to-end sweep through the Squadron stack produced four
back-to-back patch releases — v0.81.1 through v0.81.4 — and
roughly seven discrete bug fixes. None of the bugs were
catastrophic on their own. Together they are a tour of the
failure modes a real operator workflow can quietly accumulate
between major releases.

**v0.81.1 — docker-compose dev workflow.** The dev container's
`Dockerfile.dev` mounted `./cmd` and `./internal` but not
`./extension`. The v0.50 Compliance Pack stubs at
`extension/{changewindow,policy,siem}` had been making every
container hot-reload build fail with `no required module provides
package`. The bare-binary workflow built fine because the host
sees the whole tree. Anyone using docker-compose for dev had been
hitting this since v0.50 — roughly six weeks. One-line fix: add
`./extension` to the volumes list.

**v0.81.2 — proposer prompt JSON schema.** The v0.79 prompt
rewrite used `"mode":"percentage"` in both worked examples (six
occurrences total). The rollout service validator requires
`"percent"` or `"label"` — anything else is rejected before the
write lands. Every plan-kind proposal from a real LLM that
followed the prompt faithfully was silently dropped. The bridge
logged a warning and skipped the spike. Unit tests used the
correct schema, so they missed it. The v0.83 live corpus bench
would have caught it on first run — and was queued partly
because of this discovery.

**v0.81.3 — Approve / Reject dialog.** The handlers called
`window.prompt()` for notes. The third failure mode (cannot be
driven by Playwright or Chrome-MCP automation) wedged the E2E
sweep's renderer. Replaced with a single in-app Radix Dialog
plus inline 409 error display.

**v0.81.4 — Timeline humanizer + actor wire fix.** Two fixes,
one release. The `/timeline` Recent Events list was rendering
raw `event_type` strings; the v0.76 humanizer was client-side
only and didn't apply here. Ported to the handler. The
`plan.created` event's actor was being set to `system` instead
of `ai` on one code path; fixed alongside.

No release was urgent on its own. None went out as a hotfix at
3am. Each one made the next sweep cheaper. That's what cleanup
discipline looks like.

Repo at the v0.81.4 tag.

#OpenTelemetry #PlatformEngineering

## Visual asset spec

- **Filename:** `assets/post-8-git-log-v0.81-sweep.png`
- **Surface:** Terminal output of `git log --oneline
  --pretty=format:'%h %s' v0.81.0..v0.81.4` (or a `git log
  --oneline` filtered to those four tags) on the operator's
  laptop with the squadron repo checked out at the v0.81.4 tag.
  The output is the four release commit subjects in order:
  v0.81.1 docker-compose mount fix, v0.81.2 proposer prompt
  mode fix, v0.81.3 in-app approve dialog, v0.81.4 timeline
  humanizer + actor wire fix.
- **What must be visible in the crop:** all four commit subjects
  with their short SHAs. The v0.81.0 line above is fine context
  but not required.
- **Annotations:** none baked into the terminal. A caption below
  the screenshot reads "four patches from one E2E sweep, ~6
  weeks of latent issues surfaced and shipped." The point is
  the commit log itself — terminal text as evidence.
- **Crop:** terminal-only. Drop OS chrome.

## Anti-pattern guard

Resists **the hype follow-up** from linkedin-rollout.md
"Anti-patterns to avoid". The pull on a "we shipped 7 bugs!"
post is to make it triumphant. The post instead leads with "none
of the bugs were catastrophic on their own" and closes with "no
release was urgent... each one made the next sweep cheaper." The
mechanism is cleanup discipline; the takeaway is what mature
maintenance looks like, not how impressive the bug count is. The
docker-compose-mount-broken-since-v0.50 detail is the
load-bearing honesty — the team did not catch it for six weeks,
and the post says so plainly.
