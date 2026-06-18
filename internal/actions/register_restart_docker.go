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

// RestartDockerContainerType is the action that asks a runner to
// restart a Docker container. Mirrors restart-systemd-service in
// shape so dispatch + capability handling follow one pattern.
const RestartDockerContainerType = "restart-docker-container"

// RestartDockerContainerParameters is the input schema. container is
// required. timeout_seconds is optional; defaults to 10 on the
// runner side.
type RestartDockerContainerParameters struct {
	Container      string `json:"container"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

func init() {
	if err := Default.Register(RestartDockerContainerActionType()); err != nil {
		panic(fmt.Sprintf("register %s: %v", RestartDockerContainerType, err))
	}
}

// RestartDockerContainerActionType returns the ActionType definition.
func RestartDockerContainerActionType() ActionType {
	return ActionType{
		Type:               RestartDockerContainerType,
		Description:        "Restart a Docker container on the target node.",
		ValidateParameters: validateRestartDockerParameters,
		MatchesCapability:  matchesRestartDockerCapability,
	}
}

func validateRestartDockerParameters(raw json.RawMessage) error {
	var p RestartDockerContainerParameters
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode parameters: %w", err)
	}
	if strings.TrimSpace(p.Container) == "" {
		return errors.New("container is required")
	}
	// Same defense in depth as restart-systemd-service: refuse path
	// separators so a crafted container name cannot escape the
	// docker namespace.
	if strings.ContainsAny(p.Container, "/\\") {
		return errors.New("container must not contain path separators")
	}
	if p.TimeoutSeconds < 0 || p.TimeoutSeconds > 600 {
		return errors.New("timeout_seconds must be between 0 and 600")
	}
	return nil
}

// matchesRestartDockerCapability checks the operator declared
// container_glob list. Constraint shape:
//
//	type: restart-docker-container
//	constraints:
//	  container_glob:
//	    - "squadron-*"
//	    - "web-app-*"
func matchesRestartDockerCapability(raw json.RawMessage, c Capability) (bool, string) {
	if c.Type != RestartDockerContainerType {
		return false, fmt.Sprintf("capability type mismatch: %q vs %q", c.Type, RestartDockerContainerType)
	}
	var p RestartDockerContainerParameters
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, fmt.Sprintf("decode parameters: %v", err)
	}
	globs, ok := containerGlobs(c.Constraints)
	if !ok {
		return true, "" // no constraint declared = any container
	}
	for _, g := range globs {
		if match, err := filepath.Match(g, p.Container); err == nil && match {
			return true, ""
		}
	}
	return false, fmt.Sprintf("container %q does not match any glob in capability constraints", p.Container)
}

func containerGlobs(constraints map[string]any) ([]string, bool) {
	if constraints == nil {
		return nil, false
	}
	raw, ok := constraints["container_glob"]
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
