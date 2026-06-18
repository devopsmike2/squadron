// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/actions"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeSquadron stands in for the control plane. It exposes the same
// three endpoints the runner hits and records what was posted so
// the test can assert. Single-shot: returns the configured pending
// request once, then empty.
type fakeSquadron struct {
	t            *testing.T
	signer       *actions.Signer
	mu           sync.Mutex
	pending      []*types.ActionRequest
	registered   []map[string]any
	resultsByID  map[string]map[string]any
	pollHits     int
	registerHits int
}

func (f *fakeSquadron) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	f.mu.Lock()
	f.registered = append(f.registered, payload)
	f.registerHits++
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (f *fakeSquadron) handleRunnerSubpath(w http.ResponseWriter, r *http.Request) {
	// Path looks like /api/v1/runners/<id>/pending
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/runners/"), "/")
	if len(parts) >= 2 && parts[1] == "pending" {
		f.mu.Lock()
		f.pollHits++
		out := f.pending
		f.pending = nil
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": out})
		return
	}
	http.NotFound(w, r)
}

func (f *fakeSquadron) handleActionSubpath(w http.ResponseWriter, r *http.Request) {
	// Path looks like /api/v1/actions/<id>/result
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/actions/"), "/")
	if len(parts) >= 2 && parts[1] == "result" && r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		f.mu.Lock()
		f.resultsByID[parts[0]] = payload
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	http.NotFound(w, r)
}

// fakeCommandRunner records what the executor would have run and
// returns canned output. Used by the runner round-trip test so we
// never call real systemctl.
type fakeCommandRunner struct {
	calls  []fakeCall
	stdout string
	stderr string
	code   int
	err    error
}

type fakeCall struct {
	Name string
	Args []string
}

func (f *fakeCommandRunner) Run(_ context.Context, name string, args ...string) (string, string, int, error) {
	f.calls = append(f.calls, fakeCall{Name: name, Args: append([]string{}, args...)})
	return f.stdout, f.stderr, f.code, f.err
}

// newFakeSquadronAndRegister wires the server. The test gets a
// pointer to the fake so it can stage pending requests and inspect
// posted results.
func newFakeSquadronAndRegister(t *testing.T, signer *actions.Signer) (*httptest.Server, *fakeSquadron) {
	f := &fakeSquadron{
		t:           t,
		signer:      signer,
		resultsByID: map[string]map[string]any{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/runners/register", f.handleRegister)
	mux.HandleFunc("/api/v1/runners/", f.handleRunnerSubpath)
	mux.HandleFunc("/api/v1/actions/", f.handleActionSubpath)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

// signedPendingRequest builds a stored ActionRequest with a real
// signature for the test signer. Reuses the runner's wire shape so
// the verify path matches what production runners see.
func signedPendingRequest(t *testing.T, signer *actions.Signer, runnerID, unitName string, phase actions.Phase) *types.ActionRequest {
	t.Helper()
	params, err := json.Marshal(actions.RestartSystemdServiceParameters{
		UnitName:        unitName,
		RestartStrategy: "restart",
	})
	require.NoError(t, err)
	wire := &actions.Request{
		RequestID:  "req-1",
		ProposalID: "prop-1",
		RunnerID:   runnerID,
		Action: actions.ActionPayload{
			Type:       actions.RestartSystemdServiceType,
			Parameters: params,
		},
		Phase: phase,
	}
	_, err = signer.Sign(wire)
	require.NoError(t, err)
	return &types.ActionRequest{
		ID:             wire.RequestID,
		ProposalID:     wire.ProposalID,
		RunnerID:       wire.RunnerID,
		ActionType:     wire.Action.Type,
		ParametersJSON: string(params),
		Signature:      wire.Signature,
		Phase:          string(phase),
		Status:         "pending",
		IssuedAt:       wire.IssuedAt,
		ExpiresAt:      wire.ExpiresAt,
	}
}

// --- tests ------------------------------------------------------------------

func TestRunner_HappyPath_DryRun(t *testing.T) {
	signer, err := actions.GenerateSigner()
	require.NoError(t, err)
	srv, fake := newFakeSquadronAndRegister(t, signer)

	cfg := &Config{
		RunnerID:             "runner-test",
		Hostname:             "host-test",
		SquadronURL:          srv.URL,
		SquadronPublicKeyPEM: signer.PublicKeyPEM(),
		PollInterval:         50 * time.Millisecond,
		Capabilities: []actions.Capability{
			{
				Type: actions.RestartSystemdServiceType,
				Constraints: map[string]any{
					"unit_name_glob": []string{"nginx*"},
				},
			},
		},
	}
	cmdRunner := &fakeCommandRunner{stdout: "Active: active (running)", code: 0}
	exec := NewSystemdExecutor(zap.NewNop())
	exec.SetCommandRunner(cmdRunner)
	runner, err := NewRunner(cfg, exec, zap.NewNop())
	require.NoError(t, err)
	runner.SetHTTPClient(srv.Client())

	fake.pending = []*types.ActionRequest{
		signedPendingRequest(t, signer, "runner-test", "nginx.service", actions.PhaseDryRun),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	require.NoError(t, runner.Run(ctx))

	// Verify the registration happened, the executor was called with
	// the status command, and the result was posted as success.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.GreaterOrEqual(t, fake.registerHits, 1, "expected at least one registration")
	require.Equal(t, []fakeCall{{Name: "systemctl", Args: []string{"status", "nginx.service", "--no-pager"}}}, cmdRunner.calls)
	got, ok := fake.resultsByID["req-1"]
	require.True(t, ok, "result for req-1 should have been posted")
	require.Equal(t, "success", got["status"])
	require.Contains(t, got, "dry_run_output_json")
}

func TestRunner_RejectsExpiredRequest(t *testing.T) {
	signer, err := actions.GenerateSigner()
	require.NoError(t, err)
	srv, fake := newFakeSquadronAndRegister(t, signer)

	cfg := &Config{
		RunnerID:             "runner-test",
		Hostname:             "host-test",
		SquadronURL:          srv.URL,
		SquadronPublicKeyPEM: signer.PublicKeyPEM(),
		PollInterval:         50 * time.Millisecond,
		Capabilities: []actions.Capability{
			{Type: actions.RestartSystemdServiceType},
		},
	}
	cmdRunner := &fakeCommandRunner{}
	exec := NewSystemdExecutor(zap.NewNop())
	exec.SetCommandRunner(cmdRunner)
	runner, err := NewRunner(cfg, exec, zap.NewNop())
	require.NoError(t, err)
	runner.SetHTTPClient(srv.Client())
	// Move the clock forward so the request is past its 5-minute
	// expiry without sleeping in the test.
	runner.SetClock(func() time.Time { return time.Now().Add(10 * time.Minute) })

	fake.pending = []*types.ActionRequest{
		signedPendingRequest(t, signer, "runner-test", "nginx.service", actions.PhaseDryRun),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, runner.Run(ctx))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Empty(t, cmdRunner.calls, "executor must not run on expired request")
	got, ok := fake.resultsByID["req-1"]
	require.True(t, ok, "result for req-1 should have been posted")
	require.Equal(t, "denied", got["status"])
	require.Contains(t, got["denied_for"], "signature")
}

func TestRunner_RejectsWrongSigner(t *testing.T) {
	issuer, err := actions.GenerateSigner()
	require.NoError(t, err)
	attacker, err := actions.GenerateSigner()
	require.NoError(t, err)
	srv, fake := newFakeSquadronAndRegister(t, issuer)

	cfg := &Config{
		RunnerID:             "runner-test",
		Hostname:             "host-test",
		SquadronURL:          srv.URL,
		SquadronPublicKeyPEM: issuer.PublicKeyPEM(), // pin the issuer
		PollInterval:         50 * time.Millisecond,
		Capabilities: []actions.Capability{
			{Type: actions.RestartSystemdServiceType},
		},
	}
	cmdRunner := &fakeCommandRunner{}
	exec := NewSystemdExecutor(zap.NewNop())
	exec.SetCommandRunner(cmdRunner)
	runner, err := NewRunner(cfg, exec, zap.NewNop())
	require.NoError(t, err)
	runner.SetHTTPClient(srv.Client())

	// Pending request is signed by the attacker, not the pinned issuer.
	fake.pending = []*types.ActionRequest{
		signedPendingRequest(t, attacker, "runner-test", "nginx.service", actions.PhaseDryRun),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, runner.Run(ctx))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Empty(t, cmdRunner.calls, "executor must not run on wrong-signer request")
	got, ok := fake.resultsByID["req-1"]
	require.True(t, ok, "result for req-1 should have been posted")
	require.Equal(t, "denied", got["status"])
	require.Contains(t, got["denied_for"], "signature")
}

func TestRunner_DryRunOnlyDeniesExecutePhase(t *testing.T) {
	signer, err := actions.GenerateSigner()
	require.NoError(t, err)
	srv, fake := newFakeSquadronAndRegister(t, signer)

	cfg := &Config{
		RunnerID:             "runner-test",
		Hostname:             "host-test",
		SquadronURL:          srv.URL,
		SquadronPublicKeyPEM: signer.PublicKeyPEM(),
		PollInterval:         50 * time.Millisecond,
		Capabilities: []actions.Capability{
			{Type: actions.RestartSystemdServiceType},
		},
	}
	cmdRunner := &fakeCommandRunner{}
	exec := NewSystemdExecutor(zap.NewNop())
	exec.SetCommandRunner(cmdRunner)
	runner, err := NewRunner(cfg, exec, zap.NewNop())
	require.NoError(t, err)
	runner.SetHTTPClient(srv.Client())
	runner.DryRunOnly = true

	fake.pending = []*types.ActionRequest{
		signedPendingRequest(t, signer, "runner-test", "nginx.service", actions.PhaseExecute),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, runner.Run(ctx))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Empty(t, cmdRunner.calls, "executor must not run when dry-run-only and phase=execute")
	got, ok := fake.resultsByID["req-1"]
	require.True(t, ok)
	require.Equal(t, "denied", got["status"])
	require.Equal(t, "dry-run-only mode", got["denied_for"])
}

func TestRunner_OutOfCapabilityDenied(t *testing.T) {
	signer, err := actions.GenerateSigner()
	require.NoError(t, err)
	srv, fake := newFakeSquadronAndRegister(t, signer)

	cfg := &Config{
		RunnerID:             "runner-test",
		Hostname:             "host-test",
		SquadronURL:          srv.URL,
		SquadronPublicKeyPEM: signer.PublicKeyPEM(),
		PollInterval:         50 * time.Millisecond,
		Capabilities: []actions.Capability{
			{
				Type: actions.RestartSystemdServiceType,
				Constraints: map[string]any{
					"unit_name_glob": []string{"nginx*"},
				},
			},
		},
	}
	cmdRunner := &fakeCommandRunner{}
	exec := NewSystemdExecutor(zap.NewNop())
	exec.SetCommandRunner(cmdRunner)
	runner, err := NewRunner(cfg, exec, zap.NewNop())
	require.NoError(t, err)
	runner.SetHTTPClient(srv.Client())

	// unit "postgres.service" does not match the nginx* glob.
	fake.pending = []*types.ActionRequest{
		signedPendingRequest(t, signer, "runner-test", "postgres.service", actions.PhaseExecute),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, runner.Run(ctx))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Empty(t, cmdRunner.calls)
	got, ok := fake.resultsByID["req-1"]
	require.True(t, ok)
	require.Equal(t, "denied", got["status"])
	require.Equal(t, "out_of_policy", got["denied_for"])
}

func TestRunner_ExecutePhaseRunsRestart(t *testing.T) {
	signer, err := actions.GenerateSigner()
	require.NoError(t, err)
	srv, fake := newFakeSquadronAndRegister(t, signer)

	cfg := &Config{
		RunnerID:             "runner-test",
		Hostname:             "host-test",
		SquadronURL:          srv.URL,
		SquadronPublicKeyPEM: signer.PublicKeyPEM(),
		PollInterval:         50 * time.Millisecond,
		Capabilities: []actions.Capability{
			{
				Type: actions.RestartSystemdServiceType,
				Constraints: map[string]any{
					"unit_name_glob": []string{"nginx*"},
				},
			},
		},
	}
	cmdRunner := &fakeCommandRunner{stdout: "", code: 0}
	exec := NewSystemdExecutor(zap.NewNop())
	exec.SetCommandRunner(cmdRunner)
	runner, err := NewRunner(cfg, exec, zap.NewNop())
	require.NoError(t, err)
	runner.SetHTTPClient(srv.Client())

	fake.pending = []*types.ActionRequest{
		signedPendingRequest(t, signer, "runner-test", "nginx.service", actions.PhaseExecute),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, runner.Run(ctx))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, []fakeCall{{Name: "systemctl", Args: []string{"restart", "nginx.service"}}}, cmdRunner.calls)
	got, ok := fake.resultsByID["req-1"]
	require.True(t, ok)
	require.Equal(t, "success", got["status"])
	require.Contains(t, got, "execution_output_json")
}

func TestLoadConfig_Roundtrip(t *testing.T) {
	signer, err := actions.GenerateSigner()
	require.NoError(t, err)
	yamlBody := `runner_id: runner-1
hostname: example.local
squadron_url: https://squadron.example/api
squadron_public_key_pem: |
  ` + indentLines(signer.PublicKeyPEM(), "  ") + `
poll_interval: 7s
capabilities:
  - type: restart-systemd-service
    constraints:
      unit_name_glob:
        - nginx*
`
	tmp := t.TempDir() + "/c.yaml"
	require.NoError(t, writeString(tmp, yamlBody))

	cfg, err := LoadConfig(tmp)
	require.NoError(t, err)
	require.Equal(t, "runner-1", cfg.RunnerID)
	require.Equal(t, 7*time.Second, cfg.PollInterval)
	require.Len(t, cfg.Capabilities, 1)
	require.Equal(t, actions.RestartSystemdServiceType, cfg.Capabilities[0].Type)
}

func TestEncodePublicKeyPEM_Roundtrip(t *testing.T) {
	pemStr, _, err := generatePrivateKey()
	require.NoError(t, err)
	priv, err := privateKeyFromPEM(pemStr)
	require.NoError(t, err)
	out := encodePublicKeyPEM(priv)
	block, _ := pem.Decode([]byte(out))
	require.NotNil(t, block)
	require.Equal(t, "ED25519 PUBLIC KEY", block.Type)
}

// indentLines prefixes every line with the supplied indent. Used to
// stitch the multi-line PEM into the YAML literal block above.
func indentLines(s, indent string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if line == "" && i == len(lines)-1 {
			continue
		}
		if i == 0 {
			out = append(out, line)
			continue
		}
		out = append(out, indent+line)
	}
	return strings.Join(out, "\n")
}

func writeString(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
