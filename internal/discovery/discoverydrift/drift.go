// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package discoverydrift diffs two persisted discovery scans of the same scope
// and reports what changed: resources added/removed (by resource_id) and
// instrumentation flips (OTel turned on/off) for the categories that carry a
// single boolean. It is the payoff of scan persistence (continuous-discovery
// slices 1-3): "what changed in my fleet since last week?"
//
// It is cloud-agnostic: every cloud's persisted scan JSON uses the same field
// names for the shared snapshot types (resource_id / has_otel /
// has_otel_layer), so one parser covers AWS / GCP / Azure / OCI.
package discoverydrift

import "encoding/json"

type computeRow struct {
	ResourceID string `json:"resource_id"`
	HasOTel    bool   `json:"has_otel"`
}

type functionRow struct {
	ResourceID   string `json:"resource_id"`
	HasOTelLayer bool   `json:"has_otel_layer"`
}

type simpleRow struct {
	ResourceID string `json:"resource_id"`
}

// inventory is the minimal projection of a persisted scan the diff needs.
type inventory struct {
	Compute   []computeRow  `json:"compute"`
	Functions []functionRow `json:"functions"`
	Databases []simpleRow   `json:"databases"`
	Clusters  []simpleRow   `json:"clusters"`
}

// Flip is an instrumentation change on a resource present in both scans.
type Flip struct {
	ResourceID string `json:"resource_id"`
	Was        bool   `json:"was"`
	Now        bool   `json:"now"`
}

// CategoryDiff is the change set for one resource category.
type CategoryDiff struct {
	Added                  []string `json:"added"`
	Removed                []string `json:"removed"`
	InstrumentationChanged []Flip   `json:"instrumentation_changed,omitempty"`
}

// Diff is the full change set between two scans (from = older, to = newer).
type Diff struct {
	Compute   CategoryDiff `json:"compute"`
	Functions CategoryDiff `json:"functions"`
	Databases CategoryDiff `json:"databases"`
	Clusters  CategoryDiff `json:"clusters"`

	TotalAdded                  int `json:"total_added"`
	TotalRemoved                int `json:"total_removed"`
	TotalInstrumentationChanged int `json:"total_instrumentation_changed"`
}

// Between parses two persisted scan result blobs (older, newer) and returns the
// diff. A blank blob parses as an empty inventory (so a first scan vs. nothing
// reports everything as added).
func Between(olderJSON, newerJSON string) (Diff, error) {
	older, err := parseInventory(olderJSON)
	if err != nil {
		return Diff{}, err
	}
	newer, err := parseInventory(newerJSON)
	if err != nil {
		return Diff{}, err
	}

	var d Diff
	d.Compute = diffCompute(older.Compute, newer.Compute)
	d.Functions = diffFunctions(older.Functions, newer.Functions)
	d.Databases = diffSimple(older.Databases, newer.Databases)
	d.Clusters = diffSimple(older.Clusters, newer.Clusters)

	for _, cd := range []CategoryDiff{d.Compute, d.Functions, d.Databases, d.Clusters} {
		d.TotalAdded += len(cd.Added)
		d.TotalRemoved += len(cd.Removed)
		d.TotalInstrumentationChanged += len(cd.InstrumentationChanged)
	}
	return d, nil
}

func parseInventory(s string) (inventory, error) {
	var inv inventory
	if s == "" {
		return inv, nil
	}
	if err := json.Unmarshal([]byte(s), &inv); err != nil {
		return inventory{}, err
	}
	return inv, nil
}

// addedRemoved returns ids present only in newer (added) and only in older
// (removed). Deterministic order: input order of the respective slice.
func addedRemoved(oldIDs, newIDs map[string]bool, oldOrder, newOrder []string) (added, removed []string) {
	added = []string{}
	removed = []string{}
	for _, id := range newOrder {
		if !oldIDs[id] {
			added = append(added, id)
		}
	}
	for _, id := range oldOrder {
		if !newIDs[id] {
			removed = append(removed, id)
		}
	}
	return added, removed
}

func diffCompute(older, newer []computeRow) CategoryDiff {
	oldIDs, newIDs := map[string]bool{}, map[string]bool{}
	oldOtel, newOtel := map[string]bool{}, map[string]bool{}
	var oldOrder, newOrder []string
	for _, r := range older {
		oldIDs[r.ResourceID] = true
		oldOtel[r.ResourceID] = r.HasOTel
		oldOrder = append(oldOrder, r.ResourceID)
	}
	for _, r := range newer {
		newIDs[r.ResourceID] = true
		newOtel[r.ResourceID] = r.HasOTel
		newOrder = append(newOrder, r.ResourceID)
	}
	cd := CategoryDiff{}
	cd.Added, cd.Removed = addedRemoved(oldIDs, newIDs, oldOrder, newOrder)
	for _, id := range newOrder {
		if oldIDs[id] && oldOtel[id] != newOtel[id] {
			cd.InstrumentationChanged = append(cd.InstrumentationChanged, Flip{ResourceID: id, Was: oldOtel[id], Now: newOtel[id]})
		}
	}
	return cd
}

func diffFunctions(older, newer []functionRow) CategoryDiff {
	oldIDs, newIDs := map[string]bool{}, map[string]bool{}
	oldOtel, newOtel := map[string]bool{}, map[string]bool{}
	var oldOrder, newOrder []string
	for _, r := range older {
		oldIDs[r.ResourceID] = true
		oldOtel[r.ResourceID] = r.HasOTelLayer
		oldOrder = append(oldOrder, r.ResourceID)
	}
	for _, r := range newer {
		newIDs[r.ResourceID] = true
		newOtel[r.ResourceID] = r.HasOTelLayer
		newOrder = append(newOrder, r.ResourceID)
	}
	cd := CategoryDiff{}
	cd.Added, cd.Removed = addedRemoved(oldIDs, newIDs, oldOrder, newOrder)
	for _, id := range newOrder {
		if oldIDs[id] && oldOtel[id] != newOtel[id] {
			cd.InstrumentationChanged = append(cd.InstrumentationChanged, Flip{ResourceID: id, Was: oldOtel[id], Now: newOtel[id]})
		}
	}
	return cd
}

func diffSimple(older, newer []simpleRow) CategoryDiff {
	oldIDs, newIDs := map[string]bool{}, map[string]bool{}
	var oldOrder, newOrder []string
	for _, r := range older {
		oldIDs[r.ResourceID] = true
		oldOrder = append(oldOrder, r.ResourceID)
	}
	for _, r := range newer {
		newIDs[r.ResourceID] = true
		newOrder = append(newOrder, r.ResourceID)
	}
	cd := CategoryDiff{}
	cd.Added, cd.Removed = addedRemoved(oldIDs, newIDs, oldOrder, newOrder)
	return cd
}
