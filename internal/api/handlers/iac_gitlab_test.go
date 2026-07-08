// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	iacgitlab "github.com/devopsmike2/squadron/internal/iac/gitlab"
)

// fakeGitLabClient is a recording iacgitlab.Client used to prove the
// handler dispatch routes a ProviderGitLab connection to the GitLab
// factory, and that the adapter maps types + sentinel errors.
type fakeGitLabClient struct {
	getRepoCalled bool
	repo          *iacgitlab.Repo
	repoErr       error
	tree          []iacgitlab.TreeEntry
	branchSHA     string
}

func (f *fakeGitLabClient) GetRepo(ctx context.Context, owner, repo string) (*iacgitlab.Repo, error) {
	f.getRepoCalled = true
	return f.repo, f.repoErr
}
func (f *fakeGitLabClient) GetFileContent(ctx context.Context, owner, repo, path, ref string) (*iacgitlab.FileContent, error) {
	return &iacgitlab.FileContent{Path: path, SHA: "blob", DecodedContent: []byte("x")}, nil
}
func (f *fakeGitLabClient) CreateBranch(ctx context.Context, owner, repo, branchName, fromSHA string) error {
	return nil
}
func (f *fakeGitLabClient) PutFileContent(ctx context.Context, opts iacgitlab.PutFileOptions) (*iacgitlab.CommitFileResult, error) {
	return &iacgitlab.CommitFileResult{}, nil
}
func (f *fakeGitLabClient) OpenPR(ctx context.Context, opts iacgitlab.OpenPROptions) (*iacgitlab.PullRequest, error) {
	return &iacgitlab.PullRequest{Number: 7, HTMLURL: "u", HeadSHA: "h"}, nil
}
func (f *fakeGitLabClient) AddLabels(ctx context.Context, owner, repo string, prNumber int, labels []string) error {
	return nil
}
func (f *fakeGitLabClient) RequestReviewers(ctx context.Context, owner, repo string, prNumber int, teamSlugs []string) error {
	return nil
}
func (f *fakeGitLabClient) ListTree(ctx context.Context, owner, repo, ref string) ([]iacgitlab.TreeEntry, error) {
	return f.tree, nil
}
func (f *fakeGitLabClient) GetBranchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	return f.branchSHA, nil
}

func newIaCTestHandlers() *IaCGitHubHandlers {
	return NewIaCGitHubHandlers(nil, zap.NewNop())
}

func TestClientForConn_routes_gitlab_to_gitlab_factory(t *testing.T) {
	fake := &fakeGitLabClient{repo: &iacgitlab.Repo{FullName: "grp/infra", DefaultBranch: "main"}}
	h := newIaCTestHandlers().WithGitLabClientFactory(func(token string) iacgitlab.Client {
		if token != "glpat-secret" {
			t.Errorf("factory got token %q", token)
		}
		return fake
	})

	// GitLab connection → adapter over the GitLab client.
	glConn := &iacconnstore.IaCConnection{Provider: iacconnstore.ProviderGitLab}
	c := h.clientForConn(glConn, "glpat-secret")
	if _, ok := c.(*gitlabClientAdapter); !ok {
		t.Fatalf("gitlab connection did not route to gitlabClientAdapter, got %T", c)
	}
	// The GitHub-shaped call must reach the underlying GitLab client and
	// map its projected Repo type.
	r, err := c.GetRepo(context.Background(), "grp", "infra")
	if err != nil {
		t.Fatalf("adapter GetRepo error: %v", err)
	}
	if !fake.getRepoCalled {
		t.Errorf("underlying GitLab client GetRepo was not called")
	}
	if r.FullName != "grp/infra" || r.DefaultBranch != "main" {
		t.Errorf("mapped repo = %+v", r)
	}
}

func TestClientForConn_routes_github_to_github_factory(t *testing.T) {
	h := newIaCTestHandlers()
	ghConn := &iacconnstore.IaCConnection{Provider: iacconnstore.ProviderGitHub}
	c := h.clientForConn(ghConn, "ghp-secret")
	if _, ok := c.(*gitlabClientAdapter); ok {
		t.Fatalf("github connection incorrectly routed to gitlab adapter")
	}
	if _, ok := c.(*iacgithub.PATClient); !ok {
		t.Fatalf("github connection did not route to iacgithub.PATClient, got %T", c)
	}

	// Empty provider defaults to GitHub too.
	c2 := h.clientForProvider("", "ghp-secret")
	if _, ok := c2.(*iacgithub.PATClient); !ok {
		t.Fatalf("empty provider did not default to GitHub client, got %T", c2)
	}
}

func TestGitLabAdapter_maps_sentinel_errors(t *testing.T) {
	fake := &fakeGitLabClient{repoErr: iacgitlab.ErrRepoNotFound}
	a := newGitLabBackedClient(fake)
	_, err := a.GetRepo(context.Background(), "grp", "infra")
	if !errors.Is(err, iacgithub.ErrRepoNotFound) {
		t.Fatalf("err = %v, want iacgithub.ErrRepoNotFound", err)
	}
}

func TestGitLabAdapter_satisfies_placement_lister(t *testing.T) {
	fake := &fakeGitLabClient{tree: []iacgitlab.TreeEntry{
		{Path: "main.tf", Type: "blob"},
		{Path: "modules", Type: "tree"},
	}}
	a := newGitLabBackedClient(fake)
	lister, ok := a.(placementRepoLister)
	if !ok {
		t.Fatal("adapter does not satisfy placementRepoLister")
	}
	entries, err := lister.ListTree(context.Background(), "grp", "infra", "main")
	if err != nil {
		t.Fatalf("ListTree error: %v", err)
	}
	if len(entries) != 2 || entries[0].Path != "main.tf" || entries[0].Type != "blob" {
		t.Errorf("mapped tree entries = %+v", entries)
	}
}
