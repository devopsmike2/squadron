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
	"net/http"
	"sort"
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
	title := strings.TrimSpace(e.EventType)
	if title == "" {
		title = e.Action
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
