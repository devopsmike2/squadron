// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package actions

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// RestartK8sWorkloadType asks a runner to restart a Kubernetes /
// OpenShift workload (a rolling restart of a Deployment, DaemonSet, or
// StatefulSet). It mirrors restart-docker-container / restart-systemd-service
// in shape so dispatch, capability handling, and dry-run follow one pattern.
// The runner implements it by shelling out to `kubectl rollout restart`, so it
// works on any cluster where the runner's ServiceAccount has patch rights on
// the target workload.
const RestartK8sWorkloadType = "restart-k8s-workload"

// RestartK8sWorkloadParameters is the input schema. Namespace and Name are
// required. Kind is optional and defaults to "deployment" on the runner side;
// allowed kinds are deployment, daemonset, statefulset.
type RestartK8sWorkloadParameters struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name"`
}

// k8sAllowedKinds are the workload kinds a rollout restart is valid for.
var k8sAllowedKinds = map[string]bool{
	"deployment":  true,
	"daemonset":   true,
	"statefulset": true,
}

// dns1123 is the character set for Kubernetes resource + namespace names. We
// enforce it at validate time as defense in depth: the runner shells out to
// kubectl, so a name must never be able to carry a shell metacharacter, a
// flag, or a path separator.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

func init() {
	if err := Default.Register(RestartK8sWorkloadActionType()); err != nil {
		panic(fmt.Sprintf("register %s: %v", RestartK8sWorkloadType, err))
	}
}

// RestartK8sWorkloadActionType returns the ActionType definition.
func RestartK8sWorkloadActionType() ActionType {
	return ActionType{
		Type:               RestartK8sWorkloadType,
		Description:        "Restart a Kubernetes/OpenShift workload (rollout restart of a Deployment, DaemonSet, or StatefulSet).",
		ValidateParameters: validateRestartK8sParameters,
		MatchesCapability:  matchesRestartK8sCapability,
	}
}

func validateRestartK8sParameters(raw json.RawMessage) error {
	var p RestartK8sWorkloadParameters
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode parameters: %w", err)
	}
	if strings.TrimSpace(p.Namespace) == "" {
		return errors.New("namespace is required")
	}
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name is required")
	}
	kind := normalizeK8sKind(p.Kind)
	if !k8sAllowedKinds[kind] {
		return fmt.Errorf("kind must be one of deployment, daemonset, statefulset (got %q)", p.Kind)
	}
	if !dns1123.MatchString(p.Namespace) {
		return errors.New("namespace must be a valid DNS-1123 name")
	}
	if !dns1123.MatchString(p.Name) {
		return errors.New("name must be a valid DNS-1123 name")
	}
	return nil
}

// normalizeK8sKind lowercases and defaults the kind to "deployment".
func normalizeK8sKind(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	if k == "" {
		return "deployment"
	}
	return k
}

// matchesRestartK8sCapability checks the operator-declared namespace_glob and
// name_glob lists. Constraint shape:
//
//	type: restart-k8s-workload
//	constraints:
//	  namespace_glob:
//	    - "sre-monitoring-*"
//	  name_glob:
//	    - "otel-*"
//	    - "squadron-demo-collector"
//
// A declared glob list must match; an absent list means "any" on that axis, so
// an operator scopes a runner by declaring exactly the namespaces/workloads it
// is allowed to touch.
func matchesRestartK8sCapability(raw json.RawMessage, c Capability) (bool, string) {
	if c.Type != RestartK8sWorkloadType {
		return false, fmt.Sprintf("capability type mismatch: %q vs %q", c.Type, RestartK8sWorkloadType)
	}
	var p RestartK8sWorkloadParameters
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, fmt.Sprintf("decode parameters: %v", err)
	}
	if globs, ok := k8sGlobs(c.Constraints, "namespace_glob"); ok {
		if !anyGlobMatch(globs, p.Namespace) {
			return false, fmt.Sprintf("namespace %q does not match any glob in capability constraints", p.Namespace)
		}
	}
	if globs, ok := k8sGlobs(c.Constraints, "name_glob"); ok {
		if !anyGlobMatch(globs, p.Name) {
			return false, fmt.Sprintf("name %q does not match any glob in capability constraints", p.Name)
		}
	}
	return true, ""
}

func anyGlobMatch(globs []string, s string) bool {
	for _, g := range globs {
		if match, err := filepath.Match(g, s); err == nil && match {
			return true
		}
	}
	return false
}

func k8sGlobs(constraints map[string]any, key string) ([]string, bool) {
	if constraints == nil {
		return nil, false
	}
	raw, ok := constraints[key]
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
