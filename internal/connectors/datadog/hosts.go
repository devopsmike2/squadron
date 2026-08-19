// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"sort"
	"strings"
	"time"
)

// rawHostsResponse is the JSON envelope of GET /api/v1/hosts. Only the
// fields this slice consumes are modeled; Datadog adds fields freely, and
// encoding/json ignores the rest.
type rawHostsResponse struct {
	HostList      []rawHost `json:"host_list"`
	TotalReturned int       `json:"total_returned"`
	TotalMatching int       `json:"total_matching"`
}

// rawHost is one entry of host_list as Datadog returns it.
type rawHost struct {
	Name             string              `json:"name"`
	HostName         string              `json:"host_name"`
	Aliases          []string            `json:"aliases"`
	Up               bool                `json:"up"`
	LastReportedTime int64               `json:"last_reported_time"`
	IsMuted          bool                `json:"is_muted"`
	Apps             []string            `json:"apps"`
	Sources          []string            `json:"sources"`
	TagsBySource     map[string][]string `json:"tags_by_source"`
	AWSName          string              `json:"aws_name"`
}

// Host is the vendor-neutral, normalized inventory record for one host
// Datadog is tracking. It is the "what a connector produces" shape for
// this slice — a fleet roster entry, deliberately distinct from the
// query framework's metric/log/scalar result model.
type Host struct {
	// Name is the canonical Datadog host name.
	Name string `json:"name"`

	// Aliases are the other identities Datadog knows this host by
	// (host_name, cloud instance names, FQDNs). Deduplicated, canonical
	// Name excluded, sorted for stable output. These are what correlation
	// matches against so a host reporting to Squadron under an FQDN still
	// matches a Datadog record keyed by an instance id.
	Aliases []string `json:"aliases,omitempty"`

	// Up reports whether Datadog currently considers the host up.
	Up bool `json:"up"`

	// Muted reports whether the host is muted in Datadog.
	Muted bool `json:"muted"`

	// LastReportedAt is when the host last reported to Datadog. Zero when
	// the API returned no timestamp.
	LastReportedAt time.Time `json:"last_reported_at,omitempty"`

	// Sources lists the integrations reporting this host (e.g. "agent",
	// "aws"). Sorted for stable output.
	Sources []string `json:"sources,omitempty"`

	// Apps lists the apps/products observed on the host. Sorted.
	Apps []string `json:"apps,omitempty"`

	// Tags is the flattened, deduplicated set of tags across every source.
	// Sorted for stable output.
	Tags []string `json:"tags,omitempty"`
}

// normalizeHost converts a raw API host into the neutral Host model.
func normalizeHost(r rawHost) Host {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = strings.TrimSpace(r.HostName)
	}

	// Collect alias candidates, excluding the canonical name, deduped.
	aliasSet := map[string]struct{}{}
	addAlias := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || strings.EqualFold(s, name) {
			return
		}
		aliasSet[s] = struct{}{}
	}
	addAlias(r.HostName)
	addAlias(r.AWSName)
	for _, a := range r.Aliases {
		addAlias(a)
	}

	var last time.Time
	if r.LastReportedTime > 0 {
		last = time.Unix(r.LastReportedTime, 0).UTC()
	}

	// Flatten tags across all sources, deduped.
	tagSet := map[string]struct{}{}
	for _, tags := range r.TagsBySource {
		for _, t := range tags {
			if t = strings.TrimSpace(t); t != "" {
				tagSet[t] = struct{}{}
			}
		}
	}

	return Host{
		Name:           name,
		Aliases:        sortedKeys(aliasSet),
		Up:             r.Up,
		Muted:          r.IsMuted,
		LastReportedAt: last,
		Sources:        dedupSorted(r.Sources),
		Apps:           dedupSorted(r.Apps),
		Tags:           sortedKeys(tagSet),
	}
}

// identities returns every name this host could be matched by (canonical
// name plus aliases), normalized for comparison. Used by correlation.
func (h Host) identities() []string {
	out := make([]string, 0, len(h.Aliases)+1)
	if n := NormalizeHostname(h.Name); n != "" {
		out = append(out, n)
	}
	for _, a := range h.Aliases {
		if n := NormalizeHostname(a); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// NormalizeHostname canonicalizes a hostname for correlation: trimmed and
// lowercased. Datadog and OTel may disagree on case; nothing more
// aggressive (e.g. stripping domains) is done in this slice because that
// would risk false matches across distinct hosts.
func NormalizeHostname(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Coverage is the correlation verdict for a single Datadog host against
// the OTel fleet.
type Coverage struct {
	// Host is the Datadog inventory record.
	Host Host

	// Covered reports whether this host matched a host in Squadron's OTel
	// fleet (i.e. it IS reporting OTel telemetry to Squadron).
	Covered bool

	// MatchedFleetID is the fleet identity (the value the fleet index
	// mapped the matched hostname to, typically an agent id) when Covered
	// is true; empty otherwise.
	MatchedFleetID string

	// MatchedOn is the normalized hostname that produced the match; empty
	// when not covered. Useful for explaining a match in the UI.
	MatchedOn string
}

// GapReport is the observability-gap correlation result: which Datadog
// hosts are covered by Squadron's OTel fleet and which are NOT. The gaps
// are the demo-valuable output — hosts Datadog sees that Squadron is not
// receiving OTel from.
type GapReport struct {
	// TotalDatadogHosts is the number of hosts Datadog reported.
	TotalDatadogHosts int `json:"total_datadog_hosts"`

	// CoveredCount and GapCount are the split of TotalDatadogHosts.
	CoveredCount int `json:"covered_count"`
	GapCount     int `json:"gap_count"`

	// Covered lists the hosts present in both Datadog and the OTel fleet.
	Covered []Coverage `json:"covered"`

	// Gaps lists the hosts present in Datadog but NOT reporting OTel to
	// Squadron — the observability gap.
	Gaps []Coverage `json:"gaps"`
}

// CorrelateFleet correlates a Datadog host roster against Squadron's OTel
// fleet and returns the observability-gap report.
//
// fleetIndex maps a normalized hostname (see NormalizeHostname) to a fleet
// identity (typically an agent id) for every host Squadron is receiving
// OTel telemetry from. The caller builds it from the agents table — a
// host is keyed by its Labels["host.name"] (the authoritative host
// identity both the OpAMP and OTLP discovery paths populate), falling back
// to the display name. Keeping this function index-driven (rather than
// taking the agent type directly) keeps it pure and unit-testable and
// keeps this package free of an application-store dependency.
//
// A Datadog host counts as covered when ANY of its identities (canonical
// name or an alias) is present in fleetIndex. The result slices are
// deterministic: input order is preserved, and a nil fleetIndex yields an
// all-gap report (every Datadog host is a gap), which is the correct
// degraded answer when the fleet is empty or unavailable.
func CorrelateFleet(hosts []Host, fleetIndex map[string]string) GapReport {
	report := GapReport{
		TotalDatadogHosts: len(hosts),
		Covered:           []Coverage{},
		Gaps:              []Coverage{},
	}

	for _, h := range hosts {
		cov := Coverage{Host: h}
		for _, id := range h.identities() {
			if fleetID, ok := fleetIndex[id]; ok {
				cov.Covered = true
				cov.MatchedFleetID = fleetID
				cov.MatchedOn = id
				break
			}
		}
		if cov.Covered {
			report.Covered = append(report.Covered, cov)
		} else {
			report.Gaps = append(report.Gaps, cov)
		}
	}

	report.CoveredCount = len(report.Covered)
	report.GapCount = len(report.Gaps)
	return report
}

// dedupSorted returns a deduplicated, trimmed, sorted copy of in, dropping
// empties. Returns nil for an all-empty/nil input so the JSON omitempty
// tags elide the field.
func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	set := map[string]struct{}{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			set[s] = struct{}{}
		}
	}
	return sortedKeys(set)
}

// sortedKeys returns the map keys sorted, or nil for an empty set.
func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
