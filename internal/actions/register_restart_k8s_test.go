// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package actions

import (
	"encoding/json"
	"testing"
)

func k8sParams(t *testing.T, ns, kind, name string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(RestartK8sWorkloadParameters{Namespace: ns, Kind: kind, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidateRestartK8sParameters(t *testing.T) {
	at := RestartK8sWorkloadActionType()

	if err := at.ValidateParameters(k8sParams(t, "sre-monitoring-dv", "deployment", "squadron-demo-collector")); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	if err := at.ValidateParameters(k8sParams(t, "ns", "", "name")); err != nil {
		t.Fatalf("empty kind should default to deployment: %v", err)
	}
	if err := at.ValidateParameters(k8sParams(t, "", "deployment", "x")); err == nil {
		t.Fatal("missing namespace should error")
	}
	if err := at.ValidateParameters(k8sParams(t, "ns", "deployment", "")); err == nil {
		t.Fatal("missing name should error")
	}
	if err := at.ValidateParameters(k8sParams(t, "ns", "pod", "x")); err == nil {
		t.Fatal("kind=pod should error (not a rollout-restartable workload)")
	}
	// Injection defense: names carrying shell metacharacters, spaces, path
	// separators, or a leading dash (flag lookalike) must be rejected before
	// they can reach the kubectl argv.
	for _, bad := range []string{"x; rm -rf /", "-n kube-system", "a/b", "up time", "Web"} {
		if err := at.ValidateParameters(k8sParams(t, "ns", "deployment", bad)); err == nil {
			t.Fatalf("unsafe name %q must be rejected", bad)
		}
	}
}

func TestMatchesRestartK8sCapability(t *testing.T) {
	at := RestartK8sWorkloadActionType()
	params := k8sParams(t, "sre-monitoring-dv", "deployment", "squadron-demo-collector")

	if ok, _ := at.MatchesCapability(params, Capability{Type: RestartK8sWorkloadType}); !ok {
		t.Fatal("no constraints should allow any workload")
	}

	scoped := Capability{Type: RestartK8sWorkloadType, Constraints: map[string]any{
		"namespace_glob": []any{"sre-monitoring-*"},
		"name_glob":      []any{"squadron-demo-collector", "otel-*"},
	}}
	if ok, reason := at.MatchesCapability(params, scoped); !ok {
		t.Fatalf("expected match, got denied: %s", reason)
	}

	nsOut := Capability{Type: RestartK8sWorkloadType, Constraints: map[string]any{
		"namespace_glob": []any{"kube-system"},
	}}
	if ok, _ := at.MatchesCapability(params, nsOut); ok {
		t.Fatal("namespace outside glob should be denied")
	}

	nameOut := Capability{Type: RestartK8sWorkloadType, Constraints: map[string]any{
		"name_glob": []any{"other-*"},
	}}
	if ok, _ := at.MatchesCapability(params, nameOut); ok {
		t.Fatal("name outside glob should be denied")
	}

	if ok, _ := at.MatchesCapability(params, Capability{Type: RestartDockerContainerType}); ok {
		t.Fatal("capability type mismatch should be denied")
	}
}
