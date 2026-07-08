// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	iacgitlab "github.com/devopsmike2/squadron/internal/iac/gitlab"
)

// IaCGitLabClientFactory builds an iacgitlab.Client from a PAT. It is
// the GitLab sibling of IaCGitHubClientFactory: the production wire
// constructs a *iacgitlab.PATClient; tests inject a mock that records
// calls without touching real GitLab. The token is consumed inside the
// factory and never held by the handler.
type IaCGitLabClientFactory func(token string) iacgitlab.Client

// defaultIaCGitLabClientFactory is the production GitLab client
// factory. Wraps the operator's PAT in a PATClient. Mirrors
// defaultIaCGitHubClientFactory.
func defaultIaCGitLabClientFactory(token string) iacgitlab.Client {
	return iacgitlab.NewPATClient(token)
}

// newGitLabBackedClient wraps a GitLab client in an adapter that
// satisfies the GitHub-shaped iacgithub.Client interface the connect
// handlers program against. This is what lets a stored ProviderGitLab
// connection flow through the existing (provider-generic) handler
// surface with only the outbound API client differing.
func newGitLabBackedClient(inner iacgitlab.Client) iacgithub.Client {
	return &gitlabClientAdapter{inner: inner}
}

// gitlabClientAdapter adapts an iacgitlab.Client to iacgithub.Client.
// Besides the interface methods it also implements ListTree /
// GetFileContent (placementRepoLister) and GetBranchSHA
// (branchSHAGetter) — the two capabilities the handler reaches by type
// assertion — by delegating to the underlying GitLab client and
// mapping the projected types and sentinel errors across the two
// packages.
type gitlabClientAdapter struct {
	inner iacgitlab.Client
}

// gitlabTreeLister / gitlabBranchSHAGetter are the concrete-only GitLab
// capabilities (kept off iacgitlab.Client, mirroring the github
// package) that the adapter reaches by asserting the underlying value —
// exactly how the handler reaches them on the GitHub side.
type gitlabTreeLister interface {
	ListTree(ctx context.Context, owner, repo, ref string) ([]iacgitlab.TreeEntry, error)
}

type gitlabBranchSHAGetter interface {
	GetBranchSHA(ctx context.Context, owner, repo, branch string) (string, error)
}

// mapGitLabErr translates the gitlab package's sentinel errors into the
// github package's sentinels so the handler layer's errors.Is checks
// (which all reference the iacgithub.* sentinels) keep working
// unchanged for a GitLab-backed client.
func mapGitLabErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, iacgitlab.ErrDefaultBranchWriteRefused):
		return iacgithub.ErrDefaultBranchWriteRefused
	case errors.Is(err, iacgitlab.ErrAuthFailed):
		return iacgithub.ErrAuthFailed
	case errors.Is(err, iacgitlab.ErrRepoNotFound):
		return iacgithub.ErrRepoNotFound
	case errors.Is(err, iacgitlab.ErrFileNotFound):
		return iacgithub.ErrFileNotFound
	case errors.Is(err, iacgitlab.ErrFileAlreadyExists):
		return iacgithub.ErrFileAlreadyExists
	default:
		return err
	}
}

func (a *gitlabClientAdapter) GetRepo(ctx context.Context, owner, repo string) (*iacgithub.Repo, error) {
	r, err := a.inner.GetRepo(ctx, owner, repo)
	if err != nil {
		return nil, mapGitLabErr(err)
	}
	return &iacgithub.Repo{FullName: r.FullName, DefaultBranch: r.DefaultBranch}, nil
}

func (a *gitlabClientAdapter) GetFileContent(ctx context.Context, owner, repo, path, ref string) (*iacgithub.FileContent, error) {
	fc, err := a.inner.GetFileContent(ctx, owner, repo, path, ref)
	if err != nil {
		return nil, mapGitLabErr(err)
	}
	return &iacgithub.FileContent{
		Path:           fc.Path,
		SHA:            fc.SHA,
		Encoding:       fc.Encoding,
		Size:           fc.Size,
		DecodedContent: fc.DecodedContent,
	}, nil
}

func (a *gitlabClientAdapter) CreateBranch(ctx context.Context, owner, repo, branchName, fromSHA string) error {
	return mapGitLabErr(a.inner.CreateBranch(ctx, owner, repo, branchName, fromSHA))
}

func (a *gitlabClientAdapter) PutFileContent(ctx context.Context, opts iacgithub.PutFileOptions) (*iacgithub.CommitFileResult, error) {
	res, err := a.inner.PutFileContent(ctx, iacgitlab.PutFileOptions{
		Owner:   opts.Owner,
		Repo:    opts.Repo,
		Path:    opts.Path,
		Branch:  opts.Branch,
		Content: opts.Content,
		Message: opts.Message,
		FileSHA: opts.FileSHA,
	})
	if err != nil {
		return nil, mapGitLabErr(err)
	}
	return &iacgithub.CommitFileResult{BlobSHA: res.BlobSHA, CommitSHA: res.CommitSHA}, nil
}

func (a *gitlabClientAdapter) OpenPR(ctx context.Context, opts iacgithub.OpenPROptions) (*iacgithub.PullRequest, error) {
	pr, err := a.inner.OpenPR(ctx, iacgitlab.OpenPROptions{
		Owner: opts.Owner,
		Repo:  opts.Repo,
		Title: opts.Title,
		Body:  opts.Body,
		Head:  opts.Head,
		Base:  opts.Base,
	})
	if err != nil {
		return nil, mapGitLabErr(err)
	}
	return &iacgithub.PullRequest{Number: pr.Number, HTMLURL: pr.HTMLURL, HeadSHA: pr.HeadSHA}, nil
}

func (a *gitlabClientAdapter) AddLabels(ctx context.Context, owner, repo string, prNumber int, labels []string) error {
	return mapGitLabErr(a.inner.AddLabels(ctx, owner, repo, prNumber, labels))
}

func (a *gitlabClientAdapter) RequestReviewers(ctx context.Context, owner, repo string, prNumber int, teamSlugs []string) error {
	return mapGitLabErr(a.inner.RequestReviewers(ctx, owner, repo, prNumber, teamSlugs))
}

// ListTree lets the adapter satisfy placementRepoLister. It maps the
// GitLab TreeEntry slice onto the GitHub-shaped one.
func (a *gitlabClientAdapter) ListTree(ctx context.Context, owner, repo, ref string) ([]iacgithub.TreeEntry, error) {
	lister, ok := a.inner.(gitlabTreeLister)
	if !ok {
		return nil, fmt.Errorf("iac gitlab: client implementation does not support ListTree")
	}
	entries, err := lister.ListTree(ctx, owner, repo, ref)
	if err != nil {
		return nil, mapGitLabErr(err)
	}
	out := make([]iacgithub.TreeEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, iacgithub.TreeEntry{Path: e.Path, Type: e.Type})
	}
	return out, nil
}

// GetBranchSHA lets the adapter satisfy branchSHAGetter.
func (a *gitlabClientAdapter) GetBranchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	getter, ok := a.inner.(gitlabBranchSHAGetter)
	if !ok {
		return "", fmt.Errorf("iac gitlab: client implementation does not support GetBranchSHA")
	}
	sha, err := getter.GetBranchSHA(ctx, owner, repo, branch)
	return sha, mapGitLabErr(err)
}

// clientForProvider builds a provider-appropriate iacgithub.Client from
// a raw token. An empty / unknown / github provider yields the GitHub
// PATClient; a gitlab provider yields the GitLab-backed adapter. When
// the GitLab factory is unwired (should not happen — it is a
// constructor default), the handler falls back to the GitHub factory so
// the path degrades safely rather than panicking.
func (h *IaCGitHubHandlers) clientForProvider(provider, token string) iacgithub.Client {
	if iacconnstore.NormalizeProvider(provider) == iacconnstore.ProviderGitLab && h.gitlabClientFor != nil {
		return newGitLabBackedClient(h.gitlabClientFor(token))
	}
	return h.clientFor(token)
}

// clientForConn is the stored-connection variant of clientForProvider:
// it dispatches on the connection's Provider discriminator.
func (h *IaCGitHubHandlers) clientForConn(conn *iacconnstore.IaCConnection, token string) iacgithub.Client {
	if conn != nil {
		return h.clientForProvider(conn.Provider, token)
	}
	return h.clientFor(token)
}

// WithGitLabClientFactory overrides the GitLab client factory. Tests use
// this to inject a mock; production callers let the constructor default
// build a PATClient. Mirrors WithClientFactory.
func (h *IaCGitHubHandlers) WithGitLabClientFactory(f IaCGitLabClientFactory) *IaCGitHubHandlers {
	h.gitlabClientFor = f
	return h
}

// Compile-time assertions that the adapter satisfies the interfaces the
// handler reaches for.
var (
	_ iacgithub.Client    = (*gitlabClientAdapter)(nil)
	_ placementRepoLister = (*gitlabClientAdapter)(nil)
	_ branchSHAGetter     = (*gitlabClientAdapter)(nil)
)
