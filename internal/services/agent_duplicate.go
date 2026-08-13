package services

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// LabelDuplicateDismissed is the reserved agent-label key that records an
// operator's "this is not a duplicate" decision. When present with value
// "true", detectDuplicates skips the agent so it stops being flagged.
//
// A label is used (rather than a dedicated column) because the flag only ever
// lands on TELEMETRY-ONLY agents, and telemetry-only agents never re-report an
// AgentDescription over OpAMP — so nothing clobbers their labels after the
// passive-discovery create. That makes the label a durable, migration-free
// persisted flag. The "squadron.io/" prefix marks it as Squadron-internal so
// the UI can hide it from the operator-facing label chips. See
// POST /api/v1/agents/:id/dismiss-duplicate.
const LabelDuplicateDismissed = "squadron.io/duplicate-dismissed"

// duplicateHostNameLabel is the resource-attribute label the detector keys off
// for a host identity. Both paths populate it: the OpAMP server copies host.name
// out of the AgentDescription's non-identifying attributes, and passive-OTLP
// discovery stamps it from the telemetry's host.name resource attribute.
const duplicateHostNameLabel = "host.name"

// isOpAMPManaged reports whether an agent is under OpAMP management. Reported
// OpAMP capabilities are the definitive signal: the passive-OTLP discovery path
// never sets any, so a non-empty capability set means a control connection was
// negotiated. This is the "A" side of a suspected-duplicate pair.
func isOpAMPManaged(a *Agent) bool {
	return len(a.Capabilities) > 0
}

// isTelemetryOnly reports whether an agent is a passive-OTLP registration — the
// "B" side of a suspected-duplicate pair. This is the passive-OTLP signature
// from the incident: discovered over OTLP, carrying NO OpAMP capabilities and NO
// reported version. Requiring all three keeps the detector conservative — an
// agent that reports a version or any capability is never treated as a phantom.
func isTelemetryOnly(a *Agent) bool {
	return a.DiscoverySource == "otlp" && len(a.Capabilities) == 0 && !versionKnown(a.Version)
}

// versionKnown reports whether an agent carries a real reported version.
// The OpAMP path defaults an absent version to the sentinel "unknown"; passive
// discovery leaves it empty. Both count as "no version".
func versionKnown(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	return v != "" && v != "unknown"
}

// agentHostName returns the agent's normalized host.name, or "" when unknown.
// Normalization (trim + lowercase) makes "Host-1" and "host-1" compare equal so
// a case-difference between the OpAMP description and the OTLP resource doesn't
// hide a real duplicate. Only the explicit host.name label is trusted — the
// detector never guesses a host identity from the display name, so an agent with
// no host.name is never flagged (per the incident's "never flag when host.name
// is empty/unknown" rule).
func agentHostName(a *Agent) string {
	if a == nil || a.Labels == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(a.Labels[duplicateHostNameLabel]))
}

// isDuplicateDismissed reports whether an operator has marked this agent as a
// confirmed non-duplicate via the dismiss action.
func isDuplicateDismissed(a *Agent) bool {
	if a == nil || a.Labels == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.Labels[LabelDuplicateDismissed]), "true")
}

// detectDuplicates annotates each telemetry-only agent that appears to shadow an
// OpAMP-managed agent by setting its SuspectedDuplicateOf field IN PLACE. It is
// pure and side-effect-free apart from that annotation, so it is cheap to run on
// every agents read (no background job, no persistence).
//
// The rule is deliberately one-directional and conservative:
//   - B is flagged only when B is telemetry-only (passive-OTLP signature),
//     A is OpAMP-managed, they share a non-empty normalized host.name, and
//     A.ID != B.ID.
//   - Two managed agents are never flagged against each other; a telemetry-only
//     agent is never named as the "real" one; an agent with no host.name, or one
//     the operator dismissed, is never flagged.
//
// When several managed agents share the host.name (which should not normally
// happen) the match is chosen deterministically — most-capable, then oldest,
// then lowest id — and the reason notes the ambiguity.
func detectDuplicates(agents []*Agent) {
	if len(agents) < 2 {
		return
	}

	// Index OpAMP-managed agents by normalized host.name.
	managedByHost := make(map[string][]*Agent)
	for _, a := range agents {
		if a == nil || !isOpAMPManaged(a) {
			continue
		}
		host := agentHostName(a)
		if host == "" {
			continue
		}
		managedByHost[host] = append(managedByHost[host], a)
	}
	if len(managedByHost) == 0 {
		return
	}

	for _, b := range agents {
		if b == nil || isDuplicateDismissed(b) || !isTelemetryOnly(b) {
			continue
		}
		host := agentHostName(b)
		if host == "" {
			continue
		}
		candidates := candidatesExcluding(managedByHost[host], b.ID)
		if len(candidates) == 0 {
			continue
		}
		best := pickManagedMatch(candidates)
		reason := fmt.Sprintf(
			"telemetry-only agent, same host.name %q as an OpAMP-managed agent", host)
		if len(candidates) > 1 {
			reason += " (multiple managed agents share this host.name; showing the most-capable/oldest)"
		}
		b.SuspectedDuplicateOf = &SuspectedDuplicate{
			OfAgentID:   best.ID.String(),
			OfAgentName: best.Name,
			Reason:      reason,
		}
	}
}

// candidatesExcluding drops any agent whose id equals exclude (the A.ID != B.ID
// guard) and returns the rest.
func candidatesExcluding(agents []*Agent, exclude uuid.UUID) []*Agent {
	out := make([]*Agent, 0, len(agents))
	for _, a := range agents {
		if a.ID == exclude {
			continue
		}
		out = append(out, a)
	}
	return out
}

// pickManagedMatch chooses one managed agent deterministically from a set that
// shares a host.name: most capabilities first, then oldest CreatedAt, then
// lowest id string. Callers guarantee a non-empty slice.
func pickManagedMatch(candidates []*Agent) *Agent {
	sorted := make([]*Agent, len(candidates))
	copy(sorted, candidates)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if len(a.Capabilities) != len(b.Capabilities) {
			return len(a.Capabilities) > len(b.Capabilities)
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID.String() < b.ID.String()
	})
	return sorted[0]
}
