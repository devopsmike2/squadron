// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/actions"
)

func k8sRequest(t *testing.T, phase actions.Phase, kind string) *actions.Request {
	t.Helper()
	params, err := json.Marshal(actions.RestartK8sWorkloadParameters{
		Namespace: "sre-monitoring-dv",
		Kind:      kind,
		Name:      "squadron-demo-collector",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &actions.Request{
		Phase:  phase,
		Action: actions.ActionPayload{Type: actions.RestartK8sWorkloadType, Parameters: params},
	}
}

func TestExecutor_RestartK8s_Execute_RunsRolloutRestart(t *testing.T) {
	fake := &fakeCommandRunner{stdout: "deployment.apps/squadron-demo-collector restarted", code: 0}
	exec := NewSystemdExecutor(zap.NewNop())
	exec.SetCommandRunner(fake)

	res := exec.ExecuteRequest(context.Background(), k8sRequest(t, actions.PhaseExecute, "deployment"))

	if res.Status != actions.StatusSuccess {
		t.Fatalf("want success, got %s (stderr %q)", res.Status, res.Stderr)
	}
	if len(fake.calls) != 1 || fake.calls[0].Name != "kubectl" {
		t.Fatalf("want one kubectl call, got %+v", fake.calls)
	}
	want := []string{"rollout", "restart", "deployment/squadron-demo-collector", "-n", "sre-monitoring-dv"}
	if !reflect.DeepEqual(fake.calls[0].Args, want) {
		t.Fatalf("args = %v, want %v", fake.calls[0].Args, want)
	}
}

func TestExecutor_RestartK8s_DryRun_DoesNotRestart(t *testing.T) {
	fake := &fakeCommandRunner{stdout: "squadron-demo-collector generation=3", code: 0}
	exec := NewSystemdExecutor(zap.NewNop())
	exec.SetCommandRunner(fake)

	// Empty kind must default to deployment.
	res := exec.ExecuteRequest(context.Background(), k8sRequest(t, actions.PhaseDryRun, ""))

	if res.Status != actions.StatusSuccess {
		t.Fatalf("dry-run want success, got %s", res.Status)
	}
	if len(fake.calls) != 1 || fake.calls[0].Args[0] != "get" {
		t.Fatalf("dry-run must inspect with `kubectl get`, got %+v", fake.calls)
	}
	if fake.calls[0].Args[1] != "deployment/squadron-demo-collector" {
		t.Fatalf("dry-run target = %q, want deployment/squadron-demo-collector", fake.calls[0].Args[1])
	}
	for _, c := range fake.calls {
		for _, a := range c.Args {
			if a == "restart" {
				t.Fatal("dry-run must never issue a rollout restart")
			}
		}
	}
}
