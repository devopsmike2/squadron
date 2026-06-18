// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package actions

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ActionType is the static description of one named action. It
// declares the type name (which appears in Request.Action.Type),
// the parameter schema validator (ValidateParameters), and the
// capability-constraint matcher (MatchesCapability). The actual
// execution lives in the runner binary; Squadron only owns this
// description.
//
// New action types are added by calling Registry.Register at
// process init time, typically from an init() in a small per-type
// file (see register_restart_systemd.go). This keeps the catalog
// declarative.
type ActionType struct {
	// Type is the canonical name used on the wire.
	Type string

	// Description is a one-line human-readable summary surfaced in
	// the UI and the OpenAPI spec.
	Description string

	// ValidateParameters checks that a parameters payload matches
	// this action's schema. Called both at proposal time (in the
	// control plane, on the proposed RolloutInput-equivalent) and
	// at request time (in the runner, before execution). Returning
	// a non-nil error rejects the action.
	ValidateParameters func(json.RawMessage) error

	// MatchesCapability tests whether a Capability declared by a
	// runner permits this action with these parameters. Used both
	// in the control plane (to refuse signing out-of-policy
	// requests) and in the runner (defense in depth). Returns the
	// reason for any mismatch so audit can be precise.
	MatchesCapability func(params json.RawMessage, c Capability) (allowed bool, reason string)
}

// Registry is a thread-safe map of action type name to ActionType.
// Construct with NewRegistry; the zero value is not usable.
type Registry struct {
	mu    sync.RWMutex
	types map[string]ActionType
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{types: map[string]ActionType{}}
}

// Register adds an ActionType to the registry. Returns an error
// if the type name is empty or already registered. Action types
// should be registered exactly once per process; tests and
// production share the same Default registry below.
func (r *Registry) Register(at ActionType) error {
	if at.Type == "" {
		return errors.New("ActionType.Type is required")
	}
	if at.ValidateParameters == nil {
		return fmt.Errorf("ActionType %q missing ValidateParameters", at.Type)
	}
	if at.MatchesCapability == nil {
		return fmt.Errorf("ActionType %q missing MatchesCapability", at.Type)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.types[at.Type]; dup {
		return fmt.Errorf("ActionType %q already registered", at.Type)
	}
	r.types[at.Type] = at
	return nil
}

// Get returns the ActionType for the supplied name. The second
// return value distinguishes "registered with zero value" from
// "not registered" the way the comma-ok map idiom does.
func (r *Registry) Get(name string) (ActionType, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	at, ok := r.types[name]
	return at, ok
}

// Types returns the names of all registered action types, sorted
// alphabetically for deterministic UI rendering.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.types))
	for k := range r.types {
		out = append(out, k)
	}
	// Sort here so callers don't have to.
	sortStrings(out)
	return out
}

// AllowsAction returns true if the given runner's capability list
// permits the action type with these parameters. Centralizes the
// match logic so callers don't have to walk the slice themselves
// or duplicate the registry lookup.
func (r *Registry) AllowsAction(caps []Capability, typeName string, params json.RawMessage) (bool, string) {
	at, ok := r.Get(typeName)
	if !ok {
		return false, fmt.Sprintf("unknown action type %q", typeName)
	}
	for _, c := range caps {
		if c.Type != typeName {
			continue
		}
		allowed, reason := at.MatchesCapability(params, c)
		if allowed {
			return true, ""
		}
		// Keep the most specific rejection reason; if no capability
		// matches at all, the loop falls through to the not-declared
		// case below.
		if reason != "" {
			return false, reason
		}
	}
	return false, fmt.Sprintf("runner has no capability declaration for %q", typeName)
}

// sortStrings is a tiny helper that avoids pulling sort just for
// the one slice call above. Keeps the import set minimal.
func sortStrings(s []string) {
	// Simple insertion sort; the type registry is tiny.
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

// Default is the process-wide registry. Action type init()s
// register here; main.go and tests share the same catalog. Tests
// that need an isolated registry construct their own with
// NewRegistry().
var Default = NewRegistry()
