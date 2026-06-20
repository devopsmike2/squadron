# Post 7: Two-person approval is enforced server-side

**Pillar:** User friendly
**Tag at publish:** v0.81.3
**Visual evidence:** A screenshot of the in-app Approve dialog on
the Rollouts page at the v0.81.3 tag, captured during a real
attempt where the requester and approver are the same actor — the
dialog shows the inline red 409 error message returned by the
server when `requested_by == approved_by`. Two surfaces in one
frame: the Radix dialog UX and the rule made concrete.
**Hashtags:** #OpenTelemetry #SRE
**Target word count:** 200-400

## Draft

The AI proposes. A human approves. Not the same hand.

Squadron's two-person rule shipped in v0.61 and lives in the
rollout service: a 409 Conflict is returned at approval time when
the `requested_by` field on the rollout equals the `approved_by`
field on the inbound approval request. The check is server-side
and unconditional. There is no UI toggle to turn it off. There is
no admin override. A rollout proposed by `ai` cannot be approved
by `ai`. A rollout proposed by `operator-alice` cannot be approved
by `operator-alice`.

The rule predates the AI proposer by 18 versions. It exists
because the same separation makes sense for human-to-human
change management — a developer who proposes a config change to
their own production fleet shouldn't be the one who clicks
Approve on it. The AI is just one more requester whose work
flows through the same gate.

v0.81.3 closed the gap on the operator side. The pre-v0.81.3
Approve and Reject buttons called `window.prompt()` for the notes
string. Native browser prompts block the JS event loop, look
nothing like the rest of the Radix-styled UI, can be disabled by
browser settings (silent failure mode), and cannot be driven by
Playwright or Chrome-MCP automation. The third failure mode bit
during an E2E sweep — the test harness clicked Approve, the
prompt opened invisibly, and the renderer wedged waiting for a
dismissal that no automation could send.

The fix in v0.81.3: a single in-app Radix Dialog handles both
Approve and Reject. A `pendingDecision` state machine drives the
flow. The dialog renders a notes textarea, Cancel and Confirm
buttons, and inline error display for server-side 409s. When the
operator tries to approve a rollout they themselves requested,
the 409 from the v0.61 rule renders as a red error message inside
the same dialog. No page reload. No silent failure. The mechanism
is visible.

Server-side enforcement plus a dialog that surfaces the
enforcement message is the right shape for a guard rail — the
operator finds out at the point of action, not three steps later
in an audit review.

Repo at the v0.81.3 tag.

#OpenTelemetry #SRE

## Visual asset spec

- **Filename:** `assets/post-7-approve-dialog-409.png`
- **Surface:** The Rollouts page on the live deployment at the
  v0.81.3 tag. Trigger the screenshot scenario by attempting to
  approve a rollout where the same actor is recorded as
  `requested_by` — for the demo, use the bundled
  `squadron-demo-seed` operator account on a rollout that
  account proposed.
- **What must be visible in the crop:** the in-app Radix Approve
  dialog with the notes textarea, Cancel + Confirm buttons, and
  the inline red error box rendering the server's 409 message
  about the same actor failing the two-person rule. The rollout
  name and the requester field on the underlying row are visible
  behind/around the dialog so the reader can see who proposed
  it.
- **Annotations:** one small marker on the inline red error box
  with the caption "v0.61 rule, surfaced at the point of
  action", added in post-processing. The dialog itself does not
  need additional annotation — the UX speaks.
- **Crop:** include the route in the browser address bar
  (`/rollouts`). Drop OS chrome.

## Anti-pattern guard

Resists **the competitor takedown** from linkedin-rollout.md
"Anti-patterns to avoid". The pull is to frame this as "unlike
$competitor, Squadron actually enforces approval rules." The post
instead names the structural mechanism — a server-side check, a
specific HTTP status code, a Radix dialog that surfaces the
enforcement at the point of action — and trusts the reader to
notice that this is not how most tools handle it. The framing is
structural ("the AI is one more requester whose work flows
through the same gate"), not competitive. The rule's age (shipped
in v0.61, 18 versions ago) is the credibility signal, not a swipe
at anyone else.
