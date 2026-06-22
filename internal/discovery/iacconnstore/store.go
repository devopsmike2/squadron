// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacconnstore

import (
	"context"
	"errors"
	"time"
)

// Provider names the IaC hosting platform a connection points at.
// Slice 1 of #603 ships GitHub only; the field is a string-typed
// discriminator so adding GitLab / Bitbucket / Azure DevOps later
// does not require a schema change.
const (
	// ProviderGitHub is the only supported provider in slice 1.
	// docs/proposals/603-connect-iac-repo.md §2 names GitLab,
	// Bitbucket, and Azure DevOps as explicit non-goals for slice 1.
	ProviderGitHub = "github"
)

// Authentication kinds for a connection. Slice 1 ships PAT only;
// the design doc §4 calls out a GitHub App path for slice 2, hence
// the constant lives here from day one so callers don't string-type
// the discriminator.
const (
	// AuthKindPAT is the slice-1 path: a classic GitHub Personal
	// Access Token with the `repo` scope. Lives behind an Advanced
	// disclosure in the wizard; Compliance Pack hardening disables
	// this path (design doc §4).
	AuthKindPAT = "pat"

	// AuthKindGitHubApp is the slice-2 path: a GitHub App installed
	// per-repo. Reserved here so the storage shape does not change
	// between slices; the marshal helper for it lands with the App
	// implementation.
	AuthKindGitHubApp = "app"
)

// Repo layout kinds for a connection. The wizard captures this at
// connect time so the PR builder can pick the right
// branching / commit strategy without re-asking the operator.
const (
	// RepoLayoutMono is a single-repo terraform layout: everything
	// the operator manages lives in one repo. The PR builder targets
	// the placement-map file directly.
	RepoLayoutMono = "mono"

	// RepoLayoutMulti is a multi-repo terraform layout where the
	// connected repo is one of several. Slice 1 still ships one
	// connection per deployment; the discriminator is captured so
	// slice 1.5 (multi-repo per connection) is a non-breaking
	// extension.
	RepoLayoutMulti = "multi"
)

// DefaultBranchPrefix is the branch-name prefix the PR builder uses
// when the operator did not pick a custom one at connect time.
// Stored on the connection as an empty string when the operator
// accepted the default; the PR builder substitutes this constant.
const DefaultBranchPrefix = "squadron/rec"

// ErrConnectionNotFound is returned by Get when no row matches the
// supplied connection_id. Callers errors.Is against this sentinel to
// distinguish "no such connection" from infrastructure-side
// failures.
var ErrConnectionNotFound = errors.New("iacconnstore: connection not found")

// ErrConnectionConflict is returned by Create when a row already
// exists for the (provider, repo_full_name) pair. The spec calls for
// one connection per repo at the deployment scope; the unique index
// makes this a hard error at the substrate boundary instead of a
// silent overwrite. Operators rotate credentials by Delete +
// Create — there is no Update path in slice 1.
var ErrConnectionConflict = errors.New("iacconnstore: a connection already exists for this provider+repo")

// PlacementMapEntry tells the PR builder which file to append the
// snippet to for a given (provider, resource_kind) pair. Operators
// declare these rows at connect time; if a recommendation arrives
// for a kind with no row, the UI replaces "Open PR" with a tooltip
// naming the missing row (design doc §6).
//
// The canonical resource_kind strings for slice 1 (per design doc
// §6) are: ec2-otel-layer, lambda-otel-layer, rds-pi-em,
// s3-access-logging, alb-access-logs, eks-cluster-logging,
// eks-observability-addon. The substrate does not validate against
// this list — new kinds are additive — but the wizard pre-populates
// these seven rows.
type PlacementMapEntry struct {
	// Provider names the cloud the resource_kind targets (e.g.
	// "aws"). Lowercase, matches the credstore Provider values.
	Provider string `json:"provider"`

	// ResourceKind is the proposer-emitted kind string (e.g.
	// "eks-cluster-logging") that the placement applies to.
	ResourceKind string `json:"resource_kind"`

	// FilePath is the repo-relative path the snippet will be
	// appended to (e.g. "modules/eks/main.tf"). The substrate does
	// not validate the path's existence — the wizard's Validate
	// step does that against the live repo before Save.
	FilePath string `json:"file_path"`
}

// IaCConnection is one connected IaC repository. Provider-specific
// authentication material lives in CredCiphertext as a sealed
// blob — the substrate stores opaque bytes and the marshal helpers
// in github_pat.go (and, in slice 2, github_app.go) handle the
// per-auth-kind plaintext shape.
//
// CredCiphertext is opaque to the substrate. Get and List populate it
// with the on-disk bytes; only the per-auth-kind Unmarshal helper
// (called by the GitHub-client wrapper in phase 2) ever sees
// plaintext. The substrate's audit / log surface never includes
// CredCiphertext.
type IaCConnection struct {
	// ConnectionID is the substrate's primary key, a UUIDv4 stamped
	// by Create. Callers do not supply this on Create.
	ConnectionID string `json:"connection_id"`

	// Provider names the IaC hosting platform. Slice 1: "github".
	Provider string `json:"provider"`

	// AuthKind discriminates the CredCiphertext shape: "pat" in
	// slice 1, "app" in slice 2.
	AuthKind string `json:"auth_kind"`

	// RepoFullName is the canonical "owner/repo" identifier on the
	// provider. Unique per provider — the substrate enforces this
	// via a unique index.
	RepoFullName string `json:"repo_full_name"`

	// DefaultBranch is the repo's default branch as observed at
	// connect time. Used as the PR base and (combined with
	// BranchPrefix) the head-branch parent.
	DefaultBranch string `json:"default_branch"`

	// RepoLayout is "mono" or "multi". See the RepoLayout
	// constants for slice-1 semantics.
	RepoLayout string `json:"repo_layout"`

	// BranchPrefix is the operator's chosen prefix for PR branches.
	// Empty string means "use DefaultBranchPrefix at PR time" so the
	// substrate does not have to materialize the default into every
	// row.
	BranchPrefix string `json:"branch_prefix,omitempty"`

	// ReviewerTeamHandle is the optional "org/team" handle that the
	// PR builder requests review from. Empty string means no team
	// requested.
	ReviewerTeamHandle string `json:"reviewer_team_handle,omitempty"`

	// PlacementMap is the operator-declared list of
	// (provider, resource_kind, file_path) entries. Nil and empty
	// slice are equivalent: no rows configured.
	PlacementMap []PlacementMapEntry `json:"placement_map"`

	// CredCiphertext is the sealed authentication blob. Opaque to
	// the substrate; produced by MarshalGitHubPATCreds (slice 1) or
	// the future MarshalGitHubAppCreds (slice 2). Never logged,
	// never emitted in an audit payload.
	CredCiphertext []byte `json:"-"`

	// LearnFromAcceptedRecommendations is the v0.89.28 (#643 slice 1)
	// opt-in flag for the discovery proposer's accepted-examples
	// feedback loop. Default true: every new connection participates
	// in the loop unless the operator explicitly opts out via PATCH
	// /api/v1/iac/github/connections/:id. Mirrors v0.89.17's
	// Group.LearnFromVerdicts posture for the cost-spike side; an
	// operator with a per-connection privacy concern (PR titles
	// implying sensitive workload identity, etc.) can flip the flag
	// off and the proposer's prompt block goes silent for that
	// connection without any other state change.
	LearnFromAcceptedRecommendations bool `json:"learn_from_accepted_recommendations"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is the IaC-connection substrate interface. Implementations
// persist IaCConnection rows with their CredCiphertext sealed by the
// credstore Key. The interface returns plain *IaCConnection values:
// the CredCiphertext field carries the on-disk sealed bytes, never
// plaintext. Decryption is the GitHub-client wrapper's job (phase 2),
// not the substrate's.
//
// All methods are safe for concurrent use; the SQLite-backed default
// serializes writes through the database pool, and the memory-backed
// implementation uses a sync.RWMutex.
//
// Slice 1 intentionally omits an Update method for the credential
// blob. Operators rotate by Delete + Create — that path costs one
// extra wizard round-trip and removes a class of "did the rotation
// also touch the placement map?" bugs. UpdatePlacementMap is the
// only post-Create mutation.
type Store interface {
	// Create inserts a new connection row. The connection's
	// ConnectionID is generated by the substrate (callers leave it
	// empty) and the stamped value is returned by mutating the
	// passed-in struct's ConnectionID field. CreatedAt and UpdatedAt
	// are stamped to now.
	//
	// Returns ErrConnectionConflict (errors.Is-comparable) when a
	// row already exists for (Provider, RepoFullName). All other
	// failures wrap the underlying database error.
	Create(ctx context.Context, conn *IaCConnection) error

	// Get returns the connection row for the given ConnectionID.
	// Returns ErrConnectionNotFound (errors.Is-comparable) when no
	// row matches. CredCiphertext is populated with the sealed
	// on-disk bytes.
	Get(ctx context.Context, connectionID string) (*IaCConnection, error)

	// GetByRepoFullName returns the most recently created connection
	// row whose RepoFullName equals repoFullName, or
	// ErrConnectionNotFound if no row matches. v0.89.23 (#639 Stream
	// 40) — used by the GitHub webhook receiver to look up the
	// connection corresponding to an incoming pull_request event so
	// the recommendation.pr_merged audit row can carry the matching
	// connection_id.
	//
	// Slice 1 of #639 ships one connection per repo as a hard
	// invariant (the (provider, repo_full_name) unique index
	// enforces it for the GitHub provider); the "newest first" order
	// is a forward-compatibility hedge for the slice-2 question of
	// allowing multiple connections per repo, so callers don't have
	// to choose between them when that gate opens.
	GetByRepoFullName(ctx context.Context, repoFullName string) (*IaCConnection, error)

	// List returns every connection row, ordered by CreatedAt
	// ascending. Empty store returns an empty slice and no error.
	List(ctx context.Context) ([]*IaCConnection, error)

	// Delete removes the row for the given ConnectionID. Idempotent:
	// deleting a non-existent row is not an error. This is also the
	// credential-rotation path — operators delete and re-run the
	// connect wizard rather than mutating an in-place row.
	Delete(ctx context.Context, connectionID string) error

	// UpdatePlacementMap replaces the PlacementMap on the connection
	// with the supplied slice. UpdatedAt is stamped to now. No other
	// column is touched. Returns ErrConnectionNotFound if no row
	// matches.
	//
	// A nil slice is treated as an empty list (clears the map).
	UpdatePlacementMap(ctx context.Context, connectionID string, entries []PlacementMapEntry) error

	// UpdateLearnFromAcceptedRecommendations sets the per-connection
	// opt-in flag for the discovery proposer's accepted-examples
	// feedback loop. v0.89.28 (#643 slice 1). UpdatedAt is stamped
	// to now; no other column is touched. Returns
	// ErrConnectionNotFound if no row matches.
	UpdateLearnFromAcceptedRecommendations(ctx context.Context, connectionID string, learn bool) error

	// Close releases the underlying database handle. Subsequent
	// calls to other methods return an error.
	Close() error
}
