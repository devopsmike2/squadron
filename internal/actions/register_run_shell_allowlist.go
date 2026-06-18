// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package actions

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RunShellAllowlistType is the action that asks a runner to execute
// a shell command that exactly matches an entry on the operator's
// allowlist. Squadron NEVER lets the proposer or operator type a
// free-form shell command; the runner only runs commands that were
// pre approved at install time. This is the strict equivalent of
// systemd unit gating: the operator decides what the runner is
// willing to do; Squadron's dispatch can only ask for those.
//
// Capability constraint shape:
//
//	type: run-shell-allowlist
//	constraints:
//	  commands:
//	    - "systemctl reload nginx.service"
//	    - "docker logs --tail 100 squadron-app"
//
// Dispatch parameters carry exactly one of those command strings.
// Mismatch is denied at dispatch time and again at execute time.
const RunShellAllowlistType = "run-shell-allowlist"

// RunShellAllowlistParameters is the input schema. command must be
// a verbatim string from the runner's declared allowlist.
type RunShellAllowlistParameters struct {
	Command string `json:"command"`
}

func init() {
	if err := Default.Register(RunShellAllowlistActionType()); err != nil {
		panic(fmt.Sprintf("register %s: %v", RunShellAllowlistType, err))
	}
}

// RunShellAllowlistActionType returns the ActionType definition.
func RunShellAllowlistActionType() ActionType {
	return ActionType{
		Type:               RunShellAllowlistType,
		Description:        "Run an exact shell command from the runner's allowlist.",
		ValidateParameters: validateRunShellAllowlistParameters,
		MatchesCapability:  matchesRunShellAllowlistCapability,
	}
}

func validateRunShellAllowlistParameters(raw json.RawMessage) error {
	var p RunShellAllowlistParameters
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode parameters: %w", err)
	}
	if strings.TrimSpace(p.Command) == "" {
		return errors.New("command is required")
	}
	// Defense in depth: reject shell metacharacters that would let an
	// attacker chain commands even when the verbatim allowlist match
	// later checks. The runner does not expand variables or interpret
	// quotes either; the allowlist is the trust boundary.
	if strings.ContainsAny(p.Command, ";&|`$\n\r") {
		return errors.New("command must not contain shell metacharacters")
	}
	return nil
}

// matchesRunShellAllowlistCapability enforces the verbatim allowlist
// match. No globbing, no substring, no normalization beyond trimming.
// If a runner installer wants to allow variants they list each one
// separately.
func matchesRunShellAllowlistCapability(raw json.RawMessage, c Capability) (bool, string) {
	if c.Type != RunShellAllowlistType {
		return false, fmt.Sprintf("capability type mismatch: %q vs %q", c.Type, RunShellAllowlistType)
	}
	var p RunShellAllowlistParameters
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, fmt.Sprintf("decode parameters: %v", err)
	}
	cmds, ok := allowedShellCommands(c.Constraints)
	if !ok || len(cmds) == 0 {
		// An empty allowlist means the runner has the action type
		// registered but no commands approved. Refuse rather than
		// allow.
		return false, "runner has no commands on the allowlist"
	}
	want := strings.TrimSpace(p.Command)
	for _, allowed := range cmds {
		if strings.TrimSpace(allowed) == want {
			return true, ""
		}
	}
	return false, fmt.Sprintf("command %q is not on the runner's allowlist", p.Command)
}

func allowedShellCommands(constraints map[string]any) ([]string, bool) {
	if constraints == nil {
		return nil, false
	}
	raw, ok := constraints["commands"]
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
