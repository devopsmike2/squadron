// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package policy defines the boundary between the open-core rollout
// engine and the Compliance Pack's enforcement layer.
//
// Squadron OSS ships with the basic two-person approval workflow:
// a requester can mark a rollout RequireApproval=true at create time,
// and the engine refuses to advance the rollout until an approver
// calls Approve. The operator chooses, per rollout, whether the
// gate applies.
//
// Squadron Compliance Pack adds enforced policy: groups can be
// marked require_approval=true and the rollout service forces every
// rollout for that group into pending_approval regardless of what
// the requester sent. This is the actual NERC CIP / SOC 2 control —
// it removes operator discretion. The Pack also lights up the
// per-group enforcement audit payload (approval_enforced_by_policy)
// that compliance evidence packaging keys off of.
//
// The boundary is this package. It lives under extension/ (not
// internal/) so the squadron-compliance private repo can import it
// across module boundaries. The OSS binary wires NoOpProvider,
// which returns false for every group. The Compliance Pack binary
// wires its own implementation, which reads the group's
// require_approval column via the GroupReader minimal interface.
package policy

import "context"

// Group is the minimal shape of a group record that the policy
// extension boundary needs. The open core's applicationstore.Group
// is adapted into this shape at the wire layer so the Compliance
// Pack never imports internal/ packages directly.
type Group struct {
	ID              string
	RequireApproval bool
	// RequiredApprovals is the per-group minimum number of DISTINCT
	// approvers the group's compliance policy mandates (ADR 0029 —
	// N-of-M approvals). 0 means the group carries no mandate, so the
	// rollout service's max(input, provider, 1) resolution leaves the
	// open-core default of a single distinct approver in place. A
	// Compliance Pack wire layer projects a group's configured minimum
	// onto this field so its GroupPolicyProvider can surface it from
	// RequiredApprovals; the OSS build never populates it (NoOpProvider
	// ignores groups entirely).
	RequiredApprovals int
}

// GroupReader is the minimal storage surface a policy provider
// needs. The open core's application store is adapted into this
// interface at the wire layer. The Compliance Pack consumes
// GroupReader rather than the full ApplicationStore so the
// extension surface stays narrow and the internal/ packages remain
// internal.
type GroupReader interface {
	ReadGroup(ctx context.Context, id string) (*Group, error)
}

// GroupPolicyProvider is the boundary between open-core rollout
// orchestration and Compliance Pack enforcement. The rollout service
// holds a reference to a GroupPolicyProvider and consults it when
// creating a rollout; the provider is responsible for telling the
// service whether the group's policy mandates approval.
//
// nil GroupPolicyProvider is a valid runtime state: the rollout
// service treats it identically to NoOpProvider. Wiring a provider
// is the operator's opt-in to enforcement.
type GroupPolicyProvider interface {
	// RequiresApproval returns true if the group's compliance policy
	// mandates approval on every rollout into the group. When true,
	// the rollout service overrides the requester's RequireApproval
	// flag to true and records an audit payload field
	// (approval_enforced_by_policy=true) so the evidence trail can
	// distinguish operator-elected approvals from policy-enforced
	// ones.
	//
	// The OSS implementation always returns false (no enforcement).
	// The Compliance Pack implementation reads from the group's
	// require_approval setting.
	//
	// Implementations must be safe for concurrent use. The rollout
	// service calls this on every Create.
	RequiresApproval(ctx context.Context, groupID string) bool

	// RequiredApprovals returns the number of DISTINCT approvers the
	// group's compliance policy mandates for every rollout into the
	// group (ADR 0029 — N-of-M approvals). The rollout service floors
	// the returned value into its Create-time resolution:
	//
	//     required = max(input.RequiredApprovals, provider.RequiredApprovals(...), 1)
	//
	// so a provider that doesn't care returns 0 and the OSS default of
	// a single distinct approver stands. A Compliance Pack provider
	// returns e.g. 2 or 3 to enforce N-of-M at the group level,
	// removing operator discretion the same way RequiresApproval does
	// for the on/off gate.
	//
	// The OSS implementation always returns 0 (no enforcement).
	// Implementations must be safe for concurrent use.
	RequiredApprovals(ctx context.Context, groupID string) int
}

// NoOpProvider is the open-core default. It returns false for every
// group, which means: groups can carry a require_approval marker as
// metadata, but the rollout engine does not enforce it. Operators
// who need enforcement run the Compliance Pack build, which wires
// its own provider.
//
// The zero value is usable. The rollout service treats nil and
// NoOpProvider{} identically.
type NoOpProvider struct{}

// RequiresApproval implements GroupPolicyProvider and always returns
// false. See package doc for why.
func (NoOpProvider) RequiresApproval(_ context.Context, _ string) bool { return false }

// RequiredApprovals implements GroupPolicyProvider and always returns 0,
// which the rollout service floors to the OSS default of 1 distinct
// approver. See package doc and the interface method doc for why.
func (NoOpProvider) RequiredApprovals(_ context.Context, _ string) int { return 0 }
