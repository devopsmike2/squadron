// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package repocontext

import (
	"context"
	"strings"
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
)

type fakeFileClient struct {
	files map[string][]byte
}

func (f *fakeFileClient) GetFileContent(_ context.Context, _, _, path, _ string) (*iacgithub.FileContent, error) {
	b, ok := f.files[path]
	if !ok {
		return nil, iacgithub.ErrFileNotFound
	}
	return &iacgithub.FileContent{Path: path, DecodedContent: b}, nil
}

func testKey(t *testing.T) *credstore.Key {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	k, err := credstore.NewKey(raw)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return k
}

func mkConn(t *testing.T, store iacconnstore.Store, key *credstore.Key, repo string, entries []iacconnstore.PlacementMapEntry) {
	t.Helper()
	sealed, err := iacconnstore.MarshalGitHubPATCreds(iacconnstore.GitHubPATCredentials{Token: "ghp_x"}, key)
	if err != nil {
		t.Fatalf("MarshalGitHubPATCreds: %v", err)
	}
	conn := &iacconnstore.IaCConnection{
		Provider:       "github",
		AuthKind:       "pat",
		RepoFullName:   repo,
		DefaultBranch:  "main",
		RepoLayout:     "multi",
		CredCiphertext: sealed,
		PlacementMap:   entries,
	}
	if err := store.Create(context.Background(), conn); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestProvider_SingleConnection_SummarizesPlacementFile(t *testing.T) {
	key := testKey(t)
	store := iacconnstore.NewMemoryStore()
	mkConn(t, store, key, "octo/widgets", []iacconnstore.PlacementMapEntry{
		{Provider: "aws", ResourceKind: "ec2-otel-layer", FilePath: "modules/compute/main.tf"},
	})
	fc := &fakeFileClient{files: map[string][]byte{
		"modules/compute/main.tf": []byte("resource \"aws_instance\" \"this\" {}\nvariable \"region\" {}\n"),
	}}
	p := New(store, func(string) FileClient { return fc }, key, nil)
	out := p.RepoContextForScope(context.Background(), "aws", "111111111111")
	for _, want := range []string{"EXISTING TERRAFORM CONTEXT", "modules/compute/main.tf", "aws_instance.this", "region"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestProvider_NoMatchingProvider_ReturnsEmpty(t *testing.T) {
	key := testKey(t)
	store := iacconnstore.NewMemoryStore()
	mkConn(t, store, key, "octo/gcp-repo", []iacconnstore.PlacementMapEntry{
		{Provider: "gcp", ResourceKind: "gce-ops-agent", FilePath: "main.tf"},
	})
	p := New(store, func(string) FileClient { return &fakeFileClient{} }, key, nil)
	if out := p.RepoContextForScope(context.Background(), "aws", ""); out != "" {
		t.Errorf("expected empty for no aws match, got:\n%s", out)
	}
}

func TestProvider_MultipleMatches_ReturnsEmpty(t *testing.T) {
	key := testKey(t)
	store := iacconnstore.NewMemoryStore()
	for _, repo := range []string{"octo/a", "octo/b"} {
		mkConn(t, store, key, repo, []iacconnstore.PlacementMapEntry{
			{Provider: "aws", ResourceKind: "ec2-otel-layer", FilePath: "main.tf"},
		})
	}
	p := New(store, func(string) FileClient { return &fakeFileClient{} }, key, nil)
	if out := p.RepoContextForScope(context.Background(), "aws", ""); out != "" {
		t.Errorf("expected empty for ambiguous multi-match, got:\n%s", out)
	}
}

func TestProvider_MissingFile_DegradesToEmpty(t *testing.T) {
	key := testKey(t)
	store := iacconnstore.NewMemoryStore()
	mkConn(t, store, key, "octo/widgets", []iacconnstore.PlacementMapEntry{
		{Provider: "aws", ResourceKind: "ec2-otel-layer", FilePath: "modules/compute/main.tf"},
	})
	// fake client has NO files → GetFileContent returns ErrFileNotFound.
	p := New(store, func(string) FileClient { return &fakeFileClient{files: map[string][]byte{}} }, key, nil)
	if out := p.RepoContextForScope(context.Background(), "aws", ""); out != "" {
		t.Errorf("expected empty when placement file is absent, got:\n%s", out)
	}
}

func TestProvider_NilSafe(t *testing.T) {
	var p *Provider
	if out := p.RepoContextForScope(context.Background(), "aws", ""); out != "" {
		t.Error("nil provider should return empty")
	}
	if New(nil, nil, nil, nil) != nil {
		t.Error("New with nil deps should return nil")
	}
}
