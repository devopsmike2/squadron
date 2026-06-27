// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package handlers — Timeline (Postmortem View) handler.
//
// The v0.40 timeline answers the on-call's 2 AM question: "what
// happened?" Instead of bouncing between Audit, Deploy, Rollouts,
// Alerts, and Savings to reconstruct a sequence of events, this
// endpoint merges all of them into a single chronologically-sorted
// stream that the UI renders as swimlanes on one shared time axis.
//
// Sources merged (each becomes a swimlane in the UI):
//
//   - audit      — every state change Squadron records (config
//                  apply, rule edit, drift transition, etc.)
//   - deploy     — deploy_runs with their request / completion times
//                  and conclusion. A single run becomes one event
//                  pinned to its RequestedAt for placement; details
//                  carry the conclusion so the UI can color it.
//   - cost_spike — open-spike and closed-spike transitions from the
//                  cost spike detector
//
// We deliberately don't include continuous data (pipeline-health
// verdicts that change every 10s, OTLP volume rates) — those are
// already surfaced elsewhere as live state. The timeline is for
// discrete, event-shaped state changes that an incident postmortem
// would care about.
//
// Aggregation is fan-out: each source query is its own list call.
// At our scale (hundreds of events per source per day) the merge
// happens in O(N log N) at the handler. If a future scale ceiling
// forces it, we can move to a unified events table — but the
// current shape decouples concerns and avoids writing a new
// schema for v0.40.
//
// Added in v0.40.0 (postmortem timeline view).

package handlers

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/deploy"
	"github.com/devopsmike2/squadron/internal/services"
	storetypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// TimelineSource is the kind of event. The UI uses this to pick the
// swimlane, icon, and color. New sources go here AND into the UI
// constants — both lists must stay aligned or events render in the
// wrong lane.
type TimelineSource string

const (
	TimelineSourceAudit     TimelineSource = "audit"
	TimelineSourceDeploy    TimelineSource = "deploy"
	TimelineSourceCostSpike TimelineSource = "cost_spike"
)

// TimelineEvent is one normalized row in the merged stream. It's
// deliberately thin — the UI doesn't need every field from every
// source, just enough to render the marker and let the user click
// through to the originating page for details.
type TimelineEvent struct {
	// ID is unique within (source, originating-ID) — events from
	// the audit log share IDs with audit, deploy events share the
	// run ID, etc. The UI uses (source, id) as the React key.
	ID     string         `json:"id"`
	Source TimelineSource `json:"source"`
	// Time is the event's primary timestamp. For deploys this is
	// RequestedAt — the moment the operator hit "deploy" — even if
	// the run completed later. That places the marker where intent
	// happened, not where confirmation arrived.
	Time time.Time `json:"time"`
	// Title is the one-line summary the marker shows on hover.
	Title string `json:"title"`
	// Subtitle is the second line — actor, target, conclusion.
	Subtitle string `json:"subtitle,omitempty"`
	// Severity drives the marker's color when relevant. Values:
	// "info" (default), "ok", "warn", "critical".
	Severity string `json:"severity"`
	// Href is an optional deep link to the source page for full
	// details. The UI renders the marker as a button that navigates
	// to this href when clicked.
	Href string `json:"href,omitempty"`
}

// TimelineQuery narrows the merge by time window and active sources.
type TimelineQuery struct {
	Since   time.Time
	Until   time.Time
	Sources []TimelineSource
	Limit   int
}

// TimelineHandlers owns the merge logic. Constructors are nil-safe
// on any source — pass nil for a source we shouldn't query (e.g.
// cost-spikes when the detector is disabled) and the merge will
// skip it without crashing.
type TimelineHandlers struct {
	audit        services.AuditService
	deploy       *deploy.Service
	costSpikes   CostSpikeStore // reused interface from handlers/costspikes.go
	logger       *zap.Logger
	defaultLimit int
}

func NewTimelineHandlers(
	audit services.AuditService,
	deploySvc *deploy.Service,
	costSpikes CostSpikeStore,
	logger *zap.Logger,
) *TimelineHandlers {
	return &TimelineHandlers{
		audit:        audit,
		deploy:       deploySvc,
		costSpikes:   costSpikes,
		logger:       logger,
		defaultLimit: 500,
	}
}

// HandleList is GET /api/v1/timeline?since=ISO&until=ISO&source=…&limit=N
//
// All filters are optional. Default window: last 24h. Default
// sources: all three. Default limit: 500 events (cap at 2000).
func (h *TimelineHandlers) HandleList(c *gin.Context) {
	q := parseTimelineQuery(c)
	events := h.merge(c, q)
	// Sort newest-first. The UI swimlanes render bidirectionally,
	// but a stable backend order keeps caching honest and makes
	// tail-truncation predictable when Limit cuts in.
	sort.Slice(events, func(i, j int) bool {
		return events[i].Time.After(events[j].Time)
	})
	if q.Limit > 0 && len(events) > q.Limit {
		events = events[:q.Limit]
	}
	c.JSON(http.StatusOK, gin.H{
		"items": events,
		"count": len(events),
		"since": q.Since,
		"until": q.Until,
	})
}

// merge runs the per-source queries and stitches results into a
// single slice. Each branch is nil-safe so a partially-wired
// deployment (no cost-spike detector configured, for instance)
// still returns whatever data IS available.
func (h *TimelineHandlers) merge(c *gin.Context, q TimelineQuery) []TimelineEvent {
	want := map[TimelineSource]bool{}
	for _, s := range q.Sources {
		want[s] = true
	}
	// When no source filter is passed, include everything. The
	// earlier `want[""]` sentinel was a bug — map reads on a
	// missing key return the zero value (false), not "all".
	all := len(q.Sources) == 0

	out := make([]TimelineEvent, 0, 256)

	// Audit events — the broadest source. We use Since on the
	// AuditEventFilter so the database does the time clipping for
	// us. The Until clip happens client-side because the filter
	// shape predates v0.40 and we don't want to alter it here.
	if (all || want[TimelineSourceAudit]) && h.audit != nil {
		evs, err := h.audit.List(c.Request.Context(), services.AuditEventFilter{
			Since: q.Since,
			Limit: h.defaultLimit,
		})
		if err != nil {
			h.logger.Debug("timeline: audit list failed", zap.Error(err))
		} else {
			for _, ev := range evs {
				if !q.Until.IsZero() && ev.Timestamp.After(q.Until) {
					continue
				}
				out = append(out, auditToEvent(ev))
			}
		}
	}

	// Deploy runs. We fetch a generous Limit and clip in-memory
	// because DeployRunFilter doesn't support a Since field. At our
	// scale (handful of deploys per day) this is negligible.
	if (all || want[TimelineSourceDeploy]) && h.deploy != nil {
		runs, err := h.deploy.ListRuns(c.Request.Context(), storetypes.DeployRunFilter{
			Limit: h.defaultLimit,
		})
		if err != nil {
			h.logger.Debug("timeline: deploy list failed", zap.Error(err))
		} else {
			for _, r := range runs {
				t := r.RequestedAt
				if t.Before(q.Since) {
					continue
				}
				if !q.Until.IsZero() && t.After(q.Until) {
					continue
				}
				out = append(out, deployToEvent(r))
			}
		}
	}

	// Cost spikes. Each event row already has Started/Ended; we
	// emit one timeline event for the OPEN and (if closed) one for
	// the CLOSE. Operators care about both moments.
	if (all || want[TimelineSourceCostSpike]) && h.costSpikes != nil {
		spikes, err := h.costSpikes.ListCostSpikeEvents(c.Request.Context(), storetypes.CostSpikeFilter{
			Limit: h.defaultLimit,
		})
		if err != nil {
			h.logger.Debug("timeline: cost spike list failed", zap.Error(err))
		} else {
			for _, sp := range spikes {
				if !sp.StartedAt.Before(q.Since) {
					if q.Until.IsZero() || !sp.StartedAt.After(q.Until) {
						out = append(out, spikeOpenedEvent(sp))
					}
				}
				if sp.EndedAt != nil && !sp.EndedAt.Before(q.Since) {
					if q.Until.IsZero() || !sp.EndedAt.After(q.Until) {
						out = append(out, spikeClosedEvent(sp))
					}
				}
			}
		}
	}
	return out
}

// ----------------------------------------------------------------
// Adapters — convert each source's native shape to TimelineEvent.
// ----------------------------------------------------------------

func auditToEvent(e *services.AuditEvent) TimelineEvent {
	// v0.89.4 [#612] — payload-aware humanizer for the Stream 19
	// IaC + recommendation.pr_* family. When a payload-aware entry
	// is registered AND every required payload field is present, we
	// use the payload-derived (Summary, Detail) pair. Otherwise we
	// fall through to the v0.81.4 path so a malformed payload never
	// renders empty placeholders like "Opened PR # in github.com/".
	if title, sub, ok := humanizeIaCAuditEvent(e); ok {
		return TimelineEvent{
			ID:       e.ID,
			Source:   TimelineSourceAudit,
			Time:     e.Timestamp,
			Title:    title,
			Subtitle: sub,
			Severity: "info",
			Href:     "/audit",
		}
	}
	title := humanizeEventType(e.EventType, e.Action)
	// v0.89.14 (#630) — when an action.* event was plan-embedded,
	// enrich the title so the operator sees the plan context
	// inline ("Action restart-systemd-service dispatched for plan
	// abc1… step 1") rather than the generic "Action dispatched".
	// Standalone action events (no plan_id in payload) keep the
	// existing wording.
	if planTitle := planEmbeddedActionTitle(e.EventType, e.Payload); planTitle != "" {
		title = planTitle
	}
	// v0.89.22 (#638) — when a proposal.created event cited prior
	// verdicts (#531 / v0.89.17 feedback loop), enrich the title
	// so operators can tell at a glance which proposals were
	// shaped by past operator decisions. Cold-start proposals
	// (verdict_examples_used absent or empty) keep the existing
	// "AI proposal created" wording.
	if proposalTitle := proposalCreatedTitle(e.EventType, e.Payload); proposalTitle != "" {
		title = proposalTitle
	}
	// v0.89.26 (#642 Stream 43) — payload-aware enrichment for the
	// per-rollout exclude-from-learning toggle (#531 slice 2 §10 Q3).
	// new_state=false means the operator re-included the rollout;
	// surface different wording so the timeline reads honestly. When
	// the payload is missing the field (defensively impossible — the
	// service always emits it — but the humanizer treats absence as
	// "fall back to the default") we keep the default table title.
	if exTitle := excludeFromLearningTitle(e.EventType, e.Payload); exTitle != "" {
		title = exTitle
	}
	sub := strings.TrimSpace(e.Actor)
	if e.TargetType != "" {
		if sub != "" {
			sub += " · "
		}
		sub += e.TargetType
		if e.TargetID != "" && len(e.TargetID) > 12 {
			sub += " " + e.TargetID[:8]
		}
	}
	// Most audit events are informational. We deliberately don't
	// try to upgrade to warn/critical from action verbs — too easy
	// to misclassify "deleted" as alarming when it was intentional.
	return TimelineEvent{
		ID:       e.ID,
		Source:   TimelineSourceAudit,
		Time:     e.Timestamp,
		Title:    title,
		Subtitle: sub,
		Severity: "info",
		Href:     "/audit",
	}
}

func deployToEvent(r *storetypes.DeployRun) TimelineEvent {
	title := "Deploy: " + r.TargetName
	if r.TargetName == "" {
		title = "Deploy run"
	}
	sub := r.Status
	if r.Conclusion != "" {
		sub = r.Conclusion
	}
	if r.RequestedBy != "" {
		sub += " · by " + r.RequestedBy
	}
	sev := "info"
	switch r.Conclusion {
	case "success":
		sev = "ok"
	case "failure", "timed_out":
		sev = "critical"
	case "cancelled":
		sev = "warn"
	}
	return TimelineEvent{
		ID:       r.ID,
		Source:   TimelineSourceDeploy,
		Time:     r.RequestedAt,
		Title:    title,
		Subtitle: sub,
		Severity: sev,
		Href:     "/deploy",
	}
}

func spikeOpenedEvent(sp *storetypes.CostSpikeEvent) TimelineEvent {
	sev := "warn"
	if sp.Severity == "critical" {
		sev = "critical"
	}
	return TimelineEvent{
		ID:       sp.ID + ":open",
		Source:   TimelineSourceCostSpike,
		Time:     sp.StartedAt,
		Title:    "Cost spike opened",
		Subtitle: spikeSubtitle(sp),
		Severity: sev,
		Href:     "/savings",
	}
}

func spikeClosedEvent(sp *storetypes.CostSpikeEvent) TimelineEvent {
	t := sp.StartedAt
	if sp.EndedAt != nil {
		t = *sp.EndedAt
	}
	return TimelineEvent{
		ID:       sp.ID + ":close",
		Source:   TimelineSourceCostSpike,
		Time:     t,
		Title:    "Cost spike resolved",
		Subtitle: spikeSubtitle(sp),
		Severity: "ok",
		Href:     "/savings",
	}
}

func spikeSubtitle(sp *storetypes.CostSpikeEvent) string {
	pct := int(sp.PeakPctAboveBaseline * 100)
	sub := sp.Signal
	if sub == "" {
		sub = "fleet-wide"
	}
	if pct > 0 {
		sub += " · peak +" + itoa(pct) + "%"
	}
	return sub
}

// itoa avoids pulling fmt.Sprintf into this hot path. The values are
// small ints that always fit in a few digits.
func itoa(n int) string {
	return strings.TrimLeft(string(rune('0'+(n/10000)%10))+
		string(rune('0'+(n/1000)%10))+
		string(rune('0'+(n/100)%10))+
		string(rune('0'+(n/10)%10))+
		string(rune('0'+n%10)), "0")
}

// ----------------------------------------------------------------
// Query parsing
// ----------------------------------------------------------------

func parseTimelineQuery(c *gin.Context) TimelineQuery {
	q := TimelineQuery{
		// Default window: last 24 hours. Matches the SLI we expect
		// on-call to look at first; explicit since/until overrides
		// it for postmortem deep dives.
		Since: time.Now().UTC().Add(-24 * time.Hour),
		Until: time.Now().UTC(),
		Limit: 500,
	}
	if s := c.Query("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			q.Since = t
		}
	}
	if u := c.Query("until"); u != "" {
		if t, err := time.Parse(time.RFC3339, u); err == nil {
			q.Until = t
		}
	}
	if srcs := c.QueryArray("source"); len(srcs) > 0 {
		for _, s := range srcs {
			q.Sources = append(q.Sources, TimelineSource(s))
		}
	}
	if l := c.Query("limit"); l != "" {
		var n int
		// Manual atoi to dodge an strconv import for one site.
		for i := 0; i < len(l); i++ {
			c := l[i]
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int(c-'0')
		}
		if n > 0 && n <= 2000 {
			q.Limit = n
		}
	}
	return q
}

// humanizeIaCAuditEvent — v0.89.4 [#612] — payload-aware humanizer
// for the Stream 19 (Connect IaC repo) event family. Phase 2 added 4
// audit event types plus #610 added a 5th; none of them had
// humanizer entries, so the Timeline page was rendering raw
// event_type strings — same regression class as #545 from v0.76.
//
// Unlike the v0.81.4 humanizeEventType table (event_type → title
// only), these entries pull both the Summary (Title) and Detail
// (Subtitle) from the audit Payload so the operator sees the repo,
// PR number, and affected-row counts at a glance.
//
// Defensive: if any required payload field is missing or malformed
// (wrong type from a hand-edited audit row, schema drift on a
// rolling deploy), the function returns ok=false and the caller
// falls through to the v0.81.4 path. We NEVER render empty
// placeholders like "Opened PR # in github.com/ for ".
//
// Payload values arrive via json.Unmarshal into map[string]any so
// slices come back as []any (length is what we need for counts) and
// numbers come back as float64 (json's default numeric type).
func humanizeIaCAuditEvent(e *services.AuditEvent) (string, string, bool) {
	if e == nil || e.Payload == nil {
		return "", "", false
	}
	switch e.EventType {
	case services.AuditEventIaCGitHubConnectionCreated:
		repo, ok := payloadString(e.Payload, "repo_full_name")
		if !ok {
			return "", "", false
		}
		authKind, ok := payloadString(e.Payload, "auth_kind")
		if !ok {
			return "", "", false
		}
		placement, ok := payloadSlice(e.Payload, "placement_map")
		if !ok {
			return "", "", false
		}
		title := "Connected github.com/" + repo + " to Squadron"
		sub := strconv.Itoa(len(placement)) + " placement rows configured (" + authKind + ")"
		return title, sub, true

	case services.AuditEventIaCGitHubConnectionValidated:
		repo, ok := payloadString(e.Payload, "repo_full_name")
		if !ok {
			return "", "", false
		}
		branch, ok := payloadString(e.Payload, "default_branch")
		if !ok {
			return "", "", false
		}
		rows, ok := payloadSlice(e.Payload, "preflight_results")
		if !ok {
			return "", "", false
		}
		reachable := 0
		for _, r := range rows {
			row, ok := r.(map[string]any)
			if !ok {
				continue
			}
			if exists, _ := row["exists"].(bool); exists {
				reachable++
			}
		}
		title := "Validated IaC connection to github.com/" + repo
		sub := strconv.Itoa(reachable) + " of " + strconv.Itoa(len(rows)) +
			" placement files reachable on " + branch
		return title, sub, true

	case services.AuditEventIaCGitHubPlacementMapUpdated:
		repo, ok := payloadString(e.Payload, "repo_full_name")
		if !ok {
			return "", "", false
		}
		placement, ok := payloadSlice(e.Payload, "placement_map")
		if !ok {
			return "", "", false
		}
		title := "Updated placement map for github.com/" + repo
		sub := strconv.Itoa(len(placement)) + " placement rows now configured"
		return title, sub, true

	case services.AuditEventRecommendationPROpened:
		repo, ok := payloadString(e.Payload, "repo_full_name")
		if !ok {
			return "", "", false
		}
		kind, ok := payloadString(e.Payload, "resource_kind")
		if !ok {
			return "", "", false
		}
		prNum, ok := payloadInt(e.Payload, "pr_number")
		if !ok {
			return "", "", false
		}
		branch, ok := payloadString(e.Payload, "branch")
		if !ok {
			return "", "", false
		}
		filePath, ok := payloadString(e.Payload, "file_path")
		if !ok {
			return "", "", false
		}
		// commit_sha intentionally omitted — noisy and the PR link
		// already pins the diff.
		//
		// v0.89.11 (#626 Stream 27) — slice 1.5 — title gains a
		// disposition-aware suffix so the operator reading the
		// timeline sees the merge posture without clicking through.
		// `disposition` is empty on pre-v0.89.11 payloads — fall
		// back to the slice-1 phrasing for back-compat.
		//
		// v0.89.12 (#628 Stream 29) — slice 2 — disposition_actual
		// refines the title further:
		//   - patch_existing_hcl_merged → "with HCL-aware merge"
		//   - patch_existing_fell_back_to_append → "HCL merge
		//     failed; manual integration required"
		// Pre-v0.89.12 payloads have no disposition_actual; fall
		// back to the slice-1.5 phrasing keyed on `disposition`.
		title := "Opened PR #" + strconv.Itoa(prNum) + " in github.com/" + repo +
			" for " + kind
		dispActual, _ := payloadString(e.Payload, "disposition_actual")
		disp, _ := payloadString(e.Payload, "disposition")
		switch {
		case dispActual == "patch_existing_hcl_merged":
			title = "Opened PR #" + strconv.Itoa(prNum) + " in github.com/" + repo +
				" with HCL-aware merge for " + kind
		case dispActual == "patch_existing_fell_back_to_append":
			title = "Opened PR #" + strconv.Itoa(prNum) + " in github.com/" + repo +
				" for " + kind + " — HCL merge failed; manual integration required"
		case dispActual == "new_file" || disp == "new_file":
			title = "Opened PR #" + strconv.Itoa(prNum) + " in github.com/" + repo +
				" creating " + filePath
		case disp == "patch_existing":
			title = "Opened PR #" + strconv.Itoa(prNum) + " in github.com/" + repo +
				" — manual merge required for " + kind
		}
		sub := "Branch " + branch + ", file " + filePath
		return title, sub, true

	case services.AuditEventRecommendationPROpenFailed:
		repo, ok := payloadString(e.Payload, "repo_full_name")
		if !ok {
			return "", "", false
		}
		kind, ok := payloadString(e.Payload, "resource_kind")
		if !ok {
			return "", "", false
		}
		// humanized_message was already humanized by the Phase 2
		// handler — we surface it verbatim.
		msg, ok := payloadString(e.Payload, "humanized_message")
		if !ok {
			return "", "", false
		}
		title := "Could not open PR in github.com/" + repo + " for " + kind
		return title, msg, true

	// v0.89.24 (#640) — humanizer entry for the v0.89.23 (#639) PR-
	// merged webhook event. Mirrors the pr_opened shape above so an
	// operator scanning the timeline sees "Merged PR #N in
	// github.com/<repo>" alongside the earlier "Opened PR #N …"
	// row from the same recommendation arc. The audit payload was
	// written by the webhook receiver, not Squadron itself —
	// repo_full_name, pr_number, branch, and merged_by are the four
	// fields the handler guarantees are populated when a real merge
	// event is verified; recommendation_kind and connection_id can
	// be empty when the PR didn't come from a Squadron-managed
	// branch prefix (we still emit the event honestly; only the
	// title falls back to a kind-less form when kind is empty).
	case services.AuditEventRecommendationPRMerged:
		repo, ok := payloadString(e.Payload, "repo_full_name")
		if !ok {
			return "", "", false
		}
		prNum, ok := payloadInt(e.Payload, "pr_number")
		if !ok {
			return "", "", false
		}
		branch, ok := payloadString(e.Payload, "branch")
		if !ok {
			return "", "", false
		}
		mergedBy, ok := payloadString(e.Payload, "merged_by")
		if !ok {
			return "", "", false
		}
		// recommendation_kind is optional — present when the branch
		// matched the squadron/rec/ prefix, absent otherwise. The
		// title appends "for <kind>" only when we have it so a non-
		// Squadron-shaped PR (an operator hand-pushed branch that
		// somehow landed under the webhook) still gets a clean
		// title rather than "Merged PR #5 in foo/bar for ".
		title := "Merged PR #" + strconv.Itoa(prNum) + " in github.com/" + repo
		if kind, ok := payloadString(e.Payload, "recommendation_kind"); ok {
			title += " for " + kind
		}
		sub := "Branch " + branch + ", merged by " + mergedBy
		return title, sub, true

	// v0.89.28 (#643 slice 1) — discovery_proposal.created humanizer.
	// v0.89.37 (#657 Stream 55, #531 slice 2 chunk 6) extension:
	// when verdict_examples_used_by_state is present AND has any
	// non-empty bucket, the title surfaces the per-state mix
	// ("informed by N accepted + M closed + P excluded") so an
	// operator scanning the timeline can read the signal
	// composition without expanding the payload. Falls back to the
	// v0.89.28 flat-count phrasing when by-state is absent (legacy
	// fixtures + SIEM consumers that haven't ingested the slice 2
	// shape yet); falls back further to the cold-start phrasing
	// when no examples cited at all.
	case services.AuditEventDiscoveryProposalCreated:
		if title := discoveryProposalByStateTitle(e.Payload); title != "" {
			return title, "", true
		}
		n := 0
		if raw, ok := e.Payload["verdict_examples_used"]; ok {
			switch v := raw.(type) {
			case []any:
				n = len(v)
			case []string:
				n = len(v)
			}
		}
		if n == 0 {
			return "Discovery recommendations generated", "", true
		}
		plural := "PRs"
		if n == 1 {
			plural = "PR"
		}
		return fmt.Sprintf("Discovery recommendations generated (informed by %d prior accepted %s)", n, plural), "", true

	// v0.89.44 (#665 Stream 63, slice 1 chunk 4) — humanizer entries
	// for the three GitHub Checks API back-signal arc audit event
	// types. The created event fires from the chunk-2 PR-open follow-up
	// (v0.89.43). The updated event fires from chunk 3's
	// pr_merged / pr_closed_not_merged webhook handlers AND chunk 4's
	// operator-exclude handler. The failed event fires whenever the
	// underlying GitHub API call returned a structured CheckRunError.
	// See design doc §8.1.

	// iac.check_run.created — chunk-2 emit on PR open. Payload fields:
	//   pr_url, head_sha, check_run_id, owner, repo, status,
	//   recommendation_kind. Title format:
	//     "Squadron posted a check run on PR #<N> in <owner>/<repo>
	//      (kind=<kind>)."
	// We derive <N> from pr_url's trailing path segment because the
	// audit payload does not separately carry pr_number on this event
	// (the bridge emits pr_url, not a parsed number). Owner / repo
	// come from the payload directly. Fall-through ok=false rather
	// than an empty placeholder.
	case services.AuditEventIaCCheckRunCreated:
		prURL, ok := payloadString(e.Payload, "pr_url")
		if !ok {
			return "", "", false
		}
		owner, ok := payloadString(e.Payload, "owner")
		if !ok {
			return "", "", false
		}
		repo, ok := payloadString(e.Payload, "repo")
		if !ok {
			return "", "", false
		}
		kind, ok := payloadString(e.Payload, "recommendation_kind")
		if !ok {
			return "", "", false
		}
		prNum := prNumberFromURL(prURL)
		title := fmt.Sprintf("Squadron posted a check run on PR #%s in %s/%s (kind=%s)",
			prNum, owner, repo, kind)
		return title, "", true

	// iac.check_run.updated — chunks 3 + 4 emit on PR
	// merge / close / operator-exclude. Payload fields:
	//   previous_status, previous_conclusion, new_status,
	//   new_conclusion, pr_url, recommendation_kind. Title format
	//   depends on the transition (§8.1):
	//     in_progress → success → "marked SUCCESS … (operator merged)"
	//     in_progress → failure → "marked FAILURE … (operator closed
	//       without merging)"
	//     in_progress → neutral → "marked NEUTRAL … (operator excluded
	//       this kind from future recommendations)"
	//   Fallback for any other new_conclusion: a generic
	//     "updated on PR #<N> (status=<...> conclusion=<...>)".
	case services.AuditEventIaCCheckRunUpdated:
		prURL, ok := payloadString(e.Payload, "pr_url")
		if !ok {
			return "", "", false
		}
		newConclusion, ok := payloadString(e.Payload, "new_conclusion")
		if !ok {
			return "", "", false
		}
		prNum := prNumberFromURL(prURL)
		switch newConclusion {
		case "success":
			return fmt.Sprintf("Squadron's check run marked SUCCESS on PR #%s (operator merged).", prNum), "", true
		case "failure":
			return fmt.Sprintf("Squadron's check run marked FAILURE on PR #%s (operator closed without merging).", prNum), "", true
		case "neutral":
			return fmt.Sprintf("Squadron's check run marked NEUTRAL on PR #%s (operator excluded this kind from future recommendations).", prNum), "", true
		default:
			newStatus, _ := payloadString(e.Payload, "new_status")
			return fmt.Sprintf("Squadron's check run updated on PR #%s (status=%s conclusion=%s).",
				prNum, newStatus, newConclusion), "", true
		}

	// iac.check_run.failed — chunks 2/3/4 emit when the underlying
	// CreateCheckRun or UpdateCheckRun call returned an error. Payload
	// fields:
	//   pr_url, error_kind, http_status, error_message,
	//   recommendation_kind. Title format depends on error_kind
	//   (§8.1). scope_missing / rate_limit / pr_not_found get pinned
	//   fix-it copy; network / unknown fall through to a generic
	//   surface that includes error_message.
	case services.AuditEventIaCCheckRunFailed:
		prURL, ok := payloadString(e.Payload, "pr_url")
		if !ok {
			return "", "", false
		}
		errKind, ok := payloadString(e.Payload, "error_kind")
		if !ok {
			return "", "", false
		}
		prNum := prNumberFromURL(prURL)
		switch errKind {
		case "scope_missing":
			return fmt.Sprintf("Squadron couldn't post a check run on PR #%s: your IaC PAT is missing the checks:write scope.", prNum), "", true
		case "rate_limit":
			return fmt.Sprintf("Squadron couldn't post a check run on PR #%s: GitHub API rate limit exceeded.", prNum), "", true
		case "pr_not_found":
			return fmt.Sprintf("Squadron couldn't post a check run on PR #%s: the PR was not found on GitHub.", prNum), "", true
		case "network":
			msg, _ := payloadString(e.Payload, "error_message")
			if msg == "" {
				msg = "transient network failure"
			}
			return fmt.Sprintf("Squadron couldn't post a check run on PR #%s: %s.", prNum, msg), "", true
		default:
			msg, _ := payloadString(e.Payload, "error_message")
			if msg == "" {
				msg = errKind
			}
			return fmt.Sprintf("Squadron couldn't post a check run on PR #%s: %s.", prNum, msg), "", true
		}
	}
	return "", "", false
}

// prNumberFromURL extracts the trailing /pull/<N> segment from a
// GitHub PR URL. Returns "?" on malformed input so the humanized
// title still renders the rest of the line rather than emitting
// "PR # on …" with the number dropped silently. v0.89.44 (#665
// Stream 63, slice 1 chunk 4) — the chunks 2/3/4 audit payloads
// carry pr_url verbatim from the PR-open and merge-/close-webhook
// paths; chunk-4 specifically reconstructs a kindless URL (owner/repo
// only, no /pull/<n>) on the exclusion-emit path, in which case "?"
// is the honest renderer.
func prNumberFromURL(prURL string) string {
	const marker = "/pull/"
	idx := strings.LastIndex(prURL, marker)
	if idx < 0 {
		return "?"
	}
	tail := prURL[idx+len(marker):]
	// Strip anything after the number (query, fragment, trailing slash).
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		if c < '0' || c > '9' {
			tail = tail[:i]
			break
		}
	}
	if tail == "" {
		return "?"
	}
	return tail
}

// payloadString returns the string at key when it is present AND
// non-empty after TrimSpace. A missing key, wrong type, or
// empty-string value all return ok=false so the caller can fall
// through to the safe path.
func payloadString(p map[string]any, key string) (string, bool) {
	v, present := p[key]
	if !present {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

// payloadSlice returns the []any at key. JSON-unmarshalled payloads
// always shape slices as []any regardless of the original element
// type; the humanizer only cares about length so it doesn't reach
// into the elements.
func payloadSlice(p map[string]any, key string) ([]any, bool) {
	v, present := p[key]
	if !present {
		return nil, false
	}
	s, ok := v.([]any)
	if !ok {
		return nil, false
	}
	return s, true
}

// payloadInt returns the int at key. json.Unmarshal into
// map[string]any decodes every number as float64, so accept that
// shape AND a plain int (for in-process emitted events that haven't
// round-tripped through JSON). Positive values only — pr_number 0
// is "no PR yet".
func payloadInt(p map[string]any, key string) (int, bool) {
	v, present := p[key]
	if !present {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		if n <= 0 {
			return 0, false
		}
		return int(n), true
	case int:
		if n <= 0 {
			return 0, false
		}
		return n, true
	case int64:
		if n <= 0 {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

// humanizeEventType turns a machine event type ("plan.created",
// "rollout.stage_applied") into a short readable title for the
// timeline's Recent Events list. v0.81.4 (#545) — the pre-v0.81.4
// timeline emitted raw event_type strings, which read like log
// lines rather than postmortem prose. The v0.76 humanizer lives in
// the UI's AuditTimeline.tsx but Timeline.tsx renders backend-
// supplied titles directly, so humanizing has to happen server-
// side. This is intentionally a small subset of the v0.76 JS
// humanizer — the cleanup-grade scope is "the prominent plan.* and
// rollout.* family the operator stares at during an incident",
// not "every event type in the system". Unknown types fall back to
// a TitleCased version of the underscore-separated suffix so we
// never regress on what the operator saw before.
func humanizeEventType(eventType, action string) string {
	switch eventType {
	case "plan.created":
		return "Plan created"
	case "plan.approved":
		return "Plan approved"
	case "plan.rejected":
		return "Plan rejected"
	case "plan.cancelled":
		return "Plan cancelled"
	case "plan.completed":
		return "Plan completed"
	case "plan.step_started":
		return "Plan step started"
	case "plan.step_completed":
		return "Plan step completed"
	case "plan.rolled_back":
		return "Plan rolled back"
	case "rollout.created":
		return "Rollout created"
	case "rollout.approved":
		return "Rollout approved"
	case "rollout.rejected":
		return "Rollout rejected"
	case "rollout.stage_applied":
		return "Rollout stage applied"
	case "rollout.stage_advanced":
		return "Rollout stage advanced"
	case "rollout.succeeded":
		return "Rollout succeeded"
	case "rollout.aborted":
		return "Rollout aborted"
	case "rollout.paused":
		return "Rollout paused"
	case "rollout.resumed":
		return "Rollout resumed"
	case "rollout.rolled_back":
		return "Rollout rolled back"
	case "proposal.created":
		return "AI proposal created"
	case "proposal.declined":
		return "AI proposal declined"
	case "proposal.skipped":
		return "AI proposal skipped"
	case "proposal.evidence_linked":
		return "AI evidence linked"
	case "action.dispatched":
		return "Action dispatched"
	case "action.executed":
		return "Action succeeded"
	case "action.failed":
		return "Action failed"
	case "action.denied":
		return "Action denied"

	// v0.89.25 (#641) — humanizer coverage cleanup. Pre-this release,
	// 15 audit event types in the system fell through to the raw
	// event_type display because they predated the v0.81.4 humanizer
	// table (or were added later without the table being extended).
	// Operators reading the Timeline page saw "discovery.aws.scan_
	// completed" or "agent.drift.synced" verbatim alongside the
	// humanized "Rollout succeeded" — same family but inconsistent
	// presentation. This block closes the gap.
	//
	// Agent lifecycle (v0.4-ish — predates the table).
	case "agent.registered":
		return "Agent registered"
	case "agent.drift.synced":
		return "Agent config synced"
	case "agent.drift.drifted":
		return "Agent config drifted"
	// Config lifecycle (v0.4-ish).
	case "config.stored":
		return "Config stored"
	case "config.applied":
		return "Config applied"
	// Alert lifecycle (v0.3a alerts feature).
	case "alert_rule.created":
		return "Alert rule created"
	case "alert_rule.updated":
		return "Alert rule updated"
	case "alert_rule.deleted":
		return "Alert rule deleted"
	case "alert.fired":
		return "Alert fired"
	case "alert.resolved":
		return "Alert resolved"
	// Incident drafter (v0.54 Move 3).
	case "incident.drafted":
		return "Incident draft created"
	case "incident.draft_declined":
		return "Incident draft declined"
	// Discovery (v0.85+). The IaC + recommendation.pr_* events
	// already have a payload-aware humanizer at handleIaCAuditEvent;
	// the discovery.aws.* events are flat lifecycle events with no
	// payload-aware enrichment — they belong in the table.
	case "discovery.aws.connection_created":
		return "AWS connection created"
	case "discovery.aws.scan_completed":
		return "AWS scan completed"
	case "discovery.aws.scan_all_completed":
		return "Multi-account AWS scan completed"
	// v0.89.234 — discovery lifecycle humanizer coverage. The
	// v0.89.25 cleanup covered only 3 AWS discovery events; the rest
	// of the AWS lifecycle and the GCP / Azure / OCI connectors
	// (added in later slices) emitted audit events that fell through
	// to the raw event_type display on the Timeline. These flat
	// lifecycle events belong in the table alongside their AWS peers.
	case "discovery.aws.scan_started":
		return "AWS scan started"
	case "discovery.aws.recommendations_generated":
		return "AWS recommendations generated"
	case "discovery.aws.connection_read":
		return "AWS connection accessed"
	case "discovery.gcp.connection_created":
		return "GCP connection created"
	case "discovery.gcp.connection_deleted":
		return "GCP connection deleted"
	case "discovery.gcp.scan_started":
		return "GCP scan started"
	case "discovery.gcp.scan_completed":
		return "GCP scan completed"
	case "discovery.gcp.scan_failed":
		return "GCP scan failed"
	case "discovery.gcp.recommendations_generated":
		return "GCP recommendations generated"
	case "discovery.azure.connection_created":
		return "Azure connection created"
	case "discovery.azure.connection_deleted":
		return "Azure connection deleted"
	case "discovery.azure.scan_started":
		return "Azure scan started"
	case "discovery.azure.scan_completed":
		return "Azure scan completed"
	case "discovery.azure.scan_failed":
		return "Azure scan failed"
	case "discovery.azure.recommendations_generated":
		return "Azure recommendations generated"
	case "discovery.oci.connection_created":
		return "OCI connection created"
	case "discovery.oci.connection_deleted":
		return "OCI connection deleted"
	case "discovery.oci.scan_started":
		return "OCI scan started"
	case "discovery.oci.scan_completed":
		return "OCI scan completed"
	case "discovery.oci.scan_failed":
		return "OCI scan failed"
	case "discovery.oci.recommendations_generated":
		return "OCI recommendations generated"
	// v0.89.235 — humanizer coverage completeness pass. A diff of all
	// emitted audit EventType literals against this table surfaced
	// these lifecycle + discovery-request events that still fell
	// through to the raw event_type display. (recommendation.pr_* are
	// intentionally absent — they render via the payload-aware
	// humanizeIaCAuditEvent path above.)
	case "agent.decommissioned":
		return "Agent decommissioned"
	case "api_token.issued":
		return "API token issued"
	case "api_token.revoked":
		return "API token revoked"
	case "config.created":
		return "Config created"
	case "config.lint_evaluated":
		return "Config lint evaluated"
	case "incident.published":
		return "Incident published"
	case "incident.dismissed":
		return "Incident dismissed"
	case "plan.step_cancelled":
		return "Plan step cancelled"
	case "rollout.advanced":
		return "Rollout advanced"
	case "rollout.rollback_requested":
		return "Rollout rollback requested"
	case "discovery.summary.requested":
		return "Discovery summary requested"
	case "discovery.trace_coverage.requested":
		return "Trace-coverage report requested"
	case "discovery.span_quality.requested":
		return "Span-quality report requested"
	case "discovery.workload_health.requested":
		return "Workload-health report requested"
	// v0.89.28 (#643 slice 1) — discovery proposal lifecycle. The
	// payload-aware path at handleIaCAuditEvent above renders the
	// enriched title when verdict_examples_used is populated; this
	// entry is the cold-start fallback for an audit row missing the
	// payload-aware enrichment path (defensive: handleIaCAuditEvent
	// returns ok=true for non-empty payloads only).
	case "discovery_proposal.created":
		return "Discovery recommendations generated"
	// v0.89.26 (#642 Stream 43) — per-rollout exclude-from-learning
	// toggle for the #531 slice 2 feedback loop (§10 Q3). This is
	// the cold-path default the timeline renders when no payload-
	// aware enrichment fired. excludeFromLearningTitle below
	// inspects new_state to surface direction-aware wording
	// ("excluded" vs "re-included") the same way v0.89.22's
	// proposalCreatedTitle surfaces verdict counts.
	case "rollout.excluded_from_learning":
		return "AI proposal excluded from future learning"
	// v0.89.30 (#649) — webhook replay protection. Emitted when an
	// inbound GitHub webhook delivery passed HMAC verification but
	// collided with a previously-recorded X-GitHub-Delivery UUID.
	// Operators reading the timeline see this on the same humanizer
	// row as the pr_opened / pr_merged family so the replay context
	// is co-located with the rest of the IaC recommendation arc.
	case "webhook.delivery_replayed":
		return "Webhook delivery replayed"
	}
	// Fallback for event types not in the cleanup-grade table.
	// Preserves backwards compatibility with whatever the operator
	// saw before — we never want to lose information by humanizing.
	title := strings.TrimSpace(eventType)
	if title == "" {
		title = strings.TrimSpace(action)
	}
	return title
}

// planEmbeddedActionTitle returns a payload-aware title for an
// action.* event that the plan engine dispatched (plan_id +
// plan_step_index in the payload). Returns empty string for
// standalone action events so the caller falls back to the base
// humanizer entry. v0.89.14 (#630).
//
// Wording mirrors the spec's "Action <type> dispatched as part of
// plan <id_short> step <idx>" example with one normalization: the
// short plan id is the first 8 chars of the uuid, matching the
// same truncation rolloutsctl uses elsewhere.
func planEmbeddedActionTitle(eventType string, payload map[string]any) string {
	if payload == nil {
		return ""
	}
	switch eventType {
	case "action.dispatched", "action.executed", "action.failed", "action.denied":
		// fall through
	default:
		return ""
	}
	planID, ok := payloadString(payload, "plan_id")
	if !ok {
		return ""
	}
	stepIdx, hasIdx := payloadAnyInt(payload, "plan_step_index")
	actionType, _ := payloadString(payload, "action_type")
	shortPlan := planID
	if len(shortPlan) > 8 {
		shortPlan = shortPlan[:8]
	}
	verb := ""
	switch eventType {
	case "action.dispatched":
		verb = "dispatched"
	case "action.executed":
		verb = "succeeded"
	case "action.failed":
		verb = "failed"
	case "action.denied":
		verb = "denied"
	}
	prefix := "Action"
	if actionType != "" {
		prefix = "Action " + actionType
	}
	stepSuffix := ""
	if hasIdx {
		stepSuffix = fmt.Sprintf(" step %d", stepIdx)
	}
	switch verb {
	case "dispatched":
		return fmt.Sprintf("%s dispatched for plan %s%s", prefix, shortPlan, stepSuffix)
	case "succeeded":
		return fmt.Sprintf("%s succeeded for plan %s%s", prefix, shortPlan, stepSuffix)
	case "failed":
		if reason, ok := payloadString(payload, "denied_for"); ok {
			return fmt.Sprintf("%s failed for plan %s%s: %s", prefix, shortPlan, stepSuffix, reason)
		}
		return fmt.Sprintf("%s failed for plan %s%s", prefix, shortPlan, stepSuffix)
	case "denied":
		return fmt.Sprintf("%s denied by runner for plan %s%s", prefix, shortPlan, stepSuffix)
	}
	return ""
}

// proposalCreatedTitle returns a payload-aware title for a
// proposal.created event when the v0.89.17 (#633) feedback loop
// cited prior verdicts. Returns empty string for cold-start
// proposals (verdict_examples_used absent or empty) so the
// caller falls back to the base humanizer entry "AI proposal
// created". v0.89.22 (#638).
//
// The honest constraint: verdict_examples_used carries just the
// rollout IDs the proposer cited; v0.89.17 does NOT stamp the
// per-entry rejected/approved state on the audit payload.
// Surfacing the count is what we can honestly say without an
// N+1 lookup against the rollouts table at humanize time. The
// expanded payload row still shows the rollout IDs so an
// operator who wants to know which were rejections vs approvals
// can click into them.
func proposalCreatedTitle(eventType string, payload map[string]any) string {
	if payload == nil || eventType != "proposal.created" {
		return ""
	}
	// v0.89.37 (#657 Stream 55, #531 slice 2 chunk 6) — when the
	// audit row carries the per-state bucket map, prefer the
	// state-mix wording. The proposalCreatedByStateTitle helper
	// returns "" on empty/missing buckets so the fall-through to
	// the v0.89.22 flat-count phrasing is preserved for legacy
	// fixtures + cold-start rows (no by-state field at all).
	if t := proposalCreatedByStateTitle(payload); t != "" {
		return t
	}
	raw, ok := payload["verdict_examples_used"]
	if !ok {
		return ""
	}
	// The bridge writes an empty []string on cold start (not
	// omitted, per spec §8). The JSON round-trip lands as []any.
	// Treat empty slice the same as omitted — both mean "cold
	// start, no verdicts cited" and both should fall back to the
	// base "AI proposal created" title.
	n := 0
	switch v := raw.(type) {
	case []any:
		n = len(v)
	case []string:
		n = len(v)
	default:
		return ""
	}
	if n == 0 {
		return ""
	}
	plural := "verdicts"
	if n == 1 {
		plural = "verdict"
	}
	return fmt.Sprintf("AI proposal created (cited %d prior %s)", n, plural)
}

// proposalCreatedByStateTitle returns the v0.89.37 (#657 Stream 55,
// #531 slice 2 chunk 6) state-mix title for a proposal.created event
// whose payload carries verdict_examples_used_by_state. The output
// drops zero buckets and uses the cost-spike-surface state vocabulary
// ("approved" + "rejected"). Returns "" when:
//   - the by-state field is absent or the wrong shape (caller falls
//     back to the v0.89.22 flat-count phrasing),
//   - all buckets are empty (caller falls back to the cold-start
//     phrasing via the same flat-count path),
//   - only unknown-state buckets are populated (defensive — preserves
//     the flat-count fallback for forward-compat payloads).
//
// The bucket walk is in fixed approved-then-rejected order so the
// rendered title is deterministic regardless of map iteration.
func proposalCreatedByStateTitle(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	raw, ok := payload["verdict_examples_used_by_state"]
	if !ok {
		return ""
	}
	byState := payloadByStateMap(raw)
	if byState == nil {
		return ""
	}
	parts := []string{}
	if n := byState["approved"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d approved", n))
	}
	if n := byState["rejected"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d rejected", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("AI proposal created (informed by %s verdicts)", joinPlus(parts))
}

// discoveryProposalByStateTitle is the discovery-surface sibling of
// proposalCreatedByStateTitle. State vocabulary is per spec §8 (c):
// "merged" → "accepted PRs", "closed_not_merged" → "closed",
// "operator_excluded" → "excluded". Zero buckets are dropped; the
// "PRs" suffix attaches to the merged count only when it's the only
// non-zero bucket so multi-bucket lines stay readable ("informed by
// 1 accepted + 1 closed + 1 excluded"). Returns "" on the same
// fall-through conditions as proposalCreatedByStateTitle.
//
// v0.89.37 (#657 Stream 55, #531 slice 2 chunk 6).
func discoveryProposalByStateTitle(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	raw, ok := payload["verdict_examples_used_by_state"]
	if !ok {
		return ""
	}
	byState := payloadByStateMap(raw)
	if byState == nil {
		return ""
	}
	merged := byState["merged"]
	closed := byState["closed_not_merged"]
	excluded := byState["operator_excluded"]
	if merged == 0 && closed == 0 && excluded == 0 {
		return ""
	}
	parts := []string{}
	if merged > 0 {
		parts = append(parts, fmt.Sprintf("%d accepted", merged))
	}
	if closed > 0 {
		parts = append(parts, fmt.Sprintf("%d closed", closed))
	}
	if excluded > 0 {
		parts = append(parts, fmt.Sprintf("%d excluded", excluded))
	}
	// Single-bucket merged-only matches the v0.89.28 phrasing
	// ("informed by N accepted PRs") to keep the slice 1 humanizer
	// invariant when the by-state field is populated but only carries
	// the merged bucket — e.g. operators who haven't yet seen any
	// closed-without-merge or operator-excluded signal.
	if len(parts) == 1 && merged > 0 {
		plural := "PRs"
		if merged == 1 {
			plural = "PR"
		}
		return fmt.Sprintf("Discovery recommendations generated (informed by %d accepted %s)", merged, plural)
	}
	return fmt.Sprintf("Discovery recommendations generated (informed by %s)", joinPlus(parts))
}

// payloadByStateMap projects the raw audit payload value at
// verdict_examples_used_by_state into a map[state]count. Accepts the
// two shapes the audit pipeline produces:
//   - map[string]any with []any (JSON-round-tripped payloads),
//   - map[string][]string (in-process emits that bypass JSON).
//
// Returns nil for anything else so the humanizer falls through
// gracefully on malformed rows.
func payloadByStateMap(raw any) map[string]int {
	out := map[string]int{}
	switch m := raw.(type) {
	case map[string]any:
		for k, v := range m {
			switch arr := v.(type) {
			case []any:
				out[k] = len(arr)
			case []string:
				out[k] = len(arr)
			}
		}
	case map[string][]string:
		for k, v := range m {
			out[k] = len(v)
		}
	default:
		return nil
	}
	return out
}

// joinPlus joins parts with " + " so multi-bucket humanizer titles
// read as "1 approved + 2 rejected" rather than "1 approved, 2
// rejected". Pulled out of the call sites so the cost-spike and
// discovery branches share the same delimiter. v0.89.37 (#657
// Stream 55, #531 slice 2 chunk 6).
func joinPlus(parts []string) string {
	return strings.Join(parts, " + ")
}

// excludeFromLearningTitle returns a payload-aware title for a
// rollout.excluded_from_learning event. v0.89.26 (#642 Stream 43).
//
// The audit payload always carries `new_state` (per the documented
// contract — see services.AuditEventRolloutExcludedFromLearning).
// new_state=true → "AI proposal excluded from future learning"
// (matches the default table entry; we still return it so the
// dispatch chain stays consistent — the table entry is a fallback
// for the rare case where the payload is missing).
// new_state=false → "AI proposal re-included in future learning"
// so an operator scanning the timeline can tell at a glance which
// direction the toggle moved without cracking the payload row.
//
// Returns empty string for unrelated event types or when the
// payload is nil / missing new_state, so the caller falls back to
// the default humanizer entry.
func excludeFromLearningTitle(eventType string, payload map[string]any) string {
	if payload == nil || eventType != "rollout.excluded_from_learning" {
		return ""
	}
	raw, ok := payload["new_state"]
	if !ok {
		return ""
	}
	// JSON deserializes bools as bool directly. The case where the
	// bridge / service hands the humanizer a typed payload is
	// covered by the bool type assertion below. Anything else
	// (string "true", numeric 1) is intentionally unsupported —
	// the audit row is a public contract and we don't want lax
	// parsing to mask a producer-side regression.
	newState, ok := raw.(bool)
	if !ok {
		return ""
	}
	if newState {
		return "AI proposal excluded from future learning"
	}
	return "AI proposal re-included in future learning"
}

// payloadAnyInt extracts an int from a payload field that may be a
// float64 (JSON-decoded), an int, or an int64. Unlike payloadInt
// above it accepts 0 as a valid value — plan_step_index=0 is the
// first step. v0.89.14 (#630).
func payloadAnyInt(p map[string]any, key string) (int, bool) {
	v, present := p[key]
	if !present {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}
