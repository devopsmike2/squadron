// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package actions

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// RestartSystemdServiceType is the canonical type name for the
// first action Squadron ships. Exported so handlers and the
// proposer can construct an ActionPayload without stringly-typing
// the constant.
const RestartSystemdServiceType = "restart-systemd-service"

// RestartSystemdServiceParameters is the input schema for the
// action. unit_name is required; restart_strategy chooses between
// restart (default), try-restart, and reload.
type RestartSystemdServiceParameters struct {
	UnitName        string `json:"unit_name"`
	RestartStrategy string `json:"restart_strategy,omitempty"`
}

var allowedRestartStrategies = map[string]struct{}{
	"":             {}, // default = restart
	"restart":      {},
	"try-restart":  {},
	"reload":       {},
}

// init registers the action type on Default at process load. Tests
// that want isolation construct their own Registry instead.
func init() {
	if err := Default.Register(RestartSystemdServiceActionType()); err != nil {
		// Registration failures at init time are a programmer error;
		// panic so the build never reaches production with a broken
		// catalog.
		panic(fmt.Sprintf("register %s: %v", RestartSystemdServiceType, err))
	}
}

// RestartSystemdServiceActionType returns the ActionType definition
// for restart-systemd-service. Constructed as a function rather
// than a var so tests can call it to build a fresh registry without
// triggering the init() side effect.
func RestartSystemdServiceActionType() ActionType {
	return ActionType{
		Type:               RestartSystemdServiceType,
		Description:        "Restart a systemd unit on the target node.",
		ValidateParameters: validateRestartSystemdParameters,
		MatchesCapability:  matchesRestartSystemdCapability,
	}
}

func validateRestartSystemdParameters(raw json.RawMessage) error {
	var p RestartSystemdServiceParameters
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode parameters: %w", err)
	}
	if strings.TrimSpace(p.UnitName) == "" {
		return errors.New("unit_name is required")
	}
	// Reject path separators outright so an attacker cannot point at
	// a unit file outside /etc/systemd/system through a crafted name.
	if strings.ContainsAny(p.UnitName, "/\\") {
		return errors.New("unit_name must not contain path separators")
	}
	if _, ok := allowedRestartStrategies[p.RestartStrategy]; !ok {
		return fmt.Errorf("restart_strategy %q is not one of restart, try-restart, reload", p.RestartStrategy)
	}
	return nil
}

// matchesRestartSystemdCapability checks the runner's capability
// constraints against the proposed unit_name. The constraint shape
// is:
//
//	type: restart-systemd-service
//	constraints:
//	  unit_name_glob:
//	    - "squadron-*"
//	    - "nginx*"
//
// An empty or missing unit_name_glob list means the runner has
// declared the type but added no glob constraint, which we treat
// as "any unit" (the operator who installed the runner can opt
// into that by leaving the list empty; otherwise they list the
// globs they're willing to allow).
func matchesRestartSystemdCapability(raw json.RawMessage, c Capability) (bool, string) {
	if c.Type != RestartSystemdServiceType {
		return false, fmt.Sprintf("capability type mismatch: %q vs %q", c.Type, RestartSystemdServiceType)
	}
	var p RestartSystemdServiceParameters
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, fmt.Sprintf("decode parameters: %v", err)
	}
	globs, ok := unitNameGlobs(c.Constraints)
	if !ok {
		// No constraint declared on this capability; allow.
		return true, ""
	}
	for _, g := range globs {
		match, err := filepath.Match(g, p.UnitName)
		if err == nil && match {
			return true, ""
		}
	}
	return false, fmt.Sprintf("unit_name %q does not match any glob in capability constraints", p.UnitName)
}

// unitNameGlobs lifts the unit_name_glob list out of a capability's
// untyped constraints map. Tolerant of missing or empty values.
func unitNameGlobs(constraints map[string]any) ([]string, bool) {
	if constraints == nil {
		return nil, false
	}
	raw, ok := constraints["unit_name_glob"]
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case []string:
		return v, true
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	default:
		return nil, false
	}
}
