// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package actions defines Squadron's signed-action protocol. It is
// the data and crypto layer underneath Move 2 (the action runner):
// the types here describe what an action request looks like on the
// wire, the registry holds the catalog of action types the control
// plane knows how to issue, and the signer produces requests that
// runner daemons (cmd/squadron-action-runner, future work) verify
// before executing.
//
// Design constraints (see docs/action-runner-design.md):
//
//   - Every action request is signed with Ed25519. Runners pin the
//     issuer public key at install time and refuse anything else.
//   - Action types are named and schema'd. There is no "run any
//     command" action; every action is a registered ActionType with
//     a declared parameter schema and a declared phase set.
//   - Capability declarations let the operator who installs a
//     runner constrain which named action types it will perform and
//     scope each one (e.g. "restart-systemd-service may only target
//     unit names matching squadron-* or nginx*"). Squadron refuses
//     to send out-of-policy requests; the runner refuses to execute
//     them. Defense in depth.
//   - Requests carry an expiry to stop replay; signed payload
//     includes issued_at and expires_at so a stale capture is
//     rejected by signature alone.
//
// The execute side (what each action actually does on the node)
// lives in the runner binary, not in this package. Squadron only
// owns the protocol description and the signer.
package actions

import (
	"encoding/json"
	"time"
)

// Phase distinguishes the two-step request flow: Squadron first
// asks the runner to dry-run an action and report what would
// happen; on operator approval, Squadron sends a second request
// with the same parameters and phase=execute. The runner verifies
// each phase independently so a captured dry_run request cannot be
// replayed as execute.
type Phase string

const (
	PhaseDryRun  Phase = "dry_run"
	PhaseExecute Phase = "execute"
)

// Request is the wire shape Squadron signs and sends to a runner.
// Parameters is action-type-specific JSON; the runner uses the
// registered ActionType to validate it.
type Request struct {
	RequestID  string          `json:"request_id"`
	ProposalID string          `json:"proposal_id"`
	RunnerID   string          `json:"runner_id"`
	Action     ActionPayload   `json:"action"`
	IssuedAt   time.Time       `json:"issued_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	Phase      Phase           `json:"phase"`
	Signature  string          `json:"signature"`
}

// ActionPayload is the action-specific part of a request.
// Parameters is opaque JSON the runner validates against the
// registered ActionType's schema.
type ActionPayload struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

// Result is the wire shape a runner returns after a phase
// completes. Status distinguishes execution outcome from policy
// outcome: success and failure mean the action ran (or attempted
// to); denied means the runner refused to execute it because the
// signature failed, the type was unknown, or the parameters fell
// outside the capability constraints.
type Result struct {
	RequestID   string         `json:"request_id"`
	Phase       Phase          `json:"phase"`
	Status      ResultStatus   `json:"status"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at"`
	Stdout      string         `json:"stdout,omitempty"`
	Stderr      string         `json:"stderr,omitempty"`
	ExitCode    int            `json:"exit_code,omitempty"`
	DeniedFor   string         `json:"denied_for,omitempty"` // signature / unknown_type / out_of_policy
	ResultData  map[string]any `json:"result_data,omitempty"`
}

// ResultStatus values.
type ResultStatus string

const (
	StatusSuccess ResultStatus = "success"
	StatusFailure ResultStatus = "failure"
	StatusDenied  ResultStatus = "denied"
)

// Capability is one capability a runner declares it can perform.
// Type names the registered ActionType; Constraints are optional
// per-type knobs (e.g. unit_name_glob for restart-systemd-service)
// the runner publishes at registration and Squadron checks before
// issuing a request.
type Capability struct {
	Type        string            `json:"type"`
	Constraints map[string]any    `json:"constraints,omitempty"`
}

// RunnerRegistration is the payload a runner sends at first start.
// PublicKeyPEM is the runner's own Ed25519 public key; Squadron
// uses it later for return-channel authentication if we add an
// optional mutual-TLS or runner-signed result mode. (The MVP uses
// HTTPS for runner-to-Squadron auth.)
type RunnerRegistration struct {
	RunnerID     string       `json:"runner_id"`
	Hostname     string       `json:"hostname"`
	PublicKeyPEM string       `json:"public_key_pem"`
	Capabilities []Capability `json:"capabilities"`
}

// signingPayload is what the signer hashes + signs. Kept here as
// the canonical serialization so signer and verifier agree on the
// bytes without needing a JSON normalization library. Fields are
// ordered for stability; new fields go at the end with a tag bump.
type signingPayload struct {
	RequestID  string        `json:"request_id"`
	ProposalID string        `json:"proposal_id"`
	RunnerID   string        `json:"runner_id"`
	Type       string        `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
	IssuedAt   time.Time     `json:"issued_at"`
	ExpiresAt  time.Time     `json:"expires_at"`
	Phase      Phase         `json:"phase"`
}

func (r *Request) signingBytes() ([]byte, error) {
	return json.Marshal(signingPayload{
		RequestID:  r.RequestID,
		ProposalID: r.ProposalID,
		RunnerID:   r.RunnerID,
		Type:       r.Action.Type,
		Parameters: r.Action.Parameters,
		IssuedAt:   r.IssuedAt,
		ExpiresAt:  r.ExpiresAt,
		Phase:      r.Phase,
	})
}
