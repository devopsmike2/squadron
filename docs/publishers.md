# Incident publishers

When the incident drafter has a finished draft, the operator
publishes it. A publisher is the plug-in that ships the draft to a
specific destination: the operator's clipboard, a GitHub issue, a
Linear issue, a Jira issue, or a generic webhook. The plug-in
interface lives at `internal/incidents/publisher.go`; each
implementation lives next to it as `publisher_<provider>.go`.

This page is the operator reference: which publishers exist, what
they require to register, and what the operator picks in the UI.

## How registration works

The all-in-one binary builds the publisher registry at startup. The
clipboard publisher is always registered. Every other publisher is
opt-in: it registers only when the matching environment variables
are set. A missing publisher does not break the UI. The dropdown
still shows the option; the handler stamps the draft with whatever
external ID and URL the operator typed in, and the audit timeline
records that the operator picked that destination. Nothing leaks to
a real external system unless the env vars are present.

The handler resolves the provider name on a publish request to the
matching publisher. The provider name on the wire is one of
`clipboard`, `github`, `linear`, `jira`, `generic`.

## clipboard

Always registered. No env vars. The UI copies the rendered body to
the operator's clipboard from the browser; the server-side publish
exists so the audit timeline records that the operator picked
clipboard.

## github

Registers when all of `SQUADRON_GITHUB_ISSUES_OWNER`,
`SQUADRON_GITHUB_ISSUES_REPO`, and `SQUADRON_GITHUB_ISSUES_TOKEN`
are set. Optional `SQUADRON_GITHUB_ISSUES_LABELS` is a comma
separated list of label names to attach to every issue.

The token is a fine-grained Issues:write PAT or a GitHub App token.
Squadron sends it as a Bearer token with the
`X-GitHub-Api-Version: 2022-11-28` header on every call. Body
formatting is markdown; the draft's body lands in the issue
description verbatim. The publisher returns the issue number (e.g.
`42`) as the external ID and `html_url` as the external URL.

## linear

Registers when `SQUADRON_LINEAR_API_KEY` and `SQUADRON_LINEAR_TEAM_ID`
are set. Optional `SQUADRON_LINEAR_LABEL_IDS` is a comma separated
list of label IDs (Linear references labels by ID, not name; look
the IDs up from your workspace settings).

The API key is a personal API key issued in Linear's account
settings. The key starts with `lin_api_`. Linear's wire format puts
the raw key in the `Authorization` header without a Bearer prefix;
the publisher mirrors that shape. Squadron sends a GraphQL
`IssueCreate` mutation with the draft's title as the issue title and
the markdown body as the description. Linear preserves markdown
formatting in the description, so headers, bold, and bullet lists
render. The publisher returns the human-facing identifier (e.g.
`ENG-123`) as the external ID and the issue URL as the external
URL.

## jira

Registers when all of `SQUADRON_JIRA_BASE_URL`, `SQUADRON_JIRA_EMAIL`,
`SQUADRON_JIRA_API_TOKEN`, and `SQUADRON_JIRA_PROJECT_KEY` are set.
Optional `SQUADRON_JIRA_ISSUE_TYPE` (default `Task`) and
`SQUADRON_JIRA_LABELS` (comma separated label names) refine the
routing.

The base URL is the tenant URL like `https://acme.atlassian.net`;
Squadron appends `/rest/api/3/issue` to that. The email plus API
token authenticate as Jira Cloud Basic auth: the wire format is
`Authorization: Basic <base64(email:api_token)>`. This is different
from Linear (raw key) and GitHub (Bearer token), so the publisher
spells it out at the wire layer.

### Body formatting note (ADF, not markdown)

Jira Cloud REST API v3 requires the description field to be an
Atlassian Document Format (ADF) document. Squadron's incident
drafts are markdown. The Jira publisher converts the markdown body
to a minimal ADF document: each blank line separated chunk becomes
its own ADF paragraph node containing a single text node. This
preserves paragraph breaks but does not preserve inline markdown
formatting. Headers like `## Summary`, bold like `**important**`,
and bullet lists land in Jira as literal characters, not as Jira
formatting.

The trade is intentional. API v2 still accepts wiki markup and
would let us reverse-translate markdown to Jira's own wiki syntax,
but v2 is on Atlassian's deprecation path. We target v3 so the
publisher stays supported as Atlassian evolves the API.

Operators who need rich rendering in the destination should pick
Linear or GitHub Issues, both of which preserve markdown directly.

The publisher returns the human-facing issue key (e.g. `SQUAD-42`)
as the external ID and the user-facing browse URL
(`https://acme.atlassian.net/browse/SQUAD-42`) as the external URL.

## generic

The wire format includes `generic` for operators who want to point
the publish flow at a webhook of their own. The current build does
not ship a generic publisher implementation; the handler stamps the
draft with the operator supplied external ID and URL and skips the
remote call. The audit timeline still records the choice. Future
work: generic webhook publisher with HMAC signing.

## Adding a new publisher

The publisher interface is intentionally small. Implement
`Name() string` and `Publish(ctx, draft) (externalID, externalURL,
err)`. Place the implementation at
`internal/incidents/publisher_<provider>.go` and the tests at
`publisher_<provider>_test.go`. Register the publisher in
`cmd/all-in-one/main.go` behind opt-in env vars, following the
pattern the existing publishers use: warn when the config is
incomplete, log info when registration succeeds. Add a section to
this document.

If the destination requires a new wire-level provider name (not
already in the `IncidentProvider` enum on both the server and the
UI), update both `ui/src/types/incident.ts` and the comment on
`types.IncidentDraft.Provider` so the enum stays the source of
truth.
